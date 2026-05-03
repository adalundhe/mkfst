package vfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultHostCacheBudget is the default per-tree LRU ceiling for host-read
// caching. 128 MiB holds a typical Python runtime + node_modules hotset with
// headroom; tune via TreeOpts.HostCacheBudget when the workload is known.
const DefaultHostCacheBudget = 128 * 1024 * 1024

// hostPath returns the absolute host filesystem path corresponding to the
// VFS path p, or "" if no overlay is configured.
func (t *Tree) hostPath(p string) string {
	if t.hostRoot == "" {
		return ""
	}
	clean, err := cleanPath(p)
	if err != nil {
		return ""
	}
	if clean == "/" {
		return t.hostRoot
	}
	return filepath.Join(t.hostRoot, filepath.FromSlash(clean))
}

// isWhiteoutLocked reports whether p is masked by a whiteout — either an
// exact-path whiteout or one on an ancestor directory. A whiteout on a
// directory hides the entire subtree from the host overlay.
func (t *Tree) isWhiteoutLocked(p string) bool {
	if len(t.whiteouts) == 0 {
		return false
	}
	clean, err := cleanPath(p)
	if err != nil {
		return false
	}
	if _, ok := t.whiteouts[clean]; ok {
		return true
	}
	for ancestor := path.Dir(clean); ancestor != "/" && ancestor != "."; ancestor = path.Dir(ancestor) {
		if _, ok := t.whiteouts[ancestor]; ok {
			return true
		}
	}
	return false
}

// addWhiteoutLocked marks p (and implicitly anything beneath it) as hidden
// from the host overlay. Descendant whiteouts are pruned because they're
// subsumed.
func (t *Tree) addWhiteoutLocked(p string) {
	if t.whiteouts == nil {
		t.whiteouts = make(map[string]struct{})
	}
	clean, err := cleanPath(p)
	if err != nil {
		return
	}
	for k := range t.whiteouts {
		if strings.HasPrefix(k, clean+"/") {
			delete(t.whiteouts, k)
		}
	}
	t.whiteouts[clean] = struct{}{}
	// Drop any cached synthesized inode beneath this point.
	for k := range t.hostNodes {
		if k == clean || strings.HasPrefix(k, clean+"/") {
			delete(t.hostNodes, k)
		}
	}
}

// clearWhiteoutLocked removes the exact-path whiteout for p, if present.
// Called from Write/Mkdir/Symlink so a user explicitly creating something
// at a previously-whiteouted path makes it visible again. Does not touch
// whiteouts on ancestor directories — those still mask.
func (t *Tree) clearWhiteoutLocked(p string) {
	clean, err := cleanPath(p)
	if err != nil {
		return
	}
	delete(t.whiteouts, clean)
}

// synthesizeHostNodeLocked returns a node backed by the host filesystem at
// VFS path p, creating it on first lookup and caching it for stability of
// the inode ID. If the host entry's size or mtime has changed since the
// cached version, the cache is refreshed in place (preserving the inode ID
// so FUSE clients don't see entry churn).
func (t *Tree) synthesizeHostNodeLocked(p string) (*node, error) {
	if t.hostRoot == "" {
		return nil, ErrNotExist
	}
	hostPath := t.hostPath(p)
	if hostPath == "" {
		return nil, ErrInvalidPath
	}

	info, err := os.Lstat(hostPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotExist
		}
		return nil, err
	}

	if cached, ok := t.hostNodes[p]; ok {
		if hostInodeStillFresh(cached.inode, info) {
			return cached, nil
		}
		// Stale: refresh fields in place; ID stays stable.
		updateHostInode(cached.inode, info, hostPath)
		return cached, nil
	}

	inode := &Inode{
		ID: t.alloc.Next(),
	}
	updateHostInode(inode, info, hostPath)

	n := &node{inode: inode}
	if inode.IsDir() {
		// Children are populated lazily — unionDirChildrenLocked enumerates
		// the host listdir at iteration time.
		n.children = nil
	}
	t.hostNodes[p] = n
	return n, nil
}

// updateHostInode populates an Inode from a host stat. For symlinks we
// readlink up front because FUSE clients want the target without an extra
// round trip. For files we attach a RealFile-backed body so reads passthrough
// without buffering.
func updateHostInode(inode *Inode, info os.FileInfo, hostPath string) {
	inode.Mode = info.Mode()
	inode.ModTime = info.ModTime()
	switch {
	case info.IsDir():
		inode.Kind = EntryDir
		inode.Nlink = 2
		inode.Body = nil
		inode.LinkTarget = ""
	case info.Mode()&os.ModeSymlink != 0:
		inode.Kind = EntrySymlink
		inode.Nlink = 1
		inode.Body = nil
		if target, err := os.Readlink(hostPath); err == nil {
			inode.LinkTarget = target
		}
	default:
		inode.Kind = EntryFile
		inode.Nlink = 1
		inode.LinkTarget = ""
		inode.Body = NewRealFileBody(hostPath, info.Size())
	}
}

// hostInodeStillFresh reports whether a cached host inode matches the latest
// stat. Mismatches trigger an in-place refresh (keeping the ID stable).
func hostInodeStillFresh(inode *Inode, info os.FileInfo) bool {
	if inode.Mode != info.Mode() {
		return false
	}
	if !inode.ModTime.Equal(info.ModTime()) {
		return false
	}
	if inode.Kind == EntryFile && inode.Body != nil && inode.Body.Size() != info.Size() {
		return false
	}
	return true
}

// unionDirChildrenLocked returns the merged child names of the directory
// node n at VFS path p: in-memory children take precedence; host children
// fill in the rest, modulo whiteouts. Returned names are lex-sorted so
// enumeration is deterministic across runs.
func (t *Tree) unionDirChildrenLocked(p string, n *node) []string {
	seen := make(map[string]struct{}, len(n.children))
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if t.hostRoot == "" {
		sort.Strings(names)
		return names
	}
	hp := t.hostPath(p)
	if hp == "" {
		sort.Strings(names)
		return names
	}
	entries, err := os.ReadDir(hp)
	if err != nil {
		// Host dir might not exist (e.g. p is purely-in-memory above the
		// overlay root). That's fine — return what we have.
		sort.Strings(names)
		return names
	}
	for _, e := range entries {
		name := e.Name()
		if _, dup := seen[name]; dup {
			continue
		}
		childVFS := joinPath(p, name)
		if t.isWhiteoutLocked(childVFS) {
			continue
		}
		names = append(names, name)
		seen[name] = struct{}{}
	}
	sort.Strings(names)
	return names
}

// hostCachedReader wraps an underlying RealFileReader with the per-tree LRU
// host-read cache. It reads the *whole* file on miss and stores it; further
// reads at any offset hit the cache. This trades a larger first-read cost
// for amortized reads across the file — the right shape for build-context
// tarring where every file is read end-to-end.
type hostCachedReader struct {
	cache *hostReadCache
	base  RealFileReader
}

func (h *hostCachedReader) ReadAt(p string, dst []byte, off int64) (int, error) {
	if h.cache == nil {
		return h.base.ReadAt(p, dst, off)
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	if data, ok := h.cache.lookup(p, info.Size(), info.ModTime()); ok {
		return copyAt(data, dst, off)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	h.cache.store(p, info.Size(), info.ModTime(), data)
	return copyAt(data, dst, off)
}

// copyAt copies from data starting at off into dst. Returns io.EOF when the
// copy would read past the end of data; mirrors io.ReaderAt semantics.
func copyAt(data, dst []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrNegativeOffset
	}
	if off >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(dst, data[off:])
	if int64(n)+off >= int64(len(data)) {
		return n, io.EOF
	}
	return n, nil
}

// HostCacheStats returns a snapshot of the host-read cache's footprint and
// budget. (entries, currentBytes, maxBytes). Returns zeros if no overlay or
// no cache configured.
func (t *Tree) HostCacheStats() (int, int64, int64) {
	return t.hostCache.stats()
}

// ResetHostCache drops every entry in the host-read cache. Useful for tests
// or when the caller knows the overlay has changed underneath.
func (t *Tree) ResetHostCache() {
	t.hostCache.reset()
}

// hostNodeAlloc maps a VFS path to a stable inode id by way of the cached
// hostNodes map. Used by the FUSE backend to keep entries stable across
// lookups. Returns 0 if the path isn't resolvable as a host node.
func (t *Tree) hostNodeID(p string) uint64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if n, ok := t.hostNodes[p]; ok {
		return n.inode.ID
	}
	return 0
}

// hostMtime returns the cached or freshly-stat'd mtime of a host overlay
// path. Used by overlay tests; safe to call at any time.
func (t *Tree) hostMtime(p string) (time.Time, error) {
	hp := t.hostPath(p)
	if hp == "" {
		return time.Time{}, ErrNotExist
	}
	info, err := os.Stat(hp)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}
