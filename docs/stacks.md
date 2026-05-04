# Stacks (Compose-like multi-container apps)

`providers/docker/network` (the "Stacks" submodule) provides
Compose-style multi-container orchestration on top of
`providers/docker`. A **Stack** is a named bundle of **Services**
running on a private bridge network, with operator-controlled
ingress, per-service health probes, optional egress controls, and
crash-recovery via labeled resources.

This is the unit a TypeScript workflow targets (see
[TYPESCRIPT_TASKS.md](TYPESCRIPT_TASKS.md)).

## Concepts

| Concept | What it is |
|---|---|
| `Engine` | Process-wide owner — holds the docker client, stack registry, shared probe scheduler |
| `Stack` | A named group of `Service`s on its own bridge network |
| `Service` | A containerized program with image / env / cmd / volumes / depends-on / replicas / probe / restart policy |
| `Probe` | TCP / HTTP / UDP / gRPC / Exec — readiness or liveness mode |
| `Ingress` | A host-side entrypoint into the stack — in-process gateway with rules + monitoring |
| `Monitor` | Per-stack lossy event stream: connections accepted/denied/closed, probe failures, service restarts, internal errors |
| `RunOneShot` | Spawn an ephemeral container on the stack's network for one task (workflow-driven test runs, migrations, etc.) |
| `Exec` | Run a command inside an existing service replica |

## Lifecycle

State machine: `Down → Creating → Created → Starting → Up → Stopping → Down`,
with `Failed` as a terminal state for rollback. Two-phase atomic Up:
create-all → start-all-in-order. Any phase failure rolls back to a
clean Down.

```go
import "mkfst/providers/docker/network"

eng, _ := network.NewEngine(cli.SDK(), network.EngineOpts{})
defer eng.Close(ctx)

stack, _ := eng.NewStack("smoketest")
stack.MustAddService("web",
    network.Image("nginx:alpine"),
    network.Port(80),
    network.WithProbe(
        network.HTTPProbe(80, "/").WithInterval(200*time.Millisecond),
        network.ProbeReadiness,
    ),
)
stack.MustAddService("cache",
    network.Image("redis:7-alpine"),
    network.Port(6379),
    network.WithProbe(network.TCPProbe(6379), network.ProbeReadiness),
)

if err := stack.Up(ctx); err != nil {
    log.Fatal(err)
}
defer stack.Down(ctx)
```

## Isolation

Each stack lives on its own bridge network. Containers in stack A
cannot DNS-resolve or route to containers in stack B. Outbound NAT
to the internet is preserved (services can reach external Valkey /
DBs / APIs). When using TS workflows, the workflow→stack binding
adds another scope layer (see TYPESCRIPT_TASKS.md).

## Probes

```go
network.TCPProbe(6379)
network.HTTPProbe(80, "/healthz").WithExpectStatus(200)
network.UDPProbe(53, []byte("\x00\x01"))
network.GRPCProbe(50051, "")
network.ExecProbe("pg_isready", "-U", "postgres")
```

Modes:
- **Readiness**: blocks `Up` until first success; doesn't run after that.
- **Liveness**: continuous; consecutive failures (`FailureThreshold`)
  trigger container restart per the service's `RestartPolicy`.

Probes share a process-wide min-heap scheduler with a bounded worker
pool (`EngineOpts.ProbeWorkers`, default 256). Intervals are jittered
per service to avoid synchronized polling at scale.

## Ingress + Gateway

In-process Go gateway. Pure Go, no sidecar containers. Supports TCP,
UDP, HTTP, HTTPS (with optional mTLS).

```go
ing, err := stack.Ingress("web-in", "web", 80,
    network.AllowSource("10.0.0.0/8"),
    network.DenySource("10.0.0.66/32"),
    network.MaxConcurrent(5000),
    network.MaxNewPerSecondPerSource(50),
    network.LoadBalancer(network.LBConsistentHash),
    network.EnableCircuitBreaker(0.5, 5*time.Second, 1),
)

// After Up, the gateway is bound to an ephemeral host port.
log.Println("ingress at", ing.Address())
```

`BindAddress("127.0.0.1:8080")` for an explicit binding (collision
returns an error). Otherwise `127.0.0.1:0` — kernel-assigned ephemeral
port, so multiple stacks declaring `Ingress("web", 80)` get distinct
host ports automatically (k8s ClusterIP semantic without OS hacks).

## Monitor

```go
m := stack.Monitor()
for ev := range m.Events() {
    switch ev.Kind {
    case network.EventConnectionAccepted:
        log.Printf("conn from %s", ev.SourceAddr)
    case network.EventConnectionDenied:
        log.Printf("denied %s: %s", ev.SourceAddr, ev.DenyReason)
    case network.EventProbeFailed:
        log.Printf("probe failed: %s", ev.Error)
    case network.EventInternalError:
        log.Printf("stack internal: %s", ev.Error)
    }
}
```

The channel is lossy under bursty traffic (drops past buffer,
counted via `m.Dropped()`); see TYPESCRIPT_TASKS.md for ordering
guarantees.

## RunOneShot + Exec

Used by workflows to drive the stack:

```go
res, err := stack.RunOneShot(ctx, network.OneShotOpts{
    Image:   "grafana/k6:0.49",
    Cmd:     []string{"run", "-"},
    Stdin:   []byte(k6Script),
    Timeout: 60 * time.Second,
})
log.Printf("k6 exit=%d stdout=%dB", res.ExitCode, len(res.Stdout))

execRes, err := stack.Exec(ctx, "cache", 0, network.ExecOpts{
    Cmd: []string{"redis-cli", "PING"},
})
```

One-shots join the stack's network so they reach services by name
(`http://web/`, `redis:6379`). They're labeled with the stack ID +
`mkfst.role=oneshot` so crash recovery can find and reap them.

`Stack.SetMaxConcurrentOneShots(n)` caps the in-flight count to
prevent daemon overload.

## Crash recovery

Every docker resource the network module creates carries
`mkfst.engine=<id>` + `mkfst.stack=<id>` labels. On engine restart:

```go
result, _ := eng.Adopt(ctx, network.AdoptOpts{EngineID: "my-stable-id"})
// rebuilds Stack handles for prior process's resources

_ = eng.Reap(ctx, network.AdoptOpts{StackID: "abandoned-stack-id"})
// destroys all resources of a stack (containers + network)
```

## Composing with the HTTP server

```go
svc.Route("POST", "/stacks/:name/up", 200, nil,
    func(c *gin.Context, _ *sql.DB, in *struct {
        Name string `path:"name"`
    }) (struct{ State string }, error) {
        s, _ := eng.NewStack(in.Name)
        // ... define services from request body ...
        if err := s.Up(c.Request.Context()); err != nil {
            return struct{ State string }{}, err
        }
        return struct{ State string }{State: s.State().String()}, nil
    },
)
```

See [`examples/12-stacks`](../examples/12-stacks).

## Cross-platform

Linux (rootful + rootless), macOS, Windows. Loopback ephemeral binding
works identically on all three. Per-stack DNS resolver is best-effort
on platforms where binding port 53 to the bridge gateway requires
privilege.

## See also

- [docker.md](docker.md) — underlying container primitives
- [TYPESCRIPT_TASKS.md](TYPESCRIPT_TASKS.md) — TS workflows targeting stacks
- [vfs.md](vfs.md) — VFS-backed mounts in stack services
