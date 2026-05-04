package workflows

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"mkfst/providers/cache"
	"mkfst/providers/tasks"
)

// === Hand-rolled fakes ===
//
// Mockery generates mocks as `*_test.go` files which are package-
// private — the workflows package can't import the tasks- or cache-
// generated mocks. So we hand-roll fakes that satisfy the same
// interfaces and record calls. Same testing pattern as
// providers/docker/network/policy_enforcement_test.go.

// fakeScheduler captures every Enqueue call and lets a test inject
// an error per call.
type fakeScheduler struct {
	mu          sync.Mutex
	enqueued    []tasks.Task
	enqueueErr  error // returned for every Enqueue
	enqueueErrs []error // queued errors, one per call (used before enqueueErr)
}

func (f *fakeScheduler) Enqueue(ctx context.Context, t tasks.Task) (tasks.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, t)
	if len(f.enqueueErrs) > 0 {
		err := f.enqueueErrs[0]
		f.enqueueErrs = f.enqueueErrs[1:]
		if err != nil {
			return tasks.Record{}, err
		}
	} else if f.enqueueErr != nil {
		return tasks.Record{}, f.enqueueErr
	}
	return tasks.Record{Task: t, State: tasks.StatePending}, nil
}

func (f *fakeScheduler) EnqueueIn(ctx context.Context, d time.Duration, t tasks.Task) (tasks.Record, error) {
	return f.Enqueue(ctx, t)
}

func (f *fakeScheduler) EnqueueAt(ctx context.Context, when time.Time, t tasks.Task) (tasks.Record, error) {
	return f.Enqueue(ctx, t)
}

func (f *fakeScheduler) Cancel(ctx context.Context, id string) error  { return nil }
func (f *fakeScheduler) Inspect(ctx context.Context, id string) (tasks.Record, error) {
	return tasks.Record{}, tasks.ErrNotFound
}

// fakeWorker captures Register calls so tests can drive the bridge
// handler directly.
type fakeWorker struct {
	mu        sync.Mutex
	handlers  map[string]tasks.Handler
	registerErr error
}

func newFakeWorker() *fakeWorker { return &fakeWorker{handlers: map[string]tasks.Handler{}} }

func (f *fakeWorker) Register(typeName string, h tasks.Handler) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registerErr != nil {
		return f.registerErr
	}
	f.handlers[typeName] = h
	return nil
}

func (f *fakeWorker) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (f *fakeWorker) Stop()                         {}
func (f *fakeWorker) Stats() tasks.Stats             { return tasks.Stats{} }

func (f *fakeWorker) handler(typeName string) tasks.Handler {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handlers[typeName]
}

// brokenCache wraps an inner cache and lets tests force errors on
// specific operations (Set, Get, DeletePrefix).
type brokenCache struct {
	inner cache.Cache

	// Set: if non-empty key matches, return setErr.
	setErrKey string
	setErr    error

	// Get: if non-empty key matches, return getErr.
	getErrKey string
	getErr    error

	// deletePrefixCalls records the prefixes passed to DeletePrefix.
	deletePrefixCalls []string
	deletePrefixErr   error
}

func (b *brokenCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if b.getErrKey != "" && key == b.getErrKey {
		return nil, false, b.getErr
	}
	return b.inner.Get(ctx, key)
}
func (b *brokenCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if b.setErrKey != "" && strings.HasPrefix(key, b.setErrKey) {
		return b.setErr
	}
	return b.inner.Set(ctx, key, val, ttl)
}
func (b *brokenCache) Delete(ctx context.Context, key string) error { return b.inner.Delete(ctx, key) }
func (b *brokenCache) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	b.deletePrefixCalls = append(b.deletePrefixCalls, prefix)
	if b.deletePrefixErr != nil {
		return 0, b.deletePrefixErr
	}
	return b.inner.DeletePrefix(ctx, prefix)
}
func (b *brokenCache) Close() error { return b.inner.Close() }

// === helpers ===

func mockEngine(t *testing.T, sched tasks.Scheduler, worker tasks.Worker, c cache.Cache) *Engine {
	t.Helper()
	if c == nil {
		c = cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})
	}
	e, err := NewEngine(EngineOpts{Scheduler: sched, Worker: worker, Outputs: c})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

// === Submit tests ===

func TestSubmit_WritesMetaAndEnqueuesRoots(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})
	e := mockEngine(t, sched, w, c)

	def := New("wf")
	a := def.MustAdd("a", OfType("alpha"), Priority(7))
	def.MustAdd("b", OfType("beta"), Priority(3))
	def.MustAdd("c", OfType("alpha"), DependsOn(a))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	id, err := e.Submit(context.Background(), "wf", []byte("seed"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Two roots → two enqueues. Order is alphabetical (def.roots()).
	if len(sched.enqueued) != 2 {
		t.Fatalf("enqueue count: %d (%+v)", len(sched.enqueued), sched.enqueued)
	}
	if sched.enqueued[0].Type != "alpha" || sched.enqueued[1].Type != "beta" {
		t.Fatalf("enqueue order/types wrong: %+v", sched.enqueued)
	}
	for _, task := range sched.enqueued {
		if task.Tags[tagInstanceID] != id {
			t.Fatalf("instance tag missing: %+v", task.Tags)
		}
		if task.Tags[tagNode] == "" {
			t.Fatalf("node tag missing: %+v", task.Tags)
		}
		if task.UniqueKey == "" {
			t.Fatal("UniqueKey not set")
		}
		if task.MaxRetries == nil || *task.MaxRetries != 0 {
			t.Fatalf("MaxRetries default not 0: %+v", task.MaxRetries)
		}
		if task.Queue != "default" {
			t.Fatalf("Queue: %q", task.Queue)
		}
	}

	// Priorities are propagated.
	for _, task := range sched.enqueued {
		switch task.Tags[tagNode] {
		case "a":
			if task.Priority != 7 {
				t.Fatalf("a priority: %d", task.Priority)
			}
		case "b":
			if task.Priority != 3 {
				t.Fatalf("b priority: %d", task.Priority)
			}
		}
	}

	// Submit input survives in cache as the synthetic root parent.
	val, ok, err := c.Get(context.Background(), nodeOutputKey("wf:", id, ""))
	if err != nil || !ok || string(val) != "seed" {
		t.Fatalf("synthetic root input: ok=%v val=%q err=%v", ok, val, err)
	}

	// Instance meta was written.
	info, err := e.Inspect(context.Background(), id)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.State != InstanceRunning {
		t.Fatalf("expected running, got %s", info.State)
	}
}

func TestSubmit_NoInputSkipsRootParentWrite(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})
	e := mockEngine(t, sched, w, c)

	def := New("wf")
	def.MustAdd("a", OfType("t"))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	id, err := e.Submit(context.Background(), "wf", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	_, ok, _ := c.Get(context.Background(), nodeOutputKey("wf:", id, ""))
	if ok {
		t.Fatal("expected no synthetic root input write when input == nil")
	}
}

func TestSubmit_SchedulerErrorMarksInstanceFailed(t *testing.T) {
	sched := &fakeScheduler{enqueueErr: errors.New("queue down")}
	w := newFakeWorker()
	e := mockEngine(t, sched, w, nil)

	def := New("wf")
	def.MustAdd("a", OfType("t"))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	id, err := e.Submit(context.Background(), "wf", nil)
	if err == nil {
		t.Fatal("expected scheduler error to bubble")
	}
	info, ierr := e.Inspect(context.Background(), id)
	if ierr != nil {
		t.Fatalf("Inspect: %v", ierr)
	}
	if info.State != InstanceFailed {
		t.Fatalf("expected failed instance, got %s", info.State)
	}
}

func TestSubmit_UniqueViolationIsTolerated(t *testing.T) {
	sched := &fakeScheduler{enqueueErr: tasks.ErrUniqueViolation}
	w := newFakeWorker()
	e := mockEngine(t, sched, w, nil)
	def := New("wf")
	def.MustAdd("a", OfType("t"))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := e.Submit(context.Background(), "wf", nil); err != nil {
		t.Fatalf("ErrUniqueViolation should be swallowed; got %v", err)
	}
}

func TestSubmit_RejectsUnknownDefinition(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	e := mockEngine(t, sched, w, nil)
	if _, err := e.Submit(context.Background(), "missing", nil); err == nil {
		t.Fatal("expected error for unknown definition")
	}
	if len(sched.enqueued) != 0 {
		t.Fatal("scheduler should not be touched for unknown definition")
	}
}

func TestSubmit_MetaWriteFailureBubbles(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	c := &brokenCache{
		inner:     cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024}),
		setErrKey: "wf:inst:",
		setErr:    errors.New("cache disk full"),
	}
	e := mockEngine(t, sched, w, c)
	def := New("wf")
	def.MustAdd("a", OfType("t"))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := e.Submit(context.Background(), "wf", nil); err == nil {
		t.Fatal("expected meta-write failure")
	}
	if len(sched.enqueued) != 0 {
		t.Fatal("scheduler should not be reached when meta write fails")
	}
}

// === Closed engine ===

func TestSubmit_AfterCloseReturnsError(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	e := mockEngine(t, sched, w, nil)
	def := New("wf")
	def.MustAdd("a", OfType("t"))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := e.Submit(context.Background(), "wf", nil); err == nil {
		t.Fatal("expected error after Close")
	}
}

func TestRegister_AfterCloseReturnsError(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	e := mockEngine(t, sched, w, nil)
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	def := New("wf")
	def.MustAdd("a", OfType("t"))
	if err := e.Register(def); err == nil {
		t.Fatal("expected error after Close")
	}
}

// === RegisterHandler ===

func TestRegisterHandler_RegistersOnceWithWorker(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	e := mockEngine(t, sched, w, nil)

	h := func(ctx context.Context, p map[string][]byte) ([]byte, error) { return nil, nil }
	if err := e.RegisterHandler("alpha", h); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}
	if w.handler("alpha") == nil {
		t.Fatal("worker.Register was not invoked")
	}
	// Re-registering the same taskType must error rather than silently
	// rebinding.
	if err := e.RegisterHandler("alpha", h); err == nil {
		t.Fatal("expected duplicate registration error")
	}
}

func TestRegisterHandler_RejectsEmptyType(t *testing.T) {
	e := mockEngine(t, &fakeScheduler{}, newFakeWorker(), nil)
	h := func(ctx context.Context, p map[string][]byte) ([]byte, error) { return nil, nil }
	if err := e.RegisterHandler("", h); err == nil {
		t.Fatal("expected error for empty type")
	}
}

func TestRegisterHandler_RejectsNilHandler(t *testing.T) {
	e := mockEngine(t, &fakeScheduler{}, newFakeWorker(), nil)
	if err := e.RegisterHandler("alpha", nil); err == nil {
		t.Fatal("expected error for nil handler")
	}
}

func TestRegisterHandler_PropagatesWorkerError(t *testing.T) {
	w := newFakeWorker()
	w.registerErr = errors.New("worker rejected")
	e := mockEngine(t, &fakeScheduler{}, w, nil)
	h := func(ctx context.Context, p map[string][]byte) ([]byte, error) { return nil, nil }
	if err := e.RegisterHandler("alpha", h); err == nil {
		t.Fatal("expected worker error to bubble")
	}
}

// === Bridge handler ===

// driveBridge fires the bridge handler the engine registered for
// taskType, simulating what the worker would do when claiming a task.
func driveBridge(t *testing.T, w *fakeWorker, taskType string, task tasks.Task) error {
	t.Helper()
	h := w.handler(taskType)
	if h == nil {
		t.Fatalf("no bridge handler registered for %q", taskType)
	}
	return h(context.Background(), task)
}

func TestBridge_MissingTagsErrors(t *testing.T) {
	w := newFakeWorker()
	e := mockEngine(t, &fakeScheduler{}, w, nil)
	if err := e.RegisterHandler("alpha", func(ctx context.Context, p map[string][]byte) ([]byte, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}

	// No tags at all → error.
	err := driveBridge(t, w, "alpha", tasks.Task{ID: "t1", Type: "alpha"})
	if err == nil || !strings.Contains(err.Error(), "missing wf.instance") {
		t.Fatalf("expected missing-tag error, got %v", err)
	}
}

func TestBridge_LoadsParentsAndStoresOutput(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})
	e := mockEngine(t, sched, w, c)

	def := New("wf")
	a := def.MustAdd("a", OfType("alpha"))
	def.MustAdd("b", OfType("beta"), DependsOn(a))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var seenParents map[string][]byte
	bHandler := func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		seenParents = parents
		return []byte("b-out"), nil
	}
	aHandler := func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		return []byte("a-out"), nil
	}
	if err := e.RegisterHandler("alpha", aHandler); err != nil {
		t.Fatalf("RegisterHandler alpha: %v", err)
	}
	if err := e.RegisterHandler("beta", bHandler); err != nil {
		t.Fatalf("RegisterHandler beta: %v", err)
	}

	id, err := e.Submit(context.Background(), "wf", []byte("seed"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Drive a's bridge first.
	zero := 0
	aTask := tasks.Task{
		ID:         "task-a",
		Type:       "alpha",
		MaxRetries: &zero,
		Tags:       map[string]string{tagInstanceID: id, tagNode: "a"},
	}
	if err := driveBridge(t, w, "alpha", aTask); err != nil {
		t.Fatalf("bridge a: %v", err)
	}

	// a's success should have written output and enqueued b.
	if v, ok, _ := c.Get(context.Background(), nodeOutputKey("wf:", id, "a")); !ok || string(v) != "a-out" {
		t.Fatalf("a output not stored: %v / %v", v, ok)
	}
	if len(sched.enqueued) != 2 { // 1 root + 1 advanced child
		t.Fatalf("enqueued count: %d (%+v)", len(sched.enqueued), sched.enqueued)
	}
	bEnqueued := sched.enqueued[1]
	if bEnqueued.Type != "beta" || bEnqueued.Tags[tagNode] != "b" {
		t.Fatalf("expected b to be enqueued: %+v", bEnqueued)
	}

	// Now drive b's bridge.
	bTask := tasks.Task{
		ID:         "task-b",
		Type:       "beta",
		MaxRetries: &zero,
		Tags:       map[string]string{tagInstanceID: id, tagNode: "b"},
	}
	if err := driveBridge(t, w, "beta", bTask); err != nil {
		t.Fatalf("bridge b: %v", err)
	}
	if got := string(seenParents["a"]); got != "a-out" {
		t.Fatalf("b should have seen parent a-out, got %q", got)
	}

	// Instance should now be Completed.
	info, err := e.Inspect(context.Background(), id)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.State != InstanceCompleted {
		t.Fatalf("expected completed, got %s", info.State)
	}
}

func TestBridge_CancelledInstanceSkipsHandler(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})
	e := mockEngine(t, sched, w, c)

	def := New("wf")
	def.MustAdd("a", OfType("alpha"))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	called := false
	if err := e.RegisterHandler("alpha", func(ctx context.Context, p map[string][]byte) ([]byte, error) {
		called = true
		return []byte("ignored"), nil
	}); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}

	id, err := e.Submit(context.Background(), "wf", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := e.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	zero := 0
	task := tasks.Task{
		ID: "t1", Type: "alpha", MaxRetries: &zero,
		Tags: map[string]string{tagInstanceID: id, tagNode: "a"},
	}
	if err := driveBridge(t, w, "alpha", task); err != nil {
		t.Fatalf("bridge: %v", err)
	}
	if called {
		t.Fatal("user handler ran despite cancellation")
	}
}

func TestBridge_FailHaltOnFinalAttemptMarksInstanceFailed(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})
	e := mockEngine(t, sched, w, c)

	def := New("wf")
	def.MustAdd("a", OfType("alpha")) // FailHaltWorkflow default
	def.MustAdd("b", OfType("beta"))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := e.RegisterHandler("alpha", func(ctx context.Context, p map[string][]byte) ([]byte, error) {
		return nil, errors.New("kaboom")
	}); err != nil {
		t.Fatalf("alpha: %v", err)
	}
	if err := e.RegisterHandler("beta", func(ctx context.Context, p map[string][]byte) ([]byte, error) {
		return []byte("ok"), nil
	}); err != nil {
		t.Fatalf("beta: %v", err)
	}

	id, err := e.Submit(context.Background(), "wf", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	zero := 0
	aTask := tasks.Task{
		ID: "task-a", Type: "alpha", MaxRetries: &zero,
		Tags: map[string]string{tagInstanceID: id, tagNode: "a"},
	}
	err = driveBridge(t, w, "alpha", aTask)
	if err == nil {
		t.Fatal("bridge should bubble handler error")
	}

	info, ierr := e.Inspect(context.Background(), id)
	if ierr != nil {
		t.Fatalf("Inspect: %v", ierr)
	}
	if info.Nodes["a"].State != NodeFailed {
		t.Fatalf("a state: %s", info.Nodes["a"].State)
	}
	// FailHaltWorkflow: sibling b must be Skipped, instance Failed.
	if info.Nodes["b"].State != NodeSkipped {
		t.Fatalf("b should be skipped under HaltWorkflow, got %s", info.Nodes["b"].State)
	}
	if info.State != InstanceFailed {
		t.Fatalf("instance should be failed, got %s", info.State)
	}
}

func TestBridge_FailContinueAdvancesChildren(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})
	e := mockEngine(t, sched, w, c)

	def := New("wf")
	a := def.MustAdd("a", OfType("alpha"), OnFail(FailContinue))
	def.MustAdd("b", OfType("beta"), DependsOn(a))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := e.RegisterHandler("alpha", func(ctx context.Context, p map[string][]byte) ([]byte, error) {
		return nil, errors.New("ignore me")
	}); err != nil {
		t.Fatalf("alpha: %v", err)
	}
	if err := e.RegisterHandler("beta", func(ctx context.Context, p map[string][]byte) ([]byte, error) {
		return []byte("b-out"), nil
	}); err != nil {
		t.Fatalf("beta: %v", err)
	}

	id, err := e.Submit(context.Background(), "wf", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	zero := 0
	aTask := tasks.Task{
		ID: "task-a", Type: "alpha", MaxRetries: &zero,
		Tags: map[string]string{tagInstanceID: id, tagNode: "a"},
	}
	if err := driveBridge(t, w, "alpha", aTask); err == nil {
		t.Fatal("expected error bubble")
	}

	// Under FailContinue, b should be enqueued (root + b after advance = 2).
	if len(sched.enqueued) != 2 {
		t.Fatalf("enqueued count: %d (%+v)", len(sched.enqueued), sched.enqueued)
	}
	if sched.enqueued[1].Tags[tagNode] != "b" {
		t.Fatalf("expected b enqueued under FailContinue, got %+v", sched.enqueued[1].Tags)
	}
}

func TestBridge_StaleInstanceMetaErrors(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})
	e := mockEngine(t, sched, w, c)

	def := New("wf")
	def.MustAdd("a", OfType("alpha"))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := e.RegisterHandler("alpha", func(ctx context.Context, p map[string][]byte) ([]byte, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}

	// Drive bridge with an instance ID that doesn't exist in cache.
	zero := 0
	task := tasks.Task{
		ID: "t1", Type: "alpha", MaxRetries: &zero,
		Tags: map[string]string{tagInstanceID: "phantom", tagNode: "a"},
	}
	err := driveBridge(t, w, "alpha", task)
	if err == nil {
		t.Fatal("expected error for missing instance meta")
	}
}

// === Cancel / Cleanup / Inspect ===

func TestCancel_UpdatesMeta(t *testing.T) {
	sched := &fakeScheduler{}
	w := newFakeWorker()
	c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})
	e := mockEngine(t, sched, w, c)

	def := New("wf")
	def.MustAdd("a", OfType("t"))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	id, err := e.Submit(context.Background(), "wf", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if err := e.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	info, _ := e.Inspect(context.Background(), id)
	if info.State != InstanceCancelled {
		t.Fatalf("expected cancelled, got %s", info.State)
	}
	// Cancelling again is a no-op.
	if err := e.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel idempotent: %v", err)
	}
}

func TestCancel_UnknownInstance(t *testing.T) {
	e := mockEngine(t, &fakeScheduler{}, newFakeWorker(), nil)
	err := e.Cancel(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCleanup_CallsDeletePrefix(t *testing.T) {
	c := &brokenCache{inner: cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})}
	e := mockEngine(t, &fakeScheduler{}, newFakeWorker(), c)
	def := New("wf")
	def.MustAdd("a", OfType("t"))
	if err := e.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	id, err := e.Submit(context.Background(), "wf", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if err := e.Cleanup(context.Background(), id); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(c.deletePrefixCalls) != 1 {
		t.Fatalf("expected 1 DeletePrefix call, got %d", len(c.deletePrefixCalls))
	}
	want := instancePrefix("wf:", id)
	if c.deletePrefixCalls[0] != want {
		t.Fatalf("DeletePrefix called with %q, want %q", c.deletePrefixCalls[0], want)
	}
}

func TestInspect_NotFound(t *testing.T) {
	e := mockEngine(t, &fakeScheduler{}, newFakeWorker(), nil)
	if _, err := e.Inspect(context.Background(), "phantom"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
