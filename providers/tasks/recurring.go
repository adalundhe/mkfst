package tasks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"mkfst/providers/tasks/cron"
)

// RecurringOpts configures NewRecurringScheduler.
type RecurringOpts struct {
	// Store is where enqueues land. Required. The same Store used by
	// the worker pool — recurring is just a fancy enqueue path.
	Store Store

	// Tick is how often the recurring scheduler scans its registered
	// jobs to see what's due. Default 1 second. Set lower for sub-
	// second-resolution intervals, higher to reduce CPU on a busy
	// process.
	Tick time.Duration

	// OnError is invoked for transient errors (enqueue failures, cron
	// expression parse errors at runtime). Optional; nil = silent.
	OnError func(jobName string, err error)
}

// RecurringScheduler ticks once per Tick interval and enqueues every
// recurring job whose next-fire-time has passed since the last tick.
// Multiple processes may run their own RecurringScheduler against the
// same Store: dedup-on-enqueue (via UniqueKey="cron:NAME:UNIX") keeps
// at most one task enqueued per (job, fire-time) bucket.
//
// No leader election needed — the dedup primitive enforces
// exactly-once enqueue per logical fire under all failure modes
// (clock skew, GC pause, network partition between schedulers).
type RecurringScheduler struct {
	opts RecurringOpts

	mu   sync.Mutex
	jobs map[string]*recurringJob

	running atomic.Bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

type recurringJob struct {
	name     string
	expr     string         // cron expression, or "" for interval-only
	interval time.Duration  // 0 if cron-driven
	template Task
	nextFire time.Time      // when the next fire is due
	lastFire time.Time      // most recent successful enqueue
}

// NewRecurringScheduler returns a scheduler that enqueues recurring
// jobs into the given Store. Call Run(ctx) to start the tick loop;
// it blocks until ctx is cancelled.
func NewRecurringScheduler(opts RecurringOpts) (*RecurringScheduler, error) {
	if opts.Store == nil {
		return nil, errors.New("tasks.NewRecurringScheduler: Store is required")
	}
	if opts.Tick <= 0 {
		opts.Tick = time.Second
	}
	return &RecurringScheduler{
		opts:   opts,
		jobs:   make(map[string]*recurringJob),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}, nil
}

// Cron registers a recurring job with a cron expression. name is a
// unique identifier — re-registering the same name replaces the
// previous schedule. Use the gronx-vendored cron at
// providers/tasks/cron for syntax (5/6/7-field, Quartz L/W/#, tags).
func (rs *RecurringScheduler) Cron(name, expr string, taskTemplate Task) error {
	if name == "" {
		return errors.New("tasks.Cron: name required")
	}
	if !cron.New().IsValid(expr) {
		return fmt.Errorf("tasks.Cron: invalid expression %q", expr)
	}
	next, err := cron.NextTickAfter(expr, time.Now(), false)
	if err != nil {
		return fmt.Errorf("tasks.Cron: compute next tick: %w", err)
	}
	rs.mu.Lock()
	rs.jobs[name] = &recurringJob{
		name:     name,
		expr:     expr,
		template: taskTemplate,
		nextFire: next,
	}
	rs.mu.Unlock()
	return nil
}

// Every registers a recurring job that fires every `interval`. Sugar
// for callers who don't need cron-expression flexibility.
//
// Fire times are aligned to interval boundaries from the unix epoch
// — so two schedulers in different processes both compute the same
// `nextFire` for the same job. This is what makes the cross-process
// dedup-on-enqueue actually collapse to one task per logical fire,
// regardless of which scheduler ticks first.
func (rs *RecurringScheduler) Every(name string, interval time.Duration, taskTemplate Task) error {
	if name == "" {
		return errors.New("tasks.Every: name required")
	}
	if interval <= 0 {
		return errors.New("tasks.Every: interval must be positive")
	}
	rs.mu.Lock()
	rs.jobs[name] = &recurringJob{
		name:     name,
		interval: interval,
		template: taskTemplate,
		nextFire: nextAlignedFire(interval, time.Now()),
	}
	rs.mu.Unlock()
	return nil
}

// nextAlignedFire returns the next interval boundary strictly after
// `after`, anchored at the unix epoch so independent processes
// converge on the same buckets.
//
// Example: interval=50ms, after=12:00:00.073 → returns 12:00:00.100
// (next 50-ms boundary).
func nextAlignedFire(interval time.Duration, after time.Time) time.Time {
	intervalNs := interval.Nanoseconds()
	if intervalNs <= 0 {
		return after
	}
	afterNs := after.UnixNano()
	// Strictly greater: bucket = floor(after/interval) + 1.
	nextBucket := (afterNs / intervalNs) + 1
	return time.Unix(0, nextBucket*intervalNs).UTC()
}

// RemoveRecurring unregisters a previously-added recurring job.
// In-flight enqueues from the last tick aren't cancelled.
func (rs *RecurringScheduler) RemoveRecurring(name string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if _, ok := rs.jobs[name]; !ok {
		return ErrNotFound
	}
	delete(rs.jobs, name)
	return nil
}

// ListRecurring returns a snapshot of every registered recurring job.
func (rs *RecurringScheduler) ListRecurring() []RecurringInfo {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]RecurringInfo, 0, len(rs.jobs))
	for _, j := range rs.jobs {
		out = append(out, RecurringInfo{
			Name:       j.name,
			Expression: j.expr,
			Interval:   j.interval,
			NextFire:   j.nextFire,
			LastFire:   j.lastFire,
		})
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out
}

// Run starts the tick loop. Blocks until ctx is cancelled. Safe to
// call exactly once; returns an error if invoked twice.
func (rs *RecurringScheduler) Run(ctx context.Context) error {
	if !rs.running.CompareAndSwap(false, true) {
		return errors.New("tasks.RecurringScheduler.Run: already running")
	}
	defer close(rs.doneCh)

	ticker := time.NewTicker(rs.opts.Tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-rs.stopCh:
			return nil
		case now := <-ticker.C:
			rs.tick(ctx, now)
		}
	}
}

// Stop signals the tick loop to exit. Idempotent. After Stop, Run
// returns nil promptly.
func (rs *RecurringScheduler) Stop() {
	select {
	case <-rs.stopCh:
		// already closed
	default:
		close(rs.stopCh)
	}
}

// tick is one pass through the registered jobs: for each job whose
// nextFire ≤ now, build a Task from the template (with UniqueKey
// derived from name + nextFire bucket so multi-process schedulers
// dedup), enqueue it, advance nextFire.
func (rs *RecurringScheduler) tick(ctx context.Context, now time.Time) {
	// Snapshot under lock so we don't hold mu across enqueues.
	rs.mu.Lock()
	due := make([]*recurringJob, 0, len(rs.jobs))
	for _, j := range rs.jobs {
		if !j.nextFire.IsZero() && (j.nextFire.Before(now) || j.nextFire.Equal(now)) {
			due = append(due, j)
		}
	}
	rs.mu.Unlock()

	for _, j := range due {
		fireAt := j.nextFire
		task := j.template
		// Force a fresh ID so it doesn't collide across fires.
		task.ID = ""
		// UniqueKey ensures multiple schedulers firing the same job
		// at the same logical bucket all collapse to one enqueue.
		// Format: cron:<name>:<unix-nano-of-fire-time>.
		task.UniqueKey = "cron:" + j.name + ":" + strconv.FormatInt(fireAt.UnixNano(), 10)

		_, err := rs.opts.Store.Enqueue(ctx, task)
		if err != nil && !errors.Is(err, ErrUniqueViolation) {
			// Real failure (not a benign dedup hit); surface and
			// retry next tick.
			if rs.opts.OnError != nil {
				rs.opts.OnError(j.name, fmt.Errorf("enqueue: %w", err))
			}
			continue
		}

		// Advance schedule. Compute next fire time AFTER the one we
		// just fired so we don't catch up multiple periods if the
		// tick is delayed.
		next, advanceErr := rs.computeNext(j, fireAt)
		if advanceErr != nil {
			if rs.opts.OnError != nil {
				rs.opts.OnError(j.name, fmt.Errorf("compute next: %w", advanceErr))
			}
			continue
		}
		rs.mu.Lock()
		if existing, ok := rs.jobs[j.name]; ok {
			existing.nextFire = next
			existing.lastFire = fireAt
		}
		rs.mu.Unlock()
	}
}

// computeNext returns the next-fire-time for j strictly after
// `after`. Interval-driven jobs use epoch-aligned buckets (see
// nextAlignedFire) so peer schedulers all advance to the same next
// bucket. Cron-driven jobs use the parser, which is deterministic
// given the same `after` value.
func (rs *RecurringScheduler) computeNext(j *recurringJob, after time.Time) (time.Time, error) {
	if j.interval > 0 {
		next := nextAlignedFire(j.interval, after)
		// If the loop got behind (long pause, slow tick), skip ahead
		// to "next bucket strictly after now" rather than firing
		// repeatedly to catch up.
		now := time.Now()
		if next.Before(now) {
			next = nextAlignedFire(j.interval, now)
		}
		return next, nil
	}
	return cron.NextTickAfter(j.expr, after, false)
}

// === assertion: RecurringScheduler implements Recurring ===

var _ Recurring = (*recurringSchedulerAdapter)(nil)

// recurringSchedulerAdapter wraps RecurringScheduler to satisfy the
// Recurring interface (the interface uses no-error-returning
// signatures for Cron/Every; the adapter swallows nil-cases and
// returns errors for malformed inputs only).
type recurringSchedulerAdapter struct{ rs *RecurringScheduler }

// AsRecurring exposes a RecurringScheduler under the Recurring
// interface, matching the public Scheduler/Worker pattern.
func (rs *RecurringScheduler) AsRecurring() Recurring {
	return &recurringSchedulerAdapter{rs: rs}
}

func (a *recurringSchedulerAdapter) Cron(name, expr string, t Task) error {
	return a.rs.Cron(name, expr, t)
}
func (a *recurringSchedulerAdapter) Every(name string, interval time.Duration, t Task) error {
	return a.rs.Every(name, interval, t)
}
func (a *recurringSchedulerAdapter) RemoveRecurring(name string) error {
	return a.rs.RemoveRecurring(name)
}
func (a *recurringSchedulerAdapter) ListRecurring() []RecurringInfo {
	return a.rs.ListRecurring()
}
