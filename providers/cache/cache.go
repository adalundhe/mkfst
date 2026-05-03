// Package cache is mkfst's pluggable key-value cache.
//
// One Cache interface, three backends (memory, Redis/Valkey, SQL).
// Used as the durable byte-store under several mkfst features:
//
//   - providers/workflows/ stores per-node task outputs in a Cache so
//     downstream nodes can read parent outputs across processes.
//   - providers/vfs/ uses an in-memory Cache as the LRU store for its
//     read-cached host overlay.
//   - User code that wants a cache primitive without adopting an
//     external library.
//
// The Cache interface is intentionally narrow: Get/Set/Delete plus
// DeletePrefix for bulk cleanup (workflows clean up an entire
// instance's outputs at end-of-run by prefix). TTL is a per-Set
// option; backends honor it via their native expiry mechanism (LRU+
// time check for memory, EXPIRE for Redis, expires_at column +
// background cleanup for SQL).
//
// Correctness model:
//   - Get returns (value, true, nil) on hit; (nil, false, nil) on miss
//     or after TTL expiry; (nil, false, err) on backend failure.
//     Callers can branch cleanly on (found, err) without conflating
//     "not present" with "lookup failed."
//   - Set with ttl=0 stores indefinitely (no expiry).
//   - Set with ttl>0 expires the entry at now+ttl.
//   - DeletePrefix is best-effort for memory/Redis (might lag if
//     concurrent Sets land mid-scan), authoritative for SQL.
//   - Concurrent Get/Set on the same key: last-writer-wins on Set;
//     Get returns whichever value was most recently committed.
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrClosed is returned by every method after Close has been called.
var ErrClosed = errors.New("cache: closed")

// Cache is the backend abstraction. Each backend
// (memory/Redis/SQL) satisfies this interface and passes the same
// conformance tests.
type Cache interface {
	// Get returns the stored bytes for key. Second return is true if
	// the key was present and not expired. Third is the only error
	// path — backend failure (network, serialization, etc.); a miss
	// is not an error.
	Get(ctx context.Context, key string) ([]byte, bool, error)

	// Set stores value under key. ttl=0 means store indefinitely;
	// ttl>0 expires the entry at now+ttl. Overwrites any existing
	// value under the same key.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Delete removes key. Idempotent — deleting a missing key is
	// not an error.
	Delete(ctx context.Context, key string) error

	// DeletePrefix removes every key starting with prefix. Returns
	// the number of keys deleted. Used by workflows to clean up an
	// entire instance's outputs at terminal state. Implementations
	// must walk in O(matched) time, not O(total) — Redis SCAN, SQL
	// LIKE on indexed prefix, memory map traversal of the namespace.
	DeletePrefix(ctx context.Context, prefix string) (int, error)

	// Close releases backend resources. Idempotent. After Close,
	// every method returns ErrClosed.
	Close() error
}
