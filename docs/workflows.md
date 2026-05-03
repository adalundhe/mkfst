# Workflows (DAG)

`providers/workflows` layers a **directed acyclic graph** of tasks on
top of `providers/tasks`, with parent-output flow stored in
`providers/cache`. Use it when you need fan-out / fan-in, per-node
retries, per-node failure policies, and chained outputs — anywhere a
flat task queue isn't expressive enough.

## Concepts

- **Definition**: a static DAG. Built via `New(name)` + `Add(node, opts...)`.
  Pure data — no runtime state lives in the definition.
- **Engine**: drives instances of definitions. Wraps a `tasks.Scheduler` +
  `tasks.Worker` + `cache.Cache`.
- **Instance**: one running execution of a definition, identified by an
  ID returned from `Submit`.
- **Node**: one position in the DAG. Has a task `Type`, parents,
  optional `OnFail` policy, optional retry/priority overrides.
- **Handler**: `func(ctx, parents map[string][]byte) ([]byte, error)`.
  Receives parent outputs by name, returns this node's output bytes.
- **FailPolicy** per node: `FailHaltWorkflow` (default), `FailSkipDownstream`,
  or `FailContinue`.

## Construction

```go
import "mkfst/providers/workflows"

def := workflows.New("smoketest")
seed := def.MustAdd("seed", workflows.OfType("seed-task"))
load := def.MustAdd("loadtest",
    workflows.OfType("k6-task"),
    workflows.DependsOn(seed),
    workflows.OnFail(workflows.FailHaltWorkflow),
)
def.MustAdd("verify",
    workflows.OfType("verify-task"),
    workflows.DependsOn(load),
)
```

`MustAdd` panics on programmer errors (empty / duplicate name).
`Add` is the error-returning variant.

## Engine

```go
eng, err := workflows.NewEngine(workflows.EngineOpts{
    Scheduler: tasks.NewScheduler(store),
    Worker:    worker,
    Outputs:   cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 256 << 20}),
})

// Validate + register the DAG.
_ = eng.Register(def)

// Bind one Go handler per task type.
_ = eng.RegisterHandler("seed-task", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
    return []byte("seeded"), nil
})
_ = eng.RegisterHandler("k6-task", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
    seedOut := parents["seed"] // bytes from upstream
    return runK6(seedOut)
})
_ = eng.RegisterHandler("verify-task", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
    return verify(parents["loadtest"])
})
```

## Submitting + inspecting

```go
id, err := eng.Submit(ctx, "smoketest", []byte("optional initial input"))
// → poll inspect, or subscribe to events.

info, _ := eng.Inspect(ctx, id)
// info.State: pending | running | completed | failed | cancelled
// info.Nodes: per-node states + attempts + errors
```

## Failure policies

| Policy | What happens on a node's exhausted-retry failure |
|---|---|
| `FailHaltWorkflow` (default) | Whole workflow marked failed; sibling and downstream nodes skipped |
| `FailSkipDownstream` | Failed node + its descendants skipped; sibling branches continue independently; instance is failed iff any branch failed |
| `FailContinue` | Failed node treated as completed-with-empty-output; downstream nodes still run |

## Output flow

Each node's handler returns `[]byte`. The engine writes the bytes into
the configured `cache.Cache` under a key like
`wf:inst:<id>:node:<name>:out`. Children's `parents` map is built by
reading those keys back. Outputs are JSON-friendly but binary-safe.

For root nodes, the optional `Submit(input)` arg is exposed via
`parents[""]`.

## Cancellation

`Engine.Cancel(ctx, instanceID)` marks the instance cancelled. In-flight
handlers are not interrupted (the underlying tasks layer's contract is
"cancellation is a hint, not an interrupt"), but their completion will
not enqueue successors.

## Cleanup

`Engine.Cleanup(ctx, instanceID)` deletes every cache key owned by the
instance. Call after readers have consumed final outputs. Optional
`InstanceTTL` on the engine causes outputs to expire automatically.

## Cooperative multi-engine

Multiple processes can run an `Engine` against the same store + outputs
cache. Advancement is safe because successor enqueueing uses
`UniqueKey` dedup at the tasks layer. No leader election needed.

## Composing with the HTTP server

Submit + inspect via REST:

```go
svc.Route("POST", "/workflows/:name", 202, nil,
    func(c *gin.Context, _ *sql.DB, in *struct {
        Name string `path:"name"`
    }) (struct{ Instance string }, error) {
        id, err := wfEng.Submit(c.Request.Context(), in.Name, nil)
        return struct{ Instance string }{Instance: id}, err
    },
)

svc.Route("GET", "/workflows/instances/:id", 200, nil,
    func(c *gin.Context, _ *sql.DB, in *struct {
        ID string `path:"id"`
    }) (workflows.InstanceInfo, error) {
        return wfEng.Inspect(c.Request.Context(), in.ID)
    },
)
```

See [`examples/11-workflows`](../examples/11-workflows).

## See also

- [tasks.md](tasks.md) — the underlying job runtime
- [cache.md](cache.md) — output storage
- [TYPESCRIPT_TASKS.md](TYPESCRIPT_TASKS.md) — TS-authored workflows
