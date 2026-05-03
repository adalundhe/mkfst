// Package tasks is mkfst's pluggable background-job system.
//
// One API surface, three backends:
//
//   - In-memory (providers/tasks.NewMemoryStore) — single-process, no
//     persistence, ideal for tests and single-binary deployments.
//   - Redis / Valkey (providers/tasks.NewRedisStore) — distributed,
//     atomic Lua-script claims, fast.
//   - SQL: Postgres / MySQL / SQLite (providers/tasks.NewSQLStore) —
//     durable, uses mkfst's existing db.Connection so callers reuse
//     their app database. Postgres adds LISTEN/NOTIFY for low-latency
//     wakeups; MySQL/SQLite poll.
//
// Every backend satisfies the same Store interface and passes the same
// conformance test suite. Switching backends is a one-line change.
//
// Use cases:
//   - Schedule recurring jobs via Cron expressions (uses the vendored
//     parser at providers/tasks/cron — Quartz L/W/# supported).
//   - Fan out to a worker pool that runs hundreds of containers in
//     parallel via providers/docker.
//   - Generic async work: emails, file processing, periodic
//     reconciliation loops, etc.
//
// Correctness model:
//   - At-least-once delivery by default. Handlers must be idempotent.
//     Combine with Task.UniqueKey for dedup-on-enqueue.
//   - Visibility timeouts: if a worker dies mid-task, another worker
//     re-claims the task after the timeout. Handlers should heartbeat
//     for long jobs so the timeout doesn't fire under them.
//   - Conflict-free recurring schedules: dedup-on-enqueue (no leader
//     election needed), so multiple processes can run the scheduler
//     ticker simultaneously without double-firing.
package tasks

import (
	"context"
	"errors"
	"time"
)

// === public types ===

// State enumerates the lifecycle states a task progresses through.
type State string

const (
	// StatePending: in a queue, ready to be claimed.
	StatePending State = "pending"
	// StateScheduled: waiting until a future time before becoming
	// claimable. Promoted to StatePending when DelayUntil ≤ now.
	StateScheduled State = "scheduled"
	// StateRunning: claimed by a worker; visibility timeout active.
	StateRunning State = "running"
	// StateCompleted: handler returned nil; task is done.
	StateCompleted State = "completed"
	// StateFailed: handler returned an error and either MaxRetries was
	// exhausted or Deadline was passed; final state.
	StateFailed State = "failed"
	// StateCancelled: explicitly cancelled before execution.
	StateCancelled State = "cancelled"
)

// Task is a unit of work submitted to the system. Encoding of Payload
// is the caller's responsibility — JSON, protobuf, msgpack, anything.
//
// ID is server-assigned (ULID, sortable). Callers usually leave it
// empty and read it back from the returned record. Setting ID
// explicitly is allowed but rare; the store will reject duplicates
// unless the duplicate is a no-op via UniqueKey dedup.
type Task struct {
	ID       string
	Type     string        // handler name; must be Register'd on the worker
	Payload  []byte        // arbitrary bytes
	Queue    string        // empty defaults to "default"
	Priority int8          // higher first; ties broken by enqueue order
	// MaxRetries is the number of retries permitted after the initial
	// attempt. Pointer so we can distinguish "field unset, use the
	// worker's DefaultMaxRetries" from "explicitly zero, no retry."
	//
	//   nil               → fall back to WorkerOpts.DefaultMaxRetries
	//   tasks.Retries(0)  → one-shot (initial attempt only, no retry)
	//   tasks.Retries(N)  → up to N+1 total attempts
	MaxRetries *int
	Timeout    time.Duration // per-attempt; 0 inherits worker default
	Deadline   time.Time     // hard wall-clock; zero = no deadline
	DelayUntil time.Time     // earliest run; zero = immediate
	UniqueKey  string        // dedup hint within the configured window
	Tags       map[string]string
}

// Retries returns a pointer to n. Sugar for setting Task.MaxRetries
// — Go doesn't let you take the address of an int literal, so this
// keeps the call-site terse:
//
//	scheduler.Enqueue(ctx, tasks.Task{
//	    Type:       "foo",
//	    MaxRetries: tasks.Retries(3),
//	})
func Retries(n int) *int { return &n }

// Record is what a Store returns when asked about a task. Includes the
// Task itself plus the bookkeeping the system tracks: state, attempts,
// timestamps, last error.
type Record struct {
	Task         Task
	State        State
	Attempts     int
	EnqueuedAt   time.Time
	StartedAt    time.Time // zero until first claim
	CompletedAt  time.Time // zero until terminal state
	LastError    string    // most recent handler error
	NextAttempt  time.Time // for retries
	OwnerWorker  string    // worker ID currently holding the claim (if running)
	VisibilityAt time.Time // when the current claim's visibility lease expires
}

// Handler executes a task. Returning nil = success (StateCompleted).
// Returning a non-nil error triggers retry up to Task.MaxRetries with
// exponential backoff; once retries are exhausted the task transitions
// to StateFailed.
//
// The ctx passed to the handler is the per-attempt context — it's
// cancelled when Task.Timeout elapses, when the worker shuts down, or
// when the task is externally cancelled. Long-running handlers should
// respect ctx cancellation cooperatively.
type Handler func(ctx context.Context, task Task) error

// === errors ===

var (
	// ErrNotFound: no task with the given ID.
	ErrNotFound = errors.New("tasks: not found")
	// ErrNotOwner: the workerID claiming an op doesn't match the
	// recorded owner — either the visibility lease expired and the
	// task was reclaimed, or the worker has a stale ID.
	ErrNotOwner = errors.New("tasks: worker is not the current owner")
	// ErrAlreadyTerminal: tried to op on a task that's already in
	// completed/failed/cancelled state.
	ErrAlreadyTerminal = errors.New("tasks: task is in a terminal state")
	// ErrUniqueViolation: enqueue collided with an existing task whose
	// UniqueKey matches within the dedup window. Returned by Enqueue
	// when the dedup mode is strict; the caller can treat it as a
	// success (the original enqueue stands).
	ErrUniqueViolation = errors.New("tasks: unique key already enqueued in the dedup window")
	// ErrQueueClosed: scheduler/store has been Close()d.
	ErrQueueClosed = errors.New("tasks: queue is closed")
)

// === scheduler-facing API ===

// Scheduler is the API for submitting work. Wraps a Store.
type Scheduler interface {
	// Enqueue inserts a task into its queue with state = pending (or
	// scheduled if Task.DelayUntil is in the future). Returns the
	// canonical Record with the assigned ID.
	Enqueue(ctx context.Context, t Task) (Record, error)
	// EnqueueIn is a sugar for Enqueue with DelayUntil = now + delay.
	EnqueueIn(ctx context.Context, delay time.Duration, t Task) (Record, error)
	// EnqueueAt is a sugar for Enqueue with DelayUntil = when.
	EnqueueAt(ctx context.Context, when time.Time, t Task) (Record, error)
	// Cancel removes a pending or scheduled task. Running tasks are
	// not interrupted (the handler ctx is left to the worker's
	// cancellation policy); Cancel just marks them StateCancelled so
	// they won't retry.
	Cancel(ctx context.Context, id string) error
	// Inspect returns the current Record for a task. Returns
	// ErrNotFound if the task ID is unknown.
	Inspect(ctx context.Context, id string) (Record, error)
}

// === recurring jobs ===

// Recurring is the API for schedule-driven enqueueing. Implemented
// alongside Scheduler in the same engine; separated as an interface so
// users who don't need recurring can ignore it.
type Recurring interface {
	// Cron schedules taskTemplate to enqueue every time the cron
	// expression fires. name is a unique identifier — re-Register
	// overrides the previous schedule. Uses dedup-on-enqueue under
	// UniqueKey="cron:"+name+":"+nextFireUnix so multiple processes
	// running the scheduler don't double-fire.
	Cron(name, expr string, taskTemplate Task) error
	// Every is sugar for Cron with a pure interval — internally
	// converted to UniqueKey-based bucketing rather than a cron
	// expression.
	Every(name string, interval time.Duration, taskTemplate Task) error
	// RemoveRecurring stops a previously-registered recurring job.
	// In-flight enqueues from the last tick still execute.
	RemoveRecurring(name string) error
	// ListRecurring returns the names + schedules of every active
	// recurring job. Useful for admin UIs.
	ListRecurring() []RecurringInfo
}

// RecurringInfo describes one registered recurring job.
type RecurringInfo struct {
	Name       string
	Expression string
	Interval   time.Duration // 0 if cron-driven
	NextFire   time.Time
	LastFire   time.Time
}

// === worker-facing API ===

// Worker is the runtime that pulls tasks and dispatches them to
// registered handlers. Workers are created via NewWorker(...). Multiple
// workers (in the same or different processes) can share a single
// Store and they coordinate via the store's claim semantics.
type Worker interface {
	// Register binds a handler to a task type. Returns an error for
	// programmer mistakes — empty typeName, nil handler, or
	// double-registering the same type. Errors are fail-fast at
	// startup; the worker contract treats Register as call-once-per-
	// type at process init.
	Register(typeName string, h Handler) error
	// Run starts the worker pool. Blocks until ctx is cancelled.
	// Returning a non-nil error means startup failed; clean shutdown
	// returns nil.
	Run(ctx context.Context) error
	// Stop signals all goroutines to drain in-flight tasks and exit.
	// Equivalent to cancelling the ctx passed to Run. Idempotent.
	Stop()
	// Stats returns a snapshot of worker activity since startup.
	Stats() Stats
}

// Stats is a snapshot of worker activity.
type Stats struct {
	Enqueued     uint64
	Claimed      uint64
	Completed    uint64
	Failed       uint64
	Retried      uint64
	Cancelled    uint64
	Inflight     int           // currently-executing tasks across pool
	QueueDepths  map[string]int // pending count per queue
	OldestPending time.Duration  // age of the oldest pending task
}

// QueueStats is a per-queue summary returned by Store.QueueStats.
type QueueStats struct {
	Pending   int
	Scheduled int
	Running   int
	Failed    int
	OldestPendingAge time.Duration
}
