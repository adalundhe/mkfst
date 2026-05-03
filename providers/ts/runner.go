// Package ts is the top-level TypeScript task subsystem. It knits
// together the bundler, the QuickJS runtime, the host bridge, and
// mkfst's existing workflows engine into a coherent submit-and-run
// pipeline.
package ts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"mkfst/providers/cache"
	"mkfst/providers/tasks"
	"mkfst/providers/ts/bundle"
	"mkfst/providers/ts/runtime"
	"mkfst/providers/workflows"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// === Submission + execution ===

// Workflow is one registered TS workflow (post-submit). It owns its
// bundle, the parsed DAG metadata, and a runtime pool used to
// dispatch task invocations.
//
// A Workflow is BOUND TO ONE STACK at submit time. All host calls
// the workflow makes (stack ops, vfs reads, etc.) are scoped to
// that stack. Cross-stack reach is denied at the bridge.
type Workflow struct {
	Name      string
	Bundle    *bundle.Result
	DAG       *DAG
	StackName string // bound stack — empty = no host access (pure compute)
	wfEngine  *workflows.Engine
	rtEngine  runtime.Engine
	rtBridge  *runtime.Bridge
	def       *workflows.Definition

	// sourceMap is the parsed inline source map (nil if the bundle
	// was built without source maps). Used to rewrite handler error
	// messages from JS bundle positions back to original TS lines.
	sourceMap *SourceMap

	mu       sync.Mutex
	rt       runtime.Runtime // shared single runtime for v1 (pool comes later)
	rtCtx    context.Context
}

// DAG mirrors the JS-side DAGDefinition structure so Go can reason
// about the topology without re-parsing JSON every time.
type DAG struct {
	Name  string     `json:"name"`
	Tasks []DAGTask  `json:"tasks"`
	Nodes []DAGNode  `json:"nodes"`
}

// DAGTask is a single task definition.
type DAGTask struct {
	ID          int      `json:"__id"`
	Name        string   `json:"name"`
	Retries     *int     `json:"retries,omitempty"`
	TimeoutSec  *int     `json:"timeoutSec,omitempty"`
	ParentNames []string `json:"parentNames"`
}

// DAGNode is one position in the DAG.
type DAGNode struct {
	NodeID      int            `json:"__nodeId"`
	TaskName    string         `json:"taskName"`
	TaskID      int            `json:"taskId"`
	Parents     []int          `json:"parents"`
	ParentNames map[string]int `json:"parentNames"`
	OnFail      string         `json:"onFail"`
}

// === Engine: top-level facade ===

// Engine is the TS subsystem's entry point. It bundles + registers
// + runs TS workflows against an underlying workflows.Engine.
type Engine struct {
	wfEng     *workflows.Engine
	rtEng     runtime.Engine
	bridge    *runtime.Bridge
	allowlist *bundle.Allowlist
	cache     cache.Cache
	emitMaps  bool

	mu        sync.RWMutex
	workflows map[string]*Workflow
}

// EngineOpts configures NewEngine.
type EngineOpts struct {
	WorkflowEngine *workflows.Engine
	Allowlist      *bundle.Allowlist
	Bridge         *runtime.Bridge // nil = AllowAll
	Cache          cache.Cache     // nil = in-memory
	// EmitSourceMaps enables inline source maps in bundles. Helps
	// translate runtime errors back to TS lines but increases
	// bundle size ~30%. Default off.
	EmitSourceMaps bool
}

// NewEngine constructs the TS engine.
func NewEngine(opts EngineOpts) (*Engine, error) {
	if opts.WorkflowEngine == nil {
		return nil, errors.New("ts.NewEngine: WorkflowEngine is required")
	}
	if opts.Allowlist == nil {
		return nil, errors.New("ts.NewEngine: Allowlist is required")
	}
	if opts.Bridge == nil {
		opts.Bridge = runtime.NewBridge(runtime.AllowAll{})
	}
	if opts.Cache == nil {
		opts.Cache = cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 16 * 1024 * 1024})
	}
	return &Engine{
		wfEng:     opts.WorkflowEngine,
		rtEng:     runtime.NewEngine(),
		bridge:    opts.Bridge,
		allowlist: opts.Allowlist,
		cache:     opts.Cache,
		emitMaps:  opts.EmitSourceMaps,
		workflows: map[string]*Workflow{},
	}, nil
}

// SubmitOpts is the typed options form for SubmitWith.
type SubmitOpts struct {
	// Source is the TS source bytes.
	Source []byte
	// Filename is the source's name for error messages.
	Filename string
	// Stack binds the workflow to one stack — every host call the
	// workflow makes is scoped to this stack, and any reference to
	// a different stack is hard-denied. Empty Stack means the
	// workflow has NO host access (pure-compute tasks only).
	Stack string
}

// Submit is the legacy compact form: bundles + registers a workflow
// with no stack binding. New code should use SubmitWith.
func (e *Engine) Submit(ctx context.Context, source []byte, filename string) (*Workflow, error) {
	return e.SubmitWith(ctx, SubmitOpts{Source: source, Filename: filename})
}

// SubmitWith bundles + registers a TS workflow with explicit
// options including stack binding for capability scoping.
func (e *Engine) SubmitWith(ctx context.Context, opts SubmitOpts) (*Workflow, error) {
	source := opts.Source
	filename := opts.Filename
	stackName := opts.Stack
	if len(source) == 0 {
		return nil, errors.New("Submit: empty source")
	}
	res, err := bundle.Build(bundle.Opts{
		Source:         source,
		SourceFilename: filename,
		Allowlist:      e.allowlist,
		SourceMap:      e.emitMaps,
	})
	if err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}

	// Bundle hash verification: re-hash the bytes we received from
	// the bundler and compare to the SHA256 the bundle reported.
	// This is a tripwire against in-process tampering of the
	// JS bytes between bundle and load. The runtime refuses to
	// load a bundle whose recomputed hash doesn't match.
	if recomputed := sha256Hex(res.JS); recomputed != res.SHA256 {
		return nil, fmt.Errorf("bundle: hash mismatch (recorded=%s recomputed=%s)", res.SHA256, recomputed)
	}

	// Spin up a runtime, eval the bundle, read back the DAG.
	rtInst, err := e.rtEng.NewRuntime(ctx, runtime.RuntimeOpts{HostBridge: e.bridge})
	if err != nil {
		return nil, fmt.Errorf("runtime: %w", err)
	}

	// Install the bridge dispatcher as a host function. Scope
	// every call to the workflow's bound stack so cross-stack
	// reach is mechanically impossible.
	if err := installBridgeDispatch(ctx, rtInst, e.bridge, stackName); err != nil {
		_ = rtInst.Close(ctx)
		return nil, fmt.Errorf("install bridge: %w", err)
	}

	// Eval the bundle.
	v, err := rtInst.Eval(ctx, string(res.JS), runtime.EvalOpts{Filename: filename})
	if err != nil {
		_ = rtInst.Close(ctx)
		return nil, fmt.Errorf("eval bundle: %w", err)
	}
	v.Free(ctx)

	// Read back the DAG from the JS-side global.
	dagV, err := rtInst.Eval(ctx, `JSON.stringify(globalThis.__mkfst_workflow.dag)`, runtime.EvalOpts{Filename: "<introspect>"})
	if err != nil {
		_ = rtInst.Close(ctx)
		return nil, fmt.Errorf("introspect dag: %w", err)
	}
	dagJSON, err := dagV.String(ctx)
	dagV.Free(ctx)
	if err != nil {
		_ = rtInst.Close(ctx)
		return nil, err
	}
	if dagJSON == "" || dagJSON == "undefined" || dagJSON == "null" {
		_ = rtInst.Close(ctx)
		return nil, errors.New("workflow did not export a DAG (use defineDAG and export default)")
	}
	var dag DAG
	if err := json.Unmarshal([]byte(dagJSON), &dag); err != nil {
		_ = rtInst.Close(ctx)
		return nil, fmt.Errorf("decode dag: %w", err)
	}

	wf := &Workflow{
		Name:      dag.Name,
		Bundle:    res,
		DAG:       &dag,
		StackName: stackName,
		wfEngine:  e.wfEng,
		rtEngine:  e.rtEng,
		rtBridge:  e.bridge,
		rt:        rtInst,
		rtCtx:     context.Background(),
	}
	// Parse source map if present so handler errors translate
	// back to TS source positions.
	if e.emitMaps {
		if sm, smErr := ParseInlineSourceMap(res.JS); smErr == nil {
			wf.sourceMap = sm
		}
	}

	// Build the workflows.Definition mirroring the JS DAG.
	def := workflows.New(dag.Name)
	nodeRefs := map[int]*workflows.NodeRef{}
	for _, node := range dag.Nodes {
		nodeName := node.TaskName // for v1 we use taskName as nodeName
		taskType := tsTaskType(dag.Name, node.TaskName)
		opts := []workflows.NodeOption{
			workflows.OfType(taskType),
		}
		// dependencies
		var parents []*workflows.NodeRef
		for _, pid := range node.Parents {
			if ref, ok := nodeRefs[pid]; ok {
				parents = append(parents, ref)
			}
		}
		if len(parents) > 0 {
			opts = append(opts, workflows.DependsOn(parents...))
		}
		// failure policy mapping
		switch node.OnFail {
		case "skipDownstream":
			opts = append(opts, workflows.OnFail(workflows.FailSkipDownstream))
		case "continue":
			opts = append(opts, workflows.OnFail(workflows.FailContinue))
		default:
			opts = append(opts, workflows.OnFail(workflows.FailHaltWorkflow))
		}
		ref := def.Add(nodeName, opts...)
		nodeRefs[node.NodeID] = ref
	}
	if err := e.wfEng.Register(def); err != nil {
		_ = rtInst.Close(ctx)
		return nil, fmt.Errorf("register definition: %w", err)
	}
	wf.def = def

	// Register one Go-side handler per task that proxies into the
	// JS task's run() function.
	for _, task := range dag.Tasks {
		taskName := task.Name
		taskType := tsTaskType(dag.Name, taskName)
		taskIdx := task.ID
		handler := wf.makeHandler(taskName, taskIdx)
		if err := e.wfEng.RegisterHandler(taskType, handler); err != nil {
			// duplicate-handler errors are tolerated when the user
			// resubmits the same workflow definition; surface
			// otherwise.
			if !isAlreadyRegistered(err) {
				_ = rtInst.Close(ctx)
				return nil, fmt.Errorf("register handler %s: %w", taskName, err)
			}
		}
	}

	e.mu.Lock()
	e.workflows[dag.Name] = wf
	e.mu.Unlock()
	return wf, nil
}

// Run kicks off an instance of the named workflow. Returns the
// instance ID; status is read via the underlying workflows.Engine.
func (e *Engine) Run(ctx context.Context, name string, input []byte) (string, error) {
	e.mu.RLock()
	_, ok := e.workflows[name]
	e.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("ts.Engine.Run: workflow %q not registered", name)
	}
	return e.wfEng.Submit(ctx, name, input)
}

// Inspect surfaces workflow-engine state.
func (e *Engine) Inspect(ctx context.Context, instanceID string) (workflows.InstanceInfo, error) {
	return e.wfEng.Inspect(ctx, instanceID)
}

// === per-task handler ===

func (w *Workflow) makeHandler(taskName string, taskIdx int) workflows.Handler {
	return func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		w.mu.Lock()
		defer w.mu.Unlock()

		// Build a JS expression that invokes the task with the
		// parent payloads converted from bytes (UTF-8 strings)
		// into JS values via JSON.parse where possible.
		parentsJSON := make(map[string]json.RawMessage, len(parents))
		for k, v := range parents {
			if len(v) == 0 {
				parentsJSON[k] = json.RawMessage(`null`)
				continue
			}
			// Try to parse as JSON; if that fails, treat as string.
			if json.Valid(v) {
				parentsJSON[k] = json.RawMessage(v)
			} else {
				b, _ := json.Marshal(string(v))
				parentsJSON[k] = b
			}
		}
		parentsBlob, _ := json.Marshal(parentsJSON)

		invokeSrc := fmt.Sprintf(`(async () => {
			const t = globalThis.__mkfst_workflow.tasks[%d];
			const result = await t.run({
				parents: %s,
				ctx: { signal: undefined, deadline: 0, instanceId: "", nodeName: "%s" },
				log: () => {},
			});
			return JSON.stringify(result === undefined ? null : result);
		})()`, taskIdx, parentsBlob, taskName)

		promise, err := w.rt.Eval(ctx, invokeSrc, runtime.EvalOpts{Filename: "<task:" + taskName + ">"})
		if err != nil {
			return nil, err
		}
		// The IIFE always returns a Promise (it's async). Drive
		// the event loop until it settles.
		resolved, err := w.rt.Await(ctx, promise)
		promise.Free(ctx)
		if err != nil {
			// Translate JS bundle line/col references in the
			// error back to TS source lines via the source map
			// (if available).
			msg := err.Error()
			if w.sourceMap != nil {
				msg = w.sourceMap.RewriteStack(msg)
			}
			return nil, fmt.Errorf("task %s: %s", taskName, msg)
		}
		defer resolved.Free(ctx)
		s, err := resolved.String(ctx)
		if err != nil {
			return nil, err
		}
		return []byte(s), nil
	}
}

// === bridge install ===

// installBridgeDispatch wires globalThis.__mkfst_dispatch to the
// Go-side runtime.Bridge. The JS shape is:
//
//   __mkfst_dispatch(op: string, argsJSON: string, moduleName: string) -> string
//
// The third argument is the importing module's npm root package
// name; the bundler-emitted per-module @mkfst/host wrappers thread
// it through automatically. Capability enforcement scopes by
// moduleName + boundStack.
//
// boundStack is the workflow's compile-time stack binding. The
// dispatcher INTERCEPTS args before handing them to the bridge:
// any reference to a stack other than boundStack is denied
// here, before the handler ever runs. This is the load-bearing
// scoping rule — a workflow physically cannot reach into another
// stack regardless of what its TS source asks for.
//
// Errors come back as {"__error":{"code":"...","message":"..."}}.
func installBridgeDispatch(ctx context.Context, rt runtime.Runtime, br *runtime.Bridge, boundStack string) error {
	return rt.RegisterHostFunction(ctx, "__mkfst_dispatch", func(ctx context.Context, args []runtime.Value) (runtime.Value, error) {
		if len(args) < 2 {
			errBlob, _ := json.Marshal(map[string]any{"__error": map[string]string{"code": "BAD_ARGS", "message": "expected (op, argsJSON, [moduleName])"}})
			return rt.NewString(ctx, string(errBlob))
		}
		op, _ := args[0].String(ctx)
		argsRaw, _ := args[1].String(ctx)
		moduleName := ""
		if len(args) >= 3 {
			moduleName, _ = args[2].String(ctx)
		}

		// Scope check: if the workflow has no bound stack at all,
		// reject every host call (pure-compute mode).
		if boundStack == "" {
			errBlob, _ := json.Marshal(map[string]any{
				"__error": map[string]string{"code": "NO_STACK_BINDING", "message": "this workflow has no stack binding — host calls are denied. Submit with --stack <name> to enable."},
			})
			return rt.NewString(ctx, string(errBlob))
		}

		// Inspect the args for any "stack" field; rewrite to the
		// bound stack name when omitted, reject when it points
		// elsewhere. This is mechanical scoping: even a malicious
		// workflow that forges a stack name in args can only
		// target its bound stack.
		argsRaw, scopeErr := enforceStackScope([]byte(argsRaw), boundStack)
		if scopeErr != nil {
			errBlob, _ := json.Marshal(map[string]any{
				"__error": map[string]string{"code": "CROSS_STACK_DENIED", "message": scopeErr.Error()},
			})
			return rt.NewString(ctx, errBlob2string(errBlob))
		}

		bc := runtime.BridgeCtx{
			Ctx:        ctx,
			ModuleName: moduleName,
			BoundStack: boundStack,
		}
		out, err := br.Dispatch(bc, op, []byte(argsRaw))
		if err != nil {
			errBlob, _ := json.Marshal(map[string]any{
				"__error": map[string]string{"code": "DISPATCH", "message": err.Error()},
			})
			return rt.NewString(ctx, string(errBlob))
		}
		return rt.NewString(ctx, string(out))
	})
}

// enforceStackScope inspects argsJSON for a "stack" field. If
// present, it must equal boundStack; if absent, it's injected as
// boundStack. Returns the (possibly rewritten) JSON bytes.
func enforceStackScope(argsJSON []byte, boundStack string) (string, error) {
	if len(argsJSON) == 0 {
		argsJSON = []byte(`{}`)
	}
	var raw map[string]any
	if err := json.Unmarshal(argsJSON, &raw); err != nil {
		// Not an object — pass through; downstream handlers will
		// reject malformed args.
		return string(argsJSON), nil
	}
	if v, ok := raw["stack"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return "", fmt.Errorf("'stack' arg must be a string")
		}
		if s != "" && s != boundStack {
			return "", fmt.Errorf("workflow is bound to stack %q; cross-stack reference to %q denied", boundStack, s)
		}
	}
	raw["stack"] = boundStack
	out, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func errBlob2string(b []byte) string { return string(b) }

// === helpers ===

// tsTaskType returns the registered task type for a JS task,
// scoped to the workflow so two workflows can have tasks with
// the same name without colliding on the workflows.Engine.
func tsTaskType(workflowName, taskName string) string {
	return "ts:" + workflowName + ":" + taskName
}

func isAlreadyRegistered(err error) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), "already registered")
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// === unused-import shim ===
var _ = tasks.Retries
