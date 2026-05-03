//go:build linux

package vfs

import (
	"context"
	"fmt"
	"io"
	gofs "io/fs"
	"os"
	"os/exec"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// newMountDriver is the Linux build of the platform-specific mount factory.
// Uses hanwen/go-fuse, which speaks the FUSE wire protocol over /dev/fuse
// directly — no libfuse, no CGO. Output binaries on Linux are pure Go.
func newMountDriver(t *Tree, opts MountOpts) (mountDriver, chan struct{}, error) {
	if opts.Mountpoint == "" {
		return nil, nil, fmt.Errorf("vfs.Mount: empty mountpoint")
	}
	info, err := os.Stat(opts.Mountpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("vfs.Mount: stat mountpoint %q: %w", opts.Mountpoint, err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("vfs.Mount: mountpoint %q is not a directory", opts.Mountpoint)
	}

	root := &linuxFuseNode{tree: t, vfsPath: "/"}
	mountOpts := &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:     opts.Name,
			Name:       opts.Name,
			AllowOther: opts.AllowOther,
			Debug:      opts.Debug,
		},
	}
	if opts.ReadOnly {
		mountOpts.MountOptions.Options = append(mountOpts.MountOptions.Options, "ro")
	}

	server, err := fs.Mount(opts.Mountpoint, root, mountOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("vfs.Mount: %w", err)
	}

	done := make(chan struct{})
	driver := &linuxMountDriver{
		server:     server,
		mountpoint: opts.Mountpoint,
		done:       done,
	}
	go func() {
		// server.Wait blocks until the FUSE loop exits.
		server.Wait()
		close(done)
	}()
	return driver, done, nil
}

// linuxMountDriver wraps a go-fuse Server. mountpoint is retained so
// lazyUnmount has a path to point fusermount at when normal unmount is
// blocked on EBUSY.
type linuxMountDriver struct {
	server     *fuse.Server
	mountpoint string
	done       chan struct{}
}

func (d *linuxMountDriver) unmount() error { return d.server.Unmount() }

// lazyUnmount escalates to `fusermount3 -uz` (then `fusermount -uz` as a
// fallback for older systems). The -z flag tells the kernel to detach
// immediately and finalize when the last reference drops — the only
// reliable way to release a FUSE mount that's wedged on EBUSY because a
// host process briefly held a CWD ref into the mount.
func (d *linuxMountDriver) lazyUnmount() {
	for _, bin := range []string{"fusermount3", "fusermount"} {
		if err := exec.Command(bin, "-uz", d.mountpoint).Run(); err == nil {
			return
		}
	}
	// Last-ditch: umount(8) -l. Requires CAP_SYS_ADMIN; some test
	// environments grant it.
	_ = exec.Command("umount", "-l", d.mountpoint).Run()
}

func (d *linuxMountDriver) wait() error {
	<-d.done
	return nil
}

// linuxFuseNode is the FUSE inode that delegates all operations back to the
// Tree. We hold an absolute VFS path so child resolution is unambiguous —
// FUSE cookies are not stable across re-Lookup, but our VFS paths are.
//
// Exported method receivers must be value types because go-fuse's fs.Inode
// embedding takes the address of the embedded field; we satisfy by embedding
// fs.Inode and putting state on a pointer.
type linuxFuseNode struct {
	fs.Inode
	tree    *Tree
	vfsPath string
}

// childPath returns the VFS path for the named entry under this node.
func (n *linuxFuseNode) childPath(name string) string {
	return joinPath(n.vfsPath, name)
}

// Compile-time assertions that we implement every operation we declare.
var (
	_ fs.NodeLookuper   = (*linuxFuseNode)(nil)
	_ fs.NodeGetattrer  = (*linuxFuseNode)(nil)
	_ fs.NodeReaddirer  = (*linuxFuseNode)(nil)
	_ fs.NodeOpener     = (*linuxFuseNode)(nil)
	_ fs.NodeReader     = (*linuxFuseNode)(nil)
	_ fs.NodeReadlinker = (*linuxFuseNode)(nil)
	_ fs.NodeMkdirer    = (*linuxFuseNode)(nil)
	_ fs.NodeCreater    = (*linuxFuseNode)(nil)
	_ fs.NodeWriter     = (*linuxFuseNode)(nil)
	_ fs.NodeUnlinker   = (*linuxFuseNode)(nil)
	_ fs.NodeRmdirer    = (*linuxFuseNode)(nil)
	_ fs.NodeRenamer    = (*linuxFuseNode)(nil)
	_ fs.NodeSymlinker  = (*linuxFuseNode)(nil)
	_ fs.NodeSetattrer  = (*linuxFuseNode)(nil)
)

// fillAttr maps a VFS Inode into a FUSE attr block. ino is the fs-level inode
// id (we pass through the VFS inode ID for stability).
func fillAttr(inode *Inode, attr *fuse.Attr) {
	attr.Ino = inode.ID
	attr.Size = uint64(inode.Size())
	attr.Mode = uint32(toFuseMode(inode.Kind, inode.Mode))
	attr.Mtime = uint64(inode.ModTime.Unix())
	attr.Mtimensec = uint32(inode.ModTime.Nanosecond())
	attr.Atime = attr.Mtime
	attr.Atimensec = attr.Mtimensec
	attr.Ctime = attr.Mtime
	attr.Ctimensec = attr.Mtimensec
	attr.Nlink = uint32(inode.Nlink)
	if inode.IsDir() {
		// Directories have at least 2 hard links (./.. plus parent's entry).
		if attr.Nlink < 2 {
			attr.Nlink = 2
		}
	}
}

// toFuseMode converts our (Kind, Mode) pair to a Unix-style mode word with
// the right type bits set, matching what stat() returns. go-fuse expects the
// type bits in the high nibble.
func toFuseMode(kind EntryKind, mode gofs.FileMode) uint32 {
	perm := uint32(mode.Perm())
	switch kind {
	case EntryDir:
		return perm | syscall.S_IFDIR
	case EntrySymlink:
		return perm | syscall.S_IFLNK
	default:
		return perm | syscall.S_IFREG
	}
}

func (n *linuxFuseNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := n.childPath(name)
	inode, err := n.tree.Stat(childPath)
	if err != nil {
		return nil, errToErrno(err)
	}
	fillAttr(inode, &out.Attr)
	out.NodeId = inode.ID

	stable := fs.StableAttr{
		Mode: out.Attr.Mode & syscall.S_IFMT,
		Ino:  inode.ID,
	}
	child := &linuxFuseNode{tree: n.tree, vfsPath: childPath}
	return n.NewInode(ctx, child, stable), 0
}

func (n *linuxFuseNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	inode, err := n.tree.Stat(n.vfsPath)
	if err != nil {
		return errToErrno(err)
	}
	fillAttr(inode, &out.Attr)
	return 0
}

func (n *linuxFuseNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.tree.ReadDir(n.vfsPath)
	if err != nil {
		return nil, errToErrno(err)
	}
	out := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, fuse.DirEntry{
			Name: e.Name,
			Mode: toFuseMode(e.Inode.Kind, e.Inode.Mode),
			Ino:  e.Inode.ID,
		})
	}
	return fs.NewListDirStream(out), 0
}

// Open is a no-op; we don't track per-open state — Read/Write resolve from
// the path each call. FOPEN_DIRECT_IO disables kernel page caching, which
// would otherwise serve stale bytes after a Tree-level Write while a FUSE
// fd is open.
func (n *linuxFuseNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	return nil, fuse.FOPEN_DIRECT_IO, 0
}

func (n *linuxFuseNode) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	inode, err := n.tree.Stat(n.vfsPath)
	if err != nil {
		return nil, errToErrno(err)
	}
	if !inode.IsFile() {
		return nil, syscall.EISDIR
	}
	if inode.Body == nil {
		return fuse.ReadResultData(nil), 0
	}
	nread, rerr := inode.Body.ReadAt(dest, off, n.tree.reader, n.tree.store)
	if rerr != nil && rerr != io.EOF {
		return nil, errToErrno(rerr)
	}
	return fuse.ReadResultData(dest[:nread]), 0
}

func (n *linuxFuseNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	inode, err := n.tree.Stat(n.vfsPath)
	if err != nil {
		return nil, errToErrno(err)
	}
	if !inode.IsSymlink() {
		return nil, syscall.EINVAL
	}
	return []byte(inode.LinkTarget), 0
}

func (n *linuxFuseNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := n.childPath(name)
	if err := n.tree.Mkdir(childPath, gofs.FileMode(mode)); err != nil {
		return nil, errToErrno(err)
	}
	return n.Lookup(ctx, name, out)
}

func (n *linuxFuseNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	childPath := n.childPath(name)
	if err := n.tree.Write(childPath, nil, gofs.FileMode(mode)); err != nil {
		return nil, nil, 0, errToErrno(err)
	}
	child, errno := n.Lookup(ctx, name, out)
	if errno != 0 {
		return nil, nil, 0, errno
	}
	return child, nil, fuse.FOPEN_DIRECT_IO, 0
}

func (n *linuxFuseNode) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	// We don't track open file state, so each Write is a logical "splice
	// these bytes at off into the named file". Read the current content,
	// splice, write back. For large files this is O(file size) per write —
	// acceptable for VFS-as-build-context (writes are typically batch and
	// from-scratch via Tree.Write); pathological random-access workloads
	// should write through the Tree API directly.
	current, err := n.tree.Read(n.vfsPath)
	if err != nil {
		return 0, errToErrno(err)
	}
	end := int(off) + len(data)
	if end > len(current) {
		next := make([]byte, end)
		copy(next, current)
		current = next
	}
	copy(current[off:], data)
	if err := n.tree.Write(n.vfsPath, current, 0o644); err != nil {
		return 0, errToErrno(err)
	}
	return uint32(len(data)), 0
}

func (n *linuxFuseNode) Unlink(ctx context.Context, name string) syscall.Errno {
	if err := n.tree.Remove(n.childPath(name)); err != nil {
		return errToErrno(err)
	}
	return 0
}

func (n *linuxFuseNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	if err := n.tree.Remove(n.childPath(name)); err != nil {
		return errToErrno(err)
	}
	return 0
}

func (n *linuxFuseNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	parent, ok := newParent.(*linuxFuseNode)
	if !ok {
		return syscall.EXDEV
	}
	if err := n.tree.Rename(n.childPath(name), parent.childPath(newName)); err != nil {
		return errToErrno(err)
	}
	return 0
}

func (n *linuxFuseNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if err := n.tree.Symlink(target, n.childPath(name)); err != nil {
		return nil, errToErrno(err)
	}
	return n.Lookup(ctx, name, out)
}

// Setattr handles chmod, chtime, and truncate on a single op (FUSE bundles).
// We branch on the Valid bitmask to apply only the requested fields.
func (n *linuxFuseNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if mode, ok := in.GetMode(); ok {
		if err := n.tree.Chmod(n.vfsPath, gofs.FileMode(mode)); err != nil {
			return errToErrno(err)
		}
	}
	if mtime, ok := in.GetMTime(); ok {
		if err := n.tree.Chtime(n.vfsPath, mtime); err != nil {
			return errToErrno(err)
		}
	}
	if size, ok := in.GetSize(); ok {
		current, err := n.tree.Read(n.vfsPath)
		if err != nil {
			return errToErrno(err)
		}
		switch {
		case uint64(len(current)) == size:
			// no-op
		case uint64(len(current)) > size:
			current = current[:size]
		default:
			next := make([]byte, size)
			copy(next, current)
			current = next
		}
		if err := n.tree.Write(n.vfsPath, current, 0o644); err != nil {
			return errToErrno(err)
		}
	}
	inode, err := n.tree.Stat(n.vfsPath)
	if err != nil {
		return errToErrno(err)
	}
	fillAttr(inode, &out.Attr)
	return 0
}

// errToErrno maps a VFS error into the closest POSIX errno for FUSE.
func errToErrno(err error) syscall.Errno {
	switch err {
	case nil:
		return 0
	case ErrNotExist:
		return syscall.ENOENT
	case ErrExist:
		return syscall.EEXIST
	case ErrNotDir:
		return syscall.ENOTDIR
	case ErrIsDir:
		return syscall.EISDIR
	case ErrInvalidPath:
		return syscall.EINVAL
	case ErrNotEmpty:
		return syscall.ENOTEMPTY
	case ErrChunkNotFound, ErrExtentCorruption:
		return syscall.EIO
	case ErrMemoryPressure:
		return syscall.ENOSPC
	case ErrInvalidRange, ErrNegativeOffset:
		return syscall.EINVAL
	}
	// Unknown — surface as I/O error rather than a misleading specific code.
	return syscall.EIO
}

