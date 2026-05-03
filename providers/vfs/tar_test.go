package vfs

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sort"
	"testing"
	"time"
)

// readTar slurps a tar stream into a name → entry map for assertion.
type tarEntry struct {
	header *tar.Header
	body   []byte
}

func readTar(t *testing.T, r io.Reader) map[string]tarEntry {
	t.Helper()
	tr := tar.NewReader(r)
	out := map[string]tarEntry{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar body: %v", err)
		}
		out[hdr.Name] = tarEntry{header: hdr, body: body}
	}
	return out
}

func TestTarRoundtripSimpleTree(t *testing.T) {
	tree := NewTree(TreeOpts{})
	_ = tree.Write("/Dockerfile", []byte("FROM scratch\n"), 0o644)
	_ = tree.Write("/app/main.go", []byte("package main\n"), 0o644)
	_ = tree.Write("/app/go.mod", []byte("module app\n"), 0o644)

	rc := tree.Tar(context.Background(), TarOpts{})
	defer rc.Close()
	entries := readTar(t, rc)

	want := map[string]string{
		"Dockerfile":  "FROM scratch\n",
		"app/":        "",
		"app/main.go": "package main\n",
		"app/go.mod":  "module app\n",
	}
	for name, body := range want {
		got, ok := entries[name]
		if !ok {
			t.Fatalf("missing entry %q (have %v)", name, entryNames(entries))
		}
		if string(got.body) != body {
			t.Fatalf("entry %q body: got %q want %q", name, got.body, body)
		}
	}
}

func TestTarSubtreeRoot(t *testing.T) {
	tree := NewTree(TreeOpts{})
	_ = tree.Write("/outside.txt", []byte("ignore"), 0o644)
	_ = tree.Write("/ctx/Dockerfile", []byte("FROM alpine"), 0o644)
	_ = tree.Write("/ctx/app/main.go", []byte("pkg"), 0o644)

	rc := tree.Tar(context.Background(), TarOpts{Root: "/ctx"})
	defer rc.Close()
	entries := readTar(t, rc)

	if _, has := entries["outside.txt"]; has {
		t.Fatalf("outside.txt should not appear: %v", entryNames(entries))
	}
	for _, name := range []string{"Dockerfile", "app/", "app/main.go"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("missing %q (have %v)", name, entryNames(entries))
		}
	}
}

func TestTarDeterministic(t *testing.T) {
	build := func() []byte {
		tree := NewTree(TreeOpts{})
		_ = tree.Write("/a.txt", []byte("a"), 0o644)
		_ = tree.Write("/b/c.txt", []byte("c"), 0o644)
		_ = tree.Write("/b/d.txt", []byte("d"), 0o644)
		// Force a stable mtime so two runs produce identical bytes.
		mt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		for _, p := range []string{"/", "/a.txt", "/b", "/b/c.txt", "/b/d.txt"} {
			_ = tree.Chtime(p, mt)
		}
		var buf bytes.Buffer
		rc := tree.Tar(context.Background(), TarOpts{})
		defer rc.Close()
		_, _ = io.Copy(&buf, rc)
		return buf.Bytes()
	}
	one := sha256.Sum256(build())
	two := sha256.Sum256(build())
	if one != two {
		t.Fatalf("tar output not deterministic: %x vs %x", one, two)
	}
}

func TestTarSymlinkPassthrough(t *testing.T) {
	tree := NewTree(TreeOpts{})
	_ = tree.Write("/target.txt", []byte("data"), 0o644)
	_ = tree.Symlink("/target.txt", "/link")

	rc := tree.Tar(context.Background(), TarOpts{})
	defer rc.Close()
	entries := readTar(t, rc)

	link, ok := entries["link"]
	if !ok {
		t.Fatalf("link missing")
	}
	if link.header.Typeflag != tar.TypeSymlink {
		t.Fatalf("link typeflag: %d", link.header.Typeflag)
	}
	if link.header.Linkname != "/target.txt" {
		t.Fatalf("link target: %q", link.header.Linkname)
	}
}

func TestTarFollowSymlinks(t *testing.T) {
	tree := NewTree(TreeOpts{})
	_ = tree.Write("/target.txt", []byte("payload"), 0o644)
	_ = tree.Symlink("/target.txt", "/link")

	rc := tree.Tar(context.Background(), TarOpts{FollowSymlinks: true})
	defer rc.Close()
	entries := readTar(t, rc)

	link := entries["link"]
	if link.header.Typeflag != tar.TypeReg {
		t.Fatalf("link should be regular after follow: %d", link.header.Typeflag)
	}
	if string(link.body) != "payload" {
		t.Fatalf("body got %q", link.body)
	}
}

func TestTarContextCancellation(t *testing.T) {
	tree := NewTree(TreeOpts{})
	for i := 0; i < 50; i++ {
		_ = tree.Write("/file"+string(rune('a'+i%26))+".txt", bytes.Repeat([]byte("x"), 4096), 0o644)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rc := tree.Tar(ctx, TarOpts{})
	defer rc.Close()
	cancel()
	// Drain. Either we hit ctx error or we read everything that was already
	// queued in the pipe before cancellation. Both outcomes are acceptable;
	// the contract is that cancel never deadlocks.
	_, _ = io.Copy(io.Discard, rc)
}

func entryNames(m map[string]tarEntry) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
