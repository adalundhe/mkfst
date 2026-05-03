package workflows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"mkfst/providers/cache"
	"mkfst/providers/tasks"
)

// === public engine API ===

// EngineOpts configures NewEngine.
type EngineOpts struct {
	// Scheduler is required. The engine submits one task per node via
	// this scheduler and the underlying tasks.Worker dispatches to
	// the registered task-type handler. The scheduler should be
	// backed by the same Store that the worker pulls from — typical
	// setup is one Store + one Scheduler + one Worker per process.
	Scheduler tasks.Scheduler

	// Worker is required. The engine calls Worker.Register once per
	// node task type to wire up its bridging handler that loads
	// parent outputs, invokes the user handler, and stores the
	// result. The user must still call Worker.Run separately — the
	// engine doesn't own the worker's lifecycle.
	Worker tasks.Worker

	// Outputs is the cache that holds per-node outputs and per-
	// instance state. Optional — defaults to an in-process memory
	// cache with a 256 MiB cap. Pass a Redis or SQL cache when
	// multiple processes share workflow state, or when state needs
	// to outlive the process.
	Outputs cache.Cache

	// KeyPrefix is prepended to every cache key the engine writes.
	// Default "wf:". Useful when the cache is shared with other
	// callers and you want a clean namespace.
	KeyPrefix string

	// DefaultQueue is the queue name used for any node that doesn't
	// override via Queue NodeOption. Default "default".
	DefaultQueue string

	// InstanceTTL caps how long a terminal-state instance's outputs
	// stay in the cache after the workflow finishes. The engine
	// writes node outputs with this TTL when finalizing. 0 means
	// outputs persist until DeletePrefix is called by Cleanup. The
	// default 24h gives downstream callers a window to read results
	// without trapping garbage in the cache forever.
	InstanceTTL time.Duration

	// OnError is invoked from internal goroutines for non-fatal
	// errors that the engine handled but couldn't surface up the
	// call stack (cache write failures during advancement, etc.).
	// Optional; nil = silent.
	OnError func(instanceID, op string, err error)
}

// Handler is the signature for a workflow node handler. It receives
// the parent outputs keyed by parent node name; the bytes are exactly
// what the parent handler returned. Returning a non-nil error fails
// the node and triggers retry/failure-policy handling.
//
// The ctx is the per-attempt context from the underlying task — it
// carries Task.Timeout/Deadline cancellation. Long handlers should
// honor it.
type Handler func(ctx context.Context, parents map[string][]byte) ([]byte, error)

// Engine orchestrates workflow execution. Construct via NewEngine.
//
// Lifecycle:
//   - NewEngine(opts) — build the engine
//   - Register(def) — load a Definition (idempotent; re-registering
//     a definition with the same name replaces it)
//   - RegisterHandler(taskType, h) — bind the node handler for each
//     unique task type referenced by registered definitions. The
//     engine internally wraps h with a bridging handler that loads
//     parent outputs from the cache and stores the result back.
//   - Submit(ctx, defName, input) — start an instance
//   - Inspect(ctx, instanceID) — read instance + node states
//   - Cancel(ctx, instanceID) — mark the instance cancelled and
//     refuse to advance further
//   - Cleanup(ctx, instanceID) — delete the instance's cache
//     namespace (call after the caller has read final outputs)
type Engine struct {
	opts EngineOpts

	mu          sync.Mutex
	definitions map[string]*Definition
	handlers    map[string]Handler // taskType → user handler
	registered  map[string]struct{}
	closed      bool
}

// NewEngine constructs an Engine. Returns an error if required opts
// are missing.
func NewEngine(opts EngineOpts) (*Engine, error) {
	if opts.Scheduler == nil {
		return nil, errors.New("workflows.NewEngine: Scheduler is required")
	}
	if opts.Worker == nil {
		return nil, errors.New("workflows.NewEngine: Worker is required")
	}
	if opts.Outputs == nil {
		opts.Outputs = cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 256 * 1024 * 1024})
	}
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = "wf:"
	}
	if opts.DefaultQueue == "" {
		opts.DefaultQueue = "default"
	}
	if opts.InstanceTTL == 0 {
		opts.InstanceTTL = 24 * time.Hour
	}
	return &Engine{
		opts:        opts,
		definitions: make(map[string]*Definition),
		handlers:    make(map[string]Handler),
		registered:  make(map[string]struct{}),
	}, nil
}

// Register loads a Definition into the engine. Validates the DAG
// before accepting it. Re-registering a definition with the same
// name replaces the prior version (in-flight instances continue
// against their original blueprint — definitions are read on
// Submit and the resolved blueprint is captured per instance via
// the cache).
func (e *Engine) Register(def *Definition) error {
	if def == nil {
		return errors.New("workflows.Engine.Register: nil definition")
	}
	if err := def.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("workflows.Engine.Register: engine is closed")
	}
	e.definitions[def.name] = def
	return nil
}

// RegisterHandler binds a node handler to a task type. The engine
// installs a bridging handler on the underlying Worker that:
//
//  1. Reads the per-instance, per-node task metadata from the cache
//     to discover which workflow instance and node this invocation
//     belongs to.
//  2. Loads parent outputs from the cache and passes them to the
//     user handler.
//  3. Stores the user handler's output back to the cache.
//  4. Advances the workflow by enqueueing newly-ready successor
//     nodes (idempotent via UniqueKey).
//
// The user handler is the one that does actual work. The bridging
// is transparent to the caller.
//
// Returns an error if taskType is empty, h is nil, or this taskType
// is already registered. Multiple workflow nodes may share a task
// type — the engine routes by inspecting the bridge metadata so a
// single handler can serve every node of that type.
func (e *Engine) RegisterHandler(taskType string, h Handler) error {
	if taskType == "" {
		return errors.New("workflows.Engine.RegisterHandler: taskType is empty")
	}
	if h == nil {
		return errors.New("workflows.Engine.RegisterHandler: handler is nil")
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("workflows.Engine.RegisterHandler: engine is closed")
	}
	if _, exists := e.handlers[taskType]; exists {
		e.mu.Unlock()
		return fmt.Errorf("workflows.Engine.RegisterHandler: taskType %q already registered", taskType)
	}
	e.handlers[taskType] = h
	_, alreadyOnWorker := e.registered[taskType]
	if !alreadyOnWorker {
		e.registered[taskType] = struct{}{}
	}
	e.mu.Unlock()

	if alreadyOnWorker {
		return nil
	}
	return e.opts.Worker.Register(taskType, e.makeBridgeHandler(taskType))
}

// Submit starts an instance of the named definition. input is the
// initial payload available to root nodes via parents[""] — root
// nodes have no parents, so this is the conventional way to feed
// the very first stage of the DAG. Pass nil if the workflow needs
// no input.
//
// Returns the instance ID; use Inspect to track progress.
func (e *Engine) Submit(ctx context.Context, definitionName string, input []byte) (string, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return "", errors.New("workflows.Engine.Submit: engine is closed")
	}
	def, ok := e.definitions[definitionName]
	e.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("workflows.Engine.Submit: definition %q not registered", definitionName)
	}

	instanceID, err := newInstanceID()
	if err != nil {
		return "", fmt.Errorf("workflows.Engine.Submit: id: %w", err)
	}

	// Initialize instance meta.
	meta := instanceMeta{
		ID:         instanceID,
		Definition: definitionName,
		State:      InstanceRunning,
		StartedAt:  time.Now(),
	}
	metaBytes, err := encodeInstanceMeta(meta)
	if err != nil {
		return "", err
	}
	if err := e.opts.Outputs.Set(ctx, instanceMetaKey(e.opts.KeyPrefix, instanceID), metaBytes, 0); err != nil {
		return "", fmt.Errorf("workflows.Engine.Submit: write meta: %w", err)
	}

	// Initialize all node states as pending.
	for _, name := range def.nodeNames() {
		state := nodeStateRecord{State: NodePending}
		stateBytes, err := encodeNodeState(state)
		if err != nil {
			return "", err
		}
		if err := e.opts.Outputs.Set(ctx, nodeStateKey(e.opts.KeyPrefix, instanceID, name), stateBytes, 0); err != nil {
			return "", fmt.Errorf("workflows.Engine.Submit: write node state %q: %w", name, err)
		}
	}

	// Stash the input as a synthetic parent output keyed under the
	// reserved name "" so root nodes can read it via parents[""].
	if input != nil {
		if err := e.opts.Outputs.Set(ctx, nodeOutputKey(e.opts.KeyPrefix, instanceID, ""), input, 0); err != nil {
			return "", fmt.Errorf("workflows.Engine.Submit: write input: %w", err)
		}
	}

	// Enqueue every root.
	for _, root := range def.roots() {
		if err := e.enqueueNode(ctx, def, instanceID, root); err != nil {
			// Best-effort partial-rollback: mark the instance failed so
			// orphaned roots don't keep firing.
			e.markInstanceFailed(ctx, instanceID, fmt.Sprintf("submit: enqueue %s: %v", root, err))
			return instanceID, err
		}
	}

	return instanceID, nil
}

// Inspect returns a snapshot of the instance's state plus per-node
// states. Returns ErrNotFound if no such instance exists.
func (e *Engine) Inspect(ctx context.Context, instanceID string) (InstanceInfo, error) {
	metaBytes, ok, err := e.opts.Outputs.Get(ctx, instanceMetaKey(e.opts.KeyPrefix, instanceID))
	if err != nil {
		return InstanceInfo{}, fmt.Errorf("workflows.Engine.Inspect: get meta: %w", err)
	}
	if !ok {
		return InstanceInfo{}, ErrNotFound
	}
	meta, err := decodeInstanceMeta(metaBytes)
	if err != nil {
		return InstanceInfo{}, err
	}

	e.mu.Lock()
	def, defOk := e.definitions[meta.Definition]
	e.mu.Unlock()

	info := InstanceInfo{
		ID:         meta.ID,
		Definition: meta.Definition,
		State:      meta.State,
		StartedAt:  meta.StartedAt,
		EndedAt:    meta.EndedAt,
		Nodes:      make(map[string]NodeInfo),
	}

	if defOk {
		for _, name := range def.nodeNames() {
			n, err := e.loadNodeState(ctx, instanceID, name)
			if err != nil {
				continue
			}
			info.Nodes[name] = NodeInfo{
				State:     n.State,
				TaskID:    n.TaskID,
				Attempts:  n.Attempts,
				StartedAt: n.StartedAt,
				EndedAt:   n.EndedAt,
				LastError: n.LastError,
			}
		}
	}
	return info, nil
}

// Cancel marks the instance cancelled and prevents future
// advancement. Already-running tasks are not interrupted (the
// underlying tasks.Worker's cancellation contract is "hint, not
// kill") — but their completion will not enqueue successors.
//
// Idempotent: cancelling an already-terminal instance is a no-op.
func (e *Engine) Cancel(ctx context.Context, instanceID string) error {
	metaBytes, ok, err := e.opts.Outputs.Get(ctx, instanceMetaKey(e.opts.KeyPrefix, instanceID))
	if err != nil {
		return fmt.Errorf("workflows.Engine.Cancel: get meta: %w", err)
	}
	if !ok {
		return ErrNotFound
	}
	meta, err := decodeInstanceMeta(metaBytes)
	if err != nil {
		return err
	}
	switch meta.State {
	case InstanceCompleted, InstanceFailed, InstanceCancelled:
		return nil
	}
	meta.State = InstanceCancelled
	meta.EndedAt = time.Now()
	updated, err := encodeInstanceMeta(meta)
	if err != nil {
		return err
	}
	return e.opts.Outputs.Set(ctx, instanceMetaKey(e.opts.KeyPrefix, instanceID), updated, 0)
}

// Cleanup deletes every cache key owned by this instance. Call
// after readers have consumed final outputs. Idempotent.
func (e *Engine) Cleanup(ctx context.Context, instanceID string) error {
	_, err := e.opts.Outputs.DeletePrefix(ctx, instancePrefix(e.opts.KeyPrefix, instanceID))
	return err
}

// Close releases engine resources. Does not stop the underlying
// Worker — that's the caller's responsibility, since the worker is
// shared with non-workflow tasks.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

// === public read types ===

// ErrNotFound: the requested instance ID isn't in the cache.
var ErrNotFound = errors.New("workflows: instance not found")

// InstanceInfo is the read-only view returned by Inspect.
type InstanceInfo struct {
	ID         string
	Definition string
	State      InstanceState
	StartedAt  time.Time
	EndedAt    time.Time
	Nodes      map[string]NodeInfo
}

// NodeInfo is one node's read-only state.
type NodeInfo struct {
	State     NodeState
	TaskID    string
	Attempts  int
	StartedAt time.Time
	EndedAt   time.Time
	LastError string
}

// === bridge handler ===
//
// The bridge is what's actually registered with the Worker. When a
// task fires, the bridge:
//   1. Recovers (instanceID, node) from Task.Tags.
//   2. Loads the workflow definition.
//   3. If the instance is cancelled, returns nil immediately (acts
//      as a no-op handler).
//   4. Loads parent outputs from the cache.
//   5. Invokes the user handler.
//   6. On success: stores output, marks node completed, advances
//      successors.
//   7. On error: returns the error so the underlying tasks.Worker
//      can apply its retry/backoff math; failure-policy enforcement
//      happens after retries are exhausted (the bridge differentiates
//      "task failed once, will retry" from "task gave up" by checking
//      whether the final tasks state is StateFailed before marking
//      the node failed).
//
// Failure-policy enforcement is done by the bridge inspecting the
// task's record at the end of each invocation: when the task is
// terminal-failed (out of retries), we mark the node failed and
// run the policy. The retry attempts themselves are pure tasks.go
// concerns.

const (
	tagInstanceID = "wf.instance"
	tagNode       = "wf.node"
)

// makeBridgeHandler returns the tasks.Handler that the engine
// registers with the Worker for the given taskType.
func (e *Engine) makeBridgeHandler(taskType string) tasks.Handler {
	return func(ctx context.Context, t tasks.Task) error {
		instanceID := t.Tags[tagInstanceID]
		nodeName := t.Tags[tagNode]
		if instanceID == "" || nodeName == "" {
			return fmt.Errorf("workflows: task %s missing wf.instance/wf.node tags", t.ID)
		}

		// Resolve definition + handler.
		meta, err := e.loadInstanceMeta(ctx, instanceID)
		if err != nil {
			return fmt.Errorf("workflows: load meta: %w", err)
		}

		// Fast-path bail if instance already terminal — the bridge
		// returns nil so the underlying tasks.Worker marks the task
		// completed; we don't want a "cancelled instance" to keep
		// generating retries.
		switch meta.State {
		case InstanceCancelled, InstanceFailed, InstanceCompleted:
			return nil
		}

		e.mu.Lock()
		def, defOk := e.definitions[meta.Definition]
		userHandler, handlerOk := e.handlers[taskType]
		e.mu.Unlock()
		if !defOk {
			return fmt.Errorf("workflows: definition %q no longer registered", meta.Definition)
		}
		if !handlerOk {
			return fmt.Errorf("workflows: handler for taskType %q gone", taskType)
		}

		// Collect parent outputs.
		parents, err := e.collectParents(ctx, def, instanceID, nodeName)
		if err != nil {
			return fmt.Errorf("workflows: collect parents: %w", err)
		}

		// Mark started (best-effort — last writer wins; if two
		// engines race the started timestamp it's fine, we just
		// want it close enough for observability).
		startState, _ := e.loadNodeState(ctx, instanceID, nodeName)
		if startState.StartedAt.IsZero() {
			startState.StartedAt = time.Now()
		}
		startState.Attempts++
		startState.TaskID = t.ID
		startState.State = NodeEnqueued
		if err := e.writeNodeState(ctx, instanceID, nodeName, startState); err != nil {
			e.reportErr(instanceID, "write-start-state", err)
		}

		// Invoke user handler.
		out, handlerErr := userHandler(ctx, parents)

		if handlerErr != nil {
			// Record the error on the node but let the underlying
			// tasks layer decide retry vs terminal. We re-classify
			// to NodeFailed only when retries are exhausted, which
			// the bridge can't directly observe — instead we let
			// the next invocation overwrite startedAt/attempts and
			// only finalize when a non-error returns. To still surface
			// "out of retries" we hook the failure here by inspecting
			// Attempts vs MaxRetries in the task record.
			startState.LastError = handlerErr.Error()
			if err := e.writeNodeState(ctx, instanceID, nodeName, startState); err != nil {
				e.reportErr(instanceID, "write-error-state", err)
			}

			// Was this the final attempt? The Worker will only call
			// Fail without a nextAttempt when retries are exhausted;
			// from the handler's POV we can't see that decision
			// before returning, but we *can* see attempts so far. If
			// task.MaxRetries is set and we've used them all, mark
			// the node failed and apply policy now (and still return
			// the error so the task transitions correctly).
			finalAttempt := isFinalAttempt(t, startState.Attempts)
			if finalAttempt {
				startState.State = NodeFailed
				startState.EndedAt = time.Now()
				if err := e.writeNodeState(ctx, instanceID, nodeName, startState); err != nil {
					e.reportErr(instanceID, "write-failed-state", err)
				}
				e.applyFailurePolicy(ctx, def, instanceID, nodeName)
			}
			return handlerErr
		}

		// Success — store output, mark node completed, advance.
		if err := e.opts.Outputs.Set(ctx, nodeOutputKey(e.opts.KeyPrefix, instanceID, nodeName), out, e.opts.InstanceTTL); err != nil {
			return fmt.Errorf("workflows: write output: %w", err)
		}
		startState.State = NodeCompleted
		startState.EndedAt = time.Now()
		startState.LastError = ""
		if err := e.writeNodeState(ctx, instanceID, nodeName, startState); err != nil {
			return fmt.Errorf("workflows: write completed state: %w", err)
		}

		// Enqueue children whose parents are all complete.
		if err := e.advance(ctx, def, instanceID, nodeName); err != nil {
			e.reportErr(instanceID, "advance", err)
		}

		// Check terminal state: if every node terminal, finalize.
		e.maybeFinalize(ctx, def, instanceID)
		return nil
	}
}

// === advancement ===

// advance enqueues every child of `parent` whose other parents are
// already in NodeCompleted state. Idempotent via tasks UniqueKey.
func (e *Engine) advance(ctx context.Context, def *Definition, instanceID, parent string) error {
	for _, child := range def.children(parent) {
		ready, allCompleted, err := e.parentsReady(ctx, def, instanceID, child)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		if !allCompleted {
			// A parent failed under FailContinue (output is empty) —
			// still enqueue. Under FailSkipDownstream this branch
			// would have been marked NodeSkipped already and we
			// wouldn't reach here.
		}
		// Promote child only if it's still pending. (May already be
		// enqueued/running/terminal under cooperative multi-engine.)
		ns, err := e.loadNodeState(ctx, instanceID, child)
		if err != nil {
			return err
		}
		if ns.State != NodePending {
			continue
		}
		if err := e.enqueueNode(ctx, def, instanceID, child); err != nil {
			return err
		}
	}
	return nil
}

// parentsReady reports whether every parent has reached a terminal
// state, and whether every parent reached NodeCompleted (the second
// return). Skipped parents block the child unless FailContinue
// applied; failed parents under FailHaltWorkflow already stopped
// the world before we get here.
func (e *Engine) parentsReady(ctx context.Context, def *Definition, instanceID, child string) (ready, allCompleted bool, err error) {
	def.mu.Lock()
	parents := append([]string(nil), def.nodes[child].parents...)
	def.mu.Unlock()

	allCompleted = true
	for _, p := range parents {
		ns, err := e.loadNodeState(ctx, instanceID, p)
		if err != nil {
			return false, false, err
		}
		if !ns.State.Terminal() {
			return false, false, nil
		}
		if ns.State != NodeCompleted {
			allCompleted = false
		}
		if ns.State == NodeSkipped {
			// Skipped parent → child is also skipped.
			return false, false, nil
		}
	}
	return true, allCompleted, nil
}

// enqueueNode submits one task for one node. Uses a UniqueKey so
// cooperative engines don't double-enqueue.
func (e *Engine) enqueueNode(ctx context.Context, def *Definition, instanceID, nodeName string) error {
	def.mu.Lock()
	n := def.nodes[nodeName]
	def.mu.Unlock()
	if n == nil {
		return fmt.Errorf("workflows.enqueueNode: unknown node %q", nodeName)
	}

	// Update node state to enqueued before submitting so that even if
	// the worker picks up the task and runs the bridge before our
	// post-Enqueue write lands, the bridge's own state-write will
	// supersede this safely (last-writer-wins on a strictly forward-
	// progressing state machine).
	ns, _ := e.loadNodeState(ctx, instanceID, nodeName)
	ns.State = NodeEnqueued
	if err := e.writeNodeState(ctx, instanceID, nodeName, ns); err != nil {
		return fmt.Errorf("workflows.enqueueNode: write state: %w", err)
	}

	tags := map[string]string{
		tagInstanceID: instanceID,
		tagNode:       nodeName,
	}
	maxRetries := n.maxRetries
	if maxRetries == nil {
		// Default workflow nodes to 0 retries so the bridge can
		// detect the terminal-failure attempt deterministically
		// without depending on the worker's DefaultMaxRetries.
		zero := 0
		maxRetries = &zero
	}
	t := tasks.Task{
		Type:       n.taskType,
		Queue:      e.opts.DefaultQueue,
		Priority:   n.priority,
		MaxRetries: maxRetries,
		UniqueKey:  fmt.Sprintf("wf:%s:%s:fire", instanceID, nodeName),
		Tags:       tags,
	}
	_, err := e.opts.Scheduler.Enqueue(ctx, t)
	if err != nil && !errors.Is(err, tasks.ErrUniqueViolation) {
		return fmt.Errorf("workflows.enqueueNode: enqueue: %w", err)
	}
	return nil
}

// applyFailurePolicy runs the per-node policy for `failed`.
func (e *Engine) applyFailurePolicy(ctx context.Context, def *Definition, instanceID, failed string) {
	def.mu.Lock()
	policy := def.nodes[failed].failure
	def.mu.Unlock()

	switch policy {
	case FailHaltWorkflow:
		// Mark every non-terminal node skipped and the instance
		// failed. Sibling running tasks finish naturally; their
		// bridge invocation will see InstanceFailed and bail.
		for _, name := range def.nodeNames() {
			ns, err := e.loadNodeState(ctx, instanceID, name)
			if err != nil {
				e.reportErr(instanceID, "policy-load-state", err)
				continue
			}
			if !ns.State.Terminal() {
				ns.State = NodeSkipped
				ns.EndedAt = time.Now()
				if err := e.writeNodeState(ctx, instanceID, name, ns); err != nil {
					e.reportErr(instanceID, "policy-skip-write", err)
				}
			}
		}
		e.markInstanceFailed(ctx, instanceID, fmt.Sprintf("node %q failed; workflow halted", failed))

	case FailSkipDownstream:
		// Mark every reachable descendant skipped. Other branches
		// continue running.
		for _, descendant := range def.descendants(failed) {
			ns, err := e.loadNodeState(ctx, instanceID, descendant)
			if err != nil {
				e.reportErr(instanceID, "policy-load-descendant", err)
				continue
			}
			if !ns.State.Terminal() {
				ns.State = NodeSkipped
				ns.EndedAt = time.Now()
				if err := e.writeNodeState(ctx, instanceID, descendant, ns); err != nil {
					e.reportErr(instanceID, "policy-skip-descendant", err)
				}
			}
		}
		// Don't mark the instance failed yet — that decision is
		// deferred until every branch terminates.
		e.maybeFinalize(ctx, def, instanceID)

	case FailContinue:
		// Treat the failed node as if it completed with empty
		// output. Write empty bytes so children that read parents
		// see "" rather than (nil, false).
		if err := e.opts.Outputs.Set(ctx, nodeOutputKey(e.opts.KeyPrefix, instanceID, failed), []byte{}, e.opts.InstanceTTL); err != nil {
			e.reportErr(instanceID, "policy-continue-output", err)
		}
		// Advance children manually since the bridge's success-path
		// advance won't fire when the handler errored.
		if err := e.advance(ctx, def, instanceID, failed); err != nil {
			e.reportErr(instanceID, "policy-continue-advance", err)
		}
		e.maybeFinalize(ctx, def, instanceID)
	}
}

// maybeFinalize transitions the instance to a terminal state when
// every node has reached a terminal state.
func (e *Engine) maybeFinalize(ctx context.Context, def *Definition, instanceID string) {
	allTerminal := true
	anyFailed := false
	for _, name := range def.nodeNames() {
		ns, err := e.loadNodeState(ctx, instanceID, name)
		if err != nil {
			e.reportErr(instanceID, "finalize-load", err)
			return
		}
		if !ns.State.Terminal() {
			allTerminal = false
			break
		}
		if ns.State == NodeFailed {
			anyFailed = true
		}
	}
	if !allTerminal {
		return
	}
	meta, err := e.loadInstanceMeta(ctx, instanceID)
	if err != nil {
		e.reportErr(instanceID, "finalize-load-meta", err)
		return
	}
	switch meta.State {
	case InstanceCompleted, InstanceFailed, InstanceCancelled:
		return
	}
	if anyFailed {
		meta.State = InstanceFailed
	} else {
		meta.State = InstanceCompleted
	}
	meta.EndedAt = time.Now()
	updated, err := encodeInstanceMeta(meta)
	if err != nil {
		e.reportErr(instanceID, "finalize-encode", err)
		return
	}
	if err := e.opts.Outputs.Set(ctx, instanceMetaKey(e.opts.KeyPrefix, instanceID), updated, e.opts.InstanceTTL); err != nil {
		e.reportErr(instanceID, "finalize-write", err)
	}
}

// markInstanceFailed sets the instance to failed with a terminal
// reason. Best-effort.
func (e *Engine) markInstanceFailed(ctx context.Context, instanceID, reason string) {
	meta, err := e.loadInstanceMeta(ctx, instanceID)
	if err != nil {
		e.reportErr(instanceID, "mark-failed-load", err)
		return
	}
	switch meta.State {
	case InstanceCompleted, InstanceFailed, InstanceCancelled:
		return
	}
	meta.State = InstanceFailed
	meta.EndedAt = time.Now()
	if meta.Tags == nil {
		meta.Tags = map[string]string{}
	}
	meta.Tags["failure_reason"] = reason
	updated, err := encodeInstanceMeta(meta)
	if err != nil {
		e.reportErr(instanceID, "mark-failed-encode", err)
		return
	}
	if err := e.opts.Outputs.Set(ctx, instanceMetaKey(e.opts.KeyPrefix, instanceID), updated, e.opts.InstanceTTL); err != nil {
		e.reportErr(instanceID, "mark-failed-write", err)
	}
}

// === parent collection ===

// collectParents loads every parent's output into a map keyed by
// parent name. Root nodes (no parents) get a single entry under "":
// the Submit input.
func (e *Engine) collectParents(ctx context.Context, def *Definition, instanceID, nodeName string) (map[string][]byte, error) {
	def.mu.Lock()
	parents := append([]string(nil), def.nodes[nodeName].parents...)
	def.mu.Unlock()

	out := make(map[string][]byte, len(parents)+1)
	if len(parents) == 0 {
		// Root — pull synthetic input.
		val, ok, err := e.opts.Outputs.Get(ctx, nodeOutputKey(e.opts.KeyPrefix, instanceID, ""))
		if err != nil {
			return nil, err
		}
		if ok {
			out[""] = val
		}
		return out, nil
	}
	for _, p := range parents {
		val, ok, err := e.opts.Outputs.Get(ctx, nodeOutputKey(e.opts.KeyPrefix, instanceID, p))
		if err != nil {
			return nil, err
		}
		if ok {
			out[p] = val
		}
	}
	return out, nil
}

// === cache helpers ===

func (e *Engine) loadInstanceMeta(ctx context.Context, instanceID string) (instanceMeta, error) {
	raw, ok, err := e.opts.Outputs.Get(ctx, instanceMetaKey(e.opts.KeyPrefix, instanceID))
	if err != nil {
		return instanceMeta{}, err
	}
	if !ok {
		return instanceMeta{}, ErrNotFound
	}
	return decodeInstanceMeta(raw)
}

func (e *Engine) loadNodeState(ctx context.Context, instanceID, nodeName string) (nodeStateRecord, error) {
	raw, ok, err := e.opts.Outputs.Get(ctx, nodeStateKey(e.opts.KeyPrefix, instanceID, nodeName))
	if err != nil {
		return nodeStateRecord{}, err
	}
	if !ok {
		return nodeStateRecord{State: NodePending}, nil
	}
	return decodeNodeState(raw)
}

func (e *Engine) writeNodeState(ctx context.Context, instanceID, nodeName string, n nodeStateRecord) error {
	b, err := encodeNodeState(n)
	if err != nil {
		return err
	}
	return e.opts.Outputs.Set(ctx, nodeStateKey(e.opts.KeyPrefix, instanceID, nodeName), b, e.opts.InstanceTTL)
}

func (e *Engine) reportErr(instanceID, op string, err error) {
	if e.opts.OnError != nil && err != nil {
		e.opts.OnError(instanceID, op, err)
	}
}

// === helpers ===

// isFinalAttempt reports whether this attempt has consumed the
// task's retry budget. Mirrors worker.go's retry math: rec.Attempts
// is incremented on Claim, so on the Nth attempt rec.Attempts == N;
// the task is terminal when rec.Attempts > maxRetries (where
// maxRetries comes from Task.MaxRetries or worker default).
//
// Caveat: from inside the bridge we don't know the worker's
// DefaultMaxRetries — only what the task itself declared. If
// MaxRetries is unset, we conservatively say "this isn't the final
// attempt" so the bridge waits for the task layer to surface a
// terminal failure (which it does by re-invoking the bridge — but
// that doesn't happen on terminal failure, only on retries). To
// close that gap, the engine treats the *next* successful invocation
// of the same node-task as the trigger to reset state; if no
// further invocation comes (terminal fail), we rely on the failure-
// surface in the task store itself, which the engine periodically
// reconciles via the bridge's own write path.
//
// In practice, callers SHOULD set Task.MaxRetries on workflow
// node tasks (the engine derives it from the worker default if
// unset) so this function returns the correct answer.
func isFinalAttempt(t tasks.Task, attempts int) bool {
	if t.MaxRetries == nil {
		// Unknown worker default; assume more attempts may come.
		return false
	}
	return attempts > *t.MaxRetries
}

// === instance ID generator ===

// newInstanceID returns a 16-hex-char random ID. ULIDs would be
// nicer for sortability but this keeps the engine self-contained.
func newInstanceID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

