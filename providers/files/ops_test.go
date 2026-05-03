package files

import (
	"errors"
	"testing"

	"mkfst/providers/vfs"
)

func TestCopyDuplicatesContent(t *testing.T) {
	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree)
	_ = tree.Write("/src.txt", []byte("hello"), 0o600)

	if err := files.Copy("/src.txt", "/dst/copy.txt"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got, _ := tree.Read("/dst/copy.txt")
	if string(got) != "hello" {
		t.Fatalf("dst body: %q", got)
	}
	// Source should still exist.
	if got, _ := tree.Read("/src.txt"); string(got) != "hello" {
		t.Fatalf("src body changed after copy: %q", got)
	}
}

func TestCopyDirectoryFails(t *testing.T) {
	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree)
	_ = tree.MkdirAll("/dir", 0o755)
	if err := files.Copy("/dir", "/elsewhere"); err == nil {
		t.Fatalf("expected error copying a directory")
	}
}

func TestMoveRenamesInMemory(t *testing.T) {
	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree)
	_ = tree.Write("/old.txt", []byte("v1"), 0o644)

	if err := files.Move("/old.txt", "/new.txt"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := tree.Stat("/old.txt"); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("old should be gone, err=%v", err)
	}
	got, _ := tree.Read("/new.txt")
	if string(got) != "v1" {
		t.Fatalf("new body: %q", got)
	}
}

func TestHashSHA256Matches(t *testing.T) {
	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree)
	_ = tree.Write("/x.txt", []byte("abc"), 0o644)
	got, err := files.Hash("/x.txt", HashSHA256)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Fatalf("hash got %q want %q", got, want)
	}
}

func TestExistsAndStat(t *testing.T) {
	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree)
	_ = tree.Write("/here.txt", []byte("x"), 0o644)

	ok, err := files.Exists("/here.txt")
	if err != nil || !ok {
		t.Fatalf("Exists here: ok=%v err=%v", ok, err)
	}
	ok, err = files.Exists("/missing.txt")
	if err != nil {
		t.Fatalf("Exists missing should not error, got %v", err)
	}
	if ok {
		t.Fatalf("Exists missing should be false")
	}
	inode, err := files.Stat("/here.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !inode.IsFile() {
		t.Fatalf("Stat returned non-file: %+v", inode)
	}
}

func TestRemoveAndRemoveAll(t *testing.T) {
	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree)
	_ = tree.Write("/a.txt", []byte("a"), 0o644)
	_ = tree.Write("/dir/b.txt", []byte("b"), 0o644)
	_ = tree.Write("/dir/c.txt", []byte("c"), 0o644)

	if err := files.Remove("/a.txt"); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if err := files.Remove("/dir"); err == nil {
		t.Fatalf("Remove non-empty dir should fail")
	}
	if err := files.RemoveAll("/dir"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := tree.Stat("/dir"); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("dir should be gone")
	}
}
