package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"mkfst/providers/vfs"
)

func TestVFSSourceProducesTar(t *testing.T) {
	tree := vfs.NewTree(vfs.TreeOpts{})
	_ = tree.Write("/Dockerfile", []byte("FROM scratch\n"), 0o644)
	_ = tree.Write("/app/main.go", []byte("package main\n"), 0o644)

	src := NewVFSSource(tree)
	rc, err := src.Tar(context.Background())
	if err != nil {
		t.Fatalf("Tar: %v", err)
	}
	defer rc.Close()

	got := readTarNames(t, rc)
	for _, want := range []string{"Dockerfile", "app/", "app/main.go"} {
		if !contains(got, want) {
			t.Fatalf("missing %q in tar (have %v)", want, got)
		}
	}
}

func TestVFSSourceRespectsRoot(t *testing.T) {
	tree := vfs.NewTree(vfs.TreeOpts{})
	_ = tree.Write("/outside.txt", []byte("ignore"), 0o644)
	_ = tree.Write("/ctx/Dockerfile", []byte("FROM alpine"), 0o644)

	src := &VFSSource{Tree: tree, Root: "/ctx"}
	rc, err := src.Tar(context.Background())
	if err != nil {
		t.Fatalf("Tar: %v", err)
	}
	defer rc.Close()

	got := readTarNames(t, rc)
	if contains(got, "outside.txt") {
		t.Fatalf("outside.txt should not be tarred (have %v)", got)
	}
	if !contains(got, "Dockerfile") {
		t.Fatalf("missing Dockerfile (have %v)", got)
	}
}

func TestDirSourceTarsHostDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	src := DirSource(root)
	rc, err := src.Tar(context.Background())
	if err != nil {
		t.Fatalf("Tar: %v", err)
	}
	defer rc.Close()

	got := readTarNames(t, rc)
	for _, want := range []string{"Dockerfile", "src/", "src/main.go"} {
		if !contains(got, want) {
			t.Fatalf("missing %q (have %v)", want, got)
		}
	}
}

func TestDirSourceErrorsOnMissing(t *testing.T) {
	src := DirSource("/nonexistent/never/created")
	_, err := src.Tar(context.Background())
	if err == nil {
		t.Fatalf("expected error on missing dir")
	}
}

func TestDirSourceErrorsOnFile(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	src := DirSource(file)
	_, err := src.Tar(context.Background())
	if err == nil {
		t.Fatalf("expected error when source is a file")
	}
}

func TestTarSourcePassthrough(t *testing.T) {
	// Build a tiny tar in memory.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: 4})
	_, _ = tw.Write([]byte("FROM"))
	_ = tw.Close()

	src := &TarSource{Reader: &buf}
	rc, err := src.Tar(context.Background())
	if err != nil {
		t.Fatalf("Tar: %v", err)
	}
	defer rc.Close()

	got := readTarNames(t, rc)
	if len(got) != 1 || got[0] != "Dockerfile" {
		t.Fatalf("got %v", got)
	}
}

func TestTarSourceNilReaderErrors(t *testing.T) {
	src := &TarSource{}
	_, err := src.Tar(context.Background())
	if err == nil {
		t.Fatalf("expected error for nil reader")
	}
}

func TestVFSSourceNilTreeErrors(t *testing.T) {
	src := &VFSSource{}
	_, err := src.Tar(context.Background())
	if err == nil {
		t.Fatalf("expected error for nil tree")
	}
}

// readTarNames slurps a tar stream and returns the entry names (sorted).
func readTarNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	tr := tar.NewReader(r)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		names = append(names, hdr.Name)
		// Drain body so the next Next() works correctly.
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatalf("tar drain: %v", err)
		}
	}
	sort.Strings(names)
	return names
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
