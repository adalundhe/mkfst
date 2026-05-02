# Architecture

mkfst is a thin opinionated layer over four lower-level packages:

```
                    ┌──────────────────────────────────┐
                    │  service  (the entry point)      │
                    │  - holds Config + Router + OTel  │
                    │  - mounts /api/docs, /openapi.*  │
                    └─────────────┬────────────────────┘
                                  │ wraps
                                  ▼
                    ┌──────────────────────────────────┐
                    │  router  (groups, middleware)    │
                    │  - lazy: builds at Run() time    │
                    └─────────────┬────────────────────┘
                                  │ wraps
                                  ▼
       ┌──────────────────────┐   │   ┌────────────────────────────┐
       │  fizz                │◀──┴──▶│  tonic                     │
       │  - OpenAPI 3 gen     │       │  - reflection-based binder│
       │  - Swagger UI render │       │  - JSON/YAML rendering    │
       └──────────┬───────────┘       └─────────────┬─────────────┘
                  │                                 │
                  ▼                                 ▼
                              gin.Engine
```

This page is a tour through each layer in the order it appears at runtime.

---

## 1. `service.Service`

[`service/service.go`](../service/service.go) is the user-facing entry point.

```go
type Service struct {
    config config.Config
    router *router.Router
    spec   *openapi.Info
    otel   *telemetry.Context
}

func Create(opts config.Config) Service
func (s *Service) AddGroup(group router.Group) *router.Router
func (s *Service) Route(method, path string, status int,
    docs []fizz.OperationOption, handler interface{}) *router.Router
func (s *Service) Group(path, name, description string) *router.Group
func (s *Service) Middleware(handlers ...interface{}) *router.Router
func (s *Service) GetDB() *sql.DB
func (s *Service) ConfigureTracing(cfg *telemetry.TracingConfig)
func (s *Service) Run() error
```

`Create` builds the config, opens the database (unless `SkipDB` is set) and
creates an empty `Router`. None of your handlers have been registered with
Gin yet — that happens lazily in `Run()` via `router.Build()`.

`Run()` does five things, in order:

1. If telemetry was enabled with `ConfigureTracing`, defers `otel.Close()`.
2. Loads `docs/*` as HTML templates and mounts:
   - `GET /api/docs` — renders [`docs/index.tmpl`](index.tmpl) (Swagger UI).
   - `GET /openapi.json` — the spec as JSON.
   - `GET /openapi.yaml` — the spec as YAML.
3. Calls `router.Build()`, which materialises every group and route into Gin.
4. Mounts a `GET /status` liveness probe (returns `"OK"`).
5. Starts an `http.Server` listening on `Config.ToAddress()`.

The server uses Gin's stdlib transport — no custom listener — so you can put
it behind any reverse proxy.

---

## 2. `router.Router` and `router.Group`

[`router/router.go`](../router/router.go) is a *plan*, not a Gin engine.
Calls like `Route`, `Group`, `AddGroup`, `Middleware` only append to slices.
Nothing is registered with Gin until `Build()` runs.

```go
type Router struct {
    Base       *fizz.Fizz
    Db         *db.Connection
    groups     []Group
    routes     []Route
    middleware []interface{}
}

type Group struct {
    router      *Router
    Base        *fizz.RouterGroup
    path, name, description string
    routes      []Route
    middleware  []interface{}
    groups      []*Group
}
```

### Composition rules

- `router.Group(path, name, description)` creates and **registers** a group
  immediately (it has a `Base *fizz.RouterGroup` already).
- `router.CreateGroup(path, name, description)` creates a *detached* group
  with `Base == nil`. You hand it to `AddGroup` later. This is the pattern
  in [`main.go`](../main.go) — define groups as functions, return them, and
  attach in `main`.
- `Group.Group(...)` creates a **nested** group whose final path is
  `parent.path + child.path`. The parent's middleware is inherited, copied
  into the child's slice during `Build`.
- `(*Router).Middleware(...)` and `(*Group).Middleware(...)` accept any
  `interface{}` value — but at `Build` time each is wrapped with
  `tonic.Handler(mw, db, 200)`, so each middleware **must** match a tonic
  handler signature (see [handlers.md](handlers.md)).

### `Build()`

The build process, condensed:

```go
for each top-level group g:    flatten nested groups into router.groups
for each router middleware:    Base.Use(tonic.Handler(mw, db, 200))
for each router route:         Base.Handle(method, path, docs, mappedHandlers...)
for each group g:
    for each middleware in g:  g.Base.Use(tonic.Handler(mw, db, 200))
    for each route in g:       g.Base.Handle(...)
```

Routes are given a UUID-based operation ID via `fizz.ID(...)` so that
duplicates can never collide in the OpenAPI spec.

### What `SkipDB: true` does

When `Config.SkipDB` is true, `router.Create` returns a router with `Db == nil`
and uses a vanilla `gin.New()` engine. Your handlers still receive a
`*sql.DB`, but it will be `nil` — dereferencing it will panic.

---

## 3. `tonic` — the handler wrapper

[`tonic/`](../tonic/) is where the magic happens. `tonic.Handler` accepts an
arbitrary function `h` and returns a `gin.HandlerFunc` that:

1. **Inspects** `h` via reflection to determine input and output types.
   Allowed signatures are described in [handlers.md](handlers.md).
2. On every request:
   - Builds the input struct (if the handler takes one) and runs the
     `BindHook` to populate it from the request body.
   - Walks the input struct's fields, looking for `query:`, `path:` and
     `header:` tags, and populates each via the matching extractor.
   - Validates the populated struct against `validate:` tags using
     [`go-playground/validator/v10`](https://github.com/go-playground/validator).
   - Calls the handler, passing `ctx`, `db` and the bound input.
   - On error, calls the `ErrorHook` (default: 400 + `{"error": "<msg>"}`).
   - On success, calls the `RenderHook` (default: JSON with the configured
     status code).
3. **Registers** the route's metadata (input/output types, status code,
   description, summary, tags) in a package-level `routes` map keyed by a
   per-call UUID. Fizz reads from this map when generating the OpenAPI spec.

### Why `*sql.DB` is wired in this deep

`tonic` was forked specifically to inject `*sql.DB` as the second argument
of every handler, so handlers stay pure functions of `(ctx, db, input)` →
`(output, error)` rather than reaching into a global. If you do not use a
database, just ignore the parameter.

---

## 4. `fizz` — OpenAPI generation

[`fizz/`](../fizz/) is the wI2L/fizz fork that generates an OpenAPI 3 spec
from the tonic route registry. The relevant pieces:

- `fizz.Fizz` wraps a `*gin.Engine` and an `*openapi.Generator`.
- `fizz.RouterGroup` mirrors `gin.RouterGroup` and adds spec metadata.
- `fizz.Handle(...)` looks for tonic-wrapped handlers in the chain, pulls
  their input/output types and registers an `openapi.Operation`.
- `fizz.OperationOption` (alias for `func(*openapi.OperationInfo)`) is the
  option-pattern used to attach summaries, descriptions, deprecation flags,
  extra response codes, security requirements, etc. See
  [openapi.md](openapi.md) for the full list.

The OpenAPI generator never writes to disk; the spec is regenerated on every
`GET /openapi.json|yaml` request, which is fast because it operates on
already-cached reflection data.

---

## 5. `db.Connection`

[`db/db.go`](../db/db.go) is a thin wrapper that picks a driver based on
`Config.Database.Type`:

| `Type`        | Driver                                | DSN style                                  |
| ------------- | ------------------------------------- | ------------------------------------------ |
| `""`/`SQLITE` | `github.com/mattn/go-sqlite3`         | filesystem path (`Host`)                   |
| `MYSQL`       | requires `_ "github.com/go-sql-driver/mysql"` | `user:pass@tcp(host:port)/name`            |
| `POSTGRESQL`  | requires `_ "github.com/jackc/pgx/v5/stdlib"` | `postgres://user:pass@host:port/name?sslmode=disable` |

Every value can be overridden with environment variables — see
[configuration.md](configuration.md) and [database.md](database.md).

The framework imports the SQLite driver itself
(`_ "github.com/mattn/go-sqlite3"` in [main.go](../main.go)). For MySQL or
Postgres, *your* code has to add the blank import.

---

## 6. `telemetry`

[`telemetry/`](../telemetry/) is an OpenTelemetry SDK bootstrapper. Calling
`service.ConfigureTracing(cfg)` installs a tracer provider, a meter provider
and a composite text-map propagator (TraceContext + Baggage). It is **off by
default** — you must opt in. See [telemetry.md](telemetry.md) for the full
config including the stdout exporters that ship with mkfst.

---

## Request lifecycle (full diagram)

```
HTTP request
  │
  ▼
gin.Engine.ServeHTTP
  │
  ▼
Router-level middleware  (each wrapped by tonic.Handler)
  │
  ▼
Group middleware         (each wrapped by tonic.Handler; inherited from parents)
  │
  ▼
Final route handler      (wrapped by tonic.Handler)
  │   ├─ BindHook reads body (JSON or YAML)
  │   ├─ extractors read query / path / header tags
  │   ├─ validator/v10 enforces `validate:` tags
  │   ├─ user code runs
  │   ├─ RenderHook writes the response
  │   └─ ErrorHook handles returned errors
  ▼
HTTP response (JSON, by default)
```
