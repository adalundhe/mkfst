package tasks

import (
	"context"
	"time"
)

// Store is the backend abstraction. Every implementation —
// in-memory, Redis/Valkey, SQL — satisfies this interface and passes
// the same conformance tests.
//
// Atomicity guarantees expected from any implementation:
//
//   - Claim is atomic: a task can be claimed by exactly one worker.
//     Concurrent Claims for the same queue MUST return distinct tasks
//     (or one nil + one task, never the same task to two callers).
//   - Heartbeat / Complete / Fail are atomic and idempotent w.r.t.
//     ownership: only the current owner can advance the state.
//     A late call from a worker whose visibility lease already
//     expired returns ErrNotOwner.
//   - Scheduled-promotion and visibility-reclamation are atomic
//     batch operations: PromoteScheduled and ReclaimExpired must
//     never partially mutate the store.
//
// Performance expectations:
//
//   - Claim should be sub-millisecond on a healthy backend (Lua
//     script for Redis, SELECT FOR UPDATE SKIP LOCKED for SQL,
//     mutex+heap pop for memory).
//   - PromoteScheduled and ReclaimExpired are bulk and run on a
//     low-frequency tick (default 250ms). They should be cheap when
//     there's nothing to do (zero-row pass).
type Store interface {
	// Enqueue inserts a task. If t.ID is empty, the store assigns one
	// (ULID, sortable). If the task has DelayUntil in the future,
	// the store inserts it as scheduled rather than pending. UniqueKey
	// dedup is enforced server-side; ErrUniqueViolation on collision.
	Enqueue(ctx context.Context, t Task) (Record, error)

	// ScheduleAt is sugar for Enqueue with t.DelayUntil = when.
	ScheduleAt(ctx context.Context, when time.Time, t Task) (Record, error)

	// Claim atomically pops a pending task from the named queue and
	// transitions it to running with owner = workerID. The returned
	// task is guaranteed to be invisible to other claimers until
	// `visibility` elapses — at which point ReclaimExpired moves it
	// back to pending.
	//
	// Returns (nil, nil) if the queue is empty. Implementations may
	// long-poll up to a backend-specific maximum before returning
	// nil — callers should treat (nil, nil) as a signal to retry,
	// not an error.
	Claim(ctx context.Context, queue, workerID string, visibility time.Duration) (*Record, error)

	// Heartbeat extends the visibility lease for a running task.
	// Returns ErrNotOwner if workerID isn't the recorded owner (most
	// commonly: the visibility lease already expired and the task
	// was reclaimed by ReclaimExpired). Handlers running long jobs
	// should call this periodically.
	Heartbeat(ctx context.Context, id, workerID string, extend time.Duration) error

	// Complete transitions the task to completed and removes it from
	// the active set. Returns ErrNotOwner if the worker is no longer
	// the recorded owner.
	Complete(ctx context.Context, id, workerID string) error

	// Fail transitions the task to failed (or schedules a retry if
	// nextAttemptAt is non-nil). errMsg is recorded as Record.LastError.
	// Returns ErrNotOwner if the worker is no longer the recorded
	// owner.
	Fail(ctx context.Context, id, workerID string, errMsg string, nextAttemptAt *time.Time) error

	// Cancel marks a pending or scheduled task as cancelled. Running
	// tasks remain running; the entrypoint contract is "Cancel is a
	// hint to skip future execution, not an interrupt." Idempotent.
	Cancel(ctx context.Context, id string) error

	// Inspect returns the current Record for a task by ID.
	Inspect(ctx context.Context, id string) (Record, error)

	// QueueStats returns a snapshot of the named queue.
	QueueStats(ctx context.Context, queue string) (QueueStats, error)

	// PromoteScheduled moves due-scheduled tasks (DelayUntil ≤ now)
	// from the scheduled set into the pending set, atomically.
	// Returns the number promoted. Called by the engine ticker.
	PromoteScheduled(ctx context.Context, queue string, now time.Time) (int, error)

	// ReclaimExpired moves running tasks whose visibility lease has
	// expired (visibility ≤ now) back to pending so another worker
	// can claim them. Returns the number reclaimed. Called by the
	// engine ticker.
	ReclaimExpired(ctx context.Context, queue string, now time.Time) (int, error)

	// PurgeOlderThan deletes terminal-state tasks whose CompletedAt
	// is older than `cutoff`. Returns the number purged. Optional
	// hygiene op for long-lived deployments.
	PurgeOlderThan(ctx context.Context, cutoff time.Time) (int, error)

	// Close releases backend resources (DB connections, Redis pool,
	// reaper goroutines if any). Idempotent.
	Close() error
}
