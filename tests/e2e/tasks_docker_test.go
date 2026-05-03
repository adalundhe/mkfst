//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dockerprov "mkfst/providers/docker"
	"mkfst/providers/tasks"
)

// TestTasksDrivingContainers is the cross-provider integration: a
// tasks worker pool consuming from an in-memory store, with each
// task running an alpine container via providers/docker. Proves the
// "hundreds of containers in the background" use case end-to-end.
//
// Spawns N = 30 tasks, each running `alpine echo`. Worker pool of 8
// dispatches them. Verifies all N completed, all containers cleaned
// up, no leaked goroutines.
func TestTasksDrivingContainers(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "alpine:3.19")

	store := tasks.NewMemoryStore(tasks.MemoryOpts{DedupWindow: time.Minute})
	worker, err := tasks.NewWorker(tasks.WorkerOpts{
		Store:               store,
		Concurrency:         8,
		PollInterval:        25 * time.Millisecond,
		MaintenanceInterval: 100 * time.Millisecond,
		VisibilityTimeout:   60 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	type runPayload struct {
		Image string
		Cmd   []string
		Echo  string
	}

	var executed atomic.Int32
	var mu sync.Mutex
	echoesSeen := map[string]bool{}

	_ = worker.Register("docker.run", func(ctx context.Context, task tasks.Task) error {
		var p runPayload
		if err := json.Unmarshal(task.Payload, &p); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}
		// Don't AutoRemove — runWaitAndCollect calls Logs after Wait,
		// and AutoRemove'd containers vanish before Logs can read.
		// The helper's withCleanupContainer handles teardown.
		_, stdout, _ := runWaitAndCollect(t, c, p.Image,
			dockerprov.Cmd(p.Cmd...),
		)
		if !strings.Contains(stdout, p.Echo) {
			return fmt.Errorf("container output missing echo %q: %q", p.Echo, stdout)
		}
		executed.Add(1)
		mu.Lock()
		echoesSeen[p.Echo] = true
		mu.Unlock()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneRun := make(chan error, 1)
	go func() { doneRun <- worker.Run(ctx) }()

	scheduler := tasks.NewScheduler(store)

	const N = 30
	for i := 0; i < N; i++ {
		payload, _ := json.Marshal(runPayload{
			Image: "alpine:3.19",
			Cmd:   []string{"echo", fmt.Sprintf("hello-from-task-%d", i)},
			Echo:  fmt.Sprintf("hello-from-task-%d", i),
		})
		if _, err := scheduler.Enqueue(ctx, tasks.Task{
			Type:    "docker.run",
			Payload: payload,
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Wait for completion. Bound generously — 30 alpine echos via
	// docker take a few seconds depending on daemon load.
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) && executed.Load() < N {
		time.Sleep(100 * time.Millisecond)
	}

	if executed.Load() != N {
		t.Fatalf("executed %d/%d", executed.Load(), N)
	}
	mu.Lock()
	if len(echoesSeen) != N {
		t.Fatalf("only %d unique echoes seen, expected %d", len(echoesSeen), N)
	}
	mu.Unlock()

	// Confirm worker stats reflect the run.
	stats := worker.Stats()
	if stats.Completed != N {
		t.Fatalf("worker stats Completed=%d, want %d", stats.Completed, N)
	}
	if stats.Failed != 0 || stats.Retried != 0 {
		t.Fatalf("unexpected failures/retries: %+v", stats)
	}

	cancel()
	if err := <-doneRun; err != nil {
		t.Fatalf("worker.Run: %v", err)
	}
}

// TestTasksRetryThenSucceed: handler fails twice, succeeds on third.
// Proves retry semantics work end-to-end with a real-ish scenario.
func TestTasksRetryThenSucceed(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "alpine:3.19")

	store := tasks.NewMemoryStore(tasks.MemoryOpts{})
	worker, _ := tasks.NewWorker(tasks.WorkerOpts{
		Store:               store,
		Concurrency:         1,
		PollInterval:        10 * time.Millisecond,
		MaintenanceInterval: 25 * time.Millisecond,
		Backoff:             func(int) time.Duration { return 10 * time.Millisecond },
	})

	var attempts atomic.Int32
	_ = worker.Register("flaky-docker", func(ctx context.Context, task tasks.Task) error {
		n := attempts.Add(1)
		if n < 3 {
			return fmt.Errorf("simulated failure attempt %d", n)
		}
		// Real container run on the third attempt.
		exit, _, _ := runWaitAndCollect(t, c, "alpine:3.19",
			dockerprov.Cmd("true"),
		)
		if exit != 0 {
			return fmt.Errorf("container exit %d", exit)
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.Run(ctx) }()

	scheduler := tasks.NewScheduler(store)
	rec, _ := scheduler.Enqueue(ctx, tasks.Task{
		Type:       "flaky-docker",
		MaxRetries: tasks.Retries(5),
	})

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := store.Inspect(ctx, rec.Task.ID)
		if got.State == tasks.StateCompleted {
			if attempts.Load() != 3 {
				t.Fatalf("attempts: got %d want 3", attempts.Load())
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	final, _ := store.Inspect(ctx, rec.Task.ID)
	t.Fatalf("never completed: state=%v attempts=%d err=%q", final.State, final.Attempts, final.LastError)
}

// TestTasksRecurringDockerCleanup demonstrates the recurring scheduler
// firing periodic maintenance work — a "garbage collector" task that
// just runs `echo cleanup-tick` in alpine on a 200ms interval.
func TestTasksRecurringDockerCleanup(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "alpine:3.19")

	store := tasks.NewMemoryStore(tasks.MemoryOpts{DedupWindow: 10 * time.Second})
	worker, _ := tasks.NewWorker(tasks.WorkerOpts{
		Store:        store,
		Concurrency:  4,
		PollInterval: 25 * time.Millisecond,
	})

	var fires atomic.Int32
	_ = worker.Register("cleanup-tick", func(ctx context.Context, task tasks.Task) error {
		exit, _, _ := runWaitAndCollect(t, c, "alpine:3.19",
			dockerprov.Cmd("echo", "cleanup-tick"),
		)
		if exit != 0 {
			return fmt.Errorf("cleanup container failed: exit %d", exit)
		}
		fires.Add(1)
		return nil
	})

	rs, _ := tasks.NewRecurringScheduler(tasks.RecurringOpts{
		Store: store,
		Tick:  50 * time.Millisecond,
	})
	if err := rs.Every("cleanup", 200*time.Millisecond, tasks.Task{Type: "cleanup-tick"}); err != nil {
		t.Fatalf("Every: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.Run(ctx) }()
	go func() { _ = rs.Run(ctx) }()

	// Run for ~1.5s — expect ~7 fires at 200ms intervals.
	time.Sleep(1500 * time.Millisecond)
	cancel()

	got := fires.Load()
	if got < 4 || got > 9 {
		t.Fatalf("expected 4..9 cleanup fires in 1.5s, got %d", got)
	}
}
