package cache

import (
	"container/list"
	"context"
	"strings"
	"sync"
	"time"
)

// MemoryOpts configures NewMemoryCache.
type MemoryOpts struct {
	// MaxBytes caps the total live byte footprint. 0 disables the
	// cap (entries only evicted by TTL or explicit Delete).
	// LRU eviction kicks in when an insert would push past the cap;
	// the least-recently-touched entries get dropped until there's
	// room.
	MaxBytes int64

	// Now overrides time.Now for tests. Production code leaves nil.
	Now func() time.Time
}

// NewMemoryCache returns an in-process Cache. Entries live in a
// hashmap with an LRU list for eviction-when-full and a per-entry
// expires_at for TTL.
//
// Single global mutex — keeps the implementation auditable and the
// concurrency story obvious. Per-shard locking can be added later if
// profile data shows contention; in practice cache ops are short
// enough that one mutex carries millions of ops/sec on typical
// hardware.
func NewMemoryCache(opts MemoryOpts) Cache {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &memoryCache{
		entries:  make(map[string]*memoryEntry),
		order:    list.New(),
		maxBytes: opts.MaxBytes,
		now:      now,
	}
}

type memoryCache struct {
	mu       sync.Mutex
	entries  map[string]*memoryEntry
	order    *list.List // MRU at front, eviction from back
	curBytes int64
	maxBytes int64
	closed   bool
	now      func() time.Time
}

// memoryEntry is the per-key record. We keep value as []byte (no
// copy on Get — callers receive the same slice; if they mutate it,
// future Gets see the mutation. Documented in the Cache interface
// docstring's "concurrent Get/Set" note.)
type memoryEntry struct {
	key       string
	value     []byte
	expiresAt time.Time // zero = no expiry
	node      *list.Element
}

func (c *memoryCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false, ErrClosed
	}
	entry, ok := c.entries[key]
	if !ok {
		return nil, false, nil
	}
	if !entry.expiresAt.IsZero() && entry.expiresAt.Before(c.now()) {
		c.evictLocked(entry)
		return nil, false, nil
	}
	c.order.MoveToFront(entry.node)
	return entry.value, true, nil
}

func (c *memoryCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}

	// Replace existing entry under the same key.
	if existing, ok := c.entries[key]; ok {
		c.evictLocked(existing)
	}

	entry := &memoryEntry{
		key:   key,
		value: value,
	}
	if ttl > 0 {
		entry.expiresAt = c.now().Add(ttl)
	}
	entry.node = c.order.PushFront(entry)
	c.entries[key] = entry
	c.curBytes += int64(len(value))

	c.evictToBudgetLocked()
	return nil
}

func (c *memoryCache) Delete(ctx context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if entry, ok := c.entries[key]; ok {
		c.evictLocked(entry)
	}
	return nil
}

func (c *memoryCache) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, ErrClosed
	}
	var deleted int
	for k, entry := range c.entries {
		if strings.HasPrefix(k, prefix) {
			c.evictLocked(entry)
			deleted++
		}
	}
	return deleted, nil
}

func (c *memoryCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.entries = nil
	c.order = nil
	c.curBytes = 0
	return nil
}

// evictLocked removes a specific entry. Caller holds c.mu.
func (c *memoryCache) evictLocked(entry *memoryEntry) {
	if entry.node != nil {
		c.order.Remove(entry.node)
	}
	delete(c.entries, entry.key)
	c.curBytes -= int64(len(entry.value))
}

// evictToBudgetLocked drops least-recently-used entries until
// curBytes ≤ maxBytes. No-op when maxBytes is 0 (uncapped).
// Caller holds c.mu.
func (c *memoryCache) evictToBudgetLocked() {
	if c.maxBytes <= 0 {
		return
	}
	for c.curBytes > c.maxBytes {
		back := c.order.Back()
		if back == nil {
			return
		}
		entry := back.Value.(*memoryEntry)
		c.evictLocked(entry)
	}
}
