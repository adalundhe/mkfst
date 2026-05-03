//go:build darwin || windows

package vfs

import (
	"errors"
	"fmt"
	"io"
	gofs "io/fs"
	"os"
	"sync"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

// newMountDriver wires the Tree to a cgofuse FileSystemHost. The same
// implementation serves macOS (over macFUSE) and Windows (over WinFsp) —
// cgofuse is the unified userspace shim for both kernel drivers.
//
// Runtime requirements:
//   - macOS: macFUSE installed (https://osxfuse.github.io/). The kext must
//     be loadable; on recent macOS that means accepting the kext in System
//     Settings → Privacy & Security after first install.
//   - Windows: WinFsp installed (https://winfsp.dev/). The user account
//     needs permission to use it (default install grants this).
//
// If the driver isn't installed the FileSystemHost.Mount call returns
// false and we surface that as an explicit error — no silent fallback.
func newMountDriver(t *Tree, opts MountOpts) (mountDriver, chan struct{}, error) {
	if opts.Mountpoint == "" {
		return nil, nil, fmt.Errorf("vfs.Mount: empty mountpoint")
	}
	// On Windows the mountpoint can be a drive letter ("X:") or an unused
	// path; on macOS it must be an existing empty directory. We validate
	// the directory case but allow drive-letter form on Windows by
	// skipping the check when stat fails with ENOENT (Windows-style).
	info, err := os.Stat(opts.Mountpoint)
	switch {
	case err == nil:
		if !info.IsDir() {
			return nil, nil, fmt.Errorf("vfs.Mount: mountpoint %q is not a directory", opts.Mountpoint)
		}
	case errors.Is(err, os.ErrNotExist):
		// Windows drive-letter form, or "to-be-created" path on macOS — pass
		// through to cgofuse, which will reject more clearly than we can.
	default:
		return nil, nil, fmt.Errorf("vfs.Mount: stat mountpoint %q: %w", opts.Mountpoint, err)
	}

	cfs := &cgoFuseFS{
		tree:  t,
		ready: make(chan struct{}),
	}
	host := fuse.NewFileSystemHost(cfs)
	host.SetCapReaddirPlus(true)

	args := buildCgoFuseArgs(opts)

	done := make(chan struct{})
	mountResult := make(chan bool, 1)

	go func() {
		// host.Mount blocks for the lifetime of the mount. Returns false
		// if Mount couldn't start (driver missing, mountpoint busy, etc.)
		// and true after a clean unmount.
		ok := host.Mount(opts.Mountpoint, args)
		mountResult <- ok
		close(done)
	}()

	// Wait for either Init() to fire (mount is up) or the goroutine to
	// bail (mount failed). Without this, callers would proceed before
	// FUSE was actually ready, racing against the kernel's view of the
	// mountpoint. 5s is generous; macFUSE/WinFsp init usually completes
	// in well under 1s.
	select {
	case <-cfs.ready:
		return &cgoFuseDriver{host: host, done: done, mountResult: mountResult}, done, nil
	case ok := <-mountResult:
		if !ok {
			return nil, nil, fmt.Errorf("vfs.Mount: cgofuse Mount failed (driver missing or mountpoint busy?): %w", ErrFUSEUnavailable)
		}
		// Mount returned true before Init fired — shouldn't happen but be
		// defensive. Treat as success and synthesize a closed driver.
		return &cgoFuseDriver{host: host, done: done, mountResult: mountResult}, done, nil
	case <-time.After(5 * time.Second):
		host.Unmount()
		<-done
		return nil, nil, fmt.Errorf("vfs.Mount: cgofuse Init didn't complete within 5s: %w", ErrFUSEUnavailable)
	}
}

// buildCgoFuseArgs translates MountOpts into the string args cgofuse
// expects. Each backend interprets these slightly differently (macFUSE
// uses GNU-style -o key=val pairs; WinFsp accepts a similar superset),
// but the common subset we exercise here works on both.
func buildCgoFuseArgs(opts MountOpts) []string {
	args := []string{}
	if opts.Debug {
		args = append(args, "-d")
	}
	flags := []string{}
	if opts.AllowOther {
		flags = append(flags, "allow_other")
	}
	if opts.ReadOnly {
		flags = append(flags, "ro")
	}
	if opts.Name != "" {
		// fsname appears in `mount` listings on macOS and as the volume
		// label on Windows — useful for distinguishing multiple mkfst
		// mounts.
		flags = append(flags, "fsname="+opts.Name)
		flags = append(flags, "volname="+opts.Name) // macOS-specific alias
	}
	if len(flags) > 0 {
		args = append(args, "-o", joinComma(flags))
	}
	return args
}

func joinComma(items []string) string {
	if len(items) == 0 {
		return ""
	}
	out := items[0]
	for _, it := range items[1:] {
		out += "," + it
	}
	return out
}

// cgoFuseDriver wraps a FileSystemHost in the mountDriver interface.
type cgoFuseDriver struct {
	host        *fuse.FileSystemHost
	done        chan struct{}
	mountResult chan bool

	once  sync.Once
	final error
}

func (d *cgoFuseDriver) unmount() error {
	d.once.Do(func() {
		// FileSystemHost.Unmount returns true on success, false if the
		// mount isn't active. Either way, the public Mount.Unmount waits
		// on the done channel and falls back to lazyUnmount on timeout.
		_ = d.host.Unmount()
		select {
		case ok := <-d.mountResult:
			if !ok {
				d.final = fmt.Errorf("vfs.Unmount: original Mount failed")
			}
		default:
		}
	})
	return d.final
}

// lazyUnmount is a best-effort retry of host.Unmount. cgofuse doesn't
// expose a lazy-detach option (macFUSE/WinFsp don't have one in the C
// API), so we just keep nudging the host until it relents. In practice
// the second call usually succeeds because whatever ref was held has
// dropped.
func (d *cgoFuseDriver) lazyUnmount() {
	for i := 0; i < 5; i++ {
		if d.host.Unmount() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (d *cgoFuseDriver) wait() error {
	<-d.done
	return nil
}

// cgoFuseFS implements the cgofuse FileSystemInterface by delegating to
// the wrapped Tree. We embed FileSystemBase so unimplemented methods
// return -ENOSYS automatically (FUSE then falls back to default behavior
// for ops like Access, xattr, etc.).
type cgoFuseFS struct {
	fuse.FileSystemBase
	tree  *Tree
	ready chan struct{}

	readyOnce sync.Once
}

// Init signals that the mount is up and serving requests. We close the
// ready channel exactly once.
func (fs *cgoFuseFS) Init() {
	fs.readyOnce.Do(func() { close(fs.ready) })
}

// Statfs returns synthetic filesystem stats. The numbers are intentionally
// large and round — VFS isn't bounded by a real disk so reporting
// concrete usage would be misleading. macOS and Windows file managers use
// these to populate "free space" displays; large numbers keep them happy.
func (fs *cgoFuseFS) Statfs(path string, stat *fuse.Statfs_t) int {
	stat.Bsize = 4096
	stat.Frsize = 4096
	stat.Blocks = 1 << 32
	stat.Bfree = 1 << 32
	stat.Bavail = 1 << 32
	stat.Files = 1 << 32
	stat.Ffree = 1 << 32
	stat.Namemax = 255
	return 0
}

func (fs *cgoFuseFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	inode, err := fs.tree.Stat(path)
	if err != nil {
		return errToCgoErrno(err)
	}
	fillCgoStat(stat, inode)
	return 0
}

func (fs *cgoFuseFS) Opendir(path string) (int, uint64) {
	inode, err := fs.tree.Stat(path)
	if err != nil {
		return errToCgoErrno(err), 0
	}
	if !inode.IsDir() {
		return -fuse.ENOTDIR, 0
	}
	return 0, 0
}

func (fs *cgoFuseFS) Readdir(
	path string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64,
) int {
	entries, err := fs.tree.ReadDir(path)
	if err != nil {
		return errToCgoErrno(err)
	}
	if !fill(".", nil, 0) {
		return 0
	}
	if !fill("..", nil, 0) {
		return 0
	}
	for _, e := range entries {
		var stat fuse.Stat_t
		fillCgoStat(&stat, e.Inode)
		if !fill(e.Name, &stat, 0) {
			break
		}
	}
	return 0
}

// Open is a no-op — Read/Write resolve from the path each call so we
// don't need per-fd state. Returning fh=0 is fine because FileSystemBase's
// Release is a no-op too.
func (fs *cgoFuseFS) Open(path string, flags int) (int, uint64) {
	return 0, 0
}

func (fs *cgoFuseFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	inode, err := fs.tree.Stat(path)
	if err != nil {
		return errToCgoErrno(err)
	}
	if !inode.IsFile() {
		return -fuse.EISDIR
	}
	if inode.Body == nil {
		return 0
	}
	n, rerr := inode.Body.ReadAt(buff, ofst, fs.tree.reader, fs.tree.store)
	if rerr != nil && rerr != io.EOF {
		return errToCgoErrno(rerr)
	}
	return n
}

// Write splices buff into the file at ofst. Same simplification as the
// Linux backend — we don't track open-file state, so each Write reads,
// patches, and writes back. For pathological random-write workloads,
// callers should drive the Tree API directly.
func (fs *cgoFuseFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	current, err := fs.tree.Read(path)
	if err != nil {
		return errToCgoErrno(err)
	}
	end := int(ofst) + len(buff)
	if end > len(current) {
		next := make([]byte, end)
		copy(next, current)
		current = next
	}
	copy(current[ofst:], buff)
	if err := fs.tree.Write(path, current, 0o644); err != nil {
		return errToCgoErrno(err)
	}
	return len(buff)
}

func (fs *cgoFuseFS) Mknod(path string, mode uint32, dev uint64) int {
	if err := fs.tree.Write(path, nil, gofs.FileMode(mode&0o7777)); err != nil {
		return errToCgoErrno(err)
	}
	return 0
}

func (fs *cgoFuseFS) Create(path string, flags int, mode uint32) (int, uint64) {
	if err := fs.tree.Write(path, nil, gofs.FileMode(mode&0o7777)); err != nil {
		return errToCgoErrno(err), 0
	}
	return 0, 0
}

func (fs *cgoFuseFS) Mkdir(path string, mode uint32) int {
	if err := fs.tree.Mkdir(path, gofs.FileMode(mode&0o7777)); err != nil {
		return errToCgoErrno(err)
	}
	return 0
}

func (fs *cgoFuseFS) Unlink(path string) int {
	if err := fs.tree.Remove(path); err != nil {
		return errToCgoErrno(err)
	}
	return 0
}

func (fs *cgoFuseFS) Rmdir(path string) int { return fs.Unlink(path) }

func (fs *cgoFuseFS) Rename(oldpath, newpath string) int {
	if err := fs.tree.Rename(oldpath, newpath); err != nil {
		return errToCgoErrno(err)
	}
	return 0
}

func (fs *cgoFuseFS) Symlink(target, linkPath string) int {
	if err := fs.tree.Symlink(target, linkPath); err != nil {
		return errToCgoErrno(err)
	}
	return 0
}

func (fs *cgoFuseFS) Readlink(path string) (int, string) {
	inode, err := fs.tree.Stat(path)
	if err != nil {
		return errToCgoErrno(err), ""
	}
	if !inode.IsSymlink() {
		return -fuse.EINVAL, ""
	}
	return 0, inode.LinkTarget
}

func (fs *cgoFuseFS) Chmod(path string, mode uint32) int {
	if err := fs.tree.Chmod(path, gofs.FileMode(mode&0o7777)); err != nil {
		return errToCgoErrno(err)
	}
	return 0
}

// Chown is accepted but ignored — VFS doesn't model uid/gid (everything
// runs as the mounting user). Returning 0 satisfies tools like `cp -p`
// that try to preserve ownership; -ENOSYS would make them fail loudly.
func (fs *cgoFuseFS) Chown(path string, uid, gid uint32) int { return 0 }

func (fs *cgoFuseFS) Truncate(path string, size int64, fh uint64) int {
	current, err := fs.tree.Read(path)
	if err != nil {
		return errToCgoErrno(err)
	}
	switch {
	case int64(len(current)) == size:
		return 0
	case int64(len(current)) > size:
		current = current[:size]
	default:
		next := make([]byte, size)
		copy(next, current)
		current = next
	}
	if err := fs.tree.Write(path, current, 0o644); err != nil {
		return errToCgoErrno(err)
	}
	return 0
}

func (fs *cgoFuseFS) Utimens(path string, tmsp []fuse.Timespec) int {
	if len(tmsp) >= 2 {
		mtime := time.Unix(tmsp[1].Sec, tmsp[1].Nsec)
		if err := fs.tree.Chtime(path, mtime); err != nil {
			return errToCgoErrno(err)
		}
	}
	return 0
}

// fillCgoStat maps an inode into a cgofuse Stat_t.
func fillCgoStat(stat *fuse.Stat_t, inode *Inode) {
	stat.Ino = inode.ID
	stat.Mode = toCgoMode(inode.Kind, inode.Mode)
	stat.Nlink = uint32(inode.Nlink)
	if stat.Nlink == 0 {
		stat.Nlink = 1
	}
	stat.Size = inode.Size()
	ts := fuse.Timespec{Sec: inode.ModTime.Unix(), Nsec: int64(inode.ModTime.Nanosecond())}
	stat.Atim = ts
	stat.Mtim = ts
	stat.Ctim = ts
	stat.Birthtim = ts
}

func toCgoMode(kind EntryKind, mode gofs.FileMode) uint32 {
	perm := uint32(mode.Perm())
	switch kind {
	case EntryDir:
		return perm | fuse.S_IFDIR
	case EntrySymlink:
		return perm | fuse.S_IFLNK
	default:
		return perm | fuse.S_IFREG
	}
}

// errToCgoErrno maps VFS errors to negative cgofuse errno values.
// cgofuse uses POSIX-like negative ints to signal errors and 0/positive
// for success.
func errToCgoErrno(err error) int {
	switch err {
	case nil:
		return 0
	case ErrNotExist:
		return -fuse.ENOENT
	case ErrExist:
		return -fuse.EEXIST
	case ErrNotDir:
		return -fuse.ENOTDIR
	case ErrIsDir:
		return -fuse.EISDIR
	case ErrInvalidPath:
		return -fuse.EINVAL
	case ErrNotEmpty:
		return -fuse.ENOTEMPTY
	case ErrChunkNotFound, ErrExtentCorruption:
		return -fuse.EIO
	case ErrMemoryPressure:
		return -fuse.ENOSPC
	case ErrInvalidRange, ErrNegativeOffset:
		return -fuse.EINVAL
	}
	return -fuse.EIO
}
