//go:build docker_integration

package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"

	"mkfst/providers/vfs"
)

// requireDaemon constructs a Client and skips the test if the daemon is
// unreachable. Lets `-tags=docker_integration` test runs work in
// environments without docker (CI sandboxes, dev laptops with the daemon
// off) without flaking the suite.
func requireDaemon(t *testing.T) *Client {
	t.Helper()
	c, err := New(Opts{Timeout: 3 * time.Second})
	if err != nil {
		if errors.Is(err, ErrUnreachable) {
			t.Skipf("docker daemon unreachable: %v", err)
		}
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestDockerIntegrationPullSmallImage(t *testing.T) {
	c := requireDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	events, err := c.Pull(ctx, "alpine:3.19")
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	final, err := DrainEvents(events)
	if err != nil {
		t.Fatalf("Pull stream error: %v", err)
	}
	if final.Kind != EventDone {
		t.Fatalf("expected EventDone, got %+v", final)
	}
}

func TestDockerIntegrationBuildFromVFS(t *testing.T) {
	c := requireDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Put a tiny Dockerfile in an in-memory VFS — the integration we care
	// about is "VFS → tar → daemon" working end-to-end.
	tree := vfs.NewTree(vfs.TreeOpts{})
	_ = tree.Write("/Dockerfile", []byte("FROM alpine:3.19\nRUN echo hello > /hello.txt\n"), 0o644)

	events, err := c.Build(ctx, NewVFSSource(tree),
		Tag("mkfst-vfs-build:test"),
		Pull(),
		KeepIntermediate(),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var imageID string
	for ev := range events {
		switch ev.Kind {
		case EventError:
			t.Fatalf("build error: %v (msg=%q)", ev.Err, ev.Message)
		case EventAux:
			if id := ev.ImageID(); id != "" {
				imageID = id
			}
		}
	}
	if imageID == "" {
		t.Fatalf("no image ID surfaced from Build event stream")
	}
	t.Logf("built image: %s", imageID)

	// Cleanup: best-effort image removal so re-running tests doesn't pile
	// up untagged images.
	t.Cleanup(func() {
		_, _ = c.SDK().ImageRemove(context.Background(), imageID, image.RemoveOptions{
			Force:         true,
			PruneChildren: true,
		})
	})
}

func TestDockerIntegrationRunAndLogs(t *testing.T) {
	c := requireDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Use an image that's almost certainly cached on any docker host.
	pullEvents, err := c.Pull(ctx, "alpine:3.19")
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := DrainEvents(pullEvents); err != nil {
		t.Fatalf("Pull drain: %v", err)
	}

	result, err := c.Run(ctx, "alpine:3.19",
		Cmd("sh", "-c", "echo from-stdout; echo from-stderr 1>&2; exit 0"),
		AutoRemove(),
		WaitForExit(),
	)
	if err != nil {
		// AutoRemove + WaitForExit can race on the daemon side: container
		// vanishes before Wait returns its status, surfacing as "no such
		// container". Treat that as success when the container ran.
		if !strings.Contains(err.Error(), "No such container") {
			t.Fatalf("Run: %v", err)
		}
	}
	if result == nil || result.ContainerID == "" {
		t.Fatalf("RunResult: %+v", result)
	}
	if result.ExitCode != 0 && err == nil {
		t.Fatalf("expected exit 0, got %d", result.ExitCode)
	}

	t.Logf("ran container %s exit=%d", result.ContainerID, result.ExitCode)
}

func TestDockerIntegrationLifecycle(t *testing.T) {
	c := requireDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pullEvents, err := c.Pull(ctx, "alpine:3.19")
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := DrainEvents(pullEvents); err != nil {
		t.Fatalf("Pull drain: %v", err)
	}

	result, err := c.Run(ctx, "alpine:3.19",
		Cmd("sleep", "30"),
		Detach(),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	id := result.ContainerID

	t.Cleanup(func() {
		_ = c.Remove(context.Background(), id, RemoveOpts{Force: true})
	})

	// Stop with a short timeout; container is sleep(30), so SIGKILL after.
	timeout := 1 * time.Second
	if err := c.Stop(ctx, id, &timeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Inspect should now show the stopped container with State.Running=false.
	info, err := c.Inspect(ctx, id)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.State == nil {
		t.Fatalf("inspect state nil")
	}
	if info.State.Running {
		t.Fatalf("container still reports Running=true after Stop")
	}

	if err := c.Remove(ctx, id, RemoveOpts{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// List should no longer include the container.
	list, err := c.List(ctx, WithListAll())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, ci := range list {
		if ci.ID == id {
			t.Fatalf("removed container %s still in list", id)
		}
	}
}

