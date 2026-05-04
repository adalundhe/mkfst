package tasks

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
)

// === Mock-Store-driven Worker tests ===
//
// Each test wires a MockStore behind a Worker and asserts the
// Worker's behavior under contrived scheduling conditions. We
// don't need a real Store implementation — just precise control
// over what Claim/Heartbeat/Complete/Fail return.

// stopAfterClaims wires up a Worker that runs until N tasks have
// been claimed; useful so tests don't have to dance around the
// poll loop.
func runWorkerForN(t *testing.T, w *worker, claimed *atomic.Int64, target int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if claimed.Load() >= target {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	w.Stop()
	// Allow drain.
	time.Sleep(20 * time.Millisecond)
}

func mkWorker(t *testing.T, store Store, modify func(*WorkerOpts)) *worker {
	t.Helper()
	opts := WorkerOpts{
		Store:               store,
		Concurrency:         1,
		PollInterval:        2 * time.Millisecond,
		MaintenanceInterval: 5 * time.Millisecond,
		VisibilityTimeout:   5 * time.Second,
		HeartbeatInterval:   1 * time.Second,
		DefaultTimeout:      time.Second,
		DefaultMaxRetries:   3,
	}
	if modify != nil {
		modify(&opts)
	}
	w, err := NewWorker(opts)
	if err != nil {
		t.Fatal(err)
	}
	return w.(*worker)
}

// matchesClaim is the standard arg-matcher for Claim calls — any
// queue, any worker ID, any visibility duration.
func matchesClaim(_ context.Context, _ string, _ string, _ time.Duration) bool { return true }

// rec returns a Record shaped like one a real store would hand
// out post-Claim.
func rec(id, taskType string, attempts int) *Record {
	return &Record{
		Task:        Task{ID: id, Type: taskType, Queue: "default"},
		State:       StateRunning,
		Attempts:    attempts,
		EnqueuedAt:  time.Now().Add(-time.Second),
		StartedAt:   time.Now(),
		OwnerWorker: "w-test",
	}
}

// Helpers for empty/no-op responses across PromoteScheduled /
// ReclaimExpired which fire on every maintenance tick.
func setupMaintenanceNoOp(s *MockStore) {
	s.EXPECT().PromoteScheduled(mock.Anything, mock.Anything, mock.Anything).
		Return(0, nil).Maybe()
	s.EXPECT().ReclaimExpired(mock.Anything, mock.Anything, mock.Anything).
		Return(0, nil).Maybe()
	s.EXPECT().QueueStats(mock.Anything, mock.Anything).
		Return(QueueStats{}, nil).Maybe()
}

// ---------------------------------------------------------------

// TestWorker_DispatchHandlerSuccess proves a successful handler
// invocation calls Complete on the store.
func TestWorker_DispatchHandlerSuccess(t *testing.T) {
	store := NewMockStore(t)
	setupMaintenanceNoOp(store)

	var (
		claimed     atomic.Int64
		completedID string
		mu          sync.Mutex
	)
	taskRec := rec("task-1", "noop", 1)

	store.EXPECT().Claim(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _ time.Duration) (*Record, error) {
			if claimed.Add(1) == 1 {
				return taskRec, nil
			}
			return nil, nil // empty queue
		})
	store.EXPECT().Heartbeat(mock.Anything, "task-1", mock.Anything, mock.Anything).
		Return(nil).Maybe()
	store.EXPECT().Complete(mock.Anything, "task-1", mock.Anything).
		RunAndReturn(func(_ context.Context, id, _ string) error {
			mu.Lock()
			completedID = id
			mu.Unlock()
			return nil
		}).Once()

	w := mkWorker(t, store, nil)
	if err := w.Register("noop", func(ctx context.Context, t Task) error { return nil }); err != nil {
		t.Fatal(err)
	}
	runWorkerForN(t, w, &claimed, 1)

	mu.Lock()
	defer mu.Unlock()
	if completedID != "task-1" {
		t.Fatalf("expected Complete on task-1, got %q", completedID)
	}
	if w.completed.Load() != 1 {
		t.Fatalf("stats.Completed: %d", w.completed.Load())
	}
}

// TestWorker_HandlerErrorTriggersRetry proves a failed handler
// (with attempts < maxRetries) calls Fail with a non-nil
// nextAttemptAt.
func TestWorker_HandlerErrorTriggersRetry(t *testing.T) {
	store := NewMockStore(t)
	setupMaintenanceNoOp(store)

	var claimed atomic.Int64
	taskRec := rec("retry-1", "flaky", 1) // attempt 1; retries=3 → retry
	taskRec.Task.MaxRetries = nil          // use worker default

	store.EXPECT().Claim(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _ time.Duration) (*Record, error) {
			if claimed.Add(1) == 1 {
				return taskRec, nil
			}
			return nil, nil
		})
	store.EXPECT().Heartbeat(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()

	var capturedNextAttempt *time.Time
	store.EXPECT().Fail(mock.Anything, "retry-1", mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _, _ string, n *time.Time) error {
			capturedNextAttempt = n
			return nil
		}).Once()

	w := mkWorker(t, store, nil)
	_ = w.Register("flaky", func(ctx context.Context, t Task) error {
		return errors.New("transient")
	})
	runWorkerForN(t, w, &claimed, 1)

	if capturedNextAttempt == nil {
		t.Fatal("Fail should have nextAttemptAt set (retry)")
	}
	if w.retried.Load() != 1 {
		t.Fatalf("stats.Retried: %d", w.retried.Load())
	}
}

// TestWorker_RetriesExhaustedFails proves attempts > maxRetries
// causes Fail with nil nextAttemptAt (terminal).
func TestWorker_RetriesExhaustedFails(t *testing.T) {
	store := NewMockStore(t)
	setupMaintenanceNoOp(store)

	var claimed atomic.Int64
	taskRec := rec("dead", "explode", 5)        // attempts=5 already
	taskRec.Task.MaxRetries = Retries(2)         // budget=2 → 5 > 2 → terminal

	store.EXPECT().Claim(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _ time.Duration) (*Record, error) {
			if claimed.Add(1) == 1 {
				return taskRec, nil
			}
			return nil, nil
		})
	store.EXPECT().Heartbeat(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()

	var capturedNextAttempt *time.Time
	store.EXPECT().Fail(mock.Anything, "dead", mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _, _ string, n *time.Time) error {
			capturedNextAttempt = n
			return nil
		}).Once()

	w := mkWorker(t, store, nil)
	_ = w.Register("explode", func(ctx context.Context, t Task) error {
		return errors.New("permanent")
	})
	runWorkerForN(t, w, &claimed, 1)

	if capturedNextAttempt != nil {
		t.Fatalf("expected nil nextAttempt (terminal); got %v", capturedNextAttempt)
	}
	if w.failed.Load() != 1 {
		t.Fatalf("stats.Failed: %d", w.failed.Load())
	}
}

// TestWorker_PanicInHandlerRecovered proves a panicking handler
// turns into a Fail call (with retry math), not a worker crash.
func TestWorker_PanicInHandlerRecovered(t *testing.T) {
	store := NewMockStore(t)
	setupMaintenanceNoOp(store)

	var claimed atomic.Int64
	taskRec := rec("panicky", "boom", 1)
	taskRec.Task.MaxRetries = Retries(1)

	store.EXPECT().Claim(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _ time.Duration) (*Record, error) {
			if claimed.Add(1) == 1 {
				return taskRec, nil
			}
			return nil, nil
		})
	store.EXPECT().Heartbeat(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()

	var capturedMsg string
	store.EXPECT().Fail(mock.Anything, "panicky", mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _, msg string, _ *time.Time) error {
			capturedMsg = msg
			return nil
		}).Once()

	w := mkWorker(t, store, nil)
	_ = w.Register("boom", func(ctx context.Context, t Task) error {
		panic("on purpose")
	})
	runWorkerForN(t, w, &claimed, 1)

	if !strings.Contains(capturedMsg, "panicked") || !strings.Contains(capturedMsg, "on purpose") {
		t.Fatalf("expected panic message in Fail, got %q", capturedMsg)
	}
}

// TestWorker_UnknownTaskTypeFailsImmediately proves a task whose
// Type isn't registered fails terminally without retry math.
func TestWorker_UnknownTaskTypeFailsImmediately(t *testing.T) {
	store := NewMockStore(t)
	setupMaintenanceNoOp(store)

	var claimed atomic.Int64
	taskRec := rec("orphan", "no-handler-here", 1)

	store.EXPECT().Claim(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _ time.Duration) (*Record, error) {
			if claimed.Add(1) == 1 {
				return taskRec, nil
			}
			return nil, nil
		})

	var capturedNext *time.Time
	store.EXPECT().Fail(mock.Anything, "orphan", mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _, _ string, n *time.Time) error {
			capturedNext = n
			return nil
		}).Once()

	w := mkWorker(t, store, nil)
	// NB: NO Register("no-handler-here", ...).
	runWorkerForN(t, w, &claimed, 1)

	if capturedNext != nil {
		t.Fatal("unknown-task-type Fail should pass nil next-attempt (terminal)")
	}
	if w.failed.Load() != 1 {
		t.Fatalf("stats.Failed: %d", w.failed.Load())
	}
}

// TestWorker_MaintenanceDrivesPromoteAndReclaim proves the
// maintenance goroutine ticks PromoteScheduled + ReclaimExpired
// at the configured interval.
func TestWorker_MaintenanceDrivesPromoteAndReclaim(t *testing.T) {
	store := NewMockStore(t)

	var promotes, reclaims atomic.Int64
	store.EXPECT().PromoteScheduled(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, string, time.Time) (int, error) {
			promotes.Add(1)
			return 0, nil
		})
	store.EXPECT().ReclaimExpired(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, string, time.Time) (int, error) {
			reclaims.Add(1)
			return 0, nil
		})
	store.EXPECT().QueueStats(mock.Anything, mock.Anything).Return(QueueStats{}, nil).Maybe()
	// No tasks claimed.
	store.EXPECT().Claim(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, nil).Maybe()

	w := mkWorker(t, store, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	<-ctx.Done()
	w.Stop()
	time.Sleep(20 * time.Millisecond)

	if promotes.Load() < 5 {
		t.Fatalf("expected ≥5 PromoteScheduled ticks in 200ms, got %d", promotes.Load())
	}
	if reclaims.Load() < 5 {
		t.Fatalf("expected ≥5 ReclaimExpired ticks, got %d", reclaims.Load())
	}
}

// TestWorker_OnErrorReportsClaimErrors proves the OnError hook
// fires when the store returns transient errors from Claim.
func TestWorker_OnErrorReportsClaimErrors(t *testing.T) {
	store := NewMockStore(t)
	setupMaintenanceNoOp(store)

	var claimAttempts atomic.Int64
	store.EXPECT().Claim(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, string, string, time.Duration) (*Record, error) {
			claimAttempts.Add(1)
			return nil, errors.New("network blip")
		})

	var errsMu sync.Mutex
	var seen []string
	w := mkWorker(t, store, func(opts *WorkerOpts) {
		opts.OnError = func(workerID, op string, err error) {
			errsMu.Lock()
			seen = append(seen, op+":"+err.Error())
			errsMu.Unlock()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	<-ctx.Done()
	w.Stop()
	time.Sleep(20 * time.Millisecond)

	errsMu.Lock()
	defer errsMu.Unlock()
	if len(seen) == 0 {
		t.Fatal("expected ≥1 OnError callback for claim errors")
	}
	if !strings.Contains(seen[0], "claim:") {
		t.Fatalf("expected op=claim, got %q", seen[0])
	}
}

// TestWorker_ConcurrencySpawnsSlots proves Concurrency>1 dispatches
// in parallel. With 4 slots and 4 concurrently-blocking handlers,
// all four must enter their handler at the same time.
func TestWorker_ConcurrencySpawnsSlots(t *testing.T) {
	store := NewMockStore(t)
	setupMaintenanceNoOp(store)

	const N = 4
	var claimed atomic.Int64
	store.EXPECT().Claim(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, string, string, time.Duration) (*Record, error) {
			n := claimed.Add(1)
			if n <= N {
				return rec("p-"+string('0'+byte(n)), "long", 1), nil
			}
			return nil, nil
		})
	store.EXPECT().Heartbeat(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	store.EXPECT().Complete(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	gate := make(chan struct{})
	released := make(chan struct{})
	var inHandler atomic.Int32

	w := mkWorker(t, store, func(opts *WorkerOpts) { opts.Concurrency = N })
	_ = w.Register("long", func(ctx context.Context, t Task) error {
		inHandler.Add(1)
		<-gate     // hold all handlers
		<-released // and release together
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	// Wait until all 4 handlers are inside.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if inHandler.Load() == int32(N) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := inHandler.Load()
	close(gate)
	close(released)
	w.Stop()
	cancel()
	time.Sleep(20 * time.Millisecond)

	if got != N {
		t.Fatalf("expected %d concurrent handlers, got %d (no parallelism)", N, got)
	}
}
