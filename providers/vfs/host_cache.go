package vfs

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"time"

	"mkfst/providers/cache"
)

// hostReadCache is the per-Tree, content-addressable read cache for files
// that fall through to the host overlay. Build contexts and toolchain reads
// repeatedly touch the same host files (Dockerfile, package manifests,
// licenses); without a cache, every read incurs a syscall. With it, repeated
// reads of an unchanged host file are served from memory.
//
// Implementation: a thin stat-validation layer over providers/cache.Cache.
// The byte storage and LRU eviction live in the cache package so we don't
// maintain two implementations of the same primitive. This wrapper adds
// only the (size, mtime)-based invalidation that distinguishes a "host
// file content cache" from a generic key-value cache.
//
// On-wire encoding for stored values:
//
//	[8 bytes: size as int64 LE][8 bytes: mtime UnixNano as int64 LE][N bytes: content]
//
// Validation: lookup re-stats the host file each call (the read path
// already needs that stat). If the stored header doesn't match the
// freshly-stat'd (size, mtime), we evict and miss; the caller reads
// from disk and re-stores.
type hostReadCache struct {
	backing cache.Cache

	// maxBytes is retained for HostCacheStats reporting. Eviction
	// itself is delegated to the backing cache's MaxBytes.
	maxBytes int64

	// entries is a best-effort count for HostCacheStats. Not
	// authoritative under TTL eviction (which we don't use here),
	// but accurate under store/lookup-evict semantics.
	entries  atomic.Int64
	curBytes atomic.Int64
}

func newHostReadCache(maxBytes int64) *hostReadCache {
	if maxBytes <= 0 {
		return nil
	}
	return &hostReadCache{
		backing:  cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: maxBytes}),
		maxBytes: maxBytes,
	}
}

// lookup returns cached content if (path, size, modTime) matches an entry.
// A stale entry (same path, different size/mtime) is evicted; the caller
// reloads from host.
func (c *hostReadCache) lookup(path string, size int64, modTime time.Time) ([]byte, bool) {
	if c == nil || c.backing == nil {
		return nil, false
	}
	raw, ok, err := c.backing.Get(context.Background(), path)
	if err != nil || !ok {
		return nil, false
	}
	storedSize, storedModTime, content, ok := decodeHostEntry(raw)
	if !ok || storedSize != size || !storedModTime.Equal(modTime) {
		_ = c.backing.Delete(context.Background(), path)
		c.entries.Add(-1)
		c.curBytes.Add(-int64(len(raw)))
		return nil, false
	}
	return content, true
}

// store records content for (path, size, modTime). The backing cache
// handles LRU eviction via its MaxBytes budget.
func (c *hostReadCache) store(path string, size int64, modTime time.Time, content []byte) {
	if c == nil || c.backing == nil {
		return
	}
	encoded := encodeHostEntry(size, modTime, content)
	if err := c.backing.Set(context.Background(), path, encoded, 0); err != nil {
		return
	}
	c.entries.Add(1)
	c.curBytes.Add(int64(len(encoded)))
}

// stats returns (entries, bytes-held, max-bytes). Counters are
// best-effort — they're maintained alongside store/lookup-evict but
// don't observe LRU evictions inside the backing cache. For most
// workloads they're close enough; for precise accounting, query the
// backing cache directly.
func (c *hostReadCache) stats() (int, int64, int64) {
	if c == nil {
		return 0, 0, 0
	}
	return int(c.entries.Load()), c.curBytes.Load(), c.maxBytes
}

// reset clears every entry. Idempotent. Used by tests; not a
// runtime operation in production.
func (c *hostReadCache) reset() {
	if c == nil || c.backing == nil {
		return
	}
	// DeletePrefix("") matches every key under the cache's keyspace.
	// Since each host cache uses a fresh per-Tree memory backing, no
	// other state is affected.
	_, _ = c.backing.DeletePrefix(context.Background(), "")
	c.entries.Store(0)
	c.curBytes.Store(0)
}

// === entry encoding ===

// hostEntryHeaderSize is the fixed-width prefix written before content:
// 8 bytes size + 8 bytes mtime nanos.
const hostEntryHeaderSize = 16

func encodeHostEntry(size int64, modTime time.Time, content []byte) []byte {
	out := make([]byte, hostEntryHeaderSize+len(content))
	binary.LittleEndian.PutUint64(out[0:8], uint64(size))
	binary.LittleEndian.PutUint64(out[8:16], uint64(modTime.UnixNano()))
	copy(out[hostEntryHeaderSize:], content)
	return out
}

func decodeHostEntry(raw []byte) (size int64, modTime time.Time, content []byte, ok bool) {
	if len(raw) < hostEntryHeaderSize {
		return 0, time.Time{}, nil, false
	}
	size = int64(binary.LittleEndian.Uint64(raw[0:8]))
	mtimeNs := int64(binary.LittleEndian.Uint64(raw[8:16]))
	modTime = time.Unix(0, mtimeNs)
	content = raw[hostEntryHeaderSize:]
	return size, modTime, content, true
}
