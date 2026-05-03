//go:build e2e

// Package e2e holds end-to-end tests that exercise the providers/vfs +
// providers/docker + providers/files trio against a real Docker daemon
// (and optionally a FUSE mount). Tagged `e2e` so they don't run by
// default — invoke with:
//
//	go test -tags e2e -count=1 -v ./tests/e2e/
//
// Each test uses requireDaemon (and optionally requireFUSE,
// requireInternet) so missing prerequisites turn into clean skips, not
// flaky failures.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"

	"mkfst/providers/docker"
)

// requireDaemon constructs a docker Client and skips the test if the
// daemon is unreachable (no daemon, no permission, wrong endpoint). The
// returned client is closed via t.Cleanup.
func requireDaemon(t *testing.T) *docker.Client {
	t.Helper()
	c, err := docker.New(docker.Opts{Timeout: 5 * time.Second})
	if err != nil {
		if errors.Is(err, docker.ErrUnreachable) {
			t.Skipf("docker daemon unreachable: %v", err)
		}
		t.Fatalf("docker.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// tmpfsMountpoint returns a fresh, RAM-backed directory suitable for
// FUSE-mounting against. Honors the project's strict "no disk, no /tmp"
// rule by exclusively using XDG_RUNTIME_DIR (= /run/user/$UID), which
// systemd guarantees to be tmpfs and user-owned. Falls back to
// /run/user/$UID by hand if XDG_RUNTIME_DIR is unset (rare, but happens
// in raw container environments without a session manager).
//
// Skips the test if no tmpfs runtime dir is available — disk fallback
// is intentionally not provided.
//
// Cleanup is registered automatically: the mountpoint dir is removed
// after the test, but only AFTER the caller's own t.Cleanup that
// unmounts FUSE has run (Go's t.Cleanup runs in LIFO order, so this
// helper's cleanup runs first when read in source order; we register
// LAST so we run LAST in cleanup, which is what we want — unmount
// before rmdir).
func tmpfsMountpoint(t *testing.T, prefix string) string {
	t.Helper()
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		uid := os.Getuid()
		fallback := fmt.Sprintf("/run/user/%d", uid)
		if info, err := os.Stat(fallback); err == nil && info.IsDir() {
			runtimeDir = fallback
		}
	}
	if runtimeDir == "" {
		t.Skipf("no XDG_RUNTIME_DIR (and /run/user/$UID missing) — strict-no-disk policy forbids disk-backed mountpoints")
	}
	dir, err := os.MkdirTemp(runtimeDir, prefix+"-*")
	if err != nil {
		t.Fatalf("mkdir tmpfs mountpoint under %s: %v", runtimeDir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// dockerCanReachPath reports whether the Docker daemon can bind-mount
// the given host path. Docker Desktop on Linux runs the daemon inside
// a VM and only sees a restricted set of host paths via its file-share
// layer; user-mode FUSE mounts on arbitrary host paths typically don't
// surface through that layer. Tests that need bind-mounts of FUSE
// mountpoints should pre-check via this helper and skip with a clear
// message when the daemon can't see the path.
//
// We probe by trying to create-and-immediately-remove a container with
// a read-only bind of `path` to /probe. If the create succeeds, the
// daemon can reach the path; if it fails with "bind source path does
// not exist", we know we're in a Desktop-VM situation.
func dockerCanReachPath(t *testing.T, c *docker.Client, path string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Need an image to bind into. Use alpine:3.19 — pulled by other
	// tests so almost certainly cached.
	_, err := c.SDK().ImageInspect(ctx, "alpine:3.19")
	if err != nil {
		// Fast pull then inspect.
		events, perr := c.Pull(ctx, "alpine:3.19")
		if perr == nil {
			_ = drainPullEvents(events)
		}
	}

	probeName := uniqueName("mkfst-probe")
	result, err := c.Run(ctx, "alpine:3.19",
		docker.Name(probeName),
		docker.Cmd("true"),
		docker.Mount(docker.MountSpec{
			Type:     docker.MountTypeBind,
			Source:   filepath.Clean(path),
			Target:   "/probe",
			ReadOnly: true,
		}),
		docker.WaitForExit(),
	)
	if result != nil && result.ContainerID != "" {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.Remove(ctx2, result.ContainerID, docker.RemoveOpts{Force: true})
		cancel2()
	}
	if err == nil {
		return true
	}
	// Anything mentioning the bind source not existing is the
	// Desktop-VM signature; other errors propagate up.
	msg := err.Error()
	return !strings.Contains(msg, "bind source path does not exist") &&
		!strings.Contains(msg, "Mounts denied")
}

// ensureImageLocal pulls ref only if it's not already cached on the
// daemon. Useful for tests running against daemons whose network can't
// reach the registry (e.g. rootless docker with broken slirp4netns
// DNS) but that have images pre-loaded via docker save/load. Idempotent
// — repeated calls for the same ref are nearly free.
//
// On a daemon with working DNS this is equivalent to a normal Pull,
// just with one Inspect-call worth of overhead.
func ensureImageLocal(t *testing.T, c *docker.Client, ref string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := c.SDK().ImageInspect(ctx, ref); err == nil {
		cancel()
		return // already present
	}
	cancel()

	pullCtx, pullCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer pullCancel()
	events, err := c.Pull(pullCtx, ref)
	if err != nil {
		t.Fatalf("ensureImageLocal Pull %q: %v", ref, err)
	}
	if drainErr := drainPullEvents(events); drainErr != nil {
		t.Fatalf("ensureImageLocal Pull %q drain: %v", ref, drainErr)
	}
}

// requireFUSE skips tests that need /dev/fuse access. Linux-only check;
// the test would skip implicitly on darwin/windows because the cgofuse
// stub returns ErrFUSEUnavailable, but the explicit skip here gives a
// clearer reason in test output.
func requireFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("FUSE unavailable: %v", err)
	}
}

// requireFUSEAllowOther additionally checks that /etc/fuse.conf permits
// AllowOther mounts. Without this, the docker daemon (running as a
// different UID than the test) can't read FUSE-mounted files even
// though the mount itself succeeds. fusermount surfaces this as a
// terminal "code 256" exit with the helpful message printed only to
// stderr, so we pre-check the config to give the user a clear skip
// reason and a one-liner fix.
func requireFUSEAllowOther(t *testing.T) {
	t.Helper()
	requireFUSE(t)
	const path = "/etc/fuse.conf"
	body, err := os.ReadFile(path)
	if err != nil {
		// Some distros don't ship fuse.conf at all; AllowOther is then
		// implicitly disallowed.
		t.Skipf("FUSE AllowOther check: %v (enable by creating %s with `user_allow_other`)", err, path)
	}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "user_allow_other" {
			return // permitted
		}
	}
	t.Skipf("FUSE AllowOther disabled in %s — enable with:\n"+
		"  echo 'user_allow_other' | sudo tee -a %s", path, path)
}

// requireInternet skips tests that need to reach Docker Hub (or other
// registries). DNS resolution stands in for full reachability — much
// faster than an actual TCP probe and equally definitive in the cases
// we care about (offline laptops, sandboxed CI).
func requireInternet(t *testing.T) {
	t.Helper()
	if _, err := net.LookupHost("registry-1.docker.io"); err != nil {
		t.Skipf("internet unavailable: %v", err)
	}
}

// uniqueName returns a stable-prefix, run-unique name suitable for image
// tags or container names. Caller-supplied prefix should be short and
// human-meaningful for log inspection ("mkfst-e2e-go-app", "mkfst-e2e-py").
//
// Suffix is monotonic across the test process so two runs of the same
// test in the same `go test` invocation get different names — important
// for cleanup ordering.
func uniqueName(prefix string) string {
	n := nameSeq.Add(1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

var nameSeq atomic.Uint64

// withCleanupContainer schedules removal of a container after the test
// completes (regardless of whether the test passed or failed). Force
// removal so even running containers get cleaned up.
func withCleanupContainer(t *testing.T, c *docker.Client, containerID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := c.Remove(ctx, containerID, docker.RemoveOpts{Force: true, RemoveVolumes: true})
		if err != nil && !strings.Contains(err.Error(), "No such container") {
			t.Logf("cleanup container %s: %v", containerID, err)
		}
	})
}

// withCleanupImage schedules removal of an image after the test. We
// deliberately don't `Force` because aggressive image removal during a
// parallel test run can yank a layer another test still needs; the
// daemon's reference-counting handles this safely without Force.
func withCleanupImage(t *testing.T, c *docker.Client, imageID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := c.SDK().ImageRemove(ctx, imageID, dockerimage.RemoveOptions{
			PruneChildren: true,
		})
		if err != nil && !strings.Contains(err.Error(), "No such image") {
			t.Logf("cleanup image %s: %v", imageID, err)
		}
	})
}

// drainBuildEvents consumes a build event channel, returning the final
// image ID (from the EventAux carrying it) or fatally failing the test
// on EventError. The accumulated stream output is returned for inclusion
// in a t.Logf if the caller wants to surface it.
func drainBuildEvents(t *testing.T, events <-chan docker.Event) (imageID string, output string) {
	t.Helper()
	var stream strings.Builder
	for ev := range events {
		switch ev.Kind {
		case docker.EventStream:
			stream.WriteString(ev.Message)
		case docker.EventStatus:
			stream.WriteString("[status] " + ev.Message + "\n")
		case docker.EventError:
			t.Fatalf("build error: %v\nstream so far:\n%s", ev.Err, stream.String())
		case docker.EventAux:
			if id := ev.ImageID(); id != "" {
				imageID = id
			}
		}
	}
	if imageID == "" {
		t.Fatalf("no image ID from build\nstream:\n%s", stream.String())
	}
	return imageID, stream.String()
}

// drainPullEvents consumes a pull event channel; returns nil on success
// or the terminal error.
func drainPullEvents(events <-chan docker.Event) error {
	for ev := range events {
		if ev.Kind == docker.EventError {
			return ev.Err
		}
	}
	return nil
}

// runWaitAndCollect runs a container, waits for it to exit, returns the
// exit code and stdout/stderr captured via Logs. Cleanup is registered
// automatically. Used by tests that want simple "what did the container
// print" assertions.
func runWaitAndCollect(t *testing.T, c *docker.Client, image string, opts ...docker.RunOption) (exitCode int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Force WaitForExit so we can collect.
	opts = append(opts, docker.WaitForExit())

	result, err := c.Run(ctx, image, opts...)
	if result == nil {
		t.Fatalf("Run returned nil result: %v", err)
	}
	if result.ContainerID != "" {
		withCleanupContainer(t, c, result.ContainerID)
	}
	if err != nil {
		// AutoRemove + WaitForExit can produce a "No such container" race
		// on the daemon; treat as fine if exit code makes sense.
		if !strings.Contains(err.Error(), "No such container") {
			t.Fatalf("Run: %v", err)
		}
	}

	// Logs() is post-hoc — works whether the container is still running or
	// already exited (without --rm).
	logsCtx, logsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer logsCancel()
	logs, err := c.Logs(logsCtx, result.ContainerID, docker.Follow())
	if err != nil {
		t.Logf("Logs: %v (container may have been auto-removed)", err)
		return result.ExitCode, "", ""
	}
	var so, se strings.Builder
	for line := range logs {
		if line.Err != nil {
			break
		}
		if line.Stream == docker.LogStreamStdout {
			so.WriteString(line.Message)
			so.WriteString("\n")
		} else {
			se.WriteString(line.Message)
			se.WriteString("\n")
		}
	}
	return result.ExitCode, so.String(), se.String()
}

// containerStateAfterStop returns the daemon's view of a container right
// after a Stop. Used by lifecycle tests to assert State.Running=false.
func containerStateAfterStop(t *testing.T, c *docker.Client, containerID string) *dockertypes.State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := c.Inspect(ctx, containerID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return info.State
}
