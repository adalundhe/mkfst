package runtime

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mkfst/providers/ts/bundle"
)

func sdkDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "sdk")
}

// TestIntegration_BundleAndRun proves the full flow from a TS
// workflow source through bundling to JS execution: define tasks,
// build a DAG, evaluate the bundle, read back the registered DAG
// definition from JS-side state.
func TestIntegration_BundleAndRun(t *testing.T) {
	// 1. Bundle a workflow.
	al := bundle.NewAllowlist(sdkDir(t))
	if err := al.Add(bundle.ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(sdkDir(t), "mkfst-sdk"),
	}); err != nil {
		t.Fatal(err)
	}

	src := []byte(`
import { defineTask, defineDAG } from "@mkfst/sdk";

const seed = defineTask({
  name: "seed",
  run: () => "seed-out",
});

const transform = defineTask({
  name: "transform",
  parents: { seed },
  run: ({ parents }) => "transform(" + parents.seed + ")",
});

export default defineDAG("pipeline", (b) => {
  const s = b.add(seed);
  b.add(transform, { dependsOn: { seed: s } });
});
`)
	res, err := bundle.Build(bundle.Opts{
		Source:         src,
		SourceFilename: "pipeline.workflow.ts",
		Allowlist:      al,
	})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	t.Logf("bundle: %d KiB, sha=%s", res.SizeKB, res.SHA256[:12])

	// 2. Load into QuickJS.
	ctx := context.Background()
	eng := NewEngine()
	rt, err := eng.NewRuntime(ctx, RuntimeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)

	v, err := rt.Eval(ctx, string(res.JS), EvalOpts{Filename: "pipeline.bundle.js"})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	v.Free(ctx)

	// 3. Read the registered DAG via JSON.stringify of the global.
	dagV, err := rt.Eval(ctx, `JSON.stringify(globalThis.__mkfst_workflow.dag, (k, v) => typeof v === 'function' ? '[Function]' : v)`,
		EvalOpts{Filename: "<introspect>"})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	defer dagV.Free(ctx)
	jsonStr, err := dagV.String(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonStr, `"name":"pipeline"`) {
		t.Fatalf("dag JSON missing name: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"taskName":"seed"`) ||
		!strings.Contains(jsonStr, `"taskName":"transform"`) {
		t.Fatalf("dag JSON missing tasks: %s", jsonStr)
	}
	t.Logf("dag JSON: %s", jsonStr)

	// 4. Call the seed task's run function and verify result.
	out, err := rt.Eval(ctx, `globalThis.__mkfst_workflow.tasks[0].run({ parents: {}, ctx: { signal: undefined, deadline: 0, instanceId: "i", nodeName: "n" }, log: () => {} })`,
		EvalOpts{Filename: "<invoke>"})
	if err != nil {
		t.Fatalf("invoke seed: %v", err)
	}
	defer out.Free(ctx)
	s, _ := out.String(ctx)
	if s != "seed-out" {
		t.Fatalf("seed result: %q", s)
	}

	// 5. Call the transform task's run function with parent payload.
	out2, err := rt.Eval(ctx, `globalThis.__mkfst_workflow.tasks[1].run({ parents: { seed: "seed-out" }, ctx: { signal: undefined, deadline: 0, instanceId: "i", nodeName: "n" }, log: () => {} })`,
		EvalOpts{Filename: "<invoke>"})
	if err != nil {
		t.Fatalf("invoke transform: %v", err)
	}
	defer out2.Free(ctx)
	s2, _ := out2.String(ctx)
	if s2 != "transform(seed-out)" {
		t.Fatalf("transform result: %q", s2)
	}
}

// TestIntegration_HostFunctionCall proves the host bridge wiring:
// register a Go function on the runtime, call it from JS, and get
// the right value back.
func TestIntegration_HostFunctionCall(t *testing.T) {
	ctx := context.Background()
	eng := NewEngine()
	rt, err := eng.NewRuntime(ctx, RuntimeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)

	// Register a host function that doubles its input.
	called := 0
	err = rt.RegisterHostFunction(ctx, "double", func(ctx context.Context, args []Value) (Value, error) {
		called++
		if len(args) != 1 {
			return nil, nil
		}
		n, _ := args[0].Int32(ctx)
		return rt.NewInt32(ctx, n*2)
	})
	if err != nil {
		t.Fatal(err)
	}

	v, err := rt.Eval(ctx, "double(21)", EvalOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free(ctx)
	n, _ := v.Int32(ctx)
	if n != 42 {
		t.Fatalf("got %d", n)
	}
	if called != 1 {
		t.Fatalf("called=%d", called)
	}
}
