//go:build linux && fuse_integration

// Exec needs a real FUSE mount to point host processes at; we gate the
// tests on the same fuse_integration tag the VFS uses.

package files

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"mkfst/providers/vfs"
)

// requireFUSE skips when /dev/fuse isn't reachable. Mirrors the helper in
// the vfs integration suite so this file can stand alone.
func requireFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("FUSE unavailable: %v", err)
	}
}

// withMountedTree returns a FUSE-mounted *vfs.Tree at a fresh tempdir,
// with cleanup registered. Callers must populate the tree before mounting
// to keep the test setup linear.
func withMountedTree(t *testing.T, setup func(*vfs.Tree)) (*vfs.Tree, string) {
	t.Helper()
	requireFUSE(t)

	mp := t.TempDir()
	tree := vfs.NewTree(vfs.TreeOpts{})
	if setup != nil {
		setup(tree)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mount, err := tree.Mount(ctx, vfs.MountOpts{Mountpoint: mp, Name: "mkfst-files-test"})
	if err != nil {
		cancel()
		t.Fatalf("mount: %v", err)
	}
	t.Cleanup(func() {
		_ = mount.Unmount()
		cancel()
		time.Sleep(100 * time.Millisecond) // brief settle before TempDir cleanup
	})
	return tree, mp
}

func TestExecRequiresMount(t *testing.T) {
	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree)
	_, err := files.Exec(context.Background(), "/bin/true")
	if !errors.Is(err, ErrNotMounted) {
		t.Fatalf("want ErrNotMounted, got %v", err)
	}
}

func TestExecRunsHostProcessAgainstVFS(t *testing.T) {
	tree, mp := withMountedTree(t, func(tree *vfs.Tree) {
		_ = tree.Write("/script-data.txt", []byte("hello from VFS\n"), 0o644)
	})
	files := NewService(tree)

	// Run cat on the VFS-resident file via the mountpoint. The host
	// process reads it as if it were a real file.
	res, err := files.Exec(context.Background(), "cat",
		Args("script-data.txt"),
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit: %d (stderr=%s)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "hello from VFS") {
		t.Fatalf("stdout: %q", res.Stdout)
	}
	_ = mp
}

func TestExecCapturesStderr(t *testing.T) {
	tree, _ := withMountedTree(t, nil)
	files := NewService(tree)
	res, err := files.Exec(context.Background(), "sh",
		Args("-c", "echo to-stdout; echo to-stderr 1>&2; exit 7"),
	)
	if err == nil {
		t.Fatalf("expected exit-error")
	}
	if res.ExitCode != 7 {
		t.Fatalf("exit: %d", res.ExitCode)
	}
	if !strings.Contains(string(res.Stdout), "to-stdout") {
		t.Fatalf("stdout: %q", res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), "to-stderr") {
		t.Fatalf("stderr: %q", res.Stderr)
	}
}

func TestExecCombinedOrder(t *testing.T) {
	tree, _ := withMountedTree(t, nil)
	files := NewService(tree)
	res, err := files.Exec(context.Background(), "sh",
		Args("-c", "echo a; echo b 1>&2; echo c"),
		CaptureCombined(),
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	combined := string(res.Combined)
	for _, want := range []string{"a", "b", "c"} {
		if !strings.Contains(combined, want) {
			t.Fatalf("combined missing %q (got %q)", want, combined)
		}
	}
}

func TestExecDirEscapeRejected(t *testing.T) {
	tree, _ := withMountedTree(t, nil)
	files := NewService(tree)
	_, err := files.Exec(context.Background(), "/bin/true", Dir("../../etc"))
	if err == nil || !strings.Contains(err.Error(), "escapes mountpoint") {
		t.Fatalf("expected escape rejection, got %v", err)
	}
}

func TestExecEnvControl(t *testing.T) {
	tree, _ := withMountedTree(t, nil)
	files := NewService(tree)
	res, err := files.Exec(context.Background(), "sh",
		Args("-c", "echo $MKFST_TEST_VAR"),
		ExecEnv("MKFST_TEST_VAR", "from-test"),
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "from-test") {
		t.Fatalf("stdout: %q", res.Stdout)
	}

	// Check that NoEnvInherit actually drops parent env vars. We can't
	// assert PATH is empty (POSIX shells set a default) so we use a
	// uniquely-named var the parent process exports just for the test.
	t.Setenv("MKFST_TEST_INHERIT_FLAG", "should-not-leak")
	res, err = files.Exec(context.Background(), "/bin/sh",
		Args("-c", "echo flag=${MKFST_TEST_INHERIT_FLAG:-absent}"),
		NoEnvInherit(),
	)
	if err != nil {
		t.Fatalf("Exec NoEnvInherit: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "flag=absent") {
		t.Fatalf("NoEnvInherit leaked parent env: stdout=%q", res.Stdout)
	}
}
