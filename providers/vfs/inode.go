package vfs

import (
	"io/fs"
	"sync/atomic"
	"time"
)

// EntryKind discriminates the three filesystem entry types VFS supports.
type EntryKind uint8

const (
	// EntryFile is a regular file with a FileBody.
	EntryFile EntryKind = iota
	// EntryDir is a directory; holds no body, but has children in the tree.
	EntryDir
	// EntrySymlink is a symbolic link; LinkTarget holds the target path.
	EntrySymlink
)

// Inode is the metadata-and-body record for a single VFS entry. Inode IDs are
// assigned monotonically by the owning Tree and are stable for the lifetime
// of the entry. Callers should not construct Inodes directly — use the Tree's
// Mkdir/Write/Symlink methods.
type Inode struct {
	ID         uint64      // monotonically assigned, stable for lifetime
	Kind       EntryKind   // file / dir / symlink
	Mode       fs.FileMode // permission + type bits
	ModTime    time.Time   // last modification timestamp
	Nlink      int         // hard-link count; dirs always 2 (. and ..)
	Body       *FileBody   // EntryFile only
	LinkTarget string      // EntrySymlink only
}

// Clone returns a deep copy of the inode, including its body extents. The
// underlying chunks are not copied (they remain refcounted in ChunkStore).
func (i *Inode) Clone() *Inode {
	if i == nil {
		return nil
	}
	out := *i
	if i.Body != nil {
		out.Body = i.Body.Clone()
	}
	return &out
}

// Size returns the logical byte length of the inode: body size for files,
// symlink target length for symlinks, 0 for directories.
func (i *Inode) Size() int64 {
	if i == nil {
		return 0
	}
	switch i.Kind {
	case EntryFile:
		if i.Body == nil {
			return 0
		}
		return i.Body.Size()
	case EntrySymlink:
		return int64(len(i.LinkTarget))
	default:
		return 0
	}
}

// IsDir reports whether the inode is a directory.
func (i *Inode) IsDir() bool {
	return i != nil && i.Kind == EntryDir
}

// IsFile reports whether the inode is a regular file.
func (i *Inode) IsFile() bool {
	return i != nil && i.Kind == EntryFile
}

// IsSymlink reports whether the inode is a symbolic link.
func (i *Inode) IsSymlink() bool {
	return i != nil && i.Kind == EntrySymlink
}

// inodeAllocator hands out monotonically increasing inode IDs. Inode 1 is
// reserved for the root directory by convention (matches POSIX expectations
// from FUSE clients that special-case ino=1).
type inodeAllocator struct {
	next atomic.Uint64
}

func newInodeAllocator() *inodeAllocator {
	a := &inodeAllocator{}
	a.next.Store(1) // root takes 1; first allocated id will be 2
	return a
}

// Next returns the next available inode ID.
func (a *inodeAllocator) Next() uint64 {
	return a.next.Add(1)
}
