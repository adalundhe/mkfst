package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPool_ParallelHandlers proves multiple concurrent calls into
// a pool can run on disjoint runtimes. Without the pool (single
// shared Runtime), this serializes; with the pool of 4, four
// 100ms tasks complete in ~100ms total instead of ~400ms.
func TestPool_ParallelHandlers(t *testing.T) {
	ctx := context.Background()
	const size = 4
	pool, err := NewPool(ctx, PoolOpts{
		Size: size,
		Init: func(ctx context.Context, rt Runtime) error {
			// Pre-eval a function we'll call repeatedly.
			v, err := rt.Eval(ctx, `globalThis.work = function(ms) {
				const end = Date.now() + ms;
				while (Date.now() < end) {}
				return ms;
			}`, EvalOpts{})
			if err != nil {
				return err
			}
			v.Free(ctx)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close(ctx)

	const tasks = 4
	const taskMs = 80
	start := time.Now()
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < tasks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := pool.With(ctx, func(rt Runtime) error {
				v, err := rt.Eval(ctx, "work("+itoa(taskMs)+")", EvalOpts{})
				if err != nil {
					return err
				}
				defer v.Free(ctx)
				return nil
			})
			if err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if ok.Load() != tasks {
		t.Fatalf("ok=%d, want %d", ok.Load(), tasks)
	}
	// 4 parallel 80ms tasks on a 4-runtime pool should finish in
	// roughly ~100–200ms; serialization would take 4×80 = 320ms+.
	// Use a generous threshold to avoid CI flake.
	if elapsed > 280*time.Millisecond {
		t.Fatalf("parallelism didn't engage: %v elapsed for %d tasks of %dms each",
			elapsed, tasks, taskMs)
	}
}

// TestPool_ConcurrentStress hammers the pool with many short tasks
// from many goroutines. Without the race detector available in
// this build, this is our coarse safety check: if the pool's
// borrow/return state is broken, we'd see goroutines deadlock or
// the final ok-count fail to match the issued count.
func TestPool_ConcurrentStress(t *testing.T) {
	ctx := context.Background()
	pool, err := NewPool(ctx, PoolOpts{Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close(ctx)

	const goroutines = 32
	const perGoroutine = 25
	var wg sync.WaitGroup
	var ok atomic.Int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				err := pool.With(ctx, func(rt Runtime) error {
					v, err := rt.Eval(ctx, "1+1", EvalOpts{})
					if err != nil {
						return err
					}
					n, err := v.Int32(ctx)
					v.Free(ctx)
					if err != nil {
						return err
					}
					if n != 2 {
						return errBad("unexpected result")
					}
					return nil
				})
				if err == nil {
					ok.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if ok.Load() != int64(goroutines*perGoroutine) {
		t.Fatalf("ok=%d, want %d", ok.Load(), goroutines*perGoroutine)
	}
}

type errBad string

func (e errBad) Error() string { return string(e) }

func TestPool_ClosedRejects(t *testing.T) {
	ctx := context.Background()
	pool, err := NewPool(ctx, PoolOpts{Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// After Close, Borrow blocks then ctx-cancels. Use a short
	// timeout to verify the channel is permanently empty.
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = pool.Borrow(tctx)
	if err == nil {
		t.Fatal("expected error on Borrow after Close")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
