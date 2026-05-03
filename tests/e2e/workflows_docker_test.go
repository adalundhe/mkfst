//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"mkfst/providers/cache"
	dockerprov "mkfst/providers/docker"
	"mkfst/providers/tasks"
	"mkfst/providers/workflows"
)

// TestWorkflowsDockerLinearChain runs a 3-stage linear DAG of alpine
// containers. Each stage takes its parent's stdout as input and
// emits a transformed stdout. Verifies the engine routes outputs
// correctly across container boundaries:
//
//	emit  → upper      → suffix
//	"hi"  → "HI"       → "HI!"
//
// Each node is one container invocation. The bridge feeds the
// parent's stdout to the next container via `echo "$IN" | sh -c ...`
// using docker.Env to pass the input.
func TestWorkflowsDockerLinearChain(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "alpine:3.19")

	store := tasks.NewMemoryStore(tasks.MemoryOpts{DedupWindow: time.Minute})
	t.Cleanup(func() { _ = store.Close() })

	worker, err := tasks.NewWorker(tasks.WorkerOpts{
		Store:               store,
		Concurrency:         4,
		PollInterval:        25 * time.Millisecond,
		MaintenanceInterval: 100 * time.Millisecond,
		VisibilityTimeout:   60 * time.Second,
		HeartbeatInterval:   10 * time.Second,
		DefaultTimeout:      90 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	engine, err := workflows.NewEngine(workflows.EngineOpts{
		Scheduler: tasks.NewScheduler(store),
		Worker:    worker,
		Outputs:   cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 16 * 1024 * 1024}),
		OnError: func(instanceID, op string, err error) {
			t.Logf("workflow %s op=%s err=%v", instanceID, op, err)
		},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// Definition: emit → upper → suffix.
	def := workflows.New("docker-chain")
	emit := def.MustAdd("emit", workflows.OfType("docker.emit"))
	upper := def.MustAdd("upper", workflows.OfType("docker.upper"), workflows.DependsOn(emit))
	def.MustAdd("suffix", workflows.OfType("docker.suffix"), workflows.DependsOn(upper))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// emit: just `echo hi` — root, ignores parents.
	if err := engine.RegisterHandler("docker.emit", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		exit, stdout, stderr := runWaitAndCollect(t, c, "alpine:3.19",
			dockerprov.Cmd("sh", "-c", "echo -n hi"),
		)
		if exit != 0 {
			return nil, fmt.Errorf("emit exit %d: %s", exit, stderr)
		}
		return []byte(strings.TrimSpace(stdout)), nil
	}); err != nil {
		t.Fatalf("Register emit: %v", err)
	}

	// upper: `tr 'a-z' 'A-Z'` over the parent stdout.
	if err := engine.RegisterHandler("docker.upper", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		input := string(parents["emit"])
		// Pass via Env to keep payload bytes off the command line, then
		// reference it inside `sh -c`.
		exit, stdout, stderr := runWaitAndCollect(t, c, "alpine:3.19",
			dockerprov.Env("IN", input),
			dockerprov.Cmd("sh", "-c", `printf '%s' "$IN" | tr 'a-z' 'A-Z'`),
		)
		if exit != 0 {
			return nil, fmt.Errorf("upper exit %d: %s", exit, stderr)
		}
		return []byte(strings.TrimSpace(stdout)), nil
	}); err != nil {
		t.Fatalf("Register upper: %v", err)
	}

	// suffix: append "!".
	if err := engine.RegisterHandler("docker.suffix", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		input := string(parents["upper"])
		exit, stdout, stderr := runWaitAndCollect(t, c, "alpine:3.19",
			dockerprov.Env("IN", input),
			dockerprov.Cmd("sh", "-c", `printf '%s!' "$IN"`),
		)
		if exit != 0 {
			return nil, fmt.Errorf("suffix exit %d: %s", exit, stderr)
		}
		return []byte(strings.TrimSpace(stdout)), nil
	}); err != nil {
		t.Fatalf("Register suffix: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneRun := make(chan error, 1)
	go func() { doneRun <- worker.Run(ctx) }()

	id, err := engine.Submit(context.Background(), "docker-chain", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Wait for completion. Three alpine containers in series usually
	// take a few seconds depending on daemon load.
	deadline := time.Now().Add(120 * time.Second)
	var info workflows.InstanceInfo
	for time.Now().Before(deadline) {
		got, err := engine.Inspect(context.Background(), id)
		if err == nil && (got.State == workflows.InstanceCompleted || got.State == workflows.InstanceFailed) {
			info = got
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if info.State != workflows.InstanceCompleted {
		t.Fatalf("workflow did not complete: state=%s nodes=%+v", info.State, info.Nodes)
	}

	// Inspect the suffix node's output via the engine's cache: pull
	// the raw bytes by reaching into the engine's read-output API.
	out, err := engineNodeOutput(engine, id, "suffix")
	if err != nil {
		t.Fatalf("read suffix output: %v", err)
	}
	if string(out) != "HI!" {
		t.Fatalf("final output: got %q want %q", string(out), "HI!")
	}

	cancel()
	if err := <-doneRun; err != nil {
		t.Fatalf("worker.Run: %v", err)
	}
}

// TestWorkflowsDockerDiamondFanIn runs a diamond DAG:
//
//	          ┌─► uppercase ─┐
//	emit ────►│              ├─► concat
//	          └─► reverse  ──┘
//
// `emit` produces "abc"; the two parallel branches transform it; the
// terminal `concat` joins both branches as "ABC|cba". Verifies fan-out
// + fan-in via the engine's parent collection.
func TestWorkflowsDockerDiamondFanIn(t *testing.T) {
	c := requireDaemon(t)
	ensureImageLocal(t, c, "alpine:3.19")

	store := tasks.NewMemoryStore(tasks.MemoryOpts{DedupWindow: time.Minute})
	t.Cleanup(func() { _ = store.Close() })

	worker, err := tasks.NewWorker(tasks.WorkerOpts{
		Store:               store,
		Concurrency:         4,
		PollInterval:        25 * time.Millisecond,
		MaintenanceInterval: 100 * time.Millisecond,
		VisibilityTimeout:   60 * time.Second,
		HeartbeatInterval:   10 * time.Second,
		DefaultTimeout:      90 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	engine, err := workflows.NewEngine(workflows.EngineOpts{
		Scheduler: tasks.NewScheduler(store),
		Worker:    worker,
		Outputs:   cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 16 * 1024 * 1024}),
		OnError: func(instanceID, op string, err error) {
			t.Logf("workflow %s op=%s err=%v", instanceID, op, err)
		},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	def := workflows.New("docker-diamond")
	emit := def.MustAdd("emit", workflows.OfType("docker.emit"))
	upper := def.MustAdd("uppercase", workflows.OfType("docker.upper"), workflows.DependsOn(emit))
	rev := def.MustAdd("reverse", workflows.OfType("docker.reverse"), workflows.DependsOn(emit))
	def.MustAdd("concat", workflows.OfType("docker.concat"), workflows.DependsOn(upper, rev))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := engine.RegisterHandler("docker.emit", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		exit, stdout, _ := runWaitAndCollect(t, c, "alpine:3.19",
			dockerprov.Cmd("sh", "-c", "printf abc"),
		)
		if exit != 0 {
			return nil, fmt.Errorf("emit exit %d", exit)
		}
		return []byte(strings.TrimSpace(stdout)), nil
	}); err != nil {
		t.Fatalf("Register emit: %v", err)
	}
	if err := engine.RegisterHandler("docker.upper", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		exit, stdout, _ := runWaitAndCollect(t, c, "alpine:3.19",
			dockerprov.Env("IN", string(parents["emit"])),
			dockerprov.Cmd("sh", "-c", `printf '%s' "$IN" | tr 'a-z' 'A-Z'`),
		)
		if exit != 0 {
			return nil, fmt.Errorf("upper exit %d", exit)
		}
		return []byte(strings.TrimSpace(stdout)), nil
	}); err != nil {
		t.Fatalf("Register upper: %v", err)
	}
	if err := engine.RegisterHandler("docker.reverse", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		exit, stdout, _ := runWaitAndCollect(t, c, "alpine:3.19",
			dockerprov.Env("IN", string(parents["emit"])),
			dockerprov.Cmd("sh", "-c", `printf '%s' "$IN" | rev`),
		)
		if exit != 0 {
			return nil, fmt.Errorf("reverse exit %d", exit)
		}
		return []byte(strings.TrimSpace(stdout)), nil
	}); err != nil {
		t.Fatalf("Register reverse: %v", err)
	}
	if err := engine.RegisterHandler("docker.concat", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		left := string(parents["uppercase"])
		right := string(parents["reverse"])
		exit, stdout, _ := runWaitAndCollect(t, c, "alpine:3.19",
			dockerprov.Env("L", left),
			dockerprov.Env("R", right),
			dockerprov.Cmd("sh", "-c", `printf '%s|%s' "$L" "$R"`),
		)
		if exit != 0 {
			return nil, fmt.Errorf("concat exit %d", exit)
		}
		return []byte(strings.TrimSpace(stdout)), nil
	}); err != nil {
		t.Fatalf("Register concat: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneRun := make(chan error, 1)
	go func() { doneRun <- worker.Run(ctx) }()

	id, err := engine.Submit(context.Background(), "docker-diamond", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	deadline := time.Now().Add(180 * time.Second)
	var info workflows.InstanceInfo
	for time.Now().Before(deadline) {
		got, err := engine.Inspect(context.Background(), id)
		if err == nil && (got.State == workflows.InstanceCompleted || got.State == workflows.InstanceFailed) {
			info = got
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if info.State != workflows.InstanceCompleted {
		// Dump a json snapshot of node states to make debugging easier.
		dump, _ := json.MarshalIndent(info, "", "  ")
		t.Fatalf("workflow did not complete:\n%s", string(dump))
	}

	out, err := engineNodeOutput(engine, id, "concat")
	if err != nil {
		t.Fatalf("read concat output: %v", err)
	}
	if string(out) != "ABC|cba" {
		t.Fatalf("final output: got %q want %q", string(out), "ABC|cba")
	}

	cancel()
	if err := <-doneRun; err != nil {
		t.Fatalf("worker.Run: %v", err)
	}
}

// engineNodeOutput is a test-only helper: re-builds the engine's
// internal cache key and reads the raw output bytes via the engine's
// public Cleanup target. We don't expose a ReadOutput method on the
// engine itself because production callers should consume outputs
// inside their handler chain — only tests poke at them post-hoc.
func engineNodeOutput(engine *workflows.Engine, instanceID, nodeName string) ([]byte, error) {
	// The cache + key prefix are private to the engine. Exposing a
	// test-only accessor would leak implementation. Instead we
	// invoke a helper added to the workflows package keyed for tests.
	return workflows.TestReadNodeOutput(engine, instanceID, nodeName)
}
