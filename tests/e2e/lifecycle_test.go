//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"mkfst/providers/docker"
)

// TestFullLifecycle exercises every container-management method in
// sequence: Pull → Run(detach) → Inspect → Logs(follow) → Stop →
// post-stop Inspect → Remove → List (verify gone).
//
// Acts as a smoke test for the docker provider's lifecycle surface.
// If anything in this chain regresses, this test catches it.
func TestFullLifecycle(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Pull
	pullEvents, err := c.Pull(ctx, "alpine:3.19")
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if drainErr := drainPullEvents(pullEvents); drainErr != nil {
		t.Fatalf("Pull drain: %v", drainErr)
	}

	// Run a long-running container so we can inspect/log/stop it.
	result, err := c.Run(ctx, "alpine:3.19",
		docker.Name(uniqueName("mkfst-e2e-lifecycle")),
		docker.Cmd("sh", "-c", "while true; do echo tick; sleep 1; done"),
		docker.Detach(),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	withCleanupContainer(t, c, result.ContainerID)

	// Inspect — confirm Running.
	info, err := c.Inspect(ctx, result.ContainerID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.State == nil || !info.State.Running {
		t.Fatalf("container should be running, state=%+v", info.State)
	}

	// Logs(Follow) — collect a few ticks to confirm streaming works,
	// then break out so we don't block forever.
	logsCtx, logsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer logsCancel()
	logs, err := c.Logs(logsCtx, result.ContainerID, docker.Follow())
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	var ticks int
	for line := range logs {
		if line.Err != nil {
			break
		}
		if strings.Contains(line.Message, "tick") {
			ticks++
			if ticks >= 2 {
				logsCancel() // bail out of Follow
				break
			}
		}
	}
	if ticks < 2 {
		t.Fatalf("expected ≥2 tick lines, got %d", ticks)
	}

	// Stop — short timeout because the cmd has no signal handler, so
	// sigterm + 1s + sigkill is the path the daemon takes.
	stopTimeout := 1 * time.Second
	if err := c.Stop(ctx, result.ContainerID, &stopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Inspect post-Stop.
	state := containerStateAfterStop(t, c, result.ContainerID)
	if state == nil {
		t.Fatalf("nil state post-stop")
	}
	if state.Running {
		t.Fatalf("container still Running after Stop")
	}

	// Remove.
	if err := c.Remove(ctx, result.ContainerID, docker.RemoveOpts{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// List — container should not appear.
	list, err := c.List(ctx, docker.WithListAll())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, ci := range list {
		if ci.ID == result.ContainerID {
			t.Fatalf("removed container %s still in List output", result.ContainerID)
		}
	}
}

// TestKillSendsSignal verifies the Kill helper delivers a specific
// signal (rather than just SIGTERM/SIGKILL via Stop). We use an alpine
// shell that traps SIGUSR1 and exits cleanly when it receives one.
func TestKillSendsSignal(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if events, err := c.Pull(ctx, "alpine:3.19"); err != nil {
		t.Fatalf("Pull: %v", err)
	} else {
		_ = drainPullEvents(events)
	}

	result, err := c.Run(ctx, "alpine:3.19",
		docker.Cmd("sh", "-c",
			"trap 'echo got-usr1; exit 0' USR1; "+
				"while true; do sleep 0.2; done"),
		docker.Detach(),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	withCleanupContainer(t, c, result.ContainerID)

	// Give the trap a moment to install before we send the signal.
	time.Sleep(500 * time.Millisecond)

	if err := c.Kill(ctx, result.ContainerID, "SIGUSR1"); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	exitCode, err := c.Wait(ctx, result.ContainerID, "not-running")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit 0 (clean USR1 handler), got %d", exitCode)
	}

	// Verify the trap actually fired.
	logs, err := c.Logs(ctx, result.ContainerID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	var sb strings.Builder
	for line := range logs {
		if line.Err != nil {
			break
		}
		sb.WriteString(line.Message)
	}
	if !strings.Contains(sb.String(), "got-usr1") {
		t.Fatalf("trap didn't fire: %q", sb.String())
	}
}
