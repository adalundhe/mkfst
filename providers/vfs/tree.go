package vfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNotExist mirrors fs.ErrNotExist; returned when a path doesn't
	// resolve in the in-memory tree (and isn't shadowed by an overlay).
	ErrNotExist = fs.ErrNotExist
	// ErrExist mirrors fs.ErrExist; returned when a Mkdir/Symlink target
	// already exists.
	ErrExist = fs.ErrExist
	// ErrNotDir is returned when an op requires a directory but encounters
	// a file/symlink at that path.
	ErrNotDir = errors.New("vfs: not a directory")
	// ErrIsDir is returned when an op (e.g. Read) requires a file but
	// encounters a directory at that path.
	ErrIsDir = errors.New("vfs: is a directory")
	// ErrInvalidPath is returned for paths that escape the root, contain
	// empty components, or are otherwise unrepresentable.
	ErrInvalidPath = errors.New("vfs: invalid path")
	// ErrNotEmpty is returned by Remove on a non-empty directory.
	ErrNotEmpty = errors.New("vfs: directory not empty")
)

// node is the internal tree node. The exported Inode is its metadata; the
// children map is internal-only because fs.FS clients should use Walk/Open
// rather than reach into the tree directly.
type node struct {
	inode    *Inode
	children map[string]*node // dir children, keyed by basename
}

// Tree is a path-keyed in-memory filesystem tree backed by a ChunkStore.
// Optionally unioned with a read-only host directory (the overlay), so
// host files appear at their corresponding VFS paths and in-memory writes
// shadow them. Removing a path that exists only on host adds a whiteout so
// the host version stops appearing.
//
// All ops are safe for concurrent use. A single RWMutex guards the tree
// shape; per-inode body ops also take the same lock — this is conservative
// but keeps semantics simple. Read-heavy workloads still benefit because the
// ChunkStore itself takes its own RWLock and most read paths don't mutate
// the tree.
type Tree struct {
	mu    sync.RWMutex
	root  *node
	store *ChunkStore
	alloc *inodeAllocator

	chunkSize int            // chunking granularity for new file content
	reader    RealFileReader // resolver for ExtentRealFile reads

	// Overlay state.
	hostRoot  string              // absolute host path; "" disables overlay
	hostCache *hostReadCache      // LRU read cache for host fall-through
	hostNodes map[string]*node    // synthesized inodes for host-only paths
	whiteouts map[string]struct{} // VFS paths hidden from the overlay

	// Mount state. Only one FUSE mount per Tree at a time.
	mount *Mount

	// Subscription state — see events.go. Separate mutex so publishing
	// doesn't contend with tree mutations.
	subsMu      sync.RWMutex
	subscribers map[uint64]*subscriber
	subAlloc    atomic.Uint64
}

// TreeOpts configures Tree construction.
type TreeOpts struct {
	// MemoryLimit caps the underlying chunk store. 0 disables the cap.
	MemoryLimit int64
	// ChunkSize is the chunking granularity for newly-written file content.
	// 0 means "store whole content as one chunk"; positive values chunk to
	// that byte size. Larger chunks reduce per-file extent count; smaller
	// chunks improve dedup hit rate for similar files.
	ChunkSize int
	// RealReader resolves ExtentRealFile reads. If nil, OSRealFileReader is
	// used (transparently wrapped with the host LRU cache when an overlay
	// is configured).
	RealReader RealFileReader
	// HostOverlay, if non-empty, is the absolute host directory unioned in
	// read-only beneath the in-memory tree. Host files appear at their
	// corresponding VFS paths; in-memory writes shadow them; Remove on a
	// host-only path adds a whiteout so the host version stops appearing.
	HostOverlay string
	// HostCacheBudget is the LRU ceiling for the host read cache, in bytes.
	// 0 uses DefaultHostCacheBudget; negative disables the cache (every host
	// read goes straight to the OS read path).
	HostCacheBudget int64
}

// NewTree returns a Tree with a freshly-allocated chunk store and a single
// empty root directory at "/".
func NewTree(opts TreeOpts) *Tree {
	store := NewChunkStore(opts.MemoryLimit)
	alloc := newInodeAllocator()

	root := &node{
		inode: &Inode{
			ID:      1,
			Kind:    EntryDir,
			Mode:    fs.ModeDir | 0o755,
			ModTime: time.Now().UTC(),
			Nlink:   2,
		},
		children: make(map[string]*node),
	}

	base := opts.RealReader
	if base == nil {
		base = OSRealFileReader{}
	}

	t := &Tree{
		root:      root,
		store:     store,
		alloc:     alloc,
		chunkSize: opts.ChunkSize,
		hostNodes: make(map[string]*node),
		whiteouts: make(map[string]struct{}),
	}

	if opts.HostOverlay != "" {
		t.hostRoot = opts.HostOverlay
		budget := opts.HostCacheBudget
		if budget == 0 {
			budget = DefaultHostCacheBudget
		}
		if budget > 0 {
			t.hostCache = newHostReadCache(budget)
		}
		t.reader = &hostCachedReader{cache: t.hostCache, base: base}
	} else {
		t.reader = base
	}

	return t
}

// Store returns the underlying chunk store. Exposed for telemetry, the
// memory-compression layer, and tests; callers must not Put/Acquire/Release
// directly without coordinating with Tree state.
func (t *Tree) Store() *ChunkStore { return t.store }

// Mkdir creates a directory at p, allocating a new inode. Returns ErrExist
// if an entry already exists there, or fs.ErrNotExist if the parent is
// missing.
func (t *Tree) Mkdir(p string, mode fs.FileMode) error {
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	parent, name, err := t.splitPath(p)
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	parentNode, err := t.materializeDirLocked(parent)
	if err != nil {
		return err
	}
	if _, ok := parentNode.children[name]; ok {
		return ErrExist
	}
	t.clearWhiteoutLocked(clean)

	now := time.Now().UTC()
	parentNode.children[name] = &node{
		inode: &Inode{
			ID:      t.alloc.Next(),
			Kind:    EntryDir,
			Mode:    fs.ModeDir | (mode & fs.ModePerm),
			ModTime: now,
			Nlink:   2,
		},
		children: make(map[string]*node),
	}
	parentNode.inode.ModTime = now
	t.publish(ChangeEvent{Path: clean, Op: OpMkdir, ModTime: now})
	return nil
}

// MkdirAll creates p and any missing parents, similar to os.MkdirAll. mode
// is applied to newly-created directories. If p exists and is a directory,
// returns nil; if it exists and is not a directory, returns ErrNotDir.
func (t *Tree) MkdirAll(p string, mode fs.FileMode) error {
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	if clean == "/" {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	parts := splitNonEmpty(clean)
	cursor := t.root
	for i, part := range parts {
		// Memory hit (or already-materialized overlay dir).
		if existing, ok := cursor.children[part]; ok {
			if !existing.inode.IsDir() {
				return ErrNotDir
			}
			cursor = existing
			continue
		}
		// In-memory miss. With overlay, the corresponding host dir might
		// already exist — materialize it (link a real children map) instead
		// of pretending it doesn't. If host has a non-dir at this path, that
		// shadows our request and we error.
		prefix := "/" + strings.Join(parts[:i+1], "/")
		if t.hostRoot != "" && !t.isWhiteoutLocked(prefix) {
			if hostDir, ok := t.hostDirAtLocked(prefix); ok {
				cursor.children[part] = hostDir
				cursor = hostDir
				continue
			}
			if t.hostHasNonDirLocked(prefix) {
				return ErrNotDir
			}
		}
		t.clearWhiteoutLocked(prefix)
		now := time.Now().UTC()
		newDir := &node{
			inode: &Inode{
				ID:      t.alloc.Next(),
				Kind:    EntryDir,
				Mode:    fs.ModeDir | (mode & fs.ModePerm),
				ModTime: now,
				Nlink:   2,
			},
			children: make(map[string]*node),
		}
		cursor.children[part] = newDir
		cursor.inode.ModTime = now
		t.publish(ChangeEvent{Path: prefix, Op: OpMkdir, ModTime: now})
		cursor = newDir
	}
	return nil
}

// Write stores content at p, creating the file (and any missing parent
// directories) if needed. mode is applied only on creation; existing files
// keep their mode. content is chunked per Tree.chunkSize.
func (t *Tree) Write(p string, content []byte, mode fs.FileMode) error {
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	parent, name, err := t.splitPath(p)
	if err != nil {
		return err
	}
	if err := t.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Materialize the parent into the in-memory tree if it currently lives
	// only in the overlay. The user is about to add a child, so we need a
	// real parent node to attach to.
	parentNode, err := t.materializeDirLocked(parent)
	if err != nil {
		return err
	}

	body, err := t.makeFileBodyLocked(content)
	if err != nil {
		return err
	}

	t.clearWhiteoutLocked(clean)

	now := time.Now().UTC()
	if existing, ok := parentNode.children[name]; ok {
		if !existing.inode.IsFile() {
			return ErrIsDir
		}
		existing.inode.Body = body
		existing.inode.ModTime = now
		t.publish(ChangeEvent{Path: clean, Op: OpWrite, ModTime: now})
		return nil
	}

	parentNode.children[name] = &node{
		inode: &Inode{
			ID:      t.alloc.Next(),
			Kind:    EntryFile,
			Mode:    mode & fs.ModePerm,
			ModTime: now,
			Nlink:   1,
			Body:    body,
		},
	}
	parentNode.inode.ModTime = now
	t.publish(ChangeEvent{Path: clean, Op: OpWrite, ModTime: now})
	return nil
}

// WriteReader is the streaming variant of Write. It buffers r into memory
// (the whole file) before chunking; for very large files the caller should
// instead allocate a body manually and stream extents. Most build-context
// inputs are small enough that the simpler buffered API is appropriate.
func (t *Tree) WriteReader(p string, r io.Reader, mode fs.FileMode) error {
	content, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return t.Write(p, content, mode)
}

// Read returns the full content of the file at p.
func (t *Tree) Read(p string) ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	n, err := t.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	if !n.inode.IsFile() {
		return nil, ErrIsDir
	}
	if n.inode.Body == nil {
		return []byte{}, nil
	}
	out := make([]byte, n.inode.Body.Size())
	if len(out) == 0 {
		return out, nil
	}
	if _, err := n.inode.Body.ReadAt(out, 0, t.reader, t.store); err != nil && err != io.EOF {
		return nil, err
	}
	return out, nil
}

// Symlink creates a symbolic link at linkPath whose target is target. The
// target is stored as-is (relative or absolute); resolution happens at
// read time on the consumer side.
func (t *Tree) Symlink(target, linkPath string) error {
	clean, err := cleanPath(linkPath)
	if err != nil {
		return err
	}
	parent, name, err := t.splitPath(linkPath)
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	parentNode, err := t.materializeDirLocked(parent)
	if err != nil {
		return err
	}
	if _, ok := parentNode.children[name]; ok {
		return ErrExist
	}
	t.clearWhiteoutLocked(clean)
	now := time.Now().UTC()
	parentNode.children[name] = &node{
		inode: &Inode{
			ID:         t.alloc.Next(),
			Kind:       EntrySymlink,
			Mode:       fs.ModeSymlink | 0o777,
			ModTime:    now,
			Nlink:      1,
			LinkTarget: target,
		},
	}
	parentNode.inode.ModTime = now
	t.publish(ChangeEvent{Path: clean, Op: OpSymlink, ModTime: now, LinkTarget: target})
	return nil
}

// materializeDirLocked returns the in-memory node for the directory at p,
// creating in-memory shadow nodes for any host-only ancestors along the
// way so that adding a child has somewhere to attach.
//
// Why this exists: lookupLocked happily returns synthesized host nodes for
// host-only directories, but those nodes share state with the hostNodes
// cache and have nil children maps. If we attached a new child to one, we'd
// either mutate the cached synthesized node (surprising) or lose the write.
// So Mkdir/Write/Symlink go through this instead.
func (t *Tree) materializeDirLocked(p string) (*node, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	if clean == "/" {
		return t.root, nil
	}
	parts := splitNonEmpty(clean)
	cursor := t.root
	for i, part := range parts {
		if !cursor.inode.IsDir() {
			return nil, ErrNotDir
		}
		if next, ok := cursor.children[part]; ok {
			cursor = next
			continue
		}
		prefix := "/" + strings.Join(parts[:i+1], "/")
		if t.isWhiteoutLocked(prefix) {
			return nil, ErrNotExist
		}
		// Try host overlay for this prefix.
		if t.hostRoot != "" {
			if hostDir, ok := t.hostDirAtLocked(prefix); ok {
				cursor.children[part] = hostDir
				cursor = hostDir
				continue
			}
			if t.hostHasNonDirLocked(prefix) {
				return nil, ErrNotDir
			}
		}
		return nil, ErrNotExist
	}
	if !cursor.inode.IsDir() {
		return nil, ErrNotDir
	}
	return cursor, nil
}

// hostDirAtLocked returns a freshly-prepared in-memory node corresponding to
// a host directory at VFS path p, with an empty children map ready to accept
// writes. Returns (nil, false) if the host entry doesn't exist or isn't a
// directory. The synthesized hostNodes cache is bypassed because that cache
// is for read-only views; materialized dirs need their own children map.
func (t *Tree) hostDirAtLocked(p string) (*node, bool) {
	hp := t.hostPath(p)
	if hp == "" {
		return nil, false
	}
	info, err := os.Stat(hp)
	if err != nil || !info.IsDir() {
		return nil, false
	}
	return &node{
		inode: &Inode{
			ID:      t.alloc.Next(),
			Kind:    EntryDir,
			Mode:    fs.ModeDir | info.Mode().Perm(),
			ModTime: info.ModTime(),
			Nlink:   2,
		},
		children: make(map[string]*node),
	}, true
}

// hostHasNonDirLocked reports whether the overlay has a non-directory entry
// at p — used to surface ErrNotDir when a Mkdir/Write target is shadowed by
// a host file or symlink.
func (t *Tree) hostHasNonDirLocked(p string) bool {
	hp := t.hostPath(p)
	if hp == "" {
		return false
	}
	info, err := os.Lstat(hp)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// Remove deletes the entry at p from the union view.
//
//   - In-memory file/symlink: dropped, blob refs released. If overlay has
//     a same-named entry, a whiteout is recorded so the host doesn't
//     resurface.
//   - In-memory empty dir: dropped (with whiteout if overlay has it).
//     Non-empty in-memory dir returns ErrNotEmpty.
//   - Host-only file/symlink: whiteout recorded.
//   - Host-only dir: whiteout recorded; the entire subtree disappears
//     from the union (overlayfs semantics).
//   - Neither: ErrNotExist.
func (t *Tree) Remove(p string) error {
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	if clean == "/" {
		return ErrInvalidPath
	}
	parent, name, err := t.splitPath(p)
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	parentNode, err := t.lookupLocked(parent)
	if err != nil {
		return err
	}

	if target, ok := parentNode.children[name]; ok {
		if target.inode.IsDir() && len(target.children) > 0 {
			return ErrNotEmpty
		}
		t.releaseInodeLocked(target.inode)
		delete(parentNode.children, name)
		now := time.Now().UTC()
		parentNode.inode.ModTime = now
		if t.hostRoot != "" {
			t.addWhiteoutLocked(clean)
		}
		t.publish(ChangeEvent{Path: clean, Op: OpRemove, ModTime: now})
		return nil
	}

	// In-memory miss. Try overlay before declaring ErrNotExist.
	if t.hostRoot != "" {
		if t.isWhiteoutLocked(clean) {
			return ErrNotExist
		}
		hp := t.hostPath(clean)
		if hp != "" {
			if _, err := os.Lstat(hp); err == nil {
				t.addWhiteoutLocked(clean)
				now := time.Now().UTC()
				parentNode.inode.ModTime = now
				t.publish(ChangeEvent{Path: clean, Op: OpRemove, ModTime: now})
				return nil
			}
		}
	}
	return ErrNotExist
}

// RemoveAll recursively deletes p and everything beneath it from the union
// view. In-memory entries are dropped (releasing chunk refs). When an
// overlay is configured, a whiteout is added so any host entries beneath p
// disappear too. Returns nil if p doesn't exist (matches os.RemoveAll).
func (t *Tree) RemoveAll(p string) error {
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	if clean == "/" {
		return ErrInvalidPath
	}
	parent, name, err := t.splitPath(p)
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	parentNode, err := t.lookupLocked(parent)
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			return nil
		}
		return err
	}
	now := time.Now().UTC()
	mutated := false
	if target, ok := parentNode.children[name]; ok {
		t.releaseSubtreeLocked(target)
		delete(parentNode.children, name)
		parentNode.inode.ModTime = now
		mutated = true
	}
	if t.hostRoot != "" {
		t.addWhiteoutLocked(clean)
		parentNode.inode.ModTime = now
		mutated = true
	}
	if mutated {
		t.publish(ChangeEvent{Path: clean, Op: OpRemove, ModTime: now})
	}
	return nil
}

// Rename moves the entry at oldPath to newPath. Parent directories of
// newPath must exist; the destination must not exist.
func (t *Tree) Rename(oldPath, newPath string) error {
	oldParent, oldName, err := t.splitPath(oldPath)
	if err != nil {
		return err
	}
	newParent, newName, err := t.splitPath(newPath)
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	srcParent, err := t.lookupLocked(oldParent)
	if err != nil {
		return err
	}
	src, ok := srcParent.children[oldName]
	if !ok {
		return ErrNotExist
	}
	dstParent, err := t.lookupLocked(newParent)
	if err != nil {
		return err
	}
	if !dstParent.inode.IsDir() {
		return ErrNotDir
	}
	if _, exists := dstParent.children[newName]; exists {
		return ErrExist
	}
	delete(srcParent.children, oldName)
	dstParent.children[newName] = src
	now := time.Now().UTC()
	srcParent.inode.ModTime = now
	dstParent.inode.ModTime = now
	oldClean, _ := cleanPath(oldPath)
	newClean, _ := cleanPath(newPath)
	t.publish(ChangeEvent{Path: newClean, OldPath: oldClean, Op: OpRename, ModTime: now})
	return nil
}

// Stat returns a copy of the inode at p. The returned pointer is detached
// from tree state; callers may inspect freely without locking.
func (t *Tree) Stat(p string) (*Inode, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, err := t.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	return n.inode.Clone(), nil
}

// Chmod updates the permission bits of the inode at p, preserving the type
// bits.
func (t *Tree) Chmod(p string, mode fs.FileMode) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, err := t.lookupLocked(p)
	if err != nil {
		return err
	}
	n.inode.Mode = (n.inode.Mode & ^fs.ModePerm) | (mode & fs.ModePerm)
	now := time.Now().UTC()
	n.inode.ModTime = now
	clean, _ := cleanPath(p)
	t.publish(ChangeEvent{Path: clean, Op: OpChmod, ModTime: now})
	return nil
}

// Chtime updates the modification timestamp of the inode at p.
func (t *Tree) Chtime(p string, mtime time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, err := t.lookupLocked(p)
	if err != nil {
		return err
	}
	n.inode.ModTime = mtime
	clean, _ := cleanPath(p)
	t.publish(ChangeEvent{Path: clean, Op: OpChtime, ModTime: mtime})
	return nil
}

// ReadDir returns the union of children of the directory at p, sorted by
// name. In-memory entries shadow host entries; whiteouts hide host entries.
func (t *Tree) ReadDir(p string) ([]DirEntry, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, err := t.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	if !n.inode.IsDir() {
		return nil, ErrNotDir
	}
	clean, _ := cleanPath(p)
	names := t.unionDirChildrenLocked(clean, n)
	out := make([]DirEntry, 0, len(names))
	for _, name := range names {
		childPath := joinPath(clean, name)
		child, err := t.lookupLocked(childPath)
		if err != nil {
			continue
		}
		out = append(out, DirEntry{Name: name, Inode: child.inode.Clone()})
	}
	return out, nil
}

// DirEntry is a (basename, inode) pair returned by ReadDir.
type DirEntry struct {
	Name  string
	Inode *Inode
}

// WalkFunc is the visitor callback for Tree.Walk. Returning fs.SkipDir on a
// directory skips its subtree; any other non-nil error aborts the walk.
type WalkFunc func(p string, inode *Inode) error

// Walk visits every entry in the tree in lexical order, including the root.
// The walk holds the tree RLock for its duration; callbacks must not mutate
// the tree (use ReadDir + your own loop if you need a mutating walk).
func (t *Tree) Walk(fn WalkFunc) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.walkLocked("/", t.root, fn)
}

func (t *Tree) walkLocked(p string, n *node, fn WalkFunc) error {
	if err := fn(p, n.inode.Clone()); err != nil {
		if errors.Is(err, fs.SkipDir) {
			return nil
		}
		return err
	}
	if !n.inode.IsDir() {
		return nil
	}
	names := t.unionDirChildrenLocked(p, n)
	for _, name := range names {
		childPath := joinPath(p, name)
		// Resolve via lookup so the union (memory + overlay) is honored. A
		// transient lookup error (e.g. host file removed mid-walk) is
		// treated as a skip — the snapshot is best-effort across host
		// changes during the walk.
		child, err := t.lookupLocked(childPath)
		if err != nil {
			continue
		}
		if err := t.walkLocked(childPath, child, fn); err != nil {
			return err
		}
	}
	return nil
}

// Open returns an fs.File view of the entry at p, satisfying the fs.FS
// contract on the read path. The returned file is detached from tree state
// — concurrent writes to p won't surface mid-read.
func (t *Tree) Open(p string) (fs.File, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, err := t.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	clone := n.inode.Clone()
	return &openFile{
		tree:  t,
		path:  p,
		inode: clone,
	}, nil
}

// openFile satisfies fs.File for read access. Mutations to the underlying
// tree don't affect an already-opened file because the Inode pointer was
// cloned at Open time and Body extent reads resolve through ChunkStore (which
// retains chunks until released — and we don't release out from under an
// open file because the Tree clone holds extent metadata that includes the
// hash, but not a refcount). Callers that need long-held opens across
// concurrent deletes should Acquire blob hashes themselves.
type openFile struct {
	tree   *Tree
	path   string
	inode  *Inode
	offset int64
	closed bool
}

func (f *openFile) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	return &fileInfo{name: path.Base(f.path), inode: f.inode}, nil
}

func (f *openFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if f.inode.IsDir() {
		return 0, ErrIsDir
	}
	if f.inode.Body == nil {
		return 0, io.EOF
	}
	n, err := f.inode.Body.ReadAt(p, f.offset, f.tree.reader, f.tree.store)
	f.offset += int64(n)
	return n, err
}

func (f *openFile) Close() error {
	f.closed = true
	return nil
}

// fileInfo satisfies fs.FileInfo from a cloned Inode.
type fileInfo struct {
	name  string
	inode *Inode
}

func (f *fileInfo) Name() string       { return f.name }
func (f *fileInfo) Size() int64        { return f.inode.Size() }
func (f *fileInfo) Mode() fs.FileMode  { return f.inode.Mode }
func (f *fileInfo) ModTime() time.Time { return f.inode.ModTime }
func (f *fileInfo) IsDir() bool        { return f.inode.IsDir() }
func (f *fileInfo) Sys() any           { return f.inode }

// makeFileBodyLocked turns content into a FileBody using Tree.chunkSize.
// Caller must hold t.mu (write lock).
func (t *Tree) makeFileBodyLocked(content []byte) (*FileBody, error) {
	if len(content) == 0 {
		return NewEmptyFileBody(), nil
	}
	if t.chunkSize <= 0 {
		hash, _, err := t.store.Put(content)
		if err != nil {
			return nil, err
		}
		return NewBlobFileBody(hash, int64(len(content))), nil
	}
	return NewChunkedFileBody(content, t.store, t.chunkSize)
}

// releaseInodeLocked drops chunk-store references for a single inode's body.
// Caller must hold t.mu (write lock).
func (t *Tree) releaseInodeLocked(i *Inode) {
	if i == nil || i.Body == nil {
		return
	}
	for _, ex := range i.Body.Extents() {
		if ex.Kind == ExtentBlob {
			_ = t.store.Release(ex.BlobHash)
		}
	}
}

// releaseSubtreeLocked recursively releases chunk-store references for every
// inode under n. Caller must hold t.mu (write lock).
func (t *Tree) releaseSubtreeLocked(n *node) {
	if n == nil {
		return
	}
	t.releaseInodeLocked(n.inode)
	for _, child := range n.children {
		t.releaseSubtreeLocked(child)
	}
}

// lookupLocked resolves p to a node. Caller must hold t.mu (read or write).
//
// Resolution prefers the in-memory tree at every step; on a memory miss with
// an overlay configured, the lookup falls through to the host filesystem
// and synthesizes (or returns a cached) inode. Whiteouts mask host entries —
// either an exact-path whiteout or one on an ancestor.
func (t *Tree) lookupLocked(p string) (*node, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	if clean == "/" {
		return t.root, nil
	}
	parts := splitNonEmpty(clean)
	cursor := t.root
	for i, part := range parts {
		if !cursor.inode.IsDir() {
			return nil, ErrNotDir
		}
		// In-memory hit short-circuits.
		if next, ok := cursor.children[part]; ok {
			cursor = next
			continue
		}
		// In-memory miss. Try host overlay.
		prefix := "/" + strings.Join(parts[:i+1], "/")
		if t.isWhiteoutLocked(prefix) {
			return nil, ErrNotExist
		}
		if t.hostRoot == "" {
			return nil, ErrNotExist
		}
		synth, err := t.synthesizeHostNodeLocked(prefix)
		if err != nil {
			return nil, err
		}
		cursor = synth
	}
	return cursor, nil
}

// splitPath returns (parentDir, basename) for p. Both are guaranteed
// non-empty for any non-root path; root paths return ErrInvalidPath because
// you cannot Mkdir/Write/Remove the root.
func (t *Tree) splitPath(p string) (string, string, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return "", "", err
	}
	if clean == "/" {
		return "", "", ErrInvalidPath
	}
	dir, name := path.Split(clean)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "/"
	}
	if name == "" {
		return "", "", ErrInvalidPath
	}
	return dir, name, nil
}

// cleanPath normalizes p to a slash-rooted absolute form. Empty paths,
// paths containing "..", and Windows-style separators are rejected.
func cleanPath(p string) (string, error) {
	if p == "" {
		return "", ErrInvalidPath
	}
	if strings.ContainsRune(p, '\\') {
		return "", ErrInvalidPath
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	cleaned := path.Clean(p)
	if strings.HasPrefix(cleaned, "/..") || cleaned == ".." {
		return "", ErrInvalidPath
	}
	return cleaned, nil
}

// splitNonEmpty splits a "/a/b/c" path into ["a","b","c"], dropping empties.
func splitNonEmpty(p string) []string {
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// joinPath joins dir + name with a single slash, handling root correctly.
func joinPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}
