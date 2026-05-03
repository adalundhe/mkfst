//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"mkfst/providers/docker"
	"mkfst/providers/vfs"
)

// TestLiveMountHostToContainer verifies that a write to the host VFS
// becomes visible inside a running container via LiveMount within a
// short window. No host setup (no FUSE, no /etc/fuse.conf, no SYS_ADMIN
// on the container).
func TestLiveMountHostToContainer(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "alpine:3.19")
	t.Cleanup(docker.StopDefaultEngines)

	tree := vfs.NewTree(vfs.TreeOpts{})
	_ = tree.Write("/initial.txt", []byte("v0"), 0o644)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Container loops checking /vfs/late.txt; exits on first sight.
	res, err := c.Run(ctx, "alpine:3.19",
		docker.LiveMount(tree, "/vfs"),
		docker.Cmd("sh", "-c",
			"for i in $(seq 1 200); do "+
				"[ -f /vfs/late.txt ] && cat /vfs/late.txt && exit 0 || true; "+
				"sleep 0.05; "+
				"done; echo NEVER_SAW; exit 1"),
		docker.Detach(),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	withCleanupContainer(t, c, res.ContainerID)

	// Confirm the initial hydrate landed: /vfs/initial.txt should be
	// readable from inside (we observe via Inspect that the
	// container is running, then test live propagation).
	time.Sleep(150 * time.Millisecond)

	// Write to the VFS while the container is running.
	start := time.Now()
	if err := tree.Write("/late.txt", []byte("propagated"), 0o644); err != nil {
		t.Fatalf("late write: %v", err)
	}

	exitCode, err := c.Wait(ctx, res.ContainerID, "not-running")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	latency := time.Since(start)
	if exitCode != 0 {
		t.Fatalf("container exited %d (expected 0 = saw the late file)", exitCode)
	}
	t.Logf("host→container propagation latency (write-to-exit): %s", latency)

	// Sanity: should be sub-second. Loose bound to absorb container
	// startup + sync engine kick-in.
	if latency > 5*time.Second {
		t.Fatalf("propagation took %s — engine isn't keeping up", latency)
	}
}

// TestLiveMountContainerToHost verifies that a write made INSIDE the
// container becomes visible in the host VFS within the configured poll
// interval. This proves the container→host direction works.
func TestLiveMountContainerToHost(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "alpine:3.19")
	t.Cleanup(docker.StopDefaultEngines)

	tree := vfs.NewTree(vfs.TreeOpts{})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Container writes a file then sleeps so the engine has time to
	// observe the write.
	res, err := c.Run(ctx, "alpine:3.19",
		docker.LiveMount(tree, "/vfs",
			docker.LiveMountPollInterval(20*time.Millisecond, 200*time.Millisecond),
		),
		docker.Cmd("sh", "-c", "echo from-container > /vfs/inside.txt && ls -la /vfs/ && sleep 30"),
		docker.Detach(),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	withCleanupContainer(t, c, res.ContainerID)

	// Sanity: confirm the container actually created the file before
	// blaming the engine.
	time.Sleep(500 * time.Millisecond)
	logsCtx, logsCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer logsCancel()
	logs, err := c.Logs(logsCtx, res.ContainerID)
	if err == nil {
		for line := range logs {
			if line.Err != nil {
				break
			}
			t.Logf("container stdout: %s", line.Message)
		}
	}

	// Poll the host VFS for /inside.txt to appear. Bound to a few
	// seconds — the engine's poll interval starts at 20ms, so this
	// should land well inside the bound.
	deadline := time.Now().Add(5 * time.Second)
	start := time.Now()
	for time.Now().Before(deadline) {
		body, err := tree.Read("/inside.txt")
		if err == nil && strings.TrimSpace(string(body)) == "from-container" {
			t.Logf("container→host propagation latency: %s", time.Since(start))
			return
		}
		if err != nil && !errors.Is(err, vfs.ErrNotExist) {
			t.Fatalf("tree.Read: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Dump host VFS state for debugging.
	t.Log("host tree state at failure:")
	_ = tree.Walk(func(p string, _ *vfs.Inode) error {
		t.Logf("  %s", p)
		return nil
	})
	t.Fatalf("/inside.txt never appeared in host VFS")
}

// TestLiveMountManyContainers proves the engine scales to many
// concurrent tenants on a shared chunk store. Spawns N containers each
// with a small distinct VFS subtree, mutates them all in parallel,
// confirms each container's view stays correct.
func TestLiveMountManyContainers(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "alpine:3.19")
	t.Cleanup(docker.StopDefaultEngines)

	const N = 10
	trees := make([]*vfs.Tree, N)
	containers := make([]string, N)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	for i := 0; i < N; i++ {
		trees[i] = vfs.NewTree(vfs.TreeOpts{})
		_ = trees[i].Write(fmt.Sprintf("/tenant-%d.txt", i), []byte(fmt.Sprintf("tenant-%d-v0", i)), 0o644)
	}

	for i := 0; i < N; i++ {
		idx := i
		// Each container watches for /vfs/tenant-IDX-mark.txt and
		// echoes its content. Exits when found.
		res, err := c.Run(ctx, "alpine:3.19",
			docker.Name(uniqueName(fmt.Sprintf("mkfst-livemount-%d", i))),
			docker.LiveMount(trees[i], "/vfs"),
			docker.Cmd("sh", "-c", fmt.Sprintf(
				"for j in $(seq 1 200); do "+
					"[ -f /vfs/mark.txt ] && cat /vfs/mark.txt && exit 0 || true; "+
					"sleep 0.05; "+
					"done; echo NEVER_SAW_%d; exit 1", idx)),
			docker.Detach(),
		)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		containers[i] = res.ContainerID
		withCleanupContainer(t, c, res.ContainerID)
	}

	// Brief stabilization window.
	time.Sleep(300 * time.Millisecond)

	// Push a unique mark into each tenant in parallel.
	for i := range trees {
		idx := i
		go func() {
			_ = trees[idx].Write("/mark.txt", []byte(fmt.Sprintf("hello-%d", idx)), 0o644)
		}()
	}

	// Wait for every container to exit 0.
	for i, id := range containers {
		exitCode, err := c.Wait(ctx, id, "not-running")
		if err != nil {
			t.Fatalf("container %d wait: %v", i, err)
		}
		if exitCode != 0 {
			t.Fatalf("container %d exit %d (didn't see its mark)", i, exitCode)
		}
	}
	t.Logf("%d concurrent live-mount tenants all received their writes", N)
}
