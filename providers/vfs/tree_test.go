package vfs

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"
)

func TestTreeWriteReadRoundtrip(t *testing.T) {
	tree := NewTree(TreeOpts{})

	if err := tree.Write("/hello.txt", []byte("world"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := tree.Read("/hello.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "world" {
		t.Fatalf("got %q, want %q", got, "world")
	}
}

func TestTreeNestedWriteAutoCreatesParents(t *testing.T) {
	tree := NewTree(TreeOpts{})

	if err := tree.Write("/a/b/c/file.txt", []byte("nested"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := tree.Read("/a/b/c/file.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "nested" {
		t.Fatalf("got %q", got)
	}

	stat, err := tree.Stat("/a/b")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !stat.IsDir() {
		t.Fatalf("/a/b should be a directory")
	}
}

func TestTreeChunkDedupes(t *testing.T) {
	tree := NewTree(TreeOpts{})
	content := []byte("repeated content")

	for i := 0; i < 10; i++ {
		if err := tree.Write("/dup-"+string(rune('a'+i))+".txt", content, 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	stats := tree.Store().Stats()
	if stats.Chunks != 1 {
		t.Fatalf("want 1 chunk after dedup, got %d", stats.Chunks)
	}
	if stats.DedupHitCount != 9 {
		t.Fatalf("want 9 dedup hits, got %d", stats.DedupHitCount)
	}
}

func TestTreeRemoveReleasesChunks(t *testing.T) {
	tree := NewTree(TreeOpts{})
	if err := tree.Write("/file.txt", []byte("bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := tree.Store().Stats().Chunks; got != 1 {
		t.Fatalf("want 1 chunk pre-remove, got %d", got)
	}
	if err := tree.Remove("/file.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := tree.Store().Stats().Chunks; got != 0 {
		t.Fatalf("want 0 chunks post-remove, got %d", got)
	}
}

func TestTreeRemoveAllSubtree(t *testing.T) {
	tree := NewTree(TreeOpts{})
	for _, p := range []string{"/dir/a.txt", "/dir/sub/b.txt", "/dir/sub/c.txt"} {
		if err := tree.Write(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if err := tree.RemoveAll("/dir"); err != nil {
		t.Fatalf("removeall: %v", err)
	}
	if _, err := tree.Stat("/dir"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	if got := tree.Store().Stats().Chunks; got != 0 {
		t.Fatalf("want 0 chunks after RemoveAll, got %d", got)
	}
}

func TestTreeRemoveDirNonEmpty(t *testing.T) {
	tree := NewTree(TreeOpts{})
	if err := tree.Write("/d/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tree.Remove("/d"); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("want ErrNotEmpty, got %v", err)
	}
}

func TestTreeWalkLexicalOrder(t *testing.T) {
	tree := NewTree(TreeOpts{})
	for _, p := range []string{"/c.txt", "/a.txt", "/b/inner.txt", "/b/inner2.txt"} {
		if err := tree.Write(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	var paths []string
	if err := tree.Walk(func(p string, _ *Inode) error {
		paths = append(paths, p)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	want := []string{"/", "/a.txt", "/b", "/b/inner.txt", "/b/inner2.txt", "/c.txt"}
	if len(paths) != len(want) {
		t.Fatalf("walk length: got %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("walk[%d]: got %q, want %q (full: %v)", i, paths[i], want[i], paths)
		}
	}
}

func TestTreeWalkSkipDir(t *testing.T) {
	tree := NewTree(TreeOpts{})
	_ = tree.MkdirAll("/skip", 0o755)
	_ = tree.Write("/skip/a.txt", []byte("x"), 0o644)
	_ = tree.Write("/keep.txt", []byte("y"), 0o644)

	var paths []string
	if err := tree.Walk(func(p string, inode *Inode) error {
		paths = append(paths, p)
		if p == "/skip" {
			return fs.SkipDir
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, p := range paths {
		if p == "/skip/a.txt" {
			t.Fatalf("walked into skipped dir: %v", paths)
		}
	}
}

func TestTreeOpenAndReadAsFsFile(t *testing.T) {
	tree := NewTree(TreeOpts{ChunkSize: 4})
	want := []byte("0123456789ABCDEF")
	if err := tree.Write("/data.bin", want, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := tree.Open("/data.bin")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	tmp := make([]byte, 5) // smaller than chunk to test multi-extent reads
	for {
		n, err := f.Read(tmp)
		buf.Write(tmp[:n])
		if err != nil {
			break
		}
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got %q, want %q", buf.Bytes(), want)
	}
}

func TestTreeRename(t *testing.T) {
	tree := NewTree(TreeOpts{})
	if err := tree.Write("/old.txt", []byte("v1"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tree.Rename("/old.txt", "/new.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := tree.Stat("/old.txt"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("old still exists, err=%v", err)
	}
	got, err := tree.Read("/new.txt")
	if err != nil {
		t.Fatalf("read new: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("got %q", got)
	}
}

func TestTreeSymlink(t *testing.T) {
	tree := NewTree(TreeOpts{})
	if err := tree.Symlink("/target.txt", "/link"); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	stat, err := tree.Stat("/link")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !stat.IsSymlink() {
		t.Fatalf("not a symlink: %+v", stat)
	}
	if stat.LinkTarget != "/target.txt" {
		t.Fatalf("got target %q", stat.LinkTarget)
	}
}

func TestTreeInvalidPaths(t *testing.T) {
	tree := NewTree(TreeOpts{})
	// Note: "/../escape" cleans to "/escape" — that's correct Unix semantics
	// (root has no parent), so we don't reject it. The cases here are paths
	// the user couldn't reasonably mean.
	for _, p := range []string{"", "..", `/win\style`} {
		if err := tree.Write(p, []byte("x"), 0o644); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("path %q: want ErrInvalidPath, got %v", p, err)
		}
	}
}
