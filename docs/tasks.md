# Tasks (Background Jobs)

`providers/tasks` is mkfst's pluggable background-job system. One API
surface, three backends:

- **In-memory** (`NewMemoryStore`) — single-process, no persistence, ideal
  for tests + single-binary deployments.
- **Redis / Valkey** (`NewRedisStore`) — distributed, atomic Lua-script
  claims, fast.
- **SQL** (`NewSQLStore`) — durable on Postgres / MySQL / SQLite via
  mkfst's existing `db.Connection`. Postgres adds `LISTEN/NOTIFY` for
  low-latency wakeups; MySQL/SQLite poll.

Every backend passes the same conformance suite. Switching is a one-line
change.

## Concepts

| Concept | What it is |
|---|---|
| `Task` | A unit of work — `Type` (handler name), `Payload` ([]byte), `Queue`, `Priority`, `MaxRetries`, `Timeout`, `Deadline`, `DelayUntil`, `UniqueKey`, `Tags` |
| `Handler` | `func(ctx context.Context, t Task) error` — registered against a `Type` on a `Worker` |
| `Scheduler` | The enqueue-side API: `Enqueue`, `EnqueueIn`, `EnqueueAt`, `Cancel`, `Inspect` |
| `Worker` | The runtime — pulls tasks from the store, dispatches handlers, applies retries with backoff |
| `Recurring` | Cron / interval schedules with dedup-on-enqueue (no leader election) |
| `Store` | The backend (memory / Redis / SQL); satisfies all the atomicity and visibility-timeout guarantees in `providers/tasks/store.go` |

## Correctness model

- **At-least-once** delivery by default. Handlers must be idempotent.
  Combine with `Task.UniqueKey` for dedup-on-enqueue.
- **Visibility timeouts**: if a worker dies mid-task, another worker
  re-claims after the timeout. Long handlers should let the engine
  heartbeat for them.
- **Conflict-free recurring schedules**: dedup-on-enqueue means
  multiple processes can run the recurring scheduler simultaneously
  without double-firing.

## Minimal usage

```go
import (
    "context"
    "time"

    "mkfst/providers/tasks"
)

func main() {
    store := tasks.NewMemoryStore(tasks.MemoryOpts{})
    defer store.Close()

    worker, err := tasks.NewWorker(tasks.WorkerOpts{
        Store:       store,
        Concurrency: 8,
    })
    if err != nil { /* handle */ }

    _ = worker.Register("send-email", func(ctx context.Context, t tasks.Task) error {
        // t.Payload is the email address; idempotent send.
        return sendEmail(string(t.Payload))
    })

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    go func() { _ = worker.Run(ctx) }()

    sched := tasks.NewScheduler(store)
    _, _ = sched.Enqueue(ctx, tasks.Task{
        Type:       "send-email",
        Payload:    []byte("alice@example.com"),
        UniqueKey:  "welcome:alice@example.com",
        MaxRetries: tasks.Retries(3),
        Timeout:    30 * time.Second,
    })
}
```

## Scheduling

```go
// Run later.
sched.EnqueueIn(ctx, 5*time.Minute, tasks.Task{Type: "remind", Payload: []byte("user-42")})
sched.EnqueueAt(ctx, time.Now().Add(time.Hour), tasks.Task{...})

// Cancel a pending task.
_ = sched.Cancel(ctx, "task-id")
```

## Recurring (cron)

mkfst vendors a stripped cron parser at `providers/tasks/cron` (Quartz-
compatible — supports `L`, `W`, `#` modifiers).

```go
rs, _ := tasks.NewRecurringScheduler(tasks.RecurringOpts{
    Store: store,
    Tick:  time.Second,
})

_ = rs.Cron("nightly-vacuum", "0 3 * * *", tasks.Task{
    Type:    "vacuum",
    Payload: []byte("schema_a"),
})
_ = rs.Every("heartbeat", 30*time.Second, tasks.Task{Type: "heartbeat"})

go func() { _ = rs.Run(ctx) }()
```

Multiple processes can run the same `RecurringScheduler` against the same
store — dedup-on-enqueue guarantees no double-firing.

## Worker tuning

```go
tasks.WorkerOpts{
    Store:               store,
    Queues:              []string{"default", "low-priority"},
    Concurrency:         16,                 // in-flight tasks per worker
    VisibilityTimeout:   30 * time.Second,   // claim lease
    HeartbeatInterval:   10 * time.Second,
    DefaultTimeout:      5 * time.Minute,    // per-attempt
    DefaultMaxRetries:   5,
    PollInterval:        25 * time.Millisecond,
    MaintenanceInterval: 250 * time.Millisecond,
    Backoff:             tasks.Backoff,      // exponential with full jitter, default
    OnError:             func(workerID, op string, err error) { log.Printf("...") },
    Telemetry:           tasks.NewTelemetryFromGlobals(),
}
```

## Telemetry

If you pass a `*Telemetry`, the worker emits OTEL spans on every
handler invocation, plus counters / histograms for claim / complete /
fail / retry / cancel events. Cheap when nil.

## Composing with the HTTP server

```go
svc.Route("POST", "/jobs", 202,
    nil,
    func(c *gin.Context, _ *sql.DB, in *struct {
        Type string `json:"type" validate:"required"`
        Body string `json:"body"`
    }) (struct{ ID string }, error) {
        rec, err := scheduler.Enqueue(c.Request.Context(), tasks.Task{
            Type:    in.Type,
            Payload: []byte(in.Body),
        })
        return struct{ ID string }{ID: rec.Task.ID}, err
    },
)
```

See [`examples/10-tasks`](../examples/10-tasks).

## See also

- [workflows.md](workflows.md) — DAG of tasks with parent-output flow
- [TYPESCRIPT_TASKS.md](TYPESCRIPT_TASKS.md) — TS-authored workflows
