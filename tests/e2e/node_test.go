//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/docker/go-connections/nat"

	"mkfst/providers/docker"
	"mkfst/providers/vfs"
)

// TestNodeHTTPServerBuildAndCurl is the meatiest scenario: build a Node
// HTTP server in VFS, run it with a published port, and then curl the
// host port to confirm the container is actually serving requests.
//
// This exercises:
//   - VFS as build context with multiple files
//   - Build → Run with a port mapping (PortMap option)
//   - Container readiness polling from the host
//   - End-to-end network reachability through Docker's published port
func TestNodeHTTPServerBuildAndCurl(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	tree := vfs.NewTree(vfs.TreeOpts{})

	const server = `const http = require('http');
const port = 8080;
const message = process.env.MKFST_MSG || 'hello from node';
const srv = http.createServer((req, res) => {
  res.writeHead(200, {'Content-Type': 'text/plain'});
  res.end(message + '\n');
});
srv.listen(port, () => console.log('listening on ' + port));
`
	const dockerfile = `# syntax=docker/dockerfile:1
FROM node:20-alpine
WORKDIR /app
COPY server.js ./
EXPOSE 8080
CMD ["node", "/app/server.js"]
`
	_ = tree.Write("/server.js", []byte(server), 0o644)
	_ = tree.Write("/Dockerfile", []byte(dockerfile), 0o644)

	imageTag := uniqueName("mkfst-e2e-node")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	events, err := c.Build(ctx, docker.NewVFSSource(tree), docker.Tag(imageTag))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	imageID, _ := drainBuildEvents(t, events)
	withCleanupImage(t, c, imageID)

	// Run with a host-side ephemeral port; we'll discover what the daemon
	// chose via Inspect.
	result, err := c.Run(ctx, imageTag,
		docker.Port(docker.PortMap{ContainerPort: 8080, Protocol: "tcp"}),
		docker.Env("MKFST_MSG", "served via mkfst e2e"),
		docker.Detach(),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	withCleanupContainer(t, c, result.ContainerID)

	hostPort, err := publishedPort(t, c, result.ContainerID, "8080/tcp")
	if err != nil {
		t.Fatalf("discover host port: %v", err)
	}
	t.Logf("container %s published 8080 → %d", result.ContainerID[:12], hostPort)

	// Wait for the server to bind. node's http.createServer is fast but
	// there's still container start latency.
	url := fmt.Sprintf("http://127.0.0.1:%d/", hostPort)
	body, err := waitForHTTP(t, url, 30*time.Second)
	if err != nil {
		t.Fatalf("waiting for server: %v", err)
	}
	if !strings.Contains(body, "served via mkfst e2e") {
		t.Fatalf("response body: %q", body)
	}
}

// publishedPort inspects the container and returns the host-side port
// the daemon chose for the named container port. Polls briefly during
// startup before giving up.
func publishedPort(t *testing.T, c *docker.Client, containerID, containerPort string) (int, error) {
	t.Helper()
	port := nat.Port(containerPort)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err := c.Inspect(ctx, containerID)
		cancel()
		if err != nil {
			return 0, err
		}
		if info.NetworkSettings != nil {
			if bindings, ok := info.NetworkSettings.Ports[port]; ok && len(bindings) > 0 {
				var p int
				_, err := fmt.Sscanf(bindings[0].HostPort, "%d", &p)
				if err == nil && p > 0 {
					return p, nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("port %s never published", containerPort)
}

// waitForHTTP polls url with GET until it gets a 200 or the deadline
// expires. Returns the body of the first 200 response. The TCP-level
// pre-probe lets us spin tightly while the container is still starting
// without log-spamming http.Client retries.
func waitForHTTP(t *testing.T, rawURL string, timeout time.Duration) (string, error) {
	t.Helper()
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	hostport := parsed.Host

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", hostport, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			// Port is open; try a real GET.
			req, _ := http.NewRequest("GET", rawURL, nil)
			cli := &http.Client{Timeout: 2 * time.Second}
			resp, err := cli.Do(req)
			if err == nil && resp.StatusCode == 200 {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return string(body), nil
			}
			if resp != nil {
				resp.Body.Close()
			}
			lastErr = err
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("never reached %s", rawURL)
	}
	return "", lastErr
}
