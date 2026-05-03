package vfs

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
)

// TarOpts controls tar streaming.
type TarOpts struct {
	// Root scopes the tar to a subtree. Empty or "/" tars the whole tree.
	// The Root path itself is not emitted; entries are emitted relative to
	// it (e.g. Root="/ctx" makes "/ctx/Dockerfile" appear as "Dockerfile").
	Root string

	// IncludeRoot, when true, emits the root directory itself as an entry
	// (named "./"). Most build-context consumers prefer it omitted.
	IncludeRoot bool

	// FollowSymlinks dereferences symlinks at tar time, emitting the target
	// content under the symlink's name. Default (false) emits the symlink as
	// a tar.TypeSymlink entry with the literal target string.
	FollowSymlinks bool
}

// Tar streams the tree (or a subtree, per opts.Root) as a tar archive.
// Entries are emitted in lexical Walk order so two runs over an unchanged
// tree produce byte-identical output — important for build-context cache
// keys.
//
// Goroutine ownership: a single writer goroutine is spawned to feed the
// pipe. It exits cleanly on any of:
//   - the walk completes (normal termination);
//   - ctx is cancelled (writeTar checks ctx in its Walk callback);
//   - the caller closes the returned reader (the next Write fails with
//     io.ErrClosedPipe and the goroutine unwinds).
//
// To avoid leaking the writer goroutine, callers MUST do one of:
// (1) drain to EOF and call Close, (2) cancel ctx, or (3) call Close on
// the reader. Best practice: always `defer rc.Close()` and pass a
// cancellable ctx.
func (t *Tree) Tar(ctx context.Context, opts TarOpts) io.ReadCloser {
	pr, pw := io.Pipe()

	root := opts.Root
	if root == "" {
		root = "/"
	}

	go func() {
		// We translate every error into a pipe-close-with-error so the
		// consumer's next Read surfaces it. Returning early without closing
		// would deadlock the reader.
		err := t.writeTar(ctx, pw, root, opts)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()

	return pr
}

// writeTar is the goroutine body for Tar. Holds no lock for the duration —
// it calls back into Tree methods (Walk, body reads) which take their own
// locks per call. This keeps long-running tar operations from blocking
// concurrent writes for the entire stream.
func (t *Tree) writeTar(ctx context.Context, w io.Writer, root string, opts TarOpts) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	// Resolve the root subtree once up front so we fail fast if it's
	// missing. We then re-Walk on the live tree so we observe a consistent
	// snapshot of metadata at write time.
	rootInode, err := t.Stat(root)
	if err != nil {
		return fmt.Errorf("vfs.Tar: root %q: %w", root, err)
	}
	if !rootInode.IsDir() {
		return fmt.Errorf("vfs.Tar: root %q: %w", root, ErrNotDir)
	}

	emitErr := t.Walk(func(p string, inode *Inode) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Three cases relative to the scope root:
		//   1. p is the scope root or beneath it → emit (subject to IncludeRoot)
		//   2. p is a strict ancestor of root → silently descend (don't emit)
		//   3. p is unrelated → skip the subtree
		switch {
		case pathInScope(p, root):
			if p == root && !opts.IncludeRoot {
				return nil
			}
			name := tarNameFor(p, root)
			return t.writeTarEntry(tw, name, p, inode, opts)
		case isAncestor(p, root):
			return nil
		default:
			if inode.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
	})
	if emitErr != nil {
		return emitErr
	}
	return tw.Close()
}

// writeTarEntry emits one tar header (and body, for files) for the given
// inode at logical path p. name is the on-tape path relative to root.
func (t *Tree) writeTarEntry(tw *tar.Writer, name, p string, inode *Inode, opts TarOpts) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    int64(inode.Mode.Perm()),
		ModTime: inode.ModTime,
		Format:  tar.FormatPAX,
	}

	switch {
	case inode.IsDir():
		hdr.Typeflag = tar.TypeDir
		if !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		return tw.WriteHeader(hdr)

	case inode.IsSymlink():
		if opts.FollowSymlinks {
			// Resolve the link, fetch the target inode, recurse the entry as
			// the target's content under the symlink's name. We don't follow
			// chains of symlinks — one hop only, to avoid loops.
			target, err := t.Stat(inode.LinkTarget)
			if err != nil {
				return fmt.Errorf("vfs.Tar: dereference %s -> %s: %w", p, inode.LinkTarget, err)
			}
			if target.IsDir() {
				// Following a symlink into a directory at tar time is
				// ambiguous (do we recurse?). Treat as an error rather than
				// silently flatten or skip.
				return fmt.Errorf("vfs.Tar: refusing to follow symlink %s into directory", p)
			}
			cloned := *target
			return t.writeTarEntry(tw, name, inode.LinkTarget, &cloned, opts)
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = inode.LinkTarget
		return tw.WriteHeader(hdr)

	case inode.IsFile():
		hdr.Typeflag = tar.TypeReg
		hdr.Size = inode.Size()
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Size == 0 {
			return nil
		}
		return t.streamBody(tw, inode)

	default:
		return fmt.Errorf("vfs.Tar: unsupported entry kind at %s", p)
	}
}

// streamBody writes the file's bytes into tw. We re-Stat under the tree lock
// (via Open) rather than trust the inode handed to us by Walk — Walk hands a
// clone, and a concurrent Write between Walk and stream could have replaced
// the body. Open gives us a fresh snapshot.
func (t *Tree) streamBody(tw *tar.Writer, inode *Inode) error {
	if inode.Body == nil {
		return nil
	}
	buf := make([]byte, 64<<10)
	var off int64
	size := inode.Size()
	for off < size {
		want := int64(len(buf))
		if remaining := size - off; remaining < want {
			want = remaining
		}
		n, err := inode.Body.ReadAt(buf[:want], off, t.reader, t.store)
		if n > 0 {
			if _, werr := tw.Write(buf[:n]); werr != nil {
				return werr
			}
			off += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	if off != size {
		return fmt.Errorf("vfs.Tar: short body: wrote %d of %d", off, size)
	}
	return nil
}

// pathInScope reports whether p is under root. root="/" matches everything.
func pathInScope(p, root string) bool {
	if root == "/" {
		return true
	}
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+"/")
}

// isAncestor reports whether p is a strict ancestor of root. Used by the tar
// walker to descend through ancestor directories of a scoped Root without
// emitting them as entries.
func isAncestor(p, root string) bool {
	if p == "/" && root != "/" {
		return true
	}
	return strings.HasPrefix(root, p+"/")
}

// tarNameFor returns the on-tape name for p given root.
//   - If p == root, returns "./".
//   - Otherwise, strips the root prefix and any leading slash.
func tarNameFor(p, root string) string {
	if p == root {
		return "./"
	}
	if root == "/" {
		return strings.TrimPrefix(p, "/")
	}
	rel := strings.TrimPrefix(p, root+"/")
	return path.Clean(rel)
}
