package tasks

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// MemoryOpts configures NewMemoryStore.
type MemoryOpts struct {
	// DedupWindow is how long a UniqueKey blocks duplicate enqueues
	// after the first acceptance. 0 disables dedup. The window is a
	// sliding TTL — each successful enqueue with a key resets the
	// TTL. Default 5 minutes — long enough to catch double-clicks
	// and recurring-job overlap, short enough that a process churning
	// through millions of unique keys doesn't accumulate them
	// indefinitely.
	DedupWindow time.Duration
}

// NewMemoryStore returns a Store that holds everything in memory.
// Suitable for single-process deployments and the conformance suite
// — durability ends with the process. For persistence, see
// NewSQLStore (durable) or NewRedisStore (distributed).
//
// No goroutines spawned: the engine drives PromoteScheduled and
// ReclaimExpired on its own ticker, so the store is purely passive.
func NewMemoryStore(opts MemoryOpts) Store {
	return &memoryStore{
		opts:    opts,
		queues:  make(map[string]*memQueue),
		records: make(map[string]*Record),
		dedup:   make(map[string]time.Time),
		now:     time.Now,
	}
}

// memoryStore is a fully in-memory Store implementation. Single
// global mutex — keeps the implementation simple and the conformance
// guarantees obvious. Per-queue sharding could be added later if
// contention shows up; it doesn't for typical workloads (claim/
// complete are O(log n) under the lock).
type memoryStore struct {
	opts MemoryOpts

	mu      sync.Mutex
	queues  map[string]*memQueue   // queue name → state
	records map[string]*Record     // task ID → record (single source of truth)
	dedup   map[string]time.Time   // UniqueKey → expiresAt

	closed bool
	now    func() time.Time // injectable for tests
}

// memQueue is the per-queue index into memoryStore.records. Three
// heaps: ready-to-run pending, scheduled-for-future, currently-running
// (the latter ordered by visibility deadline so ReclaimExpired is O(k)
// instead of O(running)).
type memQueue struct {
	pending   *pendingHeap   // priority desc, then earliest enqueued
	scheduled *timeHeap      // earliest DelayUntil first
	running   *timeHeap      // earliest visibility deadline first
}

func newMemQueue() *memQueue {
	pending := &pendingHeap{}
	scheduled := &timeHeap{}
	running := &timeHeap{}
	heap.Init(pending)
	heap.Init(scheduled)
	heap.Init(running)
	return &memQueue{
		pending:   pending,
		scheduled: scheduled,
		running:   running,
	}
}

// queueLocked returns (and lazily creates) the named queue. Caller
// holds the store mutex.
func (s *memoryStore) queueLocked(name string) *memQueue {
	if name == "" {
		name = "default"
	}
	q, ok := s.queues[name]
	if !ok {
		q = newMemQueue()
		s.queues[name] = q
	}
	return q
}

// === Store impl ===

func (s *memoryStore) Enqueue(ctx context.Context, t Task) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Record{}, ErrQueueClosed
	}

	// Dedup check: if UniqueKey is set and an entry exists in the
	// dedup map whose TTL hasn't expired, reject as ErrUniqueViolation.
	if t.UniqueKey != "" && s.opts.DedupWindow > 0 {
		now := s.now()
		s.evictDedupExpiredLocked(now)
		if _, exists := s.dedup[t.UniqueKey]; exists {
			return Record{}, ErrUniqueViolation
		}
		s.dedup[t.UniqueKey] = now.Add(s.opts.DedupWindow)
	}

	if t.ID == "" {
		t.ID = newID()
	}
	if t.Queue == "" {
		t.Queue = "default"
	}

	now := s.now()
	rec := &Record{
		Task:       t,
		EnqueuedAt: now,
	}

	// Decide initial state from DelayUntil.
	if !t.DelayUntil.IsZero() && t.DelayUntil.After(now) {
		rec.State = StateScheduled
		s.records[t.ID] = rec
		q := s.queueLocked(t.Queue)
		heap.Push(q.scheduled, &timeHeapItem{id: t.ID, when: t.DelayUntil})
	} else {
		rec.State = StatePending
		s.records[t.ID] = rec
		q := s.queueLocked(t.Queue)
		heap.Push(q.pending, &pendingHeapItem{id: t.ID, priority: t.Priority, enqueuedAt: now})
	}

	return *rec, nil
}

func (s *memoryStore) ScheduleAt(ctx context.Context, when time.Time, t Task) (Record, error) {
	t.DelayUntil = when
	return s.Enqueue(ctx, t)
}

func (s *memoryStore) Claim(ctx context.Context, queue, workerID string, visibility time.Duration) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrQueueClosed
	}
	if queue == "" {
		queue = "default"
	}
	q, ok := s.queues[queue]
	if !ok || q.pending.Len() == 0 {
		return nil, nil
	}

	// Lazy-delete loop: skip pending items whose record was cancelled
	// or otherwise moved out of pending.
	for q.pending.Len() > 0 {
		top := heap.Pop(q.pending).(*pendingHeapItem)
		rec, exists := s.records[top.id]
		if !exists || rec.State != StatePending {
			continue // stale heap entry
		}
		// Claim it.
		now := s.now()
		rec.State = StateRunning
		rec.Attempts++
		rec.OwnerWorker = workerID
		rec.VisibilityAt = now.Add(visibility)
		if rec.StartedAt.IsZero() {
			rec.StartedAt = now
		}
		heap.Push(q.running, &timeHeapItem{id: rec.Task.ID, when: rec.VisibilityAt})
		out := *rec
		return &out, nil
	}
	return nil, nil
}

func (s *memoryStore) Heartbeat(ctx context.Context, id, workerID string, extend time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return ErrNotFound
	}
	if rec.OwnerWorker != workerID || rec.State != StateRunning {
		return ErrNotOwner
	}
	rec.VisibilityAt = s.now().Add(extend)
	q := s.queueLocked(rec.Task.Queue)
	// Push a new running entry; the old one remains and is ignored as
	// stale on the next ReclaimExpired pass (the heap-item points to
	// the old VisibilityAt, the record points to the new one — Reclaim
	// re-checks the record before acting).
	heap.Push(q.running, &timeHeapItem{id: id, when: rec.VisibilityAt})
	return nil
}

func (s *memoryStore) Complete(ctx context.Context, id, workerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return ErrNotFound
	}
	if rec.State != StateRunning {
		return ErrAlreadyTerminal
	}
	if rec.OwnerWorker != workerID {
		return ErrNotOwner
	}
	rec.State = StateCompleted
	rec.CompletedAt = s.now()
	rec.OwnerWorker = ""
	return nil
}

func (s *memoryStore) Fail(ctx context.Context, id, workerID string, errMsg string, nextAttemptAt *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return ErrNotFound
	}
	if rec.State != StateRunning {
		return ErrAlreadyTerminal
	}
	if rec.OwnerWorker != workerID {
		return ErrNotOwner
	}
	rec.LastError = errMsg
	rec.OwnerWorker = ""

	if nextAttemptAt != nil && (rec.Task.Deadline.IsZero() || nextAttemptAt.Before(rec.Task.Deadline)) {
		// Retry: schedule for re-enqueue at nextAttemptAt.
		rec.State = StateScheduled
		rec.NextAttempt = *nextAttemptAt
		q := s.queueLocked(rec.Task.Queue)
		heap.Push(q.scheduled, &timeHeapItem{id: id, when: *nextAttemptAt})
	} else {
		// Terminal failure.
		rec.State = StateFailed
		rec.CompletedAt = s.now()
	}
	return nil
}

func (s *memoryStore) Cancel(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return ErrNotFound
	}
	switch rec.State {
	case StatePending, StateScheduled:
		rec.State = StateCancelled
		rec.CompletedAt = s.now()
		// Heap entries for the old state become stale and are
		// skipped on next pop / promote / reclaim.
		return nil
	case StateRunning:
		// Cancel-while-running is a hint, not an interrupt. Force
		// no-retry on next Fail (overrides whatever the user set);
		// the handler still runs to completion.
		zero := 0
		rec.Task.MaxRetries = &zero
		return nil
	default:
		return ErrAlreadyTerminal
	}
}

func (s *memoryStore) Inspect(ctx context.Context, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	return *rec, nil
}

func (s *memoryStore) QueueStats(ctx context.Context, queue string) (QueueStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if queue == "" {
		queue = "default"
	}
	stats := QueueStats{}

	// Walk records to count by state — heap counts are not authoritative
	// because of lazy deletes.
	var oldestPending time.Time
	for _, rec := range s.records {
		if rec.Task.Queue != queue {
			continue
		}
		switch rec.State {
		case StatePending:
			stats.Pending++
			if oldestPending.IsZero() || rec.EnqueuedAt.Before(oldestPending) {
				oldestPending = rec.EnqueuedAt
			}
		case StateScheduled:
			stats.Scheduled++
		case StateRunning:
			stats.Running++
		case StateFailed:
			stats.Failed++
		}
	}
	if !oldestPending.IsZero() {
		stats.OldestPendingAge = s.now().Sub(oldestPending)
	}
	return stats, nil
}

func (s *memoryStore) PromoteScheduled(ctx context.Context, queue string, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if queue == "" {
		queue = "default"
	}
	q, ok := s.queues[queue]
	if !ok {
		return 0, nil
	}
	promoted := 0
	for q.scheduled.Len() > 0 {
		top := (*q.scheduled)[0]
		if top.when.After(now) {
			break
		}
		heap.Pop(q.scheduled)
		rec, exists := s.records[top.id]
		if !exists || rec.State != StateScheduled {
			continue // stale (cancelled, claimed somehow, etc.)
		}
		rec.State = StatePending
		heap.Push(q.pending, &pendingHeapItem{
			id:         rec.Task.ID,
			priority:   rec.Task.Priority,
			enqueuedAt: now,
		})
		promoted++
	}
	return promoted, nil
}

func (s *memoryStore) ReclaimExpired(ctx context.Context, queue string, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if queue == "" {
		queue = "default"
	}
	q, ok := s.queues[queue]
	if !ok {
		return 0, nil
	}
	reclaimed := 0
	for q.running.Len() > 0 {
		top := (*q.running)[0]
		if top.when.After(now) {
			break
		}
		heap.Pop(q.running)
		rec, exists := s.records[top.id]
		if !exists {
			continue
		}
		// Only act if this heap entry matches the current visibility
		// deadline — if Heartbeat extended it, a newer entry is
		// further down the heap with the new deadline; this stale one
		// gets skipped.
		if rec.State != StateRunning || !rec.VisibilityAt.Equal(top.when) {
			continue
		}
		// Worker has gone silent. Reset to pending, clear ownership.
		rec.State = StatePending
		rec.OwnerWorker = ""
		heap.Push(q.pending, &pendingHeapItem{
			id:         rec.Task.ID,
			priority:   rec.Task.Priority,
			enqueuedAt: now,
		})
		reclaimed++
	}
	return reclaimed, nil
}

func (s *memoryStore) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	purged := 0
	for id, rec := range s.records {
		switch rec.State {
		case StateCompleted, StateFailed, StateCancelled:
			if !rec.CompletedAt.IsZero() && rec.CompletedAt.Before(cutoff) {
				delete(s.records, id)
				purged++
			}
		}
	}
	return purged, nil
}

func (s *memoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.queues = nil
	s.records = nil
	s.dedup = nil
	return nil
}

// evictDedupExpiredLocked walks the dedup map and drops expired
// entries. Caller holds s.mu. Cheap because the map size is bounded
// by enqueue rate × DedupWindow — a few thousand entries even under
// high load.
func (s *memoryStore) evictDedupExpiredLocked(now time.Time) {
	for k, expiresAt := range s.dedup {
		if expiresAt.Before(now) {
			delete(s.dedup, k)
		}
	}
}

// === heap implementations ===

// pendingHeapItem orders by (priority desc, enqueuedAt asc, id asc).
type pendingHeapItem struct {
	id         string
	priority   int8
	enqueuedAt time.Time
}

type pendingHeap []*pendingHeapItem

func (h pendingHeap) Len() int { return len(h) }
func (h pendingHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if a.priority != b.priority {
		return a.priority > b.priority
	}
	if !a.enqueuedAt.Equal(b.enqueuedAt) {
		return a.enqueuedAt.Before(b.enqueuedAt)
	}
	return a.id < b.id
}
func (h pendingHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pendingHeap) Push(x any)   { *h = append(*h, x.(*pendingHeapItem)) }
func (h *pendingHeap) Pop() any {
	n := len(*h)
	x := (*h)[n-1]
	*h = (*h)[:n-1]
	return x
}

// timeHeapItem is a min-heap entry by `when`. Used both for scheduled
// (when = DelayUntil) and running (when = visibility deadline).
type timeHeapItem struct {
	id   string
	when time.Time
}

type timeHeap []*timeHeapItem

func (h timeHeap) Len() int { return len(h) }
func (h timeHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if !a.when.Equal(b.when) {
		return a.when.Before(b.when)
	}
	return a.id < b.id
}
func (h timeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *timeHeap) Push(x any)   { *h = append(*h, x.(*timeHeapItem)) }
func (h *timeHeap) Pop() any {
	n := len(*h)
	x := (*h)[n-1]
	*h = (*h)[:n-1]
	return x
}

// === ID generation ===

// newID returns a sortable, ~unique identifier. Format: 8-byte
// big-endian unix-nano timestamp + 8 random bytes, hex-encoded.
// Lexically sorts in time order (helps SQL backends paginate by ID).
// Uniqueness collision probability is ~2^-64 per nanosecond — for
// any realistic workload, effectively zero.
func newID() string {
	var b [16]byte
	now := uint64(time.Now().UnixNano())
	for i := 7; i >= 0; i-- {
		b[i] = byte(now)
		now >>= 8
	}
	if _, err := rand.Read(b[8:]); err != nil {
		// crypto/rand failure means the system is broken in a deeper
		// way; fall back to time-based for forward progress.
		now = uint64(time.Now().UnixNano())
		for i := 15; i >= 8; i-- {
			b[i] = byte(now)
			now >>= 8
		}
	}
	return hex.EncodeToString(b[:])
}

