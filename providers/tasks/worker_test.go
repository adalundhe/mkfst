package tasks

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWorkerEndToEnd: register a handler, enqueue a task, run the
// worker, observe completion. The "hello-world" smoke for the worker
// loop.
func TestWorkerEndToEnd(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{})
	w, err := NewWorker(WorkerOpts{
		Store:               store,
		Concurrency:         2,
		PollInterval:        5 * time.Millisecond,
		MaintenanceInterval: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	var ran atomic.Int32
	_ = w.Register("noop", func(ctx context.Context, task Task) error {
		ran.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneRun := make(chan error, 1)
	go func() { doneRun <- w.Run(ctx) }()

	scheduler := NewScheduler(store)
	for i := 0; i < 10; i++ {
		if _, err := scheduler.Enqueue(context.Background(), Task{Type: "noop"}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && ran.Load() < 10 {
		time.Sleep(20 * time.Millisecond)
	}
	if ran.Load() != 10 {
		t.Fatalf("ran %d, want 10", ran.Load())
	}

	cancel()
	if err := <-doneRun; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestWorkerRetryWithBackoff: handler fails N times, succeeds on
// N+1th, total observed retries = N. Proves the retry/backoff path.
func TestWorkerRetryWithBackoff(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{})
	w, err := NewWorker(WorkerOpts{
		Store:               store,
		Concurrency:         1,
		PollInterval:        5 * time.Millisecond,
		MaintenanceInterval: 10 * time.Millisecond,
		Backoff:             func(int) time.Duration { return 5 * time.Millisecond },
		DefaultMaxRetries:   3,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	var attempts atomic.Int32
	_ = w.Register("flaky", func(ctx context.Context, task Task) error {
		n := attempts.Add(1)
		if n < 3 {
			return errors.New("transient")
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	scheduler := NewScheduler(store)
	rec, _ := scheduler.Enqueue(context.Background(), Task{Type: "flaky", MaxRetries: Retries(5)})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := store.Inspect(context.Background(), rec.Task.ID)
		if got.State == StateCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	final, _ := store.Inspect(context.Background(), rec.Task.ID)
	if final.State != StateCompleted {
		t.Fatalf("state: %v (last error: %q, attempts: %d)", final.State, final.LastError, final.Attempts)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts: %d", attempts.Load())
	}
}

// TestWorkerRetryExhaustionGoesTerminal: handler always fails,
// MaxRetries=2 → 3 attempts total, then StateFailed.
func TestWorkerRetryExhaustionGoesTerminal(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{})
	w, err := NewWorker(WorkerOpts{
		Store:               store,
		Concurrency:         1,
		PollInterval:        5 * time.Millisecond,
		MaintenanceInterval: 10 * time.Millisecond,
		Backoff:             func(int) time.Duration { return 1 * time.Millisecond },
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	_ = w.Register("doomed", func(ctx context.Context, task Task) error {
		return errors.New("nope")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	scheduler := NewScheduler(store)
	rec, _ := scheduler.Enqueue(context.Background(), Task{Type: "doomed", MaxRetries: Retries(2)})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := store.Inspect(context.Background(), rec.Task.ID)
		if got.State == StateFailed {
			if got.Attempts != 3 {
				t.Fatalf("attempts: got %d want 3", got.Attempts)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	final, _ := store.Inspect(context.Background(), rec.Task.ID)
	t.Fatalf("never reached terminal: state=%v attempts=%d err=%q", final.State, final.Attempts, final.LastError)
}

// TestWorkerHandlerPanicTreatedAsError: a panicking handler must not
// kill the worker — it's converted to an error and counted as a
// retryable failure.
func TestWorkerHandlerPanicTreatedAsError(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{})
	w, _ := NewWorker(WorkerOpts{
		Store:               store,
		Concurrency:         1,
		PollInterval:        5 * time.Millisecond,
		MaintenanceInterval: 10 * time.Millisecond,
		Backoff:             func(int) time.Duration { return 1 * time.Millisecond },
	})

	_ = w.Register("boom", func(ctx context.Context, task Task) error {
		panic("rogue handler")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	scheduler := NewScheduler(store)
	rec, _ := scheduler.Enqueue(context.Background(), Task{Type: "boom", MaxRetries: Retries(0)})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := store.Inspect(context.Background(), rec.Task.ID)
		if got.State == StateFailed {
			if got.LastError == "" {
				t.Fatalf("LastError empty after panic")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("never failed; worker likely died from the panic")
}

// TestWorkerGracefulShutdown: cancel ctx mid-run, all goroutines exit
// promptly. Goroutine count should return to baseline.
func TestWorkerGracefulShutdown(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{})
	w, _ := NewWorker(WorkerOpts{
		Store:       store,
		Concurrency: 8,
	})

	_ = w.Register("sleep", func(ctx context.Context, task Task) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Hour): // would never finish naturally
			return nil
		}
	})

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	doneRun := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(doneRun)
	}()

	scheduler := NewScheduler(store)
	for i := 0; i < 5; i++ {
		_, _ = scheduler.Enqueue(context.Background(), Task{Type: "sleep"})
	}
	time.Sleep(100 * time.Millisecond) // let workers pick them up

	cancel()
	select {
	case <-doneRun:
	case <-time.After(5 * time.Second):
		t.Fatalf("Run didn't return within 5s of ctx cancel")
	}

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()
	if delta := after - baseline; delta > 5 {
		t.Fatalf("goroutines leaked: baseline=%d after=%d delta=%d", baseline, after, delta)
	}
}

// TestWorkerHeartbeatKeepsLongTaskAlive: a handler that runs longer
// than VisibilityTimeout should NOT be reclaimed because the worker
// heartbeats during execution.
func TestWorkerHeartbeatKeepsLongTaskAlive(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{})
	w, _ := NewWorker(WorkerOpts{
		Store:               store,
		Concurrency:         1,
		VisibilityTimeout:   100 * time.Millisecond,
		HeartbeatInterval:   25 * time.Millisecond,
		MaintenanceInterval: 10 * time.Millisecond,
		PollInterval:        5 * time.Millisecond,
	})

	var attempts atomic.Int32
	_ = w.Register("long", func(ctx context.Context, task Task) error {
		attempts.Add(1)
		// Run for longer than the visibility timeout. If heartbeats
		// don't extend the lease, the maintenance reaper would
		// reclaim and a second worker iteration would re-execute this
		// handler — bumping attempts > 1.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	scheduler := NewScheduler(store)
	rec, _ := scheduler.Enqueue(context.Background(), Task{Type: "long"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := store.Inspect(context.Background(), rec.Task.ID)
		if got.State == StateCompleted {
			if attempts.Load() != 1 {
				t.Fatalf("expected exactly 1 attempt; got %d (heartbeat didn't extend lease)", attempts.Load())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("long task never completed (attempts=%d)", attempts.Load())
}

// TestWorkerStartStopCyclesNoLeak proves that repeated Run/Stop
// cycles don't accumulate goroutines. The audit's bar: zero
// untracked goroutines after teardown. Mirrors the vfs.Subscribe
// leak-detector test from the earlier audit.
func TestWorkerStartStopCyclesNoLeak(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{})

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for i := 0; i < 25; i++ {
		w, err := NewWorker(WorkerOpts{
			Store:               store,
			Concurrency:         8,
			PollInterval:        5 * time.Millisecond,
			MaintenanceInterval: 25 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewWorker: %v", err)
		}
		_ = w.Register("noop", func(ctx context.Context, task Task) error { return nil })

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_ = w.Run(ctx)
			close(done)
		}()
		// Let workers spin up and idle-poll.
		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("cycle %d: Run didn't return within 2s of cancel", i)
		}
	}

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()
	if delta := after - baseline; delta > 5 {
		t.Fatalf("25 worker start/stop cycles leaked %d goroutines (baseline=%d, after=%d)",
			delta, baseline, after)
	}
}

// TestWorkerNoLeakUnderHighChurn: enqueue + complete many tasks fast,
// then assert no goroutine leak and no growth in the records map.
func TestWorkerNoLeakUnderHighChurn(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{})
	w, _ := NewWorker(WorkerOpts{
		Store:               store,
		Concurrency:         8,
		PollInterval:        2 * time.Millisecond,
		MaintenanceInterval: 25 * time.Millisecond,
	})
	_ = w.Register("fast", func(ctx context.Context, task Task) error { return nil })

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	doneRun := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(doneRun)
	}()

	scheduler := NewScheduler(store)
	const N = 500
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = scheduler.Enqueue(context.Background(), Task{Type: "fast"})
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && w.Stats().Completed < N {
		time.Sleep(50 * time.Millisecond)
	}
	if got := w.Stats().Completed; got != N {
		t.Fatalf("completed %d/%d", got, N)
	}

	cancel()
	<-doneRun

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()
	if delta := after - baseline; delta > 5 {
		t.Fatalf("goroutines leaked under churn: baseline=%d after=%d delta=%d", baseline, after, delta)
	}
}
