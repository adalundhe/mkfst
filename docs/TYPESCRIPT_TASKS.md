# TypeScript Tasks & Workflows

mkfst lets you author workflows in **modern TypeScript** while the
runtime — workflow scheduling, container orchestration, network
isolation, observability — stays in Go. End users see only TS. The Go
side is internal infrastructure they never touch.

This document is the load-bearing reference for that subsystem: how
the developer experience works, how the security model is enforced,
how the build and submit pipeline behaves, and where the boundaries
sit between user code, blessed modules, and the host runtime.

---

## 1. Three actors, three trust rings

| Actor | Writes | Sees | Trust |
|---|---|---|---|
| **End user / workflow author** | `*.ts` files using approved npm packages | TypeScript only | Untrusted code; runs in sandbox |
| **Module author** | A blessed npm package (e.g. `mkfst-k6`) with a `mkfst` capability manifest and a clean TS API | TypeScript + `@mkfst/host` private SDK | Trusted to declare capabilities truthfully; reviewed when added to allowlist |
| **mkfst operator** | `mkfst.yaml` server config | YAML + `mkfst` CLI | Owns the trust root: decides which modules are allowed, which stacks exist, what limits apply |

```
┌────────────────────────────────────────────────────────────┐
│  user workflow.ts                                          │  outer ring
│  imports: @mkfst/sdk, mkfst-k6, mkfst-redis, zod           │
└──────────────────────┬─────────────────────────────────────┘
                       │ no host access
                       ▼
┌────────────────────────────────────────────────────────────┐
│  blessed modules                                           │  middle ring
│  mkfst-k6, mkfst-redis, mkfst-playwright, mkfst-stack,     │
│  mkfst-vfs                                                 │
│  may import @mkfst/host (capability-gated)                 │
└──────────────────────┬─────────────────────────────────────┘
                       │ host access via @mkfst/host
                       │ each call checked against the module's
                       │ declared mkfst.capabilities block
                       ▼
┌────────────────────────────────────────────────────────────┐
│  mkfst host bridge (Go)                                    │  inner ring
│  Stack.RunOneShot, Stack.Exec, vfs.read/write, cache,      │
│  log, metrics, trace                                       │
└────────────────────────────────────────────────────────────┘
```

A blessed module **cannot** expand its capability set at runtime. Its
allowed surface is the intersection of:
- what its `package.json` `mkfst.capabilities` declares, and
- what the operator approved in `mkfst.yaml`.

Operators can narrow further (e.g. approve `mkfst-k6` but restrict
its `imageAllowList` to a private mirror).

---

## 2. Server config — `mkfst.yaml`

The operator's only file. Single source of truth for what's allowed
to run.

```yaml
server:
  listen: 0.0.0.0:8443
  tls:
    cert: /etc/mkfst/tls.pem
    key:  /etc/mkfst/tls.key

# What workflow code is allowed to import. Transitive dependencies of
# allowed modules are auto-allowed via the server-side lockfile pinned
# at module-add time.
modules:
  allow:
    - "@mkfst/sdk"           # always required (pure types + DAG builder)
    - "mkfst-k6@^1.0"
    - "mkfst-playwright@^2.0"
    - "mkfst-redis@^1.0"
    - "mkfst-stack@^1.0"
    - "zod@^3.0"             # pure utility — no host access needed
  cache: /var/lib/mkfst/modules

# Stacks are defined here and referenced by name from TS workflows.
# The user never instantiates a Stack — they ask for one by name.
stacks:
  smoketest:
    services:
      web:   { image: "nginx:alpine",    port: 80 }
      cache: { image: "redis:7-alpine",  port: 6379 }
    probes:
      web:   { http: { path: "/", port: 80 } }
      cache: { tcp:  { port: 6379 } }

# Per-tenant or per-workflow caps; defaults applied if absent.
limits:
  maxConcurrentOneShots: 8
  cpuMillicores:         2000
  memoryMB:              1024
  workflowDuration:      "10m"
  bundleSizeKB:          512

# Capability narrowing — operator overrides the module's defaults.
capabilities:
  "mkfst-k6":
    stack.runOneShot:
      imageAllowList: ["myregistry/k6:*"]   # narrower than module default
```

Operators run `mkfst stack apply` to materialize stacks declared in
the YAML. Hot reload via `mkfst config reload` re-applies module
allowlist + capability narrowing without restarting the server.

---

## 3. The end-user experience

### Author

```ts
// smoketest.workflow.ts
import { defineDAG, defineTask } from "@mkfst/sdk";
import { k6 }    from "mkfst-k6";
import { redis } from "mkfst-redis";
import { z }     from "zod";

const ReportSchema = z.object({
  passed: z.boolean(),
  p95Ms:  z.number(),
});

const seed = defineTask({
  name: "seed",
  async run() {
    await redis("cache").set("counter", "0");
  },
});

const loadtest = defineTask({
  name: "loadtest",
  async run() {
    return await k6.run({
      target: "http://web/",
      stack:  "smoketest",
      vus:    20,
      duration: "10s",
    });
  },
});

const verify = defineTask({
  name: "verify",
  parents: { metrics: loadtest },
  async run({ parents }) {
    const r = ReportSchema.parse({
      passed: parents.metrics.p95Ms < 200,
      p95Ms:  parents.metrics.p95Ms,
    });
    if (!r.passed) throw new Error(`p95 ${r.p95Ms}ms > 200ms`);
    return r;
  },
});

export default defineDAG("smoketest", (b) => {
  const s = b.add(seed);
  const l = b.add(loadtest, { dependsOn: [s] });
            b.add(verify,   { dependsOn: [l] });
});
```

No Go. No host primitives. No `runOneShot`. Just verbs the blessed
modules expose.

### Submit

```sh
$ mkfst submit ./smoketest.workflow.ts --server https://mkfst:8443
✓ uploaded
✓ tsc        — 0 errors
✓ allowlist  — all 4 imports approved
✓ bundle     — 12.4 KB, sha256=4f8a…
✓ registered as workflow "smoketest"

$ mkfst run smoketest --server https://mkfst:8443
instance=7e2b4c… state=running
  [seed]     ok    (124ms)
  [loadtest] ok    (10.3s) — 4216 reqs, p95=87ms
  [verify]   ok    (8ms)
state=completed
```

---

## 4. Submission pipeline — what the server does on receipt

```
submit smoketest.workflow.ts
       │
       ▼
1. Parse imports recursively (esbuild metafile).
2. For each resolved import:
     - Is it in modules.allow?                        → yes: continue
     - Is it a transitive dep of an allowed module?   → yes: continue
     - Otherwise: reject with the path that pulled it.
3. Type-check via tsc using only the .d.ts of allowed modules.
4. Bundle (esbuild). Output ES2020. Source maps stripped from
   the production bundle (kept on the server for debugging).
5. AST-validate: forbid eval, dynamic import, Function ctor,
   importing @mkfst/host from non-blessed modules, top-level
   side effects outside defineTask/defineDAG bodies.
6. Hash the bundle (sha256), persist with workflow definition.
7. Eagerly instantiate one runtime instance to verify the bundle
   loads and exports a default DAG.
8. Register tasks + DAG with the workflow engine.
```

Submit-time failures return structured errors:

```json
{
  "error": "ALLOWLIST_VIOLATION",
  "details": {
    "module": "fast-glob",
    "importedFrom": "node_modules/some-helper/dist/index.js",
    "fix": "ask operator to add fast-glob to modules.allow"
  }
}
```

---

## 5. Capability model

Blessed modules declare their host needs in `package.json`:

```jsonc
// mkfst-k6/package.json
{
  "name": "mkfst-k6",
  "version": "1.0.0",
  "main": "dist/index.js",
  "types": "dist/index.d.ts",
  "mkfst": {
    "capabilities": {
      "stack.runOneShot": {
        "imageAllowList": ["grafana/k6:*"],
        "maxTimeoutSec": 600
      },
      "log": true
    },
    "exposes": ["k6.run"]
  }
}
```

When the operator runs `mkfst module add mkfst-k6@1.0`, the server
records:

- The package's source (vendored or registry-fetched, then locked)
- Its declared capabilities
- The narrowed effective capabilities (operator overrides applied)

Each call from the module to `@mkfst/host` is dispatched through the
bridge, which checks the call against the module's effective
capabilities. Overreach (calling something not declared, or with an
arg outside the declared range) is a runtime exception with a clear
diagnostic — never a silent escalation.

### Capability schema

| Capability | Args / narrowing knobs |
|---|---|
| `stack.runOneShot` | `imageAllowList: string[]`, `maxTimeoutSec: number`, `mountAllowList: string[]` (paths/volumes), `envAllowList: string[]` (env keys) |
| `stack.exec` | `serviceAllowList: string[]`, `cmdRegex: string` |
| `stack.address` | `serviceAllowList: string[]` |
| `vfs.read` / `vfs.write` / `vfs.list` | `treeAllowList: string[]`, `pathPrefix: string`, `maxBytes: number` |
| `cache.get` / `cache.set` / `cache.delete` | `keyPrefix: string`, `maxBytes: number`, `ttlMaxSec: number` |
| `http.fetch` | `urlAllowList: string[]` (CIDR or domain pattern), `maxResponseBytes: number` |
| `sql.query` | `connAllowList: string[]`, `queryRegex: string` |
| `log` | `true` (always allow when declared) or `false` (no-op silently) |
| `metrics.observe` | `nameRegex: string` |
| `trace.span` | `true` |

A module with no `mkfst` block has zero host capabilities — pure
utility (zod, lodash, date-fns) lives here.

---

## 6. The two SDK packages

### `@mkfst/sdk` — public

What end users import. Pure types + the DAG builder. No host access.

```ts
// Public surface (excerpt)
export function defineTask<P, R>(opts: {
  name: string;
  parents?: Record<string, TaskHandle>;
  retries?: number;
  timeout?: string;        // "30s", "5m"
  run: (ctx: TaskCtx<P>) => Promise<R>;
}): TaskDefinition<P, R>;

export function defineDAG(
  name: string,
  build: (b: DAGBuilder) => void,
): DAGDefinition;

export interface TaskCtx<P> {
  parents: P;
  ctx: { signal: AbortSignal; deadline: number; instanceId: string };
  log: (level: "debug"|"info"|"warn"|"error", msg: string, fields?: object) => void;
}

export class TaskError extends Error {
  constructor(opts: { code: string; retryable?: boolean; details?: unknown });
}
```

### `@mkfst/host` — private (blessed modules only)

The bridge surface. Importing from a non-blessed package is a
build-time error.

```ts
// Private surface (excerpt)
export const host: {
  stack(name: string): StackHandle;
  vfs(treeName: string): VFSHandle;
  cache: CacheHandle;
  http: HTTPHandle;
  sql: SQLHandle;
  log: LogHandle;
  metrics: MetricsHandle;
  trace: TraceHandle;
};

export interface StackHandle {
  runOneShot(opts: OneShotOpts): Promise<OneShotResult>;
  exec(service: string, replica: number, opts: ExecOpts): Promise<ExecResult>;
  address(service: string): Promise<string>;
  waitHealthy(service: string, timeoutSec: number): Promise<boolean>;
}
```

Both packages ship as type declarations + minimal runtime stubs. The
real implementation lives in the Go runtime; the JS side is a thin
wrapper that emits structured bridge calls.

---

## 7. Runtime architecture

### Engine choice

We build our own thin Go binding on top of **wazero + QuickJS-NG
compiled to WASM**. We do *not* take a dependency on a third-party
QuickJS Go wrapper — load-bearing infrastructure deserves first-party
control. The binding lives at `providers/ts/runtime/quickjs/` and
wraps only the QuickJS C API surface we actually use (~30–50
functions: context lifecycle, eval, value alloc/free, function
registration, promise resolution, exception capture, GC roots,
memory cap, interrupt callback).

Why this stack:

- **wazero**: actively maintained pure-Go WebAssembly runtime
  (Tetrate). Already a transitive dep elsewhere in mkfst. Trivial
  cross-compilation to Linux / macOS / Windows on amd64 / arm64.
- **QuickJS-NG**: actively maintained fork of Bellard's QuickJS
  (https://github.com/quickjs-ng/quickjs). Modern ES2023:
  classes, async/await, Promise, optional chaining, BigInt, ES
  modules, generators, top-level await.
- **Hard sandbox by construction**: each runtime is a wazero
  instance with disjoint linear memory. No FS, no net, no syscalls
  unless we explicitly wire host imports.
- **Promise integration on our terms**: we own the dispatcher; we
  decide how Go-side `context.Context` cancellation maps to JS
  promise rejection.

The QuickJS-NG WASM binary is built once with wasi-sdk and
checked in under `providers/ts/runtime/quickjs/quickjs.wasm`
(reproducible via the build script in
`providers/ts/runtime/quickjs/build/`). The binary is ~1 MB
gzipped.

The runtime is exposed behind an `Engine` interface so the WASM /
QuickJS choice can be swapped without touching call sites:

```go
type Engine interface {
    New(opts EngineOpts) (Runtime, error)
}
type Runtime interface {
    Eval(ctx context.Context, code string, opts EvalOpts) (Value, error)
    RegisterHost(name string, fn HostFunc) error
    SetMemoryLimit(bytes uint64) error
    SetExecutionTimeout(d time.Duration) error
    Reset() error          // clear heap + globals between tasks
    Close() error
}
```

Lockdown of dangerous globals (`eval`, `Function`, dynamic `import`)
happens two ways: AST reject-list at bundle time (the primary
defense) plus deleting the globals at runtime init (defense in
depth).

### Pool

```
EngineOpts {
  Engine:       EngineWazeroQuickJS | EngineGoja
  Workers:      int          // pre-instantiated runtime count
  MaxOutbound:  int          // cap on concurrent host calls per worker
}
```

Pre-instantiated runtimes amortize cold start. Each worker takes one
task at a time; once done, the runtime is reset (heap cleared, host
state cleared) and returned to the pool.

### Bridge dispatch

User TS calls a function on an `@mkfst/host` handle:

```ts
const r = await host.stack("smoketest").runOneShot({...});
```

Under the hood:
1. JS-side stub builds a `BridgeCall { op: "stack.runOneShot", args }`
   message.
2. Calls a single host import: `__mkfst_dispatch(callBytes)`.
3. Go side decodes (msgpack), looks up the calling module's effective
   capabilities, verifies the call is permitted.
4. Executes the underlying Go primitive (`Stack.RunOneShot`).
5. Encodes the result (msgpack) and returns.
6. JS stub wraps in a Promise that resolves with the typed result.

Bytes are kept as bytes — `Uint8Array` ↔ `[]byte` round-trips without
encoding overhead.

### Async via promises

Each in-flight bridge call is a Go-side goroutine that fulfills a JS
promise on completion. Cancellation propagates: `ctx.signal` aborting
in TS triggers the Go ctx of the underlying primitive.

### Resource accounting

- WASM gas metering: tasks billed instructions; over-quota → fault.
- Memory cap per runtime instance (default 64 MB).
- Bridge call rate limit per task (default 1k/s).
- Output size cap (default 10 MiB; oversize streamed to a per-stack
  docker volume the user can `cp` from).

---

## 8. Security posture

| Concern | Defense |
|---|---|
| Arbitrary code escape | esbuild AST reject-list + bridge-only host surface + WASM hard sandbox |
| Module capability creep | manifest declared; checked at module-load and per-call; operator can narrow |
| Image abuse via runOneShot | per-module `imageAllowList`; operator override |
| Secrets exfiltration | secrets injected as tmpfs mounts only; never visible as env in bridge |
| Network exfil from one-shots | egress policy on the spawning stack applies (per-service allow/deny) |
| Resource exhaustion | per-stack `maxConcurrentOneShots`, per-engine worker cap, per-task gas/mem cap |
| WASM escape | wazero is audited; only host imports we explicitly register are reachable |
| Supply chain (npm deps) | server-side lockfile; vendored OR resolve-at-add; no submission-time fetches |
| Bundle tampering | sha256 hash recorded at submit; runtime refuses mismatching bundles |
| Time-of-check-to-time-of-use | capabilities cached at module load; mutated only via `mkfst config reload` which atomically swaps the policy snapshot |

Modules are content-addressed by sha256 of their tarball + locked
dependency tree. Two `mkfst module add mkfst-k6@1.0` invocations on
different servers produce identical hashes.

---

## 9. Module distribution

Two operator-selectable paths:

### Vendored

Operator pre-downloads approved modules into `modules.cache`.
Submissions use only what's there. Deterministic, air-gappable,
auditable.

```sh
$ mkfst module vendor mkfst-k6@1.0
✓ resolved 14 transitive deps
✓ wrote modules/mkfst-k6-1.0.tgz + lockfile
✓ effective capabilities recorded
```

### Resolve-at-add

Operator adds; server fetches once, pins.

```sh
$ mkfst module add mkfst-k6@1.0 --registry https://npm.internal
✓ fetched mkfst-k6@1.0.0 + 14 transitive deps
✓ pinned versions written to /var/lib/mkfst/lockfile.json
✓ subsequent submissions can import "mkfst-k6"
```

After either, the server refuses to load any code outside the
locked set.

---

## 10. Stacks at runtime

Stacks declared in `mkfst.yaml` are materialized as
`providers/docker/network.Stack` instances at server start (or on
`mkfst stack apply`). The TS-side `host.stack(name)` returns a
handle to the named, already-running stack — never creates one
on demand.

This means:
- Stack lifecycle is operator-controlled, not workflow-controlled.
- A workflow can't accidentally spin up a thousand stacks.
- Operators see exactly which stacks exist (`mkfst stack list`)
  and which workflows are pinned to which stacks.

```sh
$ mkfst stack list
NAME        STATE  SERVICES         INGRESS                    UPTIME
smoketest   up     web, cache       127.0.0.1:53281            2h 14m
db-perf     up     postgres, app    127.0.0.1:53415            46m
```

---

## 11. Layout

```
providers/
  docker/network/        unchanged (stacks)
  workflows/             unchanged (DAG engine)
  ts/                    NEW
    server/              HTTP API + workflow registry
    runtime/
      engine.go          Engine interface
      qjs.go             qjs (wazero+QuickJS-NG WASM) engine impl
      bridge.go          host-call dispatch
      pool.go            runtime worker pool
      capability.go      per-module capability check
      lockdown.go        deletes eval/Function/dynamic-import at boot
    bundle/
      esbuild.go         bundle pipeline
      allowlist.go       import-graph walker
      ast_validate.go    eval/dynamic-import reject-list
      tsc.go             type-check invocation
    config/
      yaml.go            mkfst.yaml parser + apply
      lockfile.go        module lockfile manager
    sdk/                 vendored TS sources (npm package builds)
      mkfst-sdk/         @mkfst/sdk package source
      mkfst-host/        @mkfst/host private package source
      mkfst-stack/       reference module: bridge to network.Stack
      mkfst-redis/       reference module: redis cli wrapper
      mkfst-k6/          reference module: k6 runner
      mkfst-playwright/  reference module: playwright runner
cmd/
  mkfst/
    main.go              CLI entrypoint
    serve.go             `mkfst serve`
    submit.go            `mkfst submit ./workflow.ts`
    run.go               `mkfst run <workflow>`
    inspect.go           `mkfst inspect <instance>`
    module.go            `mkfst module add/list/vendor`
    stack.go             `mkfst stack apply/list/inspect`
    config.go            `mkfst config reload`
docs/
  TYPESCRIPT_TASKS.md    this file
```

---

## 12. CLI surface

```
mkfst serve [--config mkfst.yaml]
mkfst submit <file.ts> [--server URL] [--name NAME]
mkfst run <workflow> [--server URL] [--input FILE]
mkfst inspect <instance> [--server URL] [--watch]
mkfst module add <pkg@ver> [--registry URL]
mkfst module vendor <pkg@ver> [--out DIR]
mkfst module list
mkfst stack apply [--config mkfst.yaml]
mkfst stack list
mkfst stack inspect <name>
mkfst stack down <name>
mkfst config reload
```

All subcommands speak a structured-error JSON output mode
(`--json`) for tooling.

---

## 13. HTTP API (between CLI and server)

```
POST /v1/workflows                  multipart: workflow.ts +
                                    optional client-side bundle
                                    cache key
GET  /v1/workflows                  list registered workflows
POST /v1/workflows/{name}/run       trigger; returns instance id
GET  /v1/instances/{id}             read state
GET  /v1/instances/{id}/events      SSE stream
POST /v1/modules                    operator-only; add module
GET  /v1/modules                    list with capabilities
POST /v1/stacks                     operator-only; apply YAML
GET  /v1/stacks                     list
POST /v1/config/reload              operator-only
```

mTLS between CLI and server. Operator-only endpoints require a
client cert with an operator role claim.

---

## 14. Build & test workflow for module authors

A blessed module author publishes a normal npm package + a `mkfst`
manifest block. To test locally:

```sh
$ cd mkfst-myextension
$ npm run build                # tsc
$ mkfst module vendor . --out ./local-vendor
$ mkfst serve --config dev.yaml --modules-cache ./local-vendor
# in another shell:
$ mkfst submit ./test-workflow.ts
```

The capability declarations are validated on `module vendor` —
malformed blocks are caught before they hit a server.

---

## 15. Operator playbook

### Adding a new approved module

```sh
# Review the module's source + capability manifest first.
$ mkfst module inspect mkfst-newthing@1.0
name:           mkfst-newthing
version:        1.0.0
sha256:         a4f2...
capabilities:
  stack.runOneShot:
    imageAllowList: ["myco/newthing:*"]
    maxTimeoutSec:  300
exposes:        ["newthing.run"]

# Approve.
$ mkfst module add mkfst-newthing@1.0
✓ written to lockfile

# (Optionally) narrow the declared capabilities.
$ mkfst config edit
# add under capabilities:
#   "mkfst-newthing":
#     stack.runOneShot:
#       imageAllowList: ["mirror.local/newthing:*"]

$ mkfst config reload
```

### Decommissioning a module

```sh
$ mkfst module remove mkfst-oldthing
✗ in use by 3 workflows: foo, bar, baz
$ mkfst module remove mkfst-oldthing --force
✓ workflows foo, bar, baz disabled (re-submit needed)
```

---

## 16. Open evolution paths

- **Real Node.js sidecar** as a third runtime engine for users who
  need full npm at runtime (today, the only npm reachable is what's
  bundled at submit time). Process-isolated, IPC-bridged. Off by
  default.
- **gRPC streaming** for very long-running tasks where polling
  /events SSE is too coarse.
- **Multi-tenant per-engine isolation**: today one mkfst server runs
  one allowlist; tenancy = separate processes. Multi-tenant in one
  process via per-tenant policy snapshots is plausible but adds
  meaningful complexity to the bridge — deferred.
- **Live module updates without bundle re-submit**: when a blessed
  module bumps a non-breaking patch, currently every workflow must
  re-bundle. A "policy version per workflow" indirection could let
  the server transparently pick up the new module — but breaks
  bundle hash determinism. Tradeoff deferred.
- **WASM gas budget per-tenant**: today we cap per-task; per-tenant
  fairness across tasks is a follow-up.
