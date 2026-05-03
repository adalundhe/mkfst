package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// hostFixture lays down a small directory tree on the host filesystem and
// returns its root. t.Cleanup removes it. Used by overlay tests as the
// read-only lower layer.
func hostFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for relPath, body := range files {
		full := filepath.Join(root, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

func TestOverlayHostFileVisible(t *testing.T) {
	root := hostFixture(t, map[string]string{
		"Dockerfile":  "FROM scratch\n",
		"app/main.go": "package main\n",
	})
	tree := NewTree(TreeOpts{HostOverlay: root})

	got, err := tree.Read("/Dockerfile")
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if string(got) != "FROM scratch\n" {
		t.Fatalf("got %q", got)
	}

	got, err = tree.Read("/app/main.go")
	if err != nil {
		t.Fatalf("read nested host file: %v", err)
	}
	if string(got) != "package main\n" {
		t.Fatalf("got %q", got)
	}
}

func TestOverlayInMemoryShadowsHost(t *testing.T) {
	root := hostFixture(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})
	tree := NewTree(TreeOpts{HostOverlay: root})

	if err := tree.Write("/Dockerfile", []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := tree.Read("/Dockerfile")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "FROM alpine\n" {
		t.Fatalf("got %q (memory should shadow host)", got)
	}
}

func TestOverlayWriteIntoHostOnlyDir(t *testing.T) {
	root := hostFixture(t, map[string]string{
		"app/main.go": "package main\n",
	})
	tree := NewTree(TreeOpts{HostOverlay: root})

	// Write a new file into a directory that exists ONLY on host. The dir
	// must materialize; the new file must coexist with the host's main.go.
	if err := tree.Write("/app/extra.txt", []byte("added"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, err := tree.ReadDir("/app")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	want := []string{"extra.txt", "main.go"}
	if len(names) != len(want) {
		t.Fatalf("dir contents: got %v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("dir[%d]: got %q want %q", i, names[i], want[i])
		}
	}
}

func TestOverlayRemoveHostOnlyAddsWhiteout(t *testing.T) {
	root := hostFixture(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
		"hidden.txt": "ignored",
	})
	tree := NewTree(TreeOpts{HostOverlay: root})

	if err := tree.Remove("/hidden.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := tree.Stat("/hidden.txt"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("after whiteout, want ErrNotExist, got %v", err)
	}
	// Other host files unaffected.
	if _, err := tree.Stat("/Dockerfile"); err != nil {
		t.Fatalf("Dockerfile should still resolve: %v", err)
	}
}

func TestOverlayRemoveAllHostDirHidesSubtree(t *testing.T) {
	root := hostFixture(t, map[string]string{
		"keep.txt":          "keep",
		"hide-me/inner.txt": "hidden",
		"hide-me/sub/x.txt": "also hidden",
	})
	tree := NewTree(TreeOpts{HostOverlay: root})

	if err := tree.RemoveAll("/hide-me"); err != nil {
		t.Fatalf("removeall: %v", err)
	}
	for _, p := range []string{"/hide-me", "/hide-me/inner.txt", "/hide-me/sub/x.txt"} {
		if _, err := tree.Stat(p); !errors.Is(err, ErrNotExist) {
			t.Fatalf("after removeall, %s should be hidden, got %v", p, err)
		}
	}
	if _, err := tree.Stat("/keep.txt"); err != nil {
		t.Fatalf("keep.txt: %v", err)
	}
}

func TestOverlayWriteUnshadowsWhiteout(t *testing.T) {
	root := hostFixture(t, map[string]string{
		"file.txt": "host",
	})
	tree := NewTree(TreeOpts{HostOverlay: root})

	// Whiteout the host file.
	if err := tree.Remove("/file.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := tree.Stat("/file.txt"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("expected hidden, got %v", err)
	}
	// Now write to it — that should clear the exact-path whiteout and
	// surface the new memory content (not the host one).
	if err := tree.Write("/file.txt", []byte("new"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got, err := tree.Read("/file.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("after un-whiteout via write, got %q want %q", got, "new")
	}
}

func TestOverlayWalkUnion(t *testing.T) {
	root := hostFixture(t, map[string]string{
		"host-only.txt": "h",
		"shared/inner":  "h",
	})
	tree := NewTree(TreeOpts{HostOverlay: root})
	_ = tree.Write("/mem-only.txt", []byte("m"), 0o644)
	_ = tree.Write("/shared/from-mem", []byte("m"), 0o644)

	var paths []string
	_ = tree.Walk(func(p string, _ *Inode) error {
		paths = append(paths, p)
		return nil
	})
	want := map[string]bool{
		"/":                true,
		"/host-only.txt":   true,
		"/mem-only.txt":    true,
		"/shared":          true,
		"/shared/from-mem": true,
		"/shared/inner":    true,
	}
	for _, p := range paths {
		delete(want, p)
	}
	if len(want) != 0 {
		t.Fatalf("missing paths in walk: %v (got %v)", want, paths)
	}
}

func TestOverlayTarIncludesUnion(t *testing.T) {
	root := hostFixture(t, map[string]string{
		"host.txt": "h",
	})
	tree := NewTree(TreeOpts{HostOverlay: root})
	_ = tree.Write("/mem.txt", []byte("m"), 0o644)

	rc := tree.Tar(context.Background(), TarOpts{})
	defer rc.Close()
	entries := readTar(t, rc)
	for _, name := range []string{"host.txt", "mem.txt"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("missing tar entry %q (have %v)", name, entryNames(entries))
		}
	}
	if string(entries["host.txt"].body) != "h" || string(entries["mem.txt"].body) != "m" {
		t.Fatalf("body mismatch: host=%q mem=%q", entries["host.txt"].body, entries["mem.txt"].body)
	}
}

func TestOverlayHostReadCachePopulates(t *testing.T) {
	root := hostFixture(t, map[string]string{
		"big.txt": "abcdefghij",
	})
	tree := NewTree(TreeOpts{HostOverlay: root, HostCacheBudget: 1 << 20})

	// Cold read populates the cache.
	if _, err := tree.Read("/big.txt"); err != nil {
		t.Fatalf("read 1: %v", err)
	}
	entries, _, _ := tree.HostCacheStats()
	if entries != 1 {
		t.Fatalf("after cold read, want 1 cache entry, got %d", entries)
	}
	// Warm read should hit cache; entries unchanged.
	if _, err := tree.Read("/big.txt"); err != nil {
		t.Fatalf("read 2: %v", err)
	}
	entries, _, _ = tree.HostCacheStats()
	if entries != 1 {
		t.Fatalf("after warm read, want still 1 cache entry, got %d", entries)
	}
}

func TestOverlayHostReadCacheInvalidatesOnMtimeChange(t *testing.T) {
	root := hostFixture(t, map[string]string{
		"x.txt": "v1",
	})
	tree := NewTree(TreeOpts{HostOverlay: root, HostCacheBudget: 1 << 20})

	if _, err := tree.Read("/x.txt"); err != nil {
		t.Fatalf("read v1: %v", err)
	}
	// Rewrite the host file with a different mtime by sleeping and then
	// re-writing. On most filesystems mtime resolution is 1ns–1s; we
	// truncate-then-write ensures both size and mtime change.
	full := filepath.Join(root, "x.txt")
	if err := os.WriteFile(full, []byte("v2-bigger"), 0o644); err != nil {
		t.Fatalf("rewrite host: %v", err)
	}
	// Bump mtime explicitly to remove timing flake.
	mtime := mustStat(t, full).ModTime().Add(2 * 1e9) // +2s
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// ResetHostCache clears the byte cache; the synthesized inode entry
	// gets re-validated on the next lookup (size/mtime mismatch triggers
	// in-place refresh of the inode body).
	tree.ResetHostCache()

	got, err := tree.Read("/x.txt")
	if err != nil && err != io.EOF {
		t.Fatalf("read v2: %v", err)
	}
	if string(got) != "v2-bigger" {
		t.Fatalf("want v2-bigger, got %q", got)
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return info
}
