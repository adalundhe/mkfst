package tasks

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecurringEveryFires(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{DedupWindow: time.Minute})
	rs, err := NewRecurringScheduler(RecurringOpts{Store: store, Tick: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewRecurringScheduler: %v", err)
	}
	if err := rs.Every("noop", 30*time.Millisecond, Task{Type: "noop"}); err != nil {
		t.Fatalf("Every: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = rs.Run(ctx) }()
	t.Cleanup(func() { cancel() })

	// Run for ~150ms; expect 4-5 fires.
	time.Sleep(150 * time.Millisecond)
	cancel()

	stats, _ := store.QueueStats(context.Background(), "default")
	total := stats.Pending + stats.Scheduled + stats.Running
	if total < 3 || total > 7 {
		t.Fatalf("expected 3..7 enqueues in 150ms with 30ms interval, got %d", total)
	}
}

func TestRecurringDedupAcrossSchedulers(t *testing.T) {
	// Two schedulers ticking the same job concurrently. The dedup
	// window collapses simultaneous fires to one enqueue per logical
	// fire-time bucket.
	store := NewMemoryStore(MemoryOpts{DedupWindow: 5 * time.Second})

	mkScheduler := func() *RecurringScheduler {
		rs, _ := NewRecurringScheduler(RecurringOpts{Store: store, Tick: 5 * time.Millisecond})
		_ = rs.Every("dup-test", 50*time.Millisecond, Task{Type: "noop"})
		return rs
	}
	rs1 := mkScheduler()
	rs2 := mkScheduler()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = rs1.Run(ctx) }()
	go func() { _ = rs2.Run(ctx) }()
	t.Cleanup(func() { cancel() })

	time.Sleep(220 * time.Millisecond)
	cancel()

	stats, _ := store.QueueStats(context.Background(), "default")
	total := stats.Pending + stats.Scheduled + stats.Running
	// 220ms / 50ms ≈ 4 fires expected. Two schedulers running with
	// dedup must NOT produce 8.
	if total > 6 {
		t.Fatalf("dedup failed: 2 schedulers @ 50ms over 220ms produced %d enqueues, expected ≤6", total)
	}
	if total < 2 {
		t.Fatalf("schedulers fired too few times: %d", total)
	}
}

func TestRecurringCronFires(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{DedupWindow: time.Minute})
	rs, err := NewRecurringScheduler(RecurringOpts{Store: store, Tick: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewRecurringScheduler: %v", err)
	}

	// Use the gronx @everysecond tag — ticks every second.
	if err := rs.Cron("sec", "@everysecond", Task{Type: "noop"}); err != nil {
		t.Fatalf("Cron: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = rs.Run(ctx) }()
	t.Cleanup(func() { cancel() })

	// Wait through ~3 seconds. Expect roughly 2-4 fires (depends on
	// when in the second we started).
	time.Sleep(3500 * time.Millisecond)
	cancel()

	stats, _ := store.QueueStats(context.Background(), "default")
	total := stats.Pending + stats.Scheduled + stats.Running
	if total < 2 || total > 5 {
		t.Fatalf("expected 2..5 enqueues from @everysecond over 3.5s, got %d", total)
	}
}

func TestRecurringRemove(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{DedupWindow: time.Minute})
	rs, _ := NewRecurringScheduler(RecurringOpts{Store: store, Tick: 10 * time.Millisecond})

	_ = rs.Every("one", 20*time.Millisecond, Task{Type: "noop"})
	_ = rs.Every("two", 20*time.Millisecond, Task{Type: "noop"})

	if err := rs.RemoveRecurring("one"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := rs.RemoveRecurring("nope"); err == nil {
		t.Fatalf("Remove of missing job should error")
	}

	jobs := rs.ListRecurring()
	if len(jobs) != 1 || jobs[0].Name != "two" {
		t.Fatalf("after remove: %v", jobs)
	}
}

func TestRecurringInvalidExprRejected(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{})
	rs, _ := NewRecurringScheduler(RecurringOpts{Store: store})
	if err := rs.Cron("bad", "this is not a cron", Task{Type: "x"}); err == nil {
		t.Fatalf("expected invalid cron to be rejected")
	}
}

func TestRecurringStopExitsCleanly(t *testing.T) {
	store := NewMemoryStore(MemoryOpts{})
	rs, _ := NewRecurringScheduler(RecurringOpts{Store: store, Tick: 5 * time.Millisecond})
	_ = rs.Every("x", time.Second, Task{Type: "noop"})

	done := make(chan error, 1)
	go func() { done <- rs.Run(context.Background()) }()

	time.Sleep(50 * time.Millisecond)
	rs.Stop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Run didn't return within 1s of Stop")
	}
}

// TestRecurringBackoffOnEnqueueError: store rejects enqueues; OnError
// fires; tick loop continues.
func TestRecurringBackoffOnEnqueueError(t *testing.T) {
	store := &errorStore{Store: NewMemoryStore(MemoryOpts{})}
	var errCount atomic.Int32
	rs, _ := NewRecurringScheduler(RecurringOpts{
		Store: store,
		Tick:  5 * time.Millisecond,
		OnError: func(name string, err error) {
			errCount.Add(1)
		},
	})
	_ = rs.Every("x", 10*time.Millisecond, Task{Type: "noop"})

	store.fail.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = rs.Run(ctx) }()
	t.Cleanup(func() { cancel() })

	time.Sleep(80 * time.Millisecond)
	if errCount.Load() < 2 {
		t.Fatalf("expected ≥2 OnError calls, got %d", errCount.Load())
	}
}

// errorStore wraps a Store and lets the test toggle Enqueue failure.
type errorStore struct {
	Store
	fail atomic.Bool
}

func (e *errorStore) Enqueue(ctx context.Context, t Task) (Record, error) {
	if e.fail.Load() {
		return Record{}, ErrQueueClosed
	}
	return e.Store.Enqueue(ctx, t)
}
