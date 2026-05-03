//go:build linux && fuse_integration

package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// requireFUSE skips the test if /dev/fuse isn't accessible — typically
// because the kernel module isn't loaded or the test user isn't in the
// fuse group. Lets developers run other tests in `-tags=fuse_integration`
// builds without forcing FUSE setup.
func requireFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("FUSE unavailable: %v", err)
	}
}

// withMountedTree spins up a Tree with the given setup, mounts it at a
// fresh tempdir, returns (tree, mountpoint, teardown). The teardown is
// registered via t.Cleanup so callers don't have to remember.
func withMountedTree(t *testing.T, setup func(*Tree)) (*Tree, string) {
	t.Helper()
	requireFUSE(t)

	mp := t.TempDir()
	tree := NewTree(TreeOpts{})
	if setup != nil {
		setup(tree)
	}

	ctx, cancel := context.WithCancel(context.Background())
	mount, err := tree.Mount(ctx, MountOpts{
		Mountpoint: mp,
		Name:       "mkfst-vfs-test",
	})
	if err != nil {
		cancel()
		t.Fatalf("mount: %v", err)
	}

	t.Cleanup(func() {
		_ = mount.Unmount()
		cancel()
		// Wait briefly for the kernel to release the mount.
		select {
		case <-mount.done:
		case <-time.After(2 * time.Second):
		}
	})
	return tree, mp
}

func TestFUSEMountReadsInMemoryFile(t *testing.T) {
	_, mp := withMountedTree(t, func(tree *Tree) {
		_ = tree.Write("/hello.txt", []byte("from vfs"), 0o644)
	})
	got, err := os.ReadFile(filepath.Join(mp, "hello.txt"))
	if err != nil {
		t.Fatalf("read via mount: %v", err)
	}
	if string(got) != "from vfs" {
		t.Fatalf("got %q", got)
	}
}

func TestFUSEMountListsDirectory(t *testing.T) {
	_, mp := withMountedTree(t, func(tree *Tree) {
		_ = tree.Write("/a.txt", []byte("a"), 0o644)
		_ = tree.Write("/b.txt", []byte("b"), 0o644)
		_ = tree.Mkdir("/sub", 0o755)
		_ = tree.Write("/sub/c.txt", []byte("c"), 0o644)
	})
	entries, err := os.ReadDir(mp)
	if err != nil {
		t.Fatalf("readdir mount: %v", err)
	}
	got := []string{}
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	want := []string{"a.txt", "b.txt", "sub"}
	if len(got) != len(want) {
		t.Fatalf("dir contents: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dir[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestFUSEMountWriteFromHost(t *testing.T) {
	tree, mp := withMountedTree(t, nil)
	target := filepath.Join(mp, "from-host.txt")
	if err := os.WriteFile(target, []byte("via host process"), 0o644); err != nil {
		t.Fatalf("write via mount: %v", err)
	}
	// The Tree should reflect the write.
	got, err := tree.Read("/from-host.txt")
	if err != nil {
		t.Fatalf("tree.Read: %v", err)
	}
	if string(got) != "via host process" {
		t.Fatalf("got %q", got)
	}
}

func TestFUSEMountUnlinkFromHost(t *testing.T) {
	tree, mp := withMountedTree(t, func(tree *Tree) {
		_ = tree.Write("/to-delete.txt", []byte("x"), 0o644)
	})
	if err := os.Remove(filepath.Join(mp, "to-delete.txt")); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := tree.Stat("/to-delete.txt"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("after unlink, want ErrNotExist, got %v", err)
	}
}

func TestFUSEMountSymlinkRoundtrip(t *testing.T) {
	_, mp := withMountedTree(t, func(tree *Tree) {
		_ = tree.Write("/target.txt", []byte("payload"), 0o644)
		_ = tree.Symlink("/target.txt", "/link")
	})
	got, err := os.Readlink(filepath.Join(mp, "link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "/target.txt" {
		t.Fatalf("got target %q", got)
	}
}

func TestFUSEMountAlreadyMounted(t *testing.T) {
	tree, _ := withMountedTree(t, nil)
	mp2 := t.TempDir()
	_, err := tree.Mount(context.Background(), MountOpts{Mountpoint: mp2})
	if !errors.Is(err, ErrAlreadyMounted) {
		t.Fatalf("want ErrAlreadyMounted, got %v", err)
	}
}

func TestFUSEMountWithOverlay(t *testing.T) {
	requireFUSE(t)
	host := hostFixture(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})

	mp := t.TempDir()
	tree := NewTree(TreeOpts{HostOverlay: host})
	_ = tree.Write("/extra.txt", []byte("memory"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mount, err := tree.Mount(ctx, MountOpts{Mountpoint: mp})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	defer mount.Unmount()

	// Both host and memory entries should be visible through the FUSE mount.
	hostBytes, err := os.ReadFile(filepath.Join(mp, "Dockerfile"))
	if err != nil {
		t.Fatalf("read overlay file: %v", err)
	}
	if string(hostBytes) != "FROM scratch\n" {
		t.Fatalf("overlay content: got %q", hostBytes)
	}
	memBytes, err := os.ReadFile(filepath.Join(mp, "extra.txt"))
	if err != nil {
		t.Fatalf("read memory file: %v", err)
	}
	if string(memBytes) != "memory" {
		t.Fatalf("memory content: got %q", memBytes)
	}
}

func TestFUSEMountStreamReadLargeFile(t *testing.T) {
	// A file much larger than typical FUSE read buffer (128 KiB default)
	// to exercise multi-Read calls.
	const size = 512 * 1024
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	_, mp := withMountedTree(t, func(tree *Tree) {
		_ = tree.Write("/big.bin", payload, 0o644)
	})
	f, err := os.Open(filepath.Join(mp, "big.bin"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("readall: %v", err)
	}
	if len(got) != size {
		t.Fatalf("size mismatch: got %d want %d", len(got), size)
	}
	for i, b := range got {
		if b != payload[i] {
			t.Fatalf("byte %d differs: got %d want %d", i, b, payload[i])
		}
	}
}
