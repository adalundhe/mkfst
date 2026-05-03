package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runCacheConformance is the shared test harness every Cache impl
// must pass. The memory backend calls it directly; SQL/Redis backends
// will reuse it from their own _test.go files (or via a separate test
// binary) once those backends are implemented.
//
// factory returns a fresh, isolated Cache per subtest.
func runCacheConformance(t *testing.T, name string, factory func(t *testing.T) Cache) {
	t.Run(name+"/SetGetRoundtrip", func(t *testing.T) {
		c := factory(t)
		defer c.Close()
		ctx := context.Background()

		if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
			t.Fatalf("set: %v", err)
		}
		got, ok, err := c.Get(ctx, "k")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !ok || string(got) != "v" {
			t.Fatalf("got=%q ok=%v", got, ok)
		}
	})

	t.Run(name+"/MissReturnsAbsent", func(t *testing.T) {
		c := factory(t)
		defer c.Close()
		_, ok, err := c.Get(context.Background(), "nope")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ok {
			t.Fatalf("expected miss")
		}
	})

	t.Run(name+"/SetOverwrites", func(t *testing.T) {
		c := factory(t)
		defer c.Close()
		ctx := context.Background()
		_ = c.Set(ctx, "k", []byte("v1"), 0)
		_ = c.Set(ctx, "k", []byte("v2"), 0)
		got, _, _ := c.Get(ctx, "k")
		if string(got) != "v2" {
			t.Fatalf("got %q want v2", got)
		}
	})

	t.Run(name+"/DeleteRemoves", func(t *testing.T) {
		c := factory(t)
		defer c.Close()
		ctx := context.Background()
		_ = c.Set(ctx, "k", []byte("v"), 0)
		if err := c.Delete(ctx, "k"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		_, ok, _ := c.Get(ctx, "k")
		if ok {
			t.Fatalf("post-delete still present")
		}
	})

	t.Run(name+"/DeleteIdempotent", func(t *testing.T) {
		c := factory(t)
		defer c.Close()
		// Delete a key that was never set — must not error.
		if err := c.Delete(context.Background(), "ghost"); err != nil {
			t.Fatalf("delete missing: %v", err)
		}
	})

	t.Run(name+"/TTLExpires", func(t *testing.T) {
		c := factory(t)
		defer c.Close()
		ctx := context.Background()
		_ = c.Set(ctx, "k", []byte("v"), 50*time.Millisecond)
		// Within TTL: hit.
		got, ok, _ := c.Get(ctx, "k")
		if !ok || string(got) != "v" {
			t.Fatalf("pre-expiry: got=%q ok=%v", got, ok)
		}
		time.Sleep(80 * time.Millisecond)
		// Past TTL: miss.
		_, ok, _ = c.Get(ctx, "k")
		if ok {
			t.Fatalf("post-expiry still present")
		}
	})

	t.Run(name+"/ZeroTTLNeverExpires", func(t *testing.T) {
		c := factory(t)
		defer c.Close()
		ctx := context.Background()
		_ = c.Set(ctx, "k", []byte("v"), 0)
		time.Sleep(60 * time.Millisecond)
		_, ok, _ := c.Get(ctx, "k")
		if !ok {
			t.Fatalf("zero-ttl entry expired")
		}
	})

	t.Run(name+"/DeletePrefixBulk", func(t *testing.T) {
		c := factory(t)
		defer c.Close()
		ctx := context.Background()
		// Three under one prefix, two under another.
		_ = c.Set(ctx, "wf:abc:n1", []byte("a"), 0)
		_ = c.Set(ctx, "wf:abc:n2", []byte("b"), 0)
		_ = c.Set(ctx, "wf:abc:n3", []byte("c"), 0)
		_ = c.Set(ctx, "wf:xyz:n1", []byte("d"), 0)
		_ = c.Set(ctx, "wf:xyz:n2", []byte("e"), 0)

		n, err := c.DeletePrefix(ctx, "wf:abc:")
		if err != nil {
			t.Fatalf("delete prefix: %v", err)
		}
		if n != 3 {
			t.Fatalf("deleted %d, want 3", n)
		}

		// abc keys gone.
		for _, k := range []string{"wf:abc:n1", "wf:abc:n2", "wf:abc:n3"} {
			if _, ok, _ := c.Get(ctx, k); ok {
				t.Fatalf("%s should be deleted", k)
			}
		}
		// xyz keys preserved.
		for _, k := range []string{"wf:xyz:n1", "wf:xyz:n2"} {
			if _, ok, _ := c.Get(ctx, k); !ok {
				t.Fatalf("%s should be preserved", k)
			}
		}
	})

	t.Run(name+"/ConcurrentSetGet", func(t *testing.T) {
		c := factory(t)
		defer c.Close()
		ctx := context.Background()
		const N = 200

		var wg sync.WaitGroup
		var setErrors, getErrors atomic.Int64
		for i := 0; i < N; i++ {
			wg.Add(2)
			i := i
			go func() {
				defer wg.Done()
				if err := c.Set(ctx, fmt.Sprintf("k%d", i), []byte(fmt.Sprintf("v%d", i)), 0); err != nil {
					setErrors.Add(1)
				}
			}()
			go func() {
				defer wg.Done()
				if _, _, err := c.Get(ctx, fmt.Sprintf("k%d", i)); err != nil {
					getErrors.Add(1)
				}
			}()
		}
		wg.Wait()
		if setErrors.Load() != 0 || getErrors.Load() != 0 {
			t.Fatalf("set errors: %d, get errors: %d", setErrors.Load(), getErrors.Load())
		}
		// All N keys should now be readable.
		for i := 0; i < N; i++ {
			got, ok, _ := c.Get(ctx, fmt.Sprintf("k%d", i))
			if !ok || string(got) != fmt.Sprintf("v%d", i) {
				t.Fatalf("k%d: got=%q ok=%v", i, got, ok)
			}
		}
	})

	t.Run(name+"/ClosedReturnsErrClosed", func(t *testing.T) {
		c := factory(t)
		_ = c.Set(context.Background(), "k", []byte("v"), 0)
		_ = c.Close()
		if _, _, err := c.Get(context.Background(), "k"); !errors.Is(err, ErrClosed) {
			t.Fatalf("get after close: %v", err)
		}
		if err := c.Set(context.Background(), "k", []byte("v"), 0); !errors.Is(err, ErrClosed) {
			t.Fatalf("set after close: %v", err)
		}
		if err := c.Delete(context.Background(), "k"); !errors.Is(err, ErrClosed) {
			t.Fatalf("delete after close: %v", err)
		}
		// Close itself is idempotent.
		if err := c.Close(); err != nil {
			t.Fatalf("double close: %v", err)
		}
	})
}

// TestMemoryCacheConformance runs the suite against the in-memory
// backend. SQL/Redis backends each have their own test binary that
// calls runCacheConformance.
func TestMemoryCacheConformance(t *testing.T) {
	runCacheConformance(t, "memory", func(t *testing.T) Cache {
		return NewMemoryCache(MemoryOpts{})
	})
}

// TestMemoryCacheLRUEviction is memory-specific: prove the byte
// ceiling kicks out the LRU entry when full.
func TestMemoryCacheLRUEviction(t *testing.T) {
	c := NewMemoryCache(MemoryOpts{MaxBytes: 30}) // 30 bytes total
	defer c.Close()
	ctx := context.Background()

	_ = c.Set(ctx, "a", []byte("0123456789"), 0) // 10 bytes
	_ = c.Set(ctx, "b", []byte("0123456789"), 0) // 20 bytes
	_ = c.Set(ctx, "c", []byte("0123456789"), 0) // 30 bytes — at cap
	// Touch "a" so "b" becomes LRU.
	_, _, _ = c.Get(ctx, "a")
	_, _, _ = c.Get(ctx, "c")
	// Insert "d" — pushes us over, evict LRU = "b".
	_ = c.Set(ctx, "d", []byte("0123456789"), 0)

	if _, ok, _ := c.Get(ctx, "b"); ok {
		t.Fatalf("b should have been evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok, _ := c.Get(ctx, k); !ok {
			t.Fatalf("%s should still be present", k)
		}
	}
}
