package tasks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runStoreConformance is the shared test harness every Store
// implementation must pass. The memory store calls it directly; the
// SQL and Redis stores will reuse it via _test.go files in their
// respective packages (or via a separate test binary that
// instantiates the appropriate backend).
//
// Each subtest gets a fresh store via the supplied factory. Tests
// must not assume in-process isolation — the SQL/Redis backends may
// share state with parallel runs, so factory should namespace via
// queue prefix or tear down completely.
func runStoreConformance(t *testing.T, name string, newStore func(t *testing.T) Store) {
	t.Run(name+"/EnqueueAndInspect", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		rec, err := s.Enqueue(ctx, Task{Type: "echo", Payload: []byte("hi")})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if rec.Task.ID == "" {
			t.Fatalf("ID not assigned")
		}
		if rec.State != StatePending {
			t.Fatalf("state: got %v want pending", rec.State)
		}

		got, err := s.Inspect(ctx, rec.Task.ID)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		if got.Task.ID != rec.Task.ID {
			t.Fatalf("inspect ID: got %s want %s", got.Task.ID, rec.Task.ID)
		}
	})

	t.Run(name+"/ClaimDispatchesOneTaskOnce", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		// Single task, two workers race to claim it. Exactly one wins.
		rec, _ := s.Enqueue(ctx, Task{Type: "x"})

		results := make(chan *Record, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r, err := s.Claim(ctx, "default", fmt.Sprintf("w%d", i), 5*time.Second)
				if err != nil {
					t.Errorf("claim worker %d: %v", i, err)
					return
				}
				results <- r
			}(i)
		}
		wg.Wait()
		close(results)
		var got []*Record
		for r := range results {
			got = append(got, r)
		}
		nonNil := 0
		for _, r := range got {
			if r != nil {
				nonNil++
				if r.Task.ID != rec.Task.ID {
					t.Fatalf("claimed wrong task: got %s want %s", r.Task.ID, rec.Task.ID)
				}
			}
		}
		if nonNil != 1 {
			t.Fatalf("expected exactly 1 successful claim, got %d", nonNil)
		}
	})

	t.Run(name+"/EmptyQueueReturnsNil", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()
		r, err := s.Claim(ctx, "default", "w", 5*time.Second)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if r != nil {
			t.Fatalf("expected nil from empty queue, got %+v", r)
		}
	})

	t.Run(name+"/CompleteRequiresOwnership", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		_, _ = s.Enqueue(ctx, Task{Type: "x"})
		rec, _ := s.Claim(ctx, "default", "owner", 5*time.Second)

		// Wrong worker can't complete.
		err := s.Complete(ctx, rec.Task.ID, "imposter")
		if !errors.Is(err, ErrNotOwner) {
			t.Fatalf("imposter complete: want ErrNotOwner, got %v", err)
		}

		// Real owner can.
		if err := s.Complete(ctx, rec.Task.ID, "owner"); err != nil {
			t.Fatalf("owner complete: %v", err)
		}

		got, _ := s.Inspect(ctx, rec.Task.ID)
		if got.State != StateCompleted {
			t.Fatalf("post-complete state: %v", got.State)
		}
	})

	t.Run(name+"/FailWithRetryReschedules", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		_, _ = s.Enqueue(ctx, Task{Type: "x", MaxRetries: Retries(3)})
		rec, _ := s.Claim(ctx, "default", "w1", 5*time.Second)

		retryAt := time.Now().Add(50 * time.Millisecond)
		if err := s.Fail(ctx, rec.Task.ID, "w1", "boom", &retryAt); err != nil {
			t.Fatalf("fail: %v", err)
		}

		// Should be scheduled, not yet pending.
		mid, _ := s.Inspect(ctx, rec.Task.ID)
		if mid.State != StateScheduled {
			t.Fatalf("after fail+retry: state %v want scheduled", mid.State)
		}
		if mid.LastError != "boom" {
			t.Fatalf("LastError: %q", mid.LastError)
		}

		// After PromoteScheduled at the retry time, it's pending.
		time.Sleep(75 * time.Millisecond)
		_, _ = s.PromoteScheduled(ctx, "default", time.Now())
		after, _ := s.Inspect(ctx, rec.Task.ID)
		if after.State != StatePending {
			t.Fatalf("after promote: state %v want pending", after.State)
		}
	})

	t.Run(name+"/FailNoRetryGoesTerminal", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		_, _ = s.Enqueue(ctx, Task{Type: "x"})
		rec, _ := s.Claim(ctx, "default", "w", 5*time.Second)

		if err := s.Fail(ctx, rec.Task.ID, "w", "permanent", nil); err != nil {
			t.Fatalf("fail: %v", err)
		}
		got, _ := s.Inspect(ctx, rec.Task.ID)
		if got.State != StateFailed {
			t.Fatalf("state: %v", got.State)
		}
	})

	t.Run(name+"/ScheduleAtDelaysUntilPromoted", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		when := time.Now().Add(100 * time.Millisecond)
		rec, err := s.ScheduleAt(ctx, when, Task{Type: "x"})
		if err != nil {
			t.Fatalf("schedule: %v", err)
		}
		if rec.State != StateScheduled {
			t.Fatalf("state: %v", rec.State)
		}

		// Claim should return nil because the task isn't pending yet.
		r, _ := s.Claim(ctx, "default", "w", time.Second)
		if r != nil {
			t.Fatalf("got task before promotion: %+v", r)
		}

		// After promotion at or after `when`, claim succeeds.
		time.Sleep(120 * time.Millisecond)
		_, _ = s.PromoteScheduled(ctx, "default", time.Now())
		r, _ = s.Claim(ctx, "default", "w", time.Second)
		if r == nil {
			t.Fatalf("post-promote claim returned nil")
		}
	})

	t.Run(name+"/HeartbeatExtendsLease", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		_, _ = s.Enqueue(ctx, Task{Type: "x"})
		rec, _ := s.Claim(ctx, "default", "w", 100*time.Millisecond)

		// Wait past the original lease, but heartbeat midway.
		time.Sleep(60 * time.Millisecond)
		if err := s.Heartbeat(ctx, rec.Task.ID, "w", 200*time.Millisecond); err != nil {
			t.Fatalf("heartbeat: %v", err)
		}

		// Now wait past what would have been the original expiration
		// and try to ReclaimExpired. The heartbeat-extended lease
		// should keep the task in running state.
		time.Sleep(80 * time.Millisecond) // 140ms total > 100ms original lease
		reclaimed, _ := s.ReclaimExpired(ctx, "default", time.Now())
		if reclaimed != 0 {
			t.Fatalf("heartbeat didn't extend lease: reclaimed %d", reclaimed)
		}

		got, _ := s.Inspect(ctx, rec.Task.ID)
		if got.State != StateRunning {
			t.Fatalf("state after extended lease: %v", got.State)
		}
	})

	t.Run(name+"/ReclaimExpiredReturnsTaskToPending", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		_, _ = s.Enqueue(ctx, Task{Type: "x"})
		rec, _ := s.Claim(ctx, "default", "w-dead", 50*time.Millisecond)

		time.Sleep(70 * time.Millisecond)
		reclaimed, _ := s.ReclaimExpired(ctx, "default", time.Now())
		if reclaimed != 1 {
			t.Fatalf("expected 1 reclaimed, got %d", reclaimed)
		}

		got, _ := s.Inspect(ctx, rec.Task.ID)
		if got.State != StatePending {
			t.Fatalf("post-reclaim state: %v", got.State)
		}

		// New worker can now claim it.
		again, err := s.Claim(ctx, "default", "w-fresh", time.Second)
		if err != nil {
			t.Fatalf("post-reclaim claim: %v", err)
		}
		if again == nil || again.Task.ID != rec.Task.ID {
			t.Fatalf("post-reclaim got %+v", again)
		}
	})

	t.Run(name+"/CancelPending", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		rec, _ := s.Enqueue(ctx, Task{Type: "x"})
		if err := s.Cancel(ctx, rec.Task.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		got, _ := s.Inspect(ctx, rec.Task.ID)
		if got.State != StateCancelled {
			t.Fatalf("state: %v", got.State)
		}
		// Cancelled task should NOT be claimable.
		r, _ := s.Claim(ctx, "default", "w", time.Second)
		if r != nil {
			t.Fatalf("claimed a cancelled task: %+v", r)
		}
	})

	t.Run(name+"/UniqueKeyDedups", func(t *testing.T) {
		s := newStore(t)
		// Skip if backend doesn't support dedup (the conformance
		// suite probes via a synthetic enqueue + retry).
		ctx := context.Background()
		rec, err := s.Enqueue(ctx, Task{Type: "x", UniqueKey: "test-key-1"})
		if err != nil {
			s.Close()
			t.Fatalf("first enqueue: %v", err)
		}
		_, err = s.Enqueue(ctx, Task{Type: "x", UniqueKey: "test-key-1"})
		s.Close()
		if !errors.Is(err, ErrUniqueViolation) {
			t.Fatalf("expected ErrUniqueViolation, got %v (rec=%s)", err, rec.Task.ID)
		}
	})

	t.Run(name+"/PriorityOrdersClaims", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		_, _ = s.Enqueue(ctx, Task{Type: "low", Priority: 0})
		_, _ = s.Enqueue(ctx, Task{Type: "high", Priority: 100})
		_, _ = s.Enqueue(ctx, Task{Type: "med", Priority: 50})

		var got []string
		for i := 0; i < 3; i++ {
			r, _ := s.Claim(ctx, "default", "w", time.Second)
			if r == nil {
				t.Fatalf("claim %d returned nil", i)
			}
			got = append(got, r.Task.Type)
		}
		want := []string{"high", "med", "low"}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("claim order: got %v want %v", got, want)
			}
		}
	})

	t.Run(name+"/PurgeOldTerminalTasks", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		rec, _ := s.Enqueue(ctx, Task{Type: "x"})
		claimed, _ := s.Claim(ctx, "default", "w", time.Second)
		_ = s.Complete(ctx, claimed.Task.ID, "w")
		_ = rec

		// Cutoff in the future — should purge all completed.
		purged, err := s.PurgeOlderThan(ctx, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if purged < 1 {
			t.Fatalf("expected ≥1 purged, got %d", purged)
		}
		// Inspect should now return ErrNotFound.
		_, err = s.Inspect(ctx, claimed.Task.ID)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("after purge inspect: want ErrNotFound, got %v", err)
		}
	})

	t.Run(name+"/HighConcurrencyClaim", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ctx := context.Background()

		const N = 200
		for i := 0; i < N; i++ {
			_, _ = s.Enqueue(ctx, Task{Type: "x"})
		}

		var got atomic.Int64
		var wg sync.WaitGroup
		for w := 0; w < 16; w++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				wid := fmt.Sprintf("w%d", idx)
				for {
					r, err := s.Claim(ctx, "default", wid, 5*time.Second)
					if err != nil {
						return
					}
					if r == nil {
						return
					}
					_ = s.Complete(ctx, r.Task.ID, wid)
					got.Add(1)
				}
			}(w)
		}
		wg.Wait()
		if got.Load() != N {
			t.Fatalf("claims: got %d want %d", got.Load(), N)
		}
	})
}

// TestMemoryStoreConformance runs the shared suite against the
// in-memory backend. Other backends will have their own _test.go that
// constructs their store and calls runStoreConformance.
func TestMemoryStoreConformance(t *testing.T) {
	runStoreConformance(t, "memory", func(t *testing.T) Store {
		return NewMemoryStore(MemoryOpts{DedupWindow: time.Minute})
	})
}
