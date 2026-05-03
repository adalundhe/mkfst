//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"mkfst/providers/docker"
	"mkfst/providers/vfs"
)

// Strict no-disk policy: every mountpoint in this file lives under
// $XDG_RUNTIME_DIR (tmpfs, RAM-backed). Never /tmp. Never any disk path.
// See tmpfsMountpoint in helpers_test.go.

// TestVFSMountedAsContainerBindMount is the showcase: write content to
// VFS in memory, FUSE-mount it on the host, then bind-mount the FUSE
// path into a container. The container reads in-memory VFS files at
// real paths, no materialization to disk. This is what makes the VFS
// useful as a docker volume source.
//
// Prereqs: /dev/fuse, /etc/fuse.conf must allow user_allow_other (most
// distros ship with this commented out — the test skips with a clear
// message if it can't enable AllowOther).
func TestVFSMountedAsContainerBindMount(t *testing.T) {
	requireFUSEAllowOther(t)
	c := requireDaemon(t)
	requireInternet(t)

	tree := vfs.NewTree(vfs.TreeOpts{})
	_ = tree.Write("/data/hello.txt", []byte("greetings from the VFS\n"), 0o644)
	_ = tree.Write("/data/nested/value.txt", []byte("nested-value-42\n"), 0o644)

	mountpoint := tmpfsMountpoint(t, "mkfst-e2e-mount")

	// AllowOther is required because the docker daemon runs as root
	// (different UID than the test process). On distros where
	// /etc/fuse.conf doesn't have user_allow_other, the mount succeeds
	// but the daemon gets EACCES on the bind-mount source — surfaced
	// here as a useful skip.
	mountCtx, mountCancel := context.WithCancel(context.Background())
	defer mountCancel()
	mount, err := tree.Mount(mountCtx, vfs.MountOpts{
		Mountpoint: mountpoint,
		Name:       "mkfst-e2e-bindsource",
		AllowOther: true,
	})
	if err != nil {
		if errors.Is(err, vfs.ErrFUSEUnavailable) {
			t.Skipf("FUSE unavailable: %v", err)
		}
		// /etc/fuse.conf needs `user_allow_other` for AllowOther to work.
		// Without it, fusermount refuses the mount entirely. Surface as a
		// skip with the fix instructions — the test logic is correct, the
		// host config just doesn't permit cross-UID FUSE access.
		if strings.Contains(err.Error(), "user_allow_other") {
			t.Skipf("AllowOther FUSE mount blocked by /etc/fuse.conf — enable with:\n"+
				"  echo 'user_allow_other' | sudo tee -a /etc/fuse.conf\nunderlying: %v", err)
		}
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { _ = mount.Unmount() })

	// Verify the mount is actually serving by reading via the host path.
	if got, err := os.ReadFile(mountpoint + "/data/hello.txt"); err != nil {
		t.Fatalf("host read of mounted VFS: %v", err)
	} else if !strings.Contains(string(got), "greetings from the VFS") {
		t.Fatalf("host read content: %q", got)
	}

	// Pre-check: Docker Desktop runs the daemon inside a VM and can't
	// see arbitrary host paths (including FUSE mounts) unless they're
	// in its file-share allowlist. Stock rootful docker-ce sees the
	// host filesystem directly. Fail fast with a useful skip message
	// rather than letting Run surface a confusing "bind source not
	// found" error.
	if !dockerCanReachPath(t, c, mountpoint) {
		t.Skipf("Docker daemon can't see host path %s — likely Docker Desktop's "+
			"VM file-share doesn't expose tmpfs/FUSE mounts. Run against rootful "+
			"docker-ce (DOCKER_HOST=unix:///var/run/docker.sock) to exercise this "+
			"scenario.", mountpoint)
	}

	// Now run a container that bind-mounts the FUSE path and cats the
	// VFS-resident files. runWaitAndCollect uses its own internal ctx.
	ensureImageLocal(t, c, "alpine:3.19")

	exitCode, stdout, stderr := runWaitAndCollect(t, c, "alpine:3.19",
		docker.Mount(docker.MountSpec{
			Type:     docker.MountTypeBind,
			Source:   mountpoint,
			Target:   "/vfs",
			ReadOnly: true,
		}),
		docker.Cmd("sh", "-c", "cat /vfs/data/hello.txt && cat /vfs/data/nested/value.txt"),
	)
	if exitCode != 0 {
		// EACCES inside the container typically means user_allow_other
		// isn't set in /etc/fuse.conf. Surface as a skip rather than a
		// test failure — the integration is correct, the host config
		// just doesn't permit cross-user FUSE access.
		if strings.Contains(stderr, "Permission denied") || strings.Contains(stderr, "EACCES") {
			t.Skipf("container couldn't read FUSE mount (likely missing user_allow_other in /etc/fuse.conf):\n%s", stderr)
		}
		t.Fatalf("container exited %d\nstdout=%q\nstderr=%q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "greetings from the VFS") {
		t.Fatalf("container didn't see hello.txt: %q", stdout)
	}
	if !strings.Contains(stdout, "nested-value-42") {
		t.Fatalf("container didn't see nested/value.txt: %q", stdout)
	}
}

// TestVFSWriteVisibleToRunningContainer extends the previous test: we
// mutate the VFS WHILE a container is running with the bind-mount, and
// verify the container sees the new file. Confirms the FUSE path is
// truly live, not a snapshot.
func TestVFSWriteVisibleToRunningContainer(t *testing.T) {
	requireFUSEAllowOther(t)
	c := requireDaemon(t)
	requireInternet(t)

	tree := vfs.NewTree(vfs.TreeOpts{})
	_ = tree.Write("/initial.txt", []byte("v1"), 0o644)

	mountpoint := tmpfsMountpoint(t, "mkfst-e2e-livemount")

	mountCtx, mountCancel := context.WithCancel(context.Background())
	defer mountCancel()
	mount, err := tree.Mount(mountCtx, vfs.MountOpts{
		Mountpoint: mountpoint,
		Name:       "mkfst-e2e-livesource",
		AllowOther: true,
	})
	if err != nil {
		if errors.Is(err, vfs.ErrFUSEUnavailable) {
			t.Skipf("FUSE unavailable: %v", err)
		}
		if strings.Contains(err.Error(), "user_allow_other") {
			t.Skipf("AllowOther FUSE mount blocked by /etc/fuse.conf — enable with:\n"+
				"  echo 'user_allow_other' | sudo tee -a /etc/fuse.conf\nunderlying: %v", err)
		}
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { _ = mount.Unmount() })

	ensureImageLocal(t, c, "alpine:3.19")

	if !dockerCanReachPath(t, c, mountpoint) {
		t.Skipf("Docker daemon can't see host path %s — likely Docker Desktop's "+
			"VM file-share. Run against rootful docker-ce to exercise this scenario.", mountpoint)
	}

	// Run a long-lived container that polls /vfs/late-arrival.txt every
	// 200ms and prints when it appears.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := c.Run(ctx, "alpine:3.19",
		docker.Mount(docker.MountSpec{
			Type:     docker.MountTypeBind,
			Source:   mountpoint,
			Target:   "/vfs",
			ReadOnly: true,
		}),
		docker.Cmd("sh", "-c",
			"for i in $(seq 1 50); do "+
				"if [ -f /vfs/late-arrival.txt ]; then "+
				"  echo SAW: $(cat /vfs/late-arrival.txt); exit 0; "+
				"fi; sleep 0.2; "+
				"done; echo NEVER_SAW; exit 1"),
		docker.Detach(),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	withCleanupContainer(t, c, result.ContainerID)

	// Give the container a moment to start its loop.
	time.Sleep(500 * time.Millisecond)

	// Mutate the VFS while the container is running.
	if err := tree.Write("/late-arrival.txt", []byte("appeared-mid-flight"), 0o644); err != nil {
		t.Fatalf("late write: %v", err)
	}

	// Wait for container exit; collect logs.
	exitCode, err := c.Wait(ctx, result.ContainerID, "not-running")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("container exited %d (expected 0 = saw the late file)", exitCode)
	}

	logsCtx, logsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer logsCancel()
	logs, err := c.Logs(logsCtx, result.ContainerID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	var sb strings.Builder
	for line := range logs {
		if line.Err != nil {
			break
		}
		sb.WriteString(line.Message)
		sb.WriteString("\n")
	}
	if !strings.Contains(sb.String(), "SAW: appeared-mid-flight") {
		t.Fatalf("container logs:\n%s", sb.String())
	}
}
