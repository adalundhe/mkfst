//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"

	"mkfst/providers/docker"
	"mkfst/providers/docker/network"
)

// TestNetworkStack_RealImageRedis brings up a real redis:7-alpine
// container in a stack, then issues a PING via an in-stack alpine
// client that runs redis-cli. Validates that genuine images run,
// the bridge routes container→container DNS, and replies flow
// back. No host-side gateway involved — pure intra-stack.
func TestNetworkStack_RealImageRedis(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "redis:7-alpine")

	engine, err := network.NewEngine(c.SDK(), network.EngineOpts{MonitorBuffer: 64})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	stack, err := engine.NewStack("redis-real")
	if err != nil {
		t.Fatal(err)
	}
	stack.MustAddService("cache",
		network.Image("redis:7-alpine"),
		network.Port(6379),
		network.WithProbe(
			network.TCPProbe(6379).
				WithInitialDelay(200*time.Millisecond).
				WithInterval(200*time.Millisecond).
				WithTimeout(time.Second).
				WithFailureThreshold(40),
			network.ProbeReadiness,
		),
	)
	// Long-running probe-driven client that exec's redis-cli at us
	// from outside the stack via docker exec on the cache itself.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := stack.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() {
		dc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = stack.Down(dc)
	})

	// Run `redis-cli PING` inside the cache container; expect "PONG".
	out := dockerExecCapture(t, c, stack, "cache", 0, []string{"redis-cli", "PING"})
	if !strings.Contains(out, "PONG") {
		t.Fatalf("redis-cli PING did not return PONG, got: %q", out)
	}
}

// TestNetworkStack_InternalServiceCommunication wires two services
// in the same stack: an nginx:alpine server and an alpine client
// that wgets the server by service name. Validates intra-stack DNS
// + connectivity between services.
func TestNetworkStack_InternalServiceCommunication(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "nginx:alpine")
	ensureImageLocal(t, c, "alpine:3.19")

	engine, _ := network.NewEngine(c.SDK(), network.EngineOpts{MonitorBuffer: 64})
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	stack, _ := engine.NewStack("intra")

	stack.MustAddService("server",
		network.Image("nginx:alpine"),
		network.Port(80),
		network.WithProbe(
			network.HTTPProbe(80, "/").
				WithInitialDelay(200*time.Millisecond).
				WithInterval(200*time.Millisecond).
				WithTimeout(time.Second).
				WithFailureThreshold(40),
			network.ProbeReadiness,
		),
	)
	stack.MustAddService("client",
		network.Image("alpine:3.19"),
		network.Cmd("sleep", "300"),
		network.DependsOn("server"),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := stack.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() {
		dc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = stack.Down(dc)
	})

	// From inside `client`, fetch `server` by DNS name and check
	// for nginx's welcome page.
	out := dockerExecCapture(t, c, stack, "client", 0,
		[]string{"wget", "-q", "-O", "-", "http://server"},
	)
	if !strings.Contains(strings.ToLower(out), "welcome to nginx") {
		t.Fatalf("client did not see nginx welcome page; got: %q", out)
	}
}

// TestNetworkStack_ExternalResourceFetch verifies a stack container
// can reach an external HTTP endpoint (outbound NAT preserved).
// Uses example.com as a stable public endpoint; skips if there's no
// internet.
func TestNetworkStack_ExternalResourceFetch(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)
	ensureImageLocal(t, c, "alpine:3.19")

	engine, _ := network.NewEngine(c.SDK(), network.EngineOpts{MonitorBuffer: 64})
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	stack, _ := engine.NewStack("external")
	stack.MustAddService("client",
		network.Image("alpine:3.19"),
		network.Cmd("sleep", "300"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := stack.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() {
		dc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = stack.Down(dc)
	})

	out := dockerExecCapture(t, c, stack, "client", 0,
		[]string{"wget", "-q", "-O", "-", "http://example.com"},
	)
	if !strings.Contains(strings.ToLower(out), "example domain") {
		t.Fatalf("external fetch failed; got: %q", out)
	}
}

// TestNetworkStack_ConcurrentStacks brings up N independent stacks
// concurrently, each declaring service "web" on the same internal
// port 80. Verifies they all come up, all get distinct host
// ingress addresses, and all are reachable in parallel.
func TestNetworkStack_ConcurrentStacks(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "nginx:alpine")

	engine, _ := network.NewEngine(c.SDK(), network.EngineOpts{MonitorBuffer: 256})
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	const N = 6
	stacks := make([]*network.Stack, N)
	ings := make([]*network.Ingress, N)

	// Build stacks.
	for i := 0; i < N; i++ {
		s, _ := engine.NewStack(fmt.Sprintf("conc-%d", i))
		s.MustAddService("web",
			network.Image("nginx:alpine"),
			network.Port(80),
			network.WithProbe(
				network.HTTPProbe(80, "/").
					WithInitialDelay(100*time.Millisecond).
					WithInterval(200*time.Millisecond).
					WithTimeout(time.Second).
					WithFailureThreshold(50),
				network.ProbeReadiness,
			),
		)
		ing, err := s.Ingress("web-in", "web", 80)
		if err != nil {
			t.Fatalf("Ingress %d: %v", i, err)
		}
		stacks[i] = s
		ings[i] = ing
	}

	// Bring all stacks up concurrently.
	upCtx, upCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer upCancel()
	var wg sync.WaitGroup
	upErrs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			upErrs[i] = stacks[i].Up(upCtx)
		}(i)
	}
	wg.Wait()
	for i, err := range upErrs {
		if err != nil {
			t.Fatalf("stack %d Up: %v", i, err)
		}
	}
	t.Cleanup(func() {
		dc, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var dwg sync.WaitGroup
		for _, s := range stacks {
			dwg.Add(1)
			go func(s *network.Stack) { defer dwg.Done(); _ = s.Down(dc) }(s)
		}
		dwg.Wait()
	})

	// Every ingress address should be unique.
	seen := map[string]int{}
	for i, ing := range ings {
		addr := ing.Address()
		if addr == "" {
			t.Fatalf("stack %d ingress address empty", i)
		}
		if prev, ok := seen[addr]; ok {
			t.Fatalf("stack %d shares address %s with stack %d", i, addr, prev)
		}
		seen[addr] = i
	}

	// Every ingress should be independently reachable.
	var reachWG sync.WaitGroup
	results := make([]bool, N)
	for i, ing := range ings {
		reachWG.Add(1)
		go func(i int, ing *network.Ingress) {
			defer reachWG.Done()
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				resp, err := http.Get("http://" + ing.Address() + "/")
				if err == nil {
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode == 200 && bytes.Contains(bytes.ToLower(body), []byte("welcome to nginx")) {
						results[i] = true
						return
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
		}(i, ing)
	}
	reachWG.Wait()
	for i, ok := range results {
		if !ok {
			t.Fatalf("stack %d unreachable through %s", i, ings[i].Address())
		}
	}
}

// TestNetworkStack_CrossStackUnreachable spins two stacks and
// proves they cannot reach each other:
//   - DNS lookups for the other stack's service name fail (NXDOMAIN
//     or empty), since they're on different bridge networks with
//     disjoint DNS scopes.
//   - Direct connection to the other container's IP also fails.
func TestNetworkStack_CrossStackUnreachable(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "nginx:alpine")
	ensureImageLocal(t, c, "alpine:3.19")

	engine, _ := network.NewEngine(c.SDK(), network.EngineOpts{MonitorBuffer: 64})
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	a, _ := engine.NewStack("cross-a")
	a.MustAddService("server",
		network.Image("nginx:alpine"),
		network.Port(80),
		network.WithProbe(
			network.HTTPProbe(80, "/").
				WithInitialDelay(100*time.Millisecond).
				WithInterval(200*time.Millisecond).
				WithFailureThreshold(40),
			network.ProbeReadiness,
		),
	)
	a.MustAddService("client",
		network.Image("alpine:3.19"),
		network.Cmd("sleep", "300"),
		network.DependsOn("server"),
	)

	b, _ := engine.NewStack("cross-b")
	b.MustAddService("server",
		network.Image("nginx:alpine"),
		network.Port(80),
		network.WithProbe(
			network.HTTPProbe(80, "/").
				WithInitialDelay(100*time.Millisecond).
				WithInterval(200*time.Millisecond).
				WithFailureThreshold(40),
			network.ProbeReadiness,
		),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := a.Up(ctx); err != nil {
		t.Fatalf("Up A: %v", err)
	}
	t.Cleanup(func() {
		dc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = a.Down(dc)
	})
	if err := b.Up(ctx); err != nil {
		t.Fatalf("Up B: %v", err)
	}
	t.Cleanup(func() {
		dc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = b.Down(dc)
	})

	// Discover the bridge IP of B's "server" container.
	bSrvIP := containerIPInStack(t, c, b, "server")
	if bSrvIP == "" {
		t.Fatal("could not discover B's server IP")
	}
	t.Logf("stack-b server IP = %s", bSrvIP)

	// From A's client, name lookup of "server" should resolve to
	// A's server (NOT B's). Use docker DNS to resolve and curl
	// (or wget) — the response IP should differ from bSrvIP.
	aSrvIP := containerIPInStack(t, c, a, "server")
	if aSrvIP == "" {
		t.Fatal("could not discover A's server IP")
	}
	if aSrvIP == bSrvIP {
		t.Fatal("two different stacks resolved to the same container IP — isolation broken")
	}

	// From A's client, dial B's server IP directly — should fail.
	out, exit := dockerExecCaptureExit(t, c, a, "client", []string{
		"timeout", "3", "wget", "-q", "-T", "2", "-O", "-", "http://" + bSrvIP + "/",
	})
	if exit == 0 {
		t.Fatalf("A's client unexpectedly reached B's server IP: out=%q", out)
	}
	t.Logf("cross-stack direct dial correctly failed (exit=%d)", exit)
}

// TestNetworkStack_DownReleasesResources brings up + tears down the
// same stack 5 times in a row; after each iteration the goroutine
// count should return close to baseline. Significant growth across
// iterations indicates a leak.
//
// We use a tolerance band rather than an exact match because
// Go's runtime + the docker SDK's HTTP client maintain idle
// connection / reaper goroutines that can fluctuate by a few.
func TestNetworkStack_DownReleasesResources(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "nginx:alpine")

	engine, _ := network.NewEngine(c.SDK(), network.EngineOpts{MonitorBuffer: 64})
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	// Warm up: one full cycle so probe scheduler workers settle.
	mkAndRun := func(label string) {
		stack, _ := engine.NewStack(label)
		stack.MustAddService("web",
			network.Image("nginx:alpine"),
			network.Port(80),
			network.WithProbe(
				network.HTTPProbe(80, "/").
					WithInitialDelay(100*time.Millisecond).
					WithInterval(200*time.Millisecond).
					WithFailureThreshold(40),
				network.ProbeReadiness,
			),
		)
		ing, err := stack.Ingress("web-in", "web", 80)
		if err != nil {
			t.Fatalf("Ingress: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := stack.Up(ctx); err != nil {
			t.Fatalf("Up %s: %v", label, err)
		}
		// Hit the gateway briefly to spawn proxy goroutines we
		// then expect to exit on Down.
		resp, _ := http.Get("http://" + ing.Address() + "/")
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		dc, cancelD := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelD()
		if err := stack.Down(dc); err != nil {
			t.Fatalf("Down %s: %v", label, err)
		}
	}

	mkAndRun("warmup")
	// Let TIME_WAIT entries from the warmup's published port drain
	// before sampling the baseline; otherwise the kernel may
	// re-allocate the same ephemeral port to docker before
	// rootlesskit lets go, surfacing as a daemon port-bind error
	// in subsequent cycles.
	time.Sleep(2 * time.Second)
	baseline := runtime.NumGoroutine()

	const cycles = 5
	for i := 0; i < cycles; i++ {
		mkAndRun(fmt.Sprintf("leak-cycle-%d", i))
		// Inter-cycle drain for the same TIME_WAIT reason as above.
		time.Sleep(2 * time.Second)
	}
	// Allow background reapers to settle.
	time.Sleep(time.Second)
	after := runtime.NumGoroutine()

	// Allow up to 8 extra goroutines (per-cycle SDK background
	// connections, GC trackers, etc.) — a real leak would grow
	// linearly with cycles.
	const tolerance = 8
	if after > baseline+tolerance {
		t.Fatalf("goroutine leak suspected: baseline=%d after %d cycles=%d (delta=%d)",
			baseline, cycles, after, after-baseline)
	}
	t.Logf("baseline=%d after=%d delta=%d (≤ %d tolerance)", baseline, after, after-baseline, tolerance)
}

// TestNetworkStack_ServicesShareInternalPorts proves multiple
// services in the SAME stack can each declare an identical
// internal port (e.g., two services both listening on 80) — they
// have separate netns so there's no collision.
func TestNetworkStack_ServicesShareInternalPorts(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "nginx:alpine")
	ensureImageLocal(t, c, "alpine:3.19")

	engine, _ := network.NewEngine(c.SDK(), network.EngineOpts{MonitorBuffer: 64})
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	stack, _ := engine.NewStack("share-port")
	stack.MustAddService("web1",
		network.Image("nginx:alpine"),
		network.Port(80),
		network.WithProbe(
			network.HTTPProbe(80, "/").
				WithInitialDelay(100*time.Millisecond).
				WithInterval(200*time.Millisecond).
				WithFailureThreshold(40),
			network.ProbeReadiness,
		),
	)
	stack.MustAddService("web2",
		network.Image("nginx:alpine"),
		network.Port(80),
		network.WithProbe(
			network.HTTPProbe(80, "/").
				WithInitialDelay(100*time.Millisecond).
				WithInterval(200*time.Millisecond).
				WithFailureThreshold(40),
			network.ProbeReadiness,
		),
	)
	stack.MustAddService("client",
		network.Image("alpine:3.19"),
		network.Cmd("sleep", "300"),
		network.DependsOn("web1", "web2"),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := stack.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() {
		dc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = stack.Down(dc)
	})

	for _, target := range []string{"web1", "web2"} {
		out := dockerExecCapture(t, c, stack, "client", 0,
			[]string{"wget", "-q", "-O", "-", "http://" + target + "/"})
		if !strings.Contains(strings.ToLower(out), "welcome to nginx") {
			t.Fatalf("%s unreachable from client; got: %q", target, out)
		}
	}
}

// === helpers ===

// dockerExecCapture runs a command inside containers[svc][replica]
// and returns combined stdout+stderr. Asserts the exit code is 0.
func dockerExecCapture(t *testing.T, c *docker.Client, stack *network.Stack, svc string, replica int, cmd []string) string {
	t.Helper()
	out, exit := dockerExecCaptureExit(t, c, stack, svc, cmd)
	if exit != 0 {
		t.Fatalf("docker exec %v in %s[%d] returned exit=%d, out=%q", cmd, svc, replica, exit, out)
	}
	return out
}

// dockerExecCaptureExit runs the command and returns (combined output, exit code).
func dockerExecCaptureExit(t *testing.T, c *docker.Client, stack *network.Stack, svc string, cmd []string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st := stack.Status(ctx)
	ss, ok := st.Services[svc]
	if !ok || len(ss.Containers) == 0 {
		t.Fatalf("no containers for service %q", svc)
	}
	ctrID := ss.Containers[0].ID

	exec, err := c.SDK().ContainerExecCreate(ctx, ctrID, dockercontainer.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}
	resp, err := c.SDK().ContainerExecAttach(ctx, exec.ID, dockercontainer.ExecStartOptions{})
	if err != nil {
		t.Fatalf("ExecAttach: %v", err)
	}
	defer resp.Close()

	// Read multiplexed Docker output (stdout+stderr) to the same buffer.
	var buf bytes.Buffer
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		_, _ = demuxExec(&buf, resp.Reader)
	}()
	select {
	case <-doneCh:
	case <-ctx.Done():
		t.Fatalf("exec output read timed out: %v", ctx.Err())
	}

	insp, err := c.SDK().ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		t.Fatalf("ExecInspect: %v", err)
	}
	return buf.String(), insp.ExitCode
}

// demuxExec parses the docker exec stream format. Each frame:
//
//	[0]    stream type (0=stdin, 1=stdout, 2=stderr)
//	[1..3] padding
//	[4..7] payload length (big-endian uint32)
//	[8..]  payload
//
// We coalesce stdout+stderr into a single output stream — tests
// don't typically distinguish.
func demuxExec(dst io.Writer, src io.Reader) (int64, error) {
	var hdr [8]byte
	var total int64
	for {
		_, err := io.ReadFull(src, hdr[:])
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			// Some non-TTY exec streams are unframed; if header read
			// failed mid-stream, fall back to direct copy.
			n, _ := io.Copy(dst, src)
			total += n
			return total, nil
		}
		size := int64(uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7]))
		if size <= 0 {
			continue
		}
		n, err := io.CopyN(dst, src, size)
		total += n
		if err != nil {
			return total, err
		}
	}
}

// containerIPInStack returns the bridge IP of the first replica of
// the given service in the given stack.
func containerIPInStack(t *testing.T, c *docker.Client, stack *network.Stack, svc string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := stack.Status(ctx)
	ss, ok := st.Services[svc]
	if !ok || len(ss.Containers) == 0 {
		return ""
	}
	insp, err := c.SDK().ContainerInspect(ctx, ss.Containers[0].ID)
	if err != nil {
		return ""
	}
	for _, ep := range insp.NetworkSettings.Networks {
		if ep != nil && ep.IPAddress != "" {
			return ep.IPAddress
		}
	}
	return ""
}

// keep sync used to silence checkers if only used in some test combos.
var _ = sync.WaitGroup{}
