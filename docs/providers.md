# Providers

mkfst started as an HTTP framework. The **`providers/`** tree is a set of
optional, composable Go packages that hang off the core service —
batteries for the things that an API server typically also needs:
caches, background jobs, container orchestration, virtual filesystems,
DAG workflows, and a TypeScript task runtime.

Each provider is independently importable. Use only what you need; the
HTTP server doesn't pay for any of them unless you wire them in.

```
providers/
  cache/             Cache (in-mem / Redis / SQL)
  docker/            Docker engine wrapper
  docker/network/    Stacks (Compose-like) on top of providers/docker
  files/             File operations against a VFS
  tasks/             Background job server (in-mem / Redis / SQL)
  ts/                TypeScript workflow subsystem
  vfs/               In-memory filesystem with FUSE mount + host overlay
  workflows/         DAG-based job orchestration over providers/tasks
```

## Map

| Provider | Use when you need... | Backends | Doc |
|---|---|---|---|
| **cache** | Pluggable key/value cache (response cache, computed-result cache, anything ephemeral) | memory (LRU), Redis/Valkey, Postgres/MySQL/SQLite | [cache.md](cache.md) |
| **tasks** | Background jobs, scheduled work, recurring jobs (cron) | memory, Redis/Valkey, Postgres/MySQL/SQLite | [tasks.md](tasks.md) |
| **workflows** | DAG of tasks with parent-output flow, fan-out/fan-in, per-node failure policies | layered on `tasks` + `cache` | [workflows.md](workflows.md) |
| **docker** | Pull / build / run / inspect containers from Go | Docker daemon (rootful or rootless) | [docker.md](docker.md) |
| **docker/network** (Stacks) | Compose-like multi-container apps with isolated networks, ingress, health probes, per-stack DNS | Docker daemon + in-process gateway | [stacks.md](stacks.md) |
| **vfs** | In-memory VFS with FUSE mount, host-overlay caching, sync to/from containers | pure Go (Linux: go-fuse; macOS/Windows: cgofuse) | [vfs.md](vfs.md) |
| **files** | High-level file API on top of VFS | n/a | [files.md](files.md) |
| **ts** | TypeScript-authored workflows submitted to a server, run in a sandboxed JS runtime | esbuild + wazero + QuickJS-NG WASM | [TYPESCRIPT_TASKS.md](TYPESCRIPT_TASKS.md) |

## How they layer

```
                       ┌────────────────────────────────┐
                       │  ts (TypeScript workflows)     │
                       └──┬──────────────┬──────────────┘
                          │              │
                  ┌───────▼─────┐   ┌────▼───────────┐
                  │ workflows   │   │ stacks         │
                  └───────┬─────┘   └────┬───────────┘
                          │              │
            ┌─────────────▼──────┐  ┌────▼──────┐
            │  tasks             │  │  docker   │
            └────────┬───────────┘  └───────────┘
                     │
                ┌────▼────┐
                │  cache  │  (also used directly by user code)
                └─────────┘
```

- **cache** is the leaf — every higher layer can use it for ephemeral
  byte storage; user code can use it directly too.
- **tasks** is the job runtime — workflows is built on top of it, and
  user code can enqueue jobs directly.
- **workflows** layers a DAG on top of `tasks`, using `cache` for
  per-node output passing.
- **docker** is the container primitive.
- **stacks** (`docker/network`) layers Compose-like service orchestration
  on top of `docker`.
- **ts** layers TypeScript-authored workflow submission on top of
  `workflows` + `stacks`.

## Composing with the HTTP server

The HTTP server (`mkfst/service`) is independent. Wire providers in as
you would any Go dependency:

```go
package main

import (
    "context"
    "database/sql"

    "github.com/gin-gonic/gin"
    "mkfst/config"
    "mkfst/fizz"
    "mkfst/providers/cache"
    "mkfst/providers/tasks"
    "mkfst/service"
)

func main() {
    svc := service.Create(config.Config{Host: "localhost", Port: 8080})

    // Per-process cache + task store + worker.
    c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 64 << 20})
    store := tasks.NewMemoryStore(tasks.MemoryOpts{})
    worker, _ := tasks.NewWorker(tasks.WorkerOpts{Store: store, Concurrency: 4})
    scheduler := tasks.NewScheduler(store)

    // Run the worker for the lifetime of the process.
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    go func() { _ = worker.Run(ctx) }()

    // Register the email task.
    _ = worker.Register("send-email", func(ctx context.Context, t tasks.Task) error {
        // ... actually send the email ...
        return nil
    })

    // HTTP route enqueues a job.
    svc.Route("POST", "/users", 202,
        []fizz.OperationOption{fizz.Summary("Create user, send welcome async")},
        func(c *gin.Context, _ *sql.DB, in *struct {
            Email string `json:"email" validate:"required,email"`
        }) (struct{ Queued string }, error) {
            rec, err := scheduler.Enqueue(c.Request.Context(), tasks.Task{
                Type:    "send-email",
                Payload: []byte(in.Email),
            })
            if err != nil {
                return struct{ Queued string }{}, err
            }
            return struct{ Queued string }{Queued: rec.Task.ID}, nil
        },
    )

    svc.Run()
}
```

Same pattern for every provider — construct it, share it via Go closures
or a small DI struct, call into it from your handlers.

## Examples

See [`examples/`](../examples/) for end-to-end runnable demos:

- [`09-cache`](../examples/09-cache) — API with response caching
- [`10-tasks`](../examples/10-tasks) — API enqueuing background jobs
- [`11-workflows`](../examples/11-workflows) — API submitting DAG workflows
- [`12-stacks`](../examples/12-stacks) — API managing a docker stack
- [`13-ts-workflows`](../examples/13-ts-workflows) — API accepting TypeScript workflows

Each example is self-contained and runnable from the repo root.
