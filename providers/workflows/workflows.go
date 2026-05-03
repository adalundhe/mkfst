// Package workflows is mkfst's DAG-based job orchestration.
//
// A Definition is a static, code-resident description of a workflow:
// nodes (each runs a registered task type) and edges (dependencies
// between nodes). At runtime, callers Submit instances of a definition;
// the Engine advances each instance by enqueueing tasks via
// providers/tasks as their parent nodes complete, storing intermediate
// outputs in providers/cache so downstream handlers can consume them.
//
// Defaults align with the choices documented in the design proposal:
//
//   - In-memory cache for outputs by default; opt in to a persisted
//     cache (Redis/SQL) via EngineOpts.Outputs.
//   - Per-node failure policy: HaltWorkflow (cancel siblings + downstream)
//     by default; per-node OnFail option overrides.
//   - Cancellation propagates to running tasks via the existing per-task
//     cancel-as-hint mechanism.
//   - Cooperative multi-engine: multiple processes can run an engine
//     against the same Store + Outputs cache; advancement is safe
//     because node-completion → successor-enqueue uses dedup-on-enqueue
//     UniqueKeys.
//
// Static DAG only in v1 — conditional and dynamically-spawned edges
// can be added later without a breaking change to the public surface.
package workflows

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Definition is a workflow blueprint: a named DAG of nodes. Build via
// New() + Add(). Definitions are pure values — no state lives in them
// at runtime; that's all on the Engine and the cache.
type Definition struct {
	mu    sync.Mutex
	name  string
	nodes map[string]*node
	order []string // declaration order, for stable validation iteration
}

type node struct {
	name       string
	taskType   string
	parents    []string
	failure    FailPolicy
	priority   int8
	maxRetries *int // nil = use engine default (0 — workflow nodes default to no retry)
}

// NodeRef is the handle returned by Definition.Add. Use it as the
// argument to DependsOn so the dependency graph wires up by reference
// rather than by string lookup. (DependsOn(name string) is also
// available via DependsOnByName when you have the name as a string.)
type NodeRef struct {
	def  *Definition
	name string
}

// FailPolicy controls what happens when a node's handler returns an
// error and retries are exhausted.
type FailPolicy int

const (
	// FailHaltWorkflow (default): the failed node marks the entire
	// workflow as failed. Sibling nodes finish naturally; downstream
	// nodes are never enqueued.
	FailHaltWorkflow FailPolicy = iota
	// FailSkipDownstream: only this node and its downstream are
	// marked failed/skipped. Sibling branches continue independently.
	// The workflow's terminal state is "failed" if any branch failed,
	// "completed" only if every branch finished successfully.
	FailSkipDownstream
	// FailContinue: failure is recorded on the node, but downstream
	// nodes still run as if the failed node had completed (with an
	// empty output). Use when downstream can tolerate missing input
	// — e.g., best-effort analytics nodes.
	FailContinue
)

// New starts a Definition builder. name uniquely identifies the
// definition within an Engine; Submit refers to it by name.
func New(name string) *Definition {
	return &Definition{
		name:  name,
		nodes: make(map[string]*node),
	}
}

// Name returns the definition's identifier.
func (d *Definition) Name() string { return d.name }

// Add appends a node to the definition. nodeName is the node's
// identifier within the workflow (must be unique within the
// definition). Use NodeOption funcs to configure the task type,
// dependencies, failure policy, etc.
//
// Returns a NodeRef so subsequent Add calls can express dependencies
// by reference rather than by string lookup. For Go code paths
// where the definition shape is statically known correct, MustAdd
// gives the same call ergonomics with panic-on-error.
//
// Returns an error on programmer errors (empty name, duplicate
// name).
func (d *Definition) Add(nodeName string, opts ...NodeOption) (*NodeRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if nodeName == "" {
		return nil, errors.New("workflows.Add: nodeName must not be empty")
	}
	if _, exists := d.nodes[nodeName]; exists {
		return nil, fmt.Errorf("workflows.Add: node %q already declared in workflow %q", nodeName, d.name)
	}
	n := &node{name: nodeName, failure: FailHaltWorkflow}
	for _, opt := range opts {
		opt(n)
	}
	d.nodes[nodeName] = n
	d.order = append(d.order, nodeName)
	return &NodeRef{def: d, name: nodeName}, nil
}

// MustAdd is the panic-on-error variant for definition-building
// code that knows its shape is correct (tests, workflow builders
// where invariants are externally enforced).
func (d *Definition) MustAdd(nodeName string, opts ...NodeOption) *NodeRef {
	ref, err := d.Add(nodeName, opts...)
	if err != nil {
		panic(err)
	}
	return ref
}

// NodeOption is a functional option for Definition.Add.
type NodeOption func(*node)

// OfType sets the task type the node runs. The Engine routes to the
// handler registered under this type via Engine.RegisterHandler.
// Required — a node without a task type can't run.
func OfType(taskType string) NodeOption {
	return func(n *node) { n.taskType = taskType }
}

// DependsOn declares this node's parents. The Engine waits for every
// parent to reach a terminal state before evaluating this node.
//
// Multiple DependsOn calls accumulate (each ref appends).
func DependsOn(refs ...*NodeRef) NodeOption {
	return func(n *node) {
		for _, r := range refs {
			n.parents = append(n.parents, r.name)
		}
	}
}

// DependsOnByName is the string-keyed alternative to DependsOn for
// callers who have parent names without NodeRef handles (e.g.
// declarative/JSON workflow builders).
func DependsOnByName(names ...string) NodeOption {
	return func(n *node) { n.parents = append(n.parents, names...) }
}

// OnFail overrides the node's failure policy. See FailPolicy
// constants.
func OnFail(policy FailPolicy) NodeOption {
	return func(n *node) { n.failure = policy }
}

// Priority sets the node's task priority. Higher values run first
// when the worker pool has multiple ready tasks competing.
func Priority(p int8) NodeOption {
	return func(n *node) { n.priority = p }
}

// Retries sets the maximum number of retry attempts for this node's
// task. The total attempt budget is N+1 (initial + N retries). If
// not set, the node defaults to zero retries — failures are surfaced
// immediately so the engine can apply the FailPolicy without waiting
// for the worker's general-purpose retry default to exhaust.
//
// Use this for nodes whose handler is genuinely transient-flake
// tolerant (e.g., HTTP calls that benefit from backoff). Leave it
// unset for handlers where retry doesn't change the outcome.
func Retries(n int) NodeOption {
	return func(node *node) {
		v := n
		node.maxRetries = &v
	}
}

// === Validation ===

// Validate checks the DAG for structural problems: cycles, unknown
// parent references, nodes without OfType, unreachable nodes (no path
// from any root), etc. Returns the first problem found, or nil.
//
// Engine.Register calls Validate automatically; users can also invoke
// it themselves during tests to catch issues without needing an
// engine.
func (d *Definition) Validate() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.nodes) == 0 {
		return errors.New("workflows.Validate: definition has zero nodes")
	}
	for _, name := range d.order {
		n := d.nodes[name]
		if n.taskType == "" {
			return fmt.Errorf("node %q is missing OfType", name)
		}
		for _, p := range n.parents {
			if _, ok := d.nodes[p]; !ok {
				return fmt.Errorf("node %q DependsOn unknown node %q", name, p)
			}
			if p == name {
				return fmt.Errorf("node %q DependsOn itself", name)
			}
		}
	}
	if err := d.detectCyclesLocked(); err != nil {
		return err
	}
	return nil
}

// detectCyclesLocked runs Tarjan-style DFS to find any back edge.
// Caller holds d.mu. Reports the cycle as a path string for clarity.
func (d *Definition) detectCyclesLocked() error {
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // finished
	)
	color := make(map[string]int, len(d.nodes))
	stack := []string{}

	var visit func(name string) error
	visit = func(name string) error {
		color[name] = gray
		stack = append(stack, name)
		for _, parent := range d.nodes[name].parents {
			switch color[parent] {
			case white:
				if err := visit(parent); err != nil {
					return err
				}
			case gray:
				// Found a back edge — parent is on our current
				// stack. The cycle is stack[stackIdx(parent):]+parent.
				idx := 0
				for i, s := range stack {
					if s == parent {
						idx = i
						break
					}
				}
				cycle := append([]string{}, stack[idx:]...)
				cycle = append(cycle, parent)
				return fmt.Errorf("workflows.Validate: cycle detected: %s", strings.Join(cycle, " -> "))
			}
		}
		color[name] = black
		stack = stack[:len(stack)-1]
		return nil
	}

	// Iterate in declaration order for stable error output.
	for _, name := range d.order {
		if color[name] == white {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// === Snapshots / introspection ===

// nodeNames returns every node name in declaration order. Caller must
// hold d.mu OR use it in a read-only context after definition is
// built.
func (d *Definition) nodeNames() []string {
	out := make([]string, len(d.order))
	copy(out, d.order)
	return out
}

// roots returns nodes with no parents — the entry points for an
// instance. Sorted alphabetically for deterministic enqueue order.
func (d *Definition) roots() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []string{}
	for _, name := range d.order {
		if len(d.nodes[name].parents) == 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// children returns the names of nodes that depend on the given
// parent. O(N) per call — fine for typical DAG sizes; we precompute
// inside the engine if a workflow has hundreds of nodes.
func (d *Definition) children(parent string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.childrenLocked(parent)
}

func (d *Definition) childrenLocked(parent string) []string {
	out := []string{}
	for _, name := range d.order {
		for _, p := range d.nodes[name].parents {
			if p == parent {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// descendants returns every node reachable from `root` via a chain
// of children, in BFS order. Used by FailSkipDownstream to mark an
// entire subtree skipped after a node fails.
func (d *Definition) descendants(root string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := map[string]struct{}{}
	out := []string{}
	queue := d.childrenLocked(root)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if _, dup := seen[next]; dup {
			continue
		}
		seen[next] = struct{}{}
		out = append(out, next)
		queue = append(queue, d.childrenLocked(next)...)
	}
	sort.Strings(out)
	return out
}
