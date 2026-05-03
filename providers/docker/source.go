package docker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mkfst/providers/vfs"
)

// Source produces a tar stream of build-context bytes for Build to upload to
// the daemon. Implementations must close the returned reader when the caller
// is done; callers must Close even on early-exit so the underlying writer
// goroutine doesn't leak.
//
// The default source for mkfst is *vfs.Tree (via VFSSource) — in-memory,
// content-addressable, deterministic. DirSource and TarSource cover the
// "I already have files on disk" and "I already have a tar" cases.
type Source interface {
	Tar(ctx context.Context) (io.ReadCloser, error)
}

// VFSSource adapts a *vfs.Tree to Source. The build context is generated
// fresh on each Tar call by streaming the tree (with optional Root scoping)
// through tar; this is what makes in-memory builds the default. The tar
// stream honors ctx cancellation.
type VFSSource struct {
	// Tree is the source of truth. Files added before Build is called are
	// included; the tree may continue to be modified after Build returns
	// (subsequent Builds see the new state).
	Tree *vfs.Tree
	// Root scopes the build context to a subtree (passed to vfs.TarOpts).
	// Empty means the entire tree is included. Set this when the tree
	// contains material outside the context (e.g. caches, source for other
	// services).
	Root string
	// FollowSymlinks dereferences symlinks at tar time. Default false
	// (symlinks are emitted as tar.TypeSymlink entries).
	FollowSymlinks bool
}

// NewVFSSource is the convenience constructor for the common case (whole
// tree, default opts). For Root or FollowSymlinks customization, construct
// VFSSource directly.
func NewVFSSource(tree *vfs.Tree) *VFSSource { return &VFSSource{Tree: tree} }

// Tar implements Source.
func (v *VFSSource) Tar(ctx context.Context) (io.ReadCloser, error) {
	if v.Tree == nil {
		return nil, fmt.Errorf("docker.VFSSource: nil tree")
	}
	return v.Tree.Tar(ctx, vfs.TarOpts{
		Root:           v.Root,
		FollowSymlinks: v.FollowSymlinks,
	}), nil
}

// DirSource adapts a host directory path to Source. The directory is walked
// and tarred at Build time; subsequent file changes inside the directory are
// not seen by an in-flight build but ARE seen by subsequent builds.
//
// Use this when you have an existing on-disk project layout you don't want
// to materialize into VFS first. For mixed workloads (some VFS, some host),
// build a VFSSource with a HostOverlay-configured tree instead — VFS handles
// the union and you keep one Source.
//
// Goroutine ownership: Tar spawns one writer goroutine to feed the pipe.
// Same caller obligations as vfs.Tree.Tar — drain to EOF, cancel ctx, or
// close the reader to avoid leaking the writer.
type DirSource string

// Tar implements Source.
func (d DirSource) Tar(ctx context.Context) (io.ReadCloser, error) {
	root := string(d)
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("docker.DirSource: stat %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("docker.DirSource: %q is not a directory", root)
	}

	pr, pw := io.Pipe()
	go func() {
		err := tarDir(ctx, pw, root)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	return pr, nil
}

// tarDir walks root and writes every entry into a tar stream on w. Names in
// the tar are emitted relative to root (so root itself isn't an entry).
// Symlinks are emitted as tar.TypeSymlink (not followed) — matches docker
// CLI behavior for `docker build .`.
func tarDir(ctx context.Context, w io.Writer, root string) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Use forward slashes in tar names regardless of host OS.
		name := filepath.ToSlash(rel)

		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name = strings.TrimSuffix(name, "/") + "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		return err
	})
}

// TarSource is a passthrough Source backed by an existing tar reader. Use
// when you've already produced a tar (e.g. via go-containerregistry, an
// upstream cache, an external builder) and want Build to forward the bytes
// without re-tarring.
type TarSource struct {
	// Reader is the tar stream. TarSource takes responsibility for closing
	// it if it implements io.Closer; otherwise it's left as-is. Reader is
	// consumed exactly once per Build.
	Reader io.Reader
}

// Tar implements Source.
func (t *TarSource) Tar(_ context.Context) (io.ReadCloser, error) {
	if t.Reader == nil {
		return nil, fmt.Errorf("docker.TarSource: nil reader")
	}
	if rc, ok := t.Reader.(io.ReadCloser); ok {
		return rc, nil
	}
	return io.NopCloser(t.Reader), nil
}
