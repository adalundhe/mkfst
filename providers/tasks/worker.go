package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	mrand "math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// WorkerOpts configures NewWorker.
type WorkerOpts struct {
	// Store is the backend the worker pulls from. Required.
	Store Store

	// Queues are the queue names this worker subscribes to. Empty
	// defaults to []string{"default"}. Order matters: workers walk
	// queues round-robin and claim from the first one with work.
	Queues []string

	// Concurrency is the max number of in-flight tasks per worker.
	// 0 defaults to runtime.NumCPU * 4 — saturates on I/O-heavy
	// workloads without exploding goroutine count on CPU-heavy ones.
	Concurrency int

	// VisibilityTimeout is the per-claim lease. If a handler doesn't
	// Heartbeat or Complete/Fail within this window, the engine
	// reclaims the task and re-enqueues it. Default 30s — long
	// enough for most handlers to finish, short enough that crashed
	// workers don't strand work for long.
	VisibilityTimeout time.Duration

	// HeartbeatInterval is how often a long-running handler's
	// background heartbeat extends the lease. Should be < half of
	// VisibilityTimeout to absorb network jitter. Default
	// VisibilityTimeout / 3.
	HeartbeatInterval time.Duration

	// DefaultTimeout caps a handler attempt when Task.Timeout is
	// zero. Default 5m.
	DefaultTimeout time.Duration

	// DefaultMaxRetries is applied to tasks that don't set
	// MaxRetries. Default 5 — exponential backoff with full jitter
	// keeps the failed-at-2-then-recovered pattern correct.
	DefaultMaxRetries int

	// PollInterval is how often a worker re-checks empty queues. The
	// store's Claim may long-poll up to its own internal max; this
	// is the floor between consecutive Claim calls when the queue
	// keeps returning empty. Default 25ms — feels live without
	// hammering the backend.
	PollInterval time.Duration

	// MaintenanceInterval is the cadence for PromoteScheduled and
	// ReclaimExpired calls. Default 250ms.
	MaintenanceInterval time.Duration

	// Backoff returns the delay before the next attempt for a task
	// that failed on its `attempt`-th try (1-indexed). Defaults to
	// exponential backoff with full jitter capped at 30s.
	Backoff func(attempt int) time.Duration

	// OnError is invoked from internal goroutines for transient
	// errors that the worker handles (claim failures, store
	// unavailability, etc.). Optional; nil = silent. Same pattern as
	// docker.SyncEngine.OnError.
	OnError func(workerID string, op string, err error)

	// WorkerID overrides the auto-generated worker identifier.
	// Useful when a process needs stable IDs (debugging, metrics).
	WorkerID string

	// Telemetry, if non-nil, emits OTEL spans on each handler
	// invocation and increments OTEL counters/histograms for
	// claim/complete/fail/retry events. nil = no telemetry. Use
	// NewTelemetryFromGlobals() to wire up against the process-wide
	// OTEL providers.
	Telemetry *Telemetry
}

// NewWorker constructs a Worker bound to a Store. Workers are not
// goroutines themselves — they spawn goroutines when Run is called.
func NewWorker(opts WorkerOpts) (Worker, error) {
	if opts.Store == nil {
		return nil, errors.New("tasks.NewWorker: Store is required")
	}
	if len(opts.Queues) == 0 {
		opts.Queues = []string{"default"}
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 16
	}
	if opts.VisibilityTimeout <= 0 {
		opts.VisibilityTimeout = 30 * time.Second
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = opts.VisibilityTimeout / 3
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = 5 * time.Minute
	}
	if opts.DefaultMaxRetries == 0 {
		opts.DefaultMaxRetries = 5
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 25 * time.Millisecond
	}
	if opts.MaintenanceInterval <= 0 {
		opts.MaintenanceInterval = 250 * time.Millisecond
	}
	if opts.Backoff == nil {
		opts.Backoff = defaultBackoff
	}
	if opts.WorkerID == "" {
		opts.WorkerID = newWorkerID()
	}

	return &worker{
		opts:     opts,
		handlers: make(map[string]Handler),
	}, nil
}

// worker is the concrete implementation of the Worker interface.
type worker struct {
	opts WorkerOpts

	registerMu sync.Mutex
	handlers   map[string]Handler
	running    atomic.Bool

	// runtime state — populated when Run is called
	runCtx    context.Context
	runCancel context.CancelFunc
	wg        sync.WaitGroup

	// stats counters
	enqueued, claimed, completed, failed, retried, cancelled atomic.Uint64
	inflight                                                 atomic.Int64
}

func (w *worker) Register(typeName string, h Handler) error {
	if typeName == "" {
		return errors.New("tasks.Worker.Register: typeName must not be empty")
	}
	if h == nil {
		return errors.New("tasks.Worker.Register: handler must not be nil")
	}
	w.registerMu.Lock()
	defer w.registerMu.Unlock()
	if _, exists := w.handlers[typeName]; exists {
		return fmt.Errorf("tasks.Worker.Register: handler %q already registered", typeName)
	}
	w.handlers[typeName] = h
	return nil
}

func (w *worker) Run(ctx context.Context) error {
	if !w.running.CompareAndSwap(false, true) {
		return errors.New("tasks.Worker.Run: already running")
	}
	defer w.running.Store(false)

	w.runCtx, w.runCancel = context.WithCancel(ctx)
	defer w.runCancel()

	// Wire OTEL queue-depth gauge to the store. Cheap callback —
	// QueueStats does one query per queue, only fires on metric
	// scrape.
	w.opts.Telemetry.SetQueueDepthSource(func() map[string]int {
		out := make(map[string]int, len(w.opts.Queues))
		for _, q := range w.opts.Queues {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			stats, err := w.opts.Store.QueueStats(ctx, q)
			cancel()
			if err == nil {
				out[q] = stats.Pending
			}
		}
		return out
	})

	// Spawn maintenance goroutine. One per worker process, ticks all
	// subscribed queues. Cheap when there's nothing to do (zero-row
	// pass on each store call).
	w.wg.Add(1)
	go w.maintenanceLoop()

	// Spawn one claim goroutine per Concurrency slot. Each one
	// long-polls Claim, dispatches the handler, and loops. Bounded
	// by w.opts.Concurrency total active goroutines for execution.
	for i := 0; i < w.opts.Concurrency; i++ {
		w.wg.Add(1)
		go w.claimLoop(i)
	}

	w.wg.Wait()
	return nil
}

func (w *worker) Stop() {
	if w.runCancel != nil {
		w.runCancel()
	}
}

func (w *worker) Stats() Stats {
	depths := make(map[string]int, len(w.opts.Queues))
	var oldestPending time.Duration
	for _, q := range w.opts.Queues {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		stats, err := w.opts.Store.QueueStats(ctx, q)
		cancel()
		if err == nil {
			depths[q] = stats.Pending
			if stats.OldestPendingAge > oldestPending {
				oldestPending = stats.OldestPendingAge
			}
		}
	}
	return Stats{
		Enqueued:      w.enqueued.Load(),
		Claimed:       w.claimed.Load(),
		Completed:     w.completed.Load(),
		Failed:        w.failed.Load(),
		Retried:       w.retried.Load(),
		Cancelled:     w.cancelled.Load(),
		Inflight:      int(w.inflight.Load()),
		QueueDepths:   depths,
		OldestPending: oldestPending,
	}
}

// === claim loop ===

// claimLoop runs in one of Concurrency goroutines. Each iteration:
//  1. Walk subscribed queues round-robin, try to Claim from each.
//  2. On hit: dispatch the handler with timeout + heartbeat.
//  3. On miss across all queues: backoff to PollInterval and retry.
//  4. ctx-cancel exits the loop cleanly after current task finishes.
func (w *worker) claimLoop(slot int) {
	defer w.wg.Done()

	queueIdx := slot % len(w.opts.Queues)

	for {
		if w.runCtx.Err() != nil {
			return
		}

		// Try to claim from queues round-robin starting at queueIdx.
		var claimed *Record
		for tried := 0; tried < len(w.opts.Queues); tried++ {
			q := w.opts.Queues[(queueIdx+tried)%len(w.opts.Queues)]
			rec, err := w.opts.Store.Claim(w.runCtx, q, w.opts.WorkerID, w.opts.VisibilityTimeout)
			if err != nil {
				if errors.Is(err, ErrQueueClosed) || errors.Is(err, context.Canceled) {
					return
				}
				w.reportErr("claim", err)
				continue
			}
			if rec != nil {
				claimed = rec
				queueIdx = (queueIdx + tried + 1) % len(w.opts.Queues) // advance past this queue
				break
			}
		}

		if claimed == nil {
			// All queues empty. Backoff and retry.
			select {
			case <-w.runCtx.Done():
				return
			case <-time.After(w.opts.PollInterval):
			}
			continue
		}

		w.claimed.Add(1)
		w.dispatchOne(*claimed)
	}
}

// dispatchOne runs one task: registers a heartbeat sub-goroutine that
// extends the visibility lease, invokes the handler with a per-attempt
// context (Timeout + Deadline), and finalizes via Complete or Fail
// based on the handler's outcome.
func (w *worker) dispatchOne(rec Record) {
	w.inflight.Add(1)
	defer w.inflight.Add(-1)

	w.opts.Telemetry.recordClaim(w.runCtx, rec.Task)

	handler, ok := w.lookupHandler(rec.Task.Type)
	if !ok {
		// Unknown task type — fail immediately, no retry. The handler
		// must be Register'd; treating this as a permanent failure
		// surfaces the misconfiguration loudly. If the Fail call
		// itself errors we still mark stats and surface via OnError
		// so the underlying store outage doesn't go silent.
		failMsg := fmt.Sprintf("no handler registered for type %q", rec.Task.Type)
		if err := w.opts.Store.Fail(w.runCtx, rec.Task.ID, w.opts.WorkerID, failMsg, nil); err != nil &&
			!errors.Is(err, ErrNotOwner) && !errors.Is(err, ErrAlreadyTerminal) {
			w.reportErr("fail-unknown-handler", err)
		}
		w.failed.Add(1)
		w.opts.Telemetry.recordFail(w.runCtx, rec.Task, 0, false)
		return
	}

	// Per-attempt context: Task.Timeout (or DefaultTimeout) capped by
	// the runtime context. Task.Deadline further caps if set.
	timeout := rec.Task.Timeout
	if timeout <= 0 {
		timeout = w.opts.DefaultTimeout
	}
	attemptCtx, cancelAttempt := context.WithTimeout(w.runCtx, timeout)
	defer cancelAttempt()

	if !rec.Task.Deadline.IsZero() {
		attemptCtx, cancelAttempt = context.WithDeadline(attemptCtx, rec.Task.Deadline)
		defer cancelAttempt()
	}

	// Heartbeat sub-goroutine: extends the visibility lease at
	// HeartbeatInterval. Exits when attemptCtx is cancelled (which
	// happens automatically when dispatchOne returns).
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(w.opts.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-attemptCtx.Done():
				return
			case <-ticker.C:
				if err := w.opts.Store.Heartbeat(w.runCtx, rec.Task.ID, w.opts.WorkerID, w.opts.VisibilityTimeout); err != nil {
					if errors.Is(err, ErrNotOwner) {
						// Lost our claim. Cancel the attempt — the
						// task has been re-enqueued; whatever this
						// handler produces is now stale.
						cancelAttempt()
						return
					}
					if !errors.Is(err, context.Canceled) {
						w.reportErr("heartbeat", err)
					}
				}
			}
		}
	}()

	// Open OTEL span over the handler invocation. Span context
	// chains through task.Tags so distributed traces survive the
	// enqueue→claim handoff.
	spanCtx, span := w.opts.Telemetry.startSpan(attemptCtx, "tasks.execute", rec.Task)
	if span.IsRecording() {
		defer span.End()
	}

	// Run the handler. Recover from panics and treat as errors so a
	// buggy handler doesn't kill the worker.
	startedAt := time.Now()
	var handlerErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				handlerErr = fmt.Errorf("handler panicked: %v", r)
			}
		}()
		handlerErr = handler(spanCtx, rec.Task)
	}()
	executeDuration := time.Since(startedAt)

	// Cancel the attempt ctx so the heartbeat goroutine exits before
	// we move on to the finalize call.
	cancelAttempt()
	<-heartbeatDone

	// Finalize.
	if handlerErr == nil {
		if err := w.opts.Store.Complete(w.runCtx, rec.Task.ID, w.opts.WorkerID); err != nil {
			if !errors.Is(err, ErrNotOwner) && !errors.Is(err, ErrAlreadyTerminal) {
				w.reportErr("complete", err)
			}
		}
		w.completed.Add(1)
		w.opts.Telemetry.recordComplete(w.runCtx, rec.Task, executeDuration)
		return
	}

	// Handler failed. Decide retry vs terminal.
	// Task.MaxRetries is *int so explicit-zero (one-shot) is
	// distinguishable from unset (use worker default). The retry
	// budget is N+1 attempts total: rec.Attempts was incremented at
	// Claim, so the comparison `rec.Attempts <= maxRetries` retries
	// while we haven't exhausted the additional-attempt allowance.
	maxRetries := w.opts.DefaultMaxRetries
	if rec.Task.MaxRetries != nil {
		maxRetries = *rec.Task.MaxRetries
	}
	var nextAttempt *time.Time
	if rec.Attempts <= maxRetries {
		t := time.Now().Add(w.opts.Backoff(rec.Attempts))
		nextAttempt = &t
		w.retried.Add(1)
	} else {
		w.failed.Add(1)
	}
	if err := w.opts.Store.Fail(w.runCtx, rec.Task.ID, w.opts.WorkerID, handlerErr.Error(), nextAttempt); err != nil {
		if !errors.Is(err, ErrNotOwner) && !errors.Is(err, ErrAlreadyTerminal) {
			w.reportErr("fail", err)
		}
	}
	w.opts.Telemetry.recordFail(w.runCtx, rec.Task, executeDuration, nextAttempt != nil)
}

// lookupHandler returns the registered handler for typeName, or
// (nil, false) if missing. Read-locked; Register holds the write lock.
func (w *worker) lookupHandler(typeName string) (Handler, bool) {
	w.registerMu.Lock()
	defer w.registerMu.Unlock()
	h, ok := w.handlers[typeName]
	return h, ok
}

// === maintenance loop ===

// maintenanceLoop ticks PromoteScheduled + ReclaimExpired across all
// subscribed queues. One goroutine per worker. Cheap when there's no
// work to do.
func (w *worker) maintenanceLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.opts.MaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.runCtx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			for _, q := range w.opts.Queues {
				if _, err := w.opts.Store.PromoteScheduled(w.runCtx, q, now); err != nil {
					if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrQueueClosed) {
						w.reportErr("promote-scheduled", err)
					}
				}
				if _, err := w.opts.Store.ReclaimExpired(w.runCtx, q, now); err != nil {
					if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrQueueClosed) {
						w.reportErr("reclaim-expired", err)
					}
				}
			}
		}
	}
}

func (w *worker) reportErr(op string, err error) {
	if w.opts.OnError != nil && err != nil {
		w.opts.OnError(w.opts.WorkerID, op, err)
	}
}

// === backoff ===

// defaultBackoff returns an exponential backoff with full jitter,
// capped at 30 seconds. attempt is 1-indexed (1 = first failure).
//
// Math: sleep = jitter(min(cap, base * 2^(attempt-1)))
//   attempt=1 → sleep ∈ [0, 1s)
//   attempt=2 → sleep ∈ [0, 2s)
//   attempt=3 → sleep ∈ [0, 4s)
//   attempt=4 → sleep ∈ [0, 8s)
//   attempt=5 → sleep ∈ [0, 16s)
//   attempt=6+ → sleep ∈ [0, 30s)
//
// Full jitter (vs. equal/decorrelated jitter) was the AWS-blog
// recommendation for years and remains the right default — minimizes
// thundering-herd retries when many tasks fail simultaneously (e.g.,
// a downstream service blip).
func defaultBackoff(attempt int) time.Duration {
	const base = time.Second
	const cap = 30 * time.Second
	if attempt < 1 {
		attempt = 1
	}
	exp := math.Min(float64(cap), float64(base)*math.Pow(2, float64(attempt-1)))
	return time.Duration(mrand.Int63n(int64(exp)))
}

// === worker ID generation ===

func newWorkerID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to time-based — extremely unlikely path.
		now := time.Now().UnixNano()
		for i := 7; i >= 0; i-- {
			b[i] = byte(now)
			now >>= 8
		}
	}
	return "w-" + hex.EncodeToString(b[:])
}
