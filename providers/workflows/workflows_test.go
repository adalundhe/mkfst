package workflows

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mkfst/providers/cache"
	"mkfst/providers/tasks"
)

// === Definition / Validate tests ===

func TestDefinition_AddPanicsOnEmpty(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty node name")
		}
	}()
	New("wf").Add("")
}

func TestDefinition_AddPanicsOnDuplicate(t *testing.T) {
	d := New("wf")
	d.Add("a", OfType("t"))
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate")
		}
	}()
	d.Add("a", OfType("t"))
}

func TestDefinition_ValidateEmpty(t *testing.T) {
	if err := New("wf").Validate(); err == nil {
		t.Fatal("expected error on zero-node definition")
	}
}

func TestDefinition_ValidateMissingType(t *testing.T) {
	d := New("wf")
	d.Add("a")
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "OfType") {
		t.Fatalf("expected missing OfType error, got %v", err)
	}
}

func TestDefinition_ValidateUnknownParent(t *testing.T) {
	d := New("wf")
	d.Add("a", OfType("t"), DependsOnByName("ghost"))
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected unknown parent error, got %v", err)
	}
}

func TestDefinition_ValidateSelfLoop(t *testing.T) {
	d := New("wf")
	d.Add("a", OfType("t"), DependsOnByName("a"))
	if err := d.Validate(); err == nil {
		t.Fatal("expected self-loop error")
	}
}

func TestDefinition_ValidateCycle(t *testing.T) {
	d := New("wf")
	d.Add("a", OfType("t"), DependsOnByName("c"))
	d.Add("b", OfType("t"), DependsOnByName("a"))
	d.Add("c", OfType("t"), DependsOnByName("b"))
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestDefinition_ValidateOK(t *testing.T) {
	d := New("wf")
	a := d.Add("a", OfType("t"))
	b := d.Add("b", OfType("t"), DependsOn(a))
	d.Add("c", OfType("t"), DependsOn(a, b))
	if err := d.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDefinition_RootsAndChildren(t *testing.T) {
	d := New("wf")
	a := d.Add("a", OfType("t"))
	b := d.Add("b", OfType("t"))
	d.Add("c", OfType("t"), DependsOn(a, b))
	roots := d.roots()
	if len(roots) != 2 || roots[0] != "a" || roots[1] != "b" {
		t.Fatalf("unexpected roots: %v", roots)
	}
	kids := d.children("a")
	if len(kids) != 1 || kids[0] != "c" {
		t.Fatalf("unexpected children: %v", kids)
	}
}

func TestDefinition_Descendants(t *testing.T) {
	d := New("wf")
	a := d.Add("a", OfType("t"))
	b := d.Add("b", OfType("t"), DependsOn(a))
	c := d.Add("c", OfType("t"), DependsOn(b))
	d.Add("d", OfType("t"), DependsOn(c))
	desc := d.descendants("a")
	if len(desc) != 3 {
		t.Fatalf("expected 3 descendants, got %v", desc)
	}
}

// === engine end-to-end tests using memory store ===

func newTestEngine(t *testing.T) (*Engine, tasks.Worker, context.CancelFunc) {
	t.Helper()
	store := tasks.NewMemoryStore(tasks.MemoryOpts{})
	t.Cleanup(func() { _ = store.Close() })

	worker, err := tasks.NewWorker(tasks.WorkerOpts{
		Store:               store,
		Concurrency:         4,
		PollInterval:        2 * time.Millisecond,
		MaintenanceInterval: 5 * time.Millisecond,
		VisibilityTimeout:   5 * time.Second,
		HeartbeatInterval:   1 * time.Second,
		DefaultTimeout:      5 * time.Second,
		DefaultMaxRetries:   0,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	scheduler := tasks.NewScheduler(store)

	engine, err := NewEngine(EngineOpts{
		Scheduler: scheduler,
		Worker:    worker,
		Outputs:   cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 10 * 1024 * 1024}),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = worker.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return engine, worker, cancel
}

func waitForState(t *testing.T, engine *Engine, instanceID string, want InstanceState, deadline time.Duration) InstanceInfo {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		info, err := engine.Inspect(context.Background(), instanceID)
		if err == nil && info.State == want {
			return info
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, _ := engine.Inspect(context.Background(), instanceID)
	t.Fatalf("instance %s never reached %s — last state %s, nodes %+v", instanceID, want, info.State, info.Nodes)
	return InstanceInfo{}
}

func TestEngine_LinearChain(t *testing.T) {
	engine, _, _ := newTestEngine(t)

	def := New("linear")
	a := def.Add("a", OfType("step"))
	b := def.Add("b", OfType("step"), DependsOn(a))
	def.Add("c", OfType("step"), DependsOn(b))

	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var (
		mu     sync.Mutex
		fired  []string
		inputs []string
	)
	if err := engine.RegisterHandler("step", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		mu.Lock()
		// Identify which node this is by looking at parents shape:
		// — root has parents[""] = "go"
		// — b has parents["a"] = "a-out"
		// — c has parents["b"] = "b-out"
		switch {
		case len(parents) == 1 && parents[""] != nil:
			fired = append(fired, "a")
			inputs = append(inputs, string(parents[""]))
			mu.Unlock()
			return []byte("a-out"), nil
		case parents["a"] != nil:
			fired = append(fired, "b")
			inputs = append(inputs, string(parents["a"]))
			mu.Unlock()
			return []byte("b-out"), nil
		case parents["b"] != nil:
			fired = append(fired, "c")
			inputs = append(inputs, string(parents["b"]))
			mu.Unlock()
			return []byte("c-out"), nil
		}
		mu.Unlock()
		return nil, errors.New("unexpected parents")
	}); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}

	id, err := engine.Submit(context.Background(), "linear", []byte("go"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	info := waitForState(t, engine, id, InstanceCompleted, 5*time.Second)
	if info.Nodes["c"].State != NodeCompleted {
		t.Fatalf("c not completed: %+v", info.Nodes)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 3 {
		t.Fatalf("expected 3 fires, got %v", fired)
	}
	if inputs[0] != "go" || inputs[1] != "a-out" || inputs[2] != "b-out" {
		t.Fatalf("unexpected input chain: %v", inputs)
	}
}

func TestEngine_Diamond(t *testing.T) {
	engine, _, _ := newTestEngine(t)

	def := New("diamond")
	a := def.Add("a", OfType("step"))
	b := def.Add("b", OfType("step"), DependsOn(a))
	c := def.Add("c", OfType("step"), DependsOn(a))
	def.Add("d", OfType("step"), DependsOn(b, c))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var (
		mu        sync.Mutex
		dParents  map[string][]byte
		dFireCount int
	)

	err := engine.RegisterHandler("step", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		switch {
		case len(parents) == 1 && parents[""] != nil:
			return []byte("from-a"), nil
		case len(parents) == 1 && parents["a"] != nil:
			return []byte("via-" + string(parents["a"])), nil
		case len(parents) == 2 && parents["b"] != nil && parents["c"] != nil:
			mu.Lock()
			dFireCount++
			dParents = make(map[string][]byte, len(parents))
			for k, v := range parents {
				dParents[k] = append([]byte{}, v...)
			}
			mu.Unlock()
			return []byte("done"), nil
		}
		return nil, errors.New("unexpected parents")
	})
	if err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}

	id, err := engine.Submit(context.Background(), "diamond", []byte("seed"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, engine, id, InstanceCompleted, 5*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if dFireCount != 1 {
		t.Fatalf("d fired %d times, want 1", dFireCount)
	}
	if string(dParents["b"]) != "via-from-a" || string(dParents["c"]) != "via-from-a" {
		t.Fatalf("unexpected d parents: %+v", dParents)
	}
}

func TestEngine_FailHaltWorkflow(t *testing.T) {
	engine, _, _ := newTestEngine(t)
	def := New("halt")
	a := def.Add("a", OfType("ok"))
	b := def.Add("b", OfType("boom"), DependsOn(a))
	def.Add("c", OfType("ok"), DependsOn(b))
	def.Add("d", OfType("ok"), DependsOn(a)) // sibling branch
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var dFired atomic.Bool
	if err := engine.RegisterHandler("ok", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		// Detect "d" by it depending on a and a alone (parents["a"] set).
		if _, fromA := parents["a"]; fromA {
			// Could be b (boom handler), c (depends on b), or d.
			// We're handling "ok" so this is c or d. c won't run
			// because halted. So if we fire here it's d.
			dFired.Store(true)
		}
		return []byte("ok"), nil
	}); err != nil {
		t.Fatalf("RegisterHandler ok: %v", err)
	}
	if err := engine.RegisterHandler("boom", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		return nil, errors.New("kaboom")
	}); err != nil {
		t.Fatalf("RegisterHandler boom: %v", err)
	}

	id, err := engine.Submit(context.Background(), "halt", []byte("x"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	info := waitForState(t, engine, id, InstanceFailed, 5*time.Second)
	if info.Nodes["b"].State != NodeFailed {
		t.Fatalf("b not marked failed: %+v", info.Nodes["b"])
	}
	if info.Nodes["c"].State != NodeSkipped {
		t.Fatalf("c not skipped: %+v", info.Nodes["c"])
	}
}

func TestEngine_FailSkipDownstream(t *testing.T) {
	engine, _, _ := newTestEngine(t)
	def := New("skip")
	a := def.Add("a", OfType("ok"))
	b := def.Add("b", OfType("boom"), DependsOn(a), OnFail(FailSkipDownstream))
	def.Add("c", OfType("ok"), DependsOn(b))
	d := def.Add("d", OfType("ok"), DependsOn(a))
	def.Add("e", OfType("ok"), DependsOn(d))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var eFired atomic.Bool
	if err := engine.RegisterHandler("ok", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		// e is the only "ok" node downstream of d; mark when we see
		// parents["d"].
		if _, fromD := parents["d"]; fromD {
			eFired.Store(true)
		}
		return []byte("ok"), nil
	}); err != nil {
		t.Fatalf("RegisterHandler ok: %v", err)
	}
	if err := engine.RegisterHandler("boom", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		return nil, errors.New("kaboom")
	}); err != nil {
		t.Fatalf("RegisterHandler boom: %v", err)
	}

	id, err := engine.Submit(context.Background(), "skip", []byte("x"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	info := waitForState(t, engine, id, InstanceFailed, 5*time.Second)
	if info.Nodes["c"].State != NodeSkipped {
		t.Fatalf("c not skipped: %+v", info.Nodes["c"])
	}
	if info.Nodes["e"].State != NodeCompleted {
		t.Fatalf("e not completed in sibling branch: %+v", info.Nodes["e"])
	}
	if !eFired.Load() {
		t.Fatal("e handler never fired despite sibling branch surviving")
	}
}

func TestEngine_FailContinue(t *testing.T) {
	engine, _, _ := newTestEngine(t)
	def := New("continue")
	a := def.Add("a", OfType("ok"))
	b := def.Add("b", OfType("boom"), DependsOn(a), OnFail(FailContinue))
	def.Add("c", OfType("merge"), DependsOn(b))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := engine.RegisterHandler("ok", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		return []byte("a-out"), nil
	}); err != nil {
		t.Fatalf("RegisterHandler ok: %v", err)
	}
	if err := engine.RegisterHandler("boom", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		return nil, errors.New("kaboom")
	}); err != nil {
		t.Fatalf("RegisterHandler boom: %v", err)
	}

	var cInputLen int
	if err := engine.RegisterHandler("merge", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		cInputLen = len(parents["b"])
		return []byte("c-out"), nil
	}); err != nil {
		t.Fatalf("RegisterHandler merge: %v", err)
	}

	id, err := engine.Submit(context.Background(), "continue", []byte("seed"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Instance ends up Failed because b is NodeFailed even though c
	// ran via empty-output relay.
	info := waitForState(t, engine, id, InstanceFailed, 5*time.Second)
	if info.Nodes["c"].State != NodeCompleted {
		t.Fatalf("c not completed under FailContinue: %+v", info.Nodes["c"])
	}
	if cInputLen != 0 {
		t.Fatalf("expected empty parent payload from continued failure, got %d bytes", cInputLen)
	}
}

func TestEngine_CancelStopsAdvancement(t *testing.T) {
	engine, _, _ := newTestEngine(t)
	def := New("cancel")
	a := def.Add("a", OfType("slow"))
	def.Add("b", OfType("step"), DependsOn(a))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var bFired atomic.Bool
	if err := engine.RegisterHandler("slow", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
			return []byte("a-out"), nil
		}
	}); err != nil {
		t.Fatalf("RegisterHandler slow: %v", err)
	}
	if err := engine.RegisterHandler("step", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		bFired.Store(true)
		return []byte("b-out"), nil
	}); err != nil {
		t.Fatalf("RegisterHandler step: %v", err)
	}

	id, err := engine.Submit(context.Background(), "cancel", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Cancel before a finishes. (a may still complete on the worker
	// — that's fine; the bridge returns nil under cancellation so b
	// doesn't enqueue.)
	time.Sleep(20 * time.Millisecond)
	if err := engine.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Wait long enough that b would have fired by now if it were
	// going to.
	time.Sleep(400 * time.Millisecond)
	if bFired.Load() {
		t.Fatal("b fired despite cancellation")
	}
	info, err := engine.Inspect(context.Background(), id)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.State != InstanceCancelled {
		t.Fatalf("expected cancelled, got %s", info.State)
	}
}

func TestEngine_CleanupRemovesKeys(t *testing.T) {
	c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1024 * 1024})
	store := tasks.NewMemoryStore(tasks.MemoryOpts{})
	t.Cleanup(func() { _ = store.Close() })
	worker, err := tasks.NewWorker(tasks.WorkerOpts{Store: store, Concurrency: 2, PollInterval: 2 * time.Millisecond, MaintenanceInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	engine, err := NewEngine(EngineOpts{
		Scheduler: tasks.NewScheduler(store),
		Worker:    worker,
		Outputs:   c,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = worker.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	def := New("oneshot")
	def.Add("a", OfType("noop"))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := engine.RegisterHandler("noop", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		return []byte("done"), nil
	}); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}

	id, err := engine.Submit(context.Background(), "oneshot", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, engine, id, InstanceCompleted, 5*time.Second)

	// Confirm meta key exists.
	_, ok, err := c.Get(context.Background(), instanceMetaKey("wf:", id))
	if err != nil || !ok {
		t.Fatalf("meta missing pre-cleanup: ok=%v err=%v", ok, err)
	}

	if err := engine.Cleanup(context.Background(), id); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	_, ok, _ = c.Get(context.Background(), instanceMetaKey("wf:", id))
	if ok {
		t.Fatal("meta still present after cleanup")
	}
}

func TestEngine_RejectUnregisteredDefinition(t *testing.T) {
	engine, _, _ := newTestEngine(t)
	if _, err := engine.Submit(context.Background(), "missing", nil); err == nil {
		t.Fatal("expected error for unregistered definition")
	}
}

func TestEngine_RegisterValidatesDAG(t *testing.T) {
	engine, _, _ := newTestEngine(t)
	def := New("bad")
	def.Add("a", OfType("t"), DependsOnByName("a"))
	if err := engine.Register(def); err == nil {
		t.Fatal("expected validation failure")
	}
}
