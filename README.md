# mkfst

> **mk**ake **f**a**st** — an opinionated Go web framework that turns a
> typed handler signature into a routed endpoint, an OpenAPI 3 operation,
> and a Swagger UI entry, with a database connection, JWT authentication
> and OpenTelemetry waiting to be plugged in.

```go
svc.Route("GET", "/users/:id", 200, nil,
    func(ctx *gin.Context, db *sql.DB, in *struct {
        ID int `path:"id" validate:"required,min=1"`
    }) (User, error) {
        return loadUser(db, in.ID)
    },
)
```

That single call gives you:

- a Gin route at `GET /users/:id`,
- request binding + validation for the path parameter,
- JSON rendering of the returned `User`,
- a `400` response if `id` is missing or `< 1`,
- an OpenAPI 3 operation with the right schemas,
- an entry in the Swagger UI at `/api/docs`.

---

## Table of contents

- [Why mkfst?](#why-mkfst)
- [Feature overview](#feature-overview)
- [Install](#install)
- [Hello world](#hello-world)
- [The handler contract](#the-handler-contract)
- [Routing & groups](#routing--groups)
- [Built-in middleware](#built-in-middleware)
- [Database](#database)
- [OpenAPI & Swagger UI](#openapi--swagger-ui)
- [Authentication](#authentication)
- [OpenTelemetry](#opentelemetry)
- [Configuration & environment variables](#configuration--environment-variables)
- [Project layout](#project-layout)
- [Examples](#examples)
- [Documentation](#documentation)
- [Status & versioning](#status--versioning)
- [Acknowledgements](#acknowledgements)
- [License](#license)

---

## Why mkfst?

mkfst exists because three things are usually re-implemented in every Go
backend, and they shouldn't be:

1. **Wiring an HTTP router to typed input/output structs**. Most teams
   end up writing one ad-hoc binder per project.
2. **Keeping OpenAPI in sync with what the code actually does**. Hand-
   maintained specs rot the moment a field is renamed.
3. **Bootstrapping config / database / auth / telemetry**. Every project
   reinvents the same flag parsing and DSN-building logic.

mkfst stitches together [Gin](https://github.com/gin-gonic/gin) (router),
a fork of [tonic](https://github.com/loopfz/gadgeto/tree/master/tonic)
(reflection-based binder), and [fizz](https://github.com/wI2L/fizz)
(OpenAPI generator) and adds opinionated layers for config, DB,
authentication and OpenTelemetry. The idea is that **the handler
signature is the contract** — get the function shape right and the rest
falls out for free.

---

## Feature overview

| Area | What you get |
| ---- | ------------ |
| **Routing** | Declarative groups, nested groups, lazy build, per-group middleware. |
| **Binding** | `path:`, `query:`, `header:`, JSON / YAML body — all from struct tags. |
| **Validation** | `validate:"…"` powered by [`go-playground/validator/v10`](https://github.com/go-playground/validator). |
| **OpenAPI** | OpenAPI 3.0 spec at `/openapi.json`/`.yaml` and Swagger UI at `/api/docs`, generated from your handler types. |
| **Database** | SQLite (built-in), MySQL and PostgreSQL via `Database.Type`. Single `*sql.DB` injected into every handler. |
| **Auth** | JWT + social-login providers (GitHub, Google, Facebook, Microsoft, Apple, Yandex, Twitter, Battle.net, Patreon, Telegram, Dev). Avatar proxy with pluggable storage. |
| **Middleware** | Bundled CORS and OpenTelemetry middleware; plain Gin middleware mounts on `svc.Router.Base.Engine()`. |
| **Telemetry** | OpenTelemetry SDK setup with stdout exporters out of the box; OTLP / Jaeger / Zipkin via the standard exporters. |
| **Config** | One struct, env-var overrides for every field, sensible defaults. |
| **Hooks** | Replace `BindHook`, `RenderHook`, `ErrorHook`, `ExecHook` to switch JSON for msgpack, plug in structured error responses, add panic recovery. |

---

## Install

```bash
go get mkfst@latest
```

Requires Go ≥ 1.21. The default SQLite backend is CGO; if you have
disabled CGO globally, either re-enable it for builds, change the driver
(`Database.Type = "MYSQL"` / `"POSTGRESQL"`), or set `SkipDB: true` and
skip database opening entirely.

---

## Hello world

```go
package main

import (
    "database/sql"

    "github.com/gin-gonic/gin"

    "mkfst/config"
    "mkfst/fizz/openapi"
    "mkfst/service"
)

func main() {
    svc := service.Create(config.Config{
        Host:   "localhost",
        Port:   8080,
        SkipDB: true,
        Spec: openapi.Info{
            Title:   "Hello API",
            Version: "v1.0.0",
        },
    })

    svc.Route("GET", "/hello", 200, nil,
        func(ctx *gin.Context, _ *sql.DB) (string, error) {
            return "hello, world", nil
        },
    )

    svc.Run()
}
```

Run it from the repo root (the framework looks for `docs/index.tmpl`
relative to the working directory to render Swagger UI):

```bash
go run ./examples/01-hello
```

| URL                               | Purpose                          |
| --------------------------------- | -------------------------------- |
| `http://localhost:8081/hello`     | Your handler                     |
| `http://localhost:8081/api/docs`  | Swagger UI                       |
| `http://localhost:8081/openapi.json` | Generated OpenAPI 3 spec      |
| `http://localhost:8081/openapi.yaml` | The same, as YAML             |
| `http://localhost:8081/status`    | Built-in liveness probe (`OK`)   |

---

## The handler contract

Every mkfst handler is a regular Go function that conforms to one of
four shapes. `tonic` reflects over your function at registration time
and rejects anything else with a panic — so you find out at startup,
never at request time.

```go
// minimal
func(*gin.Context, *sql.DB) error

// with a typed response
func(*gin.Context, *sql.DB) (Output, error)

// with a typed input
func(*gin.Context, *sql.DB, *Input) error

// both
func(*gin.Context, *sql.DB, *Input) (Output, error)
```

The first parameter is the Gin context, the second is the framework's
`*sql.DB` (use it or ignore it), and the optional third is a **pointer
to a struct** whose fields are populated from the request.

### Where input fields come from

| Tag      | Source                            | Example                          |
| -------- | --------------------------------- | -------------------------------- |
| `path`   | URL path parameter                | `/users/:id` → `ID int \`path:"id"\``|
| `query`  | URL query string                  | `?limit=10`                      |
| `header` | HTTP header                       | `Authorization: …`               |
| `json`   | Request body (Gin JSON binding)   | the usual `json:"name"`          |
| `yaml`   | Request body when `Content-Type` is YAML | the usual `yaml:"name"`     |

### Tag cheat sheet

| Tag        | Meaning                                                                    |
| ---------- | -------------------------------------------------------------------------- |
| `default`  | Default value if the source provides nothing.                              |
| `enum`     | Comma-separated whitelist; rejected with 400 otherwise.                    |
| `validate` | Forwarded to `validator/v10`: `required,min=1,max=200,email,oneof=a b c`.  |
| `explode`  | `false` = parse `?tag=a,b,c` as a list. Default `true`.                    |

### Worked example

```go
type ListUsersInput struct {
    OrgID   int      `path:"org_id"  validate:"required"`
    Limit   int      `query:"limit"  default:"20" validate:"min=1,max=200"`
    Order   string   `query:"order"  default:"asc" enum:"asc,desc"`
    Tags    []string `query:"tag"    explode:"false"` // ?tag=a,b,c
    TraceID string   `header:"X-Trace-Id"`
}

func ListUsers(ctx *gin.Context, db *sql.DB, in *ListUsersInput) ([]User, error) {
    return store.List(db, in.OrgID, in.Limit, in.Order, in.Tags)
}
```

Returning `nil, nil` produces an empty body with the route's default
status. Returning a non-nil error runs the active `ErrorHook` (default:
400 with `{"error": "<msg>"}`).

---

## Routing & groups

mkfst's router is **declarative**: every `Route`, `Group`, `AddGroup`
and `Middleware` call only buffers metadata. Nothing is registered with
Gin until `service.Run` calls `router.Build`, so you can compose
groups freely from anywhere.

```go
// Define a group as a function ...
func usersGroup() router.Group {
    g := router.CreateGroup("/users", "Users", "User CRUD")
    g.Route("GET",  "/",   200, nil, listUsers)
    g.Route("POST", "/",   201, nil, createUser)
    g.Route("GET",  "/:id", 200, nil, getUser)

    // ... nested group inherits parent middleware and prefix.
    admin := g.Group("/admin", "User admin", "Admin-only operations")
    admin.Middleware(requireAdmin)
    admin.Route("DELETE", "/:id", 204, nil, hardDelete)
    return *g
}

func main() {
    svc := service.Create(...)
    svc.Middleware(requestID)        // every route
    svc.AddGroup(usersGroup())       // attach the group tree
    svc.Run()
}
```

Middleware execution order, for `DELETE /users/admin/42`:

```
service-level mw  →  /users mw  →  /users/admin mw  →  handler
```

Middleware is just another tonic handler — return `nil, nil` to pass,
`ctx.AbortWithStatus(...)` to short-circuit, or return an error to let
the `ErrorHook` write the response.

---

## Built-in middleware

```go
import "mkfst/middleware/cors"

svc.Middleware(cors.CORS(cors.Config{
    AllowOrigins:     []string{"https://app.example.com", "https://*.example.com"},
    AllowMethods:     []string{"GET", "POST", "PATCH", "DELETE"},
    AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
    ExposeHeaders:    []string{"X-Request-Id"},
    AllowCredentials: true,
    AllowWildcard:    true,
    MaxAge:           12 * time.Hour,
}))
```

Use `cors.Default()` for an allow-everything config in development.

For OpenTelemetry:

```go
import otelmw "mkfst/middleware/opentel"

engine := svc.Router.Base.Engine()      // *gin.Engine
engine.Use(otelmw.Middleware("users-api"))
```

The OTel middleware is a plain `gin.HandlerFunc` (not a tonic handler),
so it mounts directly on the underlying engine.

---

## Database

```go
svc := service.Create(config.Config{
    Database: db.ConnectionInfo{
        Type:     "POSTGRESQL",
        Host:     "db.internal",
        Port:     "5432",
        Username: "app",
        Password: os.Getenv("DB_PASSWORD"),
        Database: "users",
    },
})
```

Driver matrix:

| `Type`        | Driver to import                          | DSN built for you                                       |
| ------------- | ----------------------------------------- | ------------------------------------------------------- |
| `""`/`SQLITE` | bundled (`github.com/mattn/go-sqlite3`)   | `<Host>` is the file path                               |
| `MYSQL`       | `_ "github.com/go-sql-driver/mysql"`      | `user:pass@tcp(host:port)/db`                            |
| `POSTGRESQL`  | `_ "github.com/jackc/pgx/v5/stdlib"`      | `postgres://user:pass@host:port/db?sslmode=disable`      |

In handlers, always pass `ctx.Request.Context()` so cancellation and
trace context reach the driver:

```go
func ListUsers(ctx *gin.Context, db *sql.DB) ([]User, error) {
    rows, err := db.QueryContext(ctx.Request.Context(),
        `SELECT id, name FROM users ORDER BY id`)
    if err != nil { return nil, err }
    defer rows.Close()

    var out []User
    for rows.Next() {
        var u User
        if err := rows.Scan(&u.ID, &u.Name); err != nil { return nil, err }
        out = append(out, u)
    }
    return out, rows.Err()
}
```

If your service is stateless, set `SkipDB: true` and the second handler
parameter will always be `nil`.

---

## OpenAPI & Swagger UI

The OpenAPI document is regenerated on every request to `/openapi.json`
or `/openapi.yaml`, and Swagger UI is served from `/api/docs`. You get
a baseline spec for free; richer docs come through `[]fizz.OperationOption`:

```go
import (
    "mkfst/fizz"
    "mkfst/fizz/openapi"
)

docs := []fizz.OperationOption{
    fizz.Summary("Create a user"),
    fizz.Description("Returns 201 on success, 409 if the email is taken."),
    fizz.ID("createUser"),
    fizz.Header("X-Request-Id", "Server-side trace ID", fizz.String),
    fizz.Response("409", "Email already exists", APIError{}, nil,
        APIError{Code: "EMAIL_TAKEN", Message: "email already exists"}),
    fizz.XCodeSample(&openapi.XCodeSample{
        Lang:   "curl",
        Source: `curl -XPOST http://localhost:8080/users -d '{"name":"a","email":"a@b.c"}'`,
    }),
}

svc.Route("POST", "/users", 201, docs, createUser)
```

Full option list in [docs/openapi.md](docs/openapi.md).

---

## Authentication

mkfst bundles a fork of [`go-pkgz/auth`](https://github.com/go-pkgz/auth)
glued to the framework's tonic handler shape. It provides JWT issuance +
verification (cookie / header / query), an `Auth` middleware that
rejects unauthenticated requests, a `Trace` middleware that loads the
user but lets anonymous requests through, and providers for:

GitHub, Google, Facebook, Microsoft, Apple, Yandex, Twitter,
Battle.net, Patreon, Telegram, plus a **Dev** provider for local
testing without real OAuth credentials, and a **Direct** provider for
plain username/password.

```go
authSvc := auth.NewService(auth.Opts{
    SecretReader: token.SecretFunc(func(string) (string, error) {
        return os.Getenv("JWT_SECRET"), nil
    }),
    TokenDuration:  5 * time.Minute,
    CookieDuration: 24 * time.Hour,
    Issuer:         "users-api",
    URL:            "https://api.example.com",
    AvatarStore:    avatar.NewLocalFS("/var/lib/avatars"),
    SecureCookies:  true,
})
authSvc.AddProvider("github", os.Getenv("GH_CID"), os.Getenv("GH_CSECRET"))

authRoute, avatarRoute := authSvc.Handlers()
mw := authSvc.Middleware()

api := router.CreateGroup("/api/v1", "v1", "v1 API")
api.Route("GET", "/auth",   200, nil, authRoute)
api.Route("GET", "/avatar", 200, nil, avatarRoute)

me := api.Group("/me", "Me", "Authenticated user routes")
me.Middleware(mw.Auth)
me.Route("GET", "/", 200, nil, func(ctx *gin.Context, _ *sql.DB) (token.User, error) {
    return token.GetUserInfo(ctx.Request)
})
```

A complete working app (with the Dev provider so you don't need a real
OAuth client) is in [`examples/06-auth`](examples/06-auth).

---

## OpenTelemetry

```go
import "mkfst/telemetry"

svc.ConfigureTracing(telemetry.Default()) // stdout traces + metrics
```

For real backends, build a `telemetry.TracingConfig` around the OTLP
exporters:

```go
traceExp, _ := otlptracegrpc.New(ctx,
    otlptracegrpc.WithEndpoint("otel-collector:4317"),
    otlptracegrpc.WithInsecure())

metricExp, _ := otlpmetricgrpc.New(ctx,
    otlpmetricgrpc.WithEndpoint("otel-collector:4317"),
    otlpmetricgrpc.WithInsecure())

svc.ConfigureTracing(&telemetry.TracingConfig{
    TraceExporter:  traceExp,
    MetricExporter: metricExp,
    Sampler:        sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1)),
    TraceOptions: []sdktrace.TracerProviderOption{
        sdktrace.WithBatcher(traceExp),
    },
    MetricOptions: []sdkmetric.PeriodicReaderOption{
        sdkmetric.WithInterval(15 * time.Second),
    },
})
```

Once configured, `otel.Tracer(...)` is wired up globally and you can
create spans inside handlers in the usual way. Mount the bundled
`middleware/opentel` middleware on the Gin engine to get one span per
request automatically.

---

## Configuration & environment variables

`config.Config` is the single argument to `service.Create`. Every field
that has an environment override falls back to the env var first, then
to a built-in default — but **a non-zero value on the struct disables
the env override for that field** (read [docs/configuration.md](docs/configuration.md)
for the full precedence rules).

| Var              | Field                | Default     |
| ---------------- | -------------------- | ----------- |
| `APP_HOST`       | `Host`               | `0.0.0.0`   |
| `APP_PORT`       | `Port`               | `8000`      |
| `APP_SKIP_HTTPS` | `UseHTTPS` (inverse) | `false`     |
| `APP_SKIP_DB`    | `SkipDB`             | `false`     |
| `DB_TYPE`        | `Database.Type`      | `SQLITE`    |
| `DB_HOST`        | `Database.Host`      | `app.db` (sqlite) / `localhost` |
| `DB_PORT`        | `Database.Port`      | `3306` / `5432` |
| `DB_USERNAME`    | `Database.Username`  | required for MySQL/PG |
| `DB_PASSWORD`    | `Database.Password`  | required for MySQL/PG |
| `DB_NAME`        | `Database.Database`  | `app`       |
| `DB_USE_SSL`     | `Database.UseSSL`    | `false`     |

---

## Project layout

mkfst doesn't impose a layout, but this scales well:

```
your-app/
├── cmd/
│   └── api/main.go            # service.Create + AddGroup + Run
├── internal/
│   ├── users/                 # one router.Group per domain
│   │   ├── handlers.go
│   │   ├── store.go
│   │   └── types.go
│   ├── billing/...
│   └── …
├── docs/
│   └── index.tmpl             # mkfst's Swagger UI template (copy from this repo)
└── go.mod
```

Production binaries need `docs/index.tmpl` next to them at runtime —
either copy it during the Docker build, or skip `service.Run` and wire
the Fizz engine yourself.

---

## Examples

Eight runnable examples ship under [`examples/`](examples/). Each one
listens on its own port so you can run several side-by-side.

| # | Folder                                  | Port | Concept |
| - | --------------------------------------- | ---- | ------- |
| 01 | [`hello`](examples/01-hello)           | 8081 | Smallest possible server |
| 02 | [`routing`](examples/02-routing)       | 8082 | Groups, nested groups, middleware order |
| 03 | [`binding`](examples/03-binding)       | 8083 | path / query / header / body + `validate` |
| 04 | [`database`](examples/04-database)     | 8084 | SQLite CRUD |
| 05 | [`cors`](examples/05-cors)             | 8085 | Bundled CORS middleware |
| 06 | [`auth`](examples/06-auth)             | 8086 (+ 8084 dev OAuth) | Dev OAuth + JWT-protected `/me` |
| 07 | [`telemetry`](examples/07-telemetry)   | 8087 | Stdout OTel exporters + manual span |
| 08 | [`openapi`](examples/08-openapi)       | 8088 | Rich OpenAPI metadata, custom errors |

Run any of them from the **repo root**:

```bash
go run ./examples/04-database
```

(The "from the repo root" caveat is because of `LoadHTMLGlob("docs/*")`
inside `service.Run` — see [examples/README.md](examples/README.md).)

---

## Documentation

Long-form docs live under [`docs/`](docs/). Read in order:

1. [getting-started.md](docs/getting-started.md) — install, first server, layout
2. [architecture.md](docs/architecture.md) — request lifecycle and how the layers fit
3. [configuration.md](docs/configuration.md) — `Config` fields and env vars
4. [handlers.md](docs/handlers.md) — handler signatures, binding, validation
5. [routing.md](docs/routing.md) — `Service` / `Router` / `Group` composition
6. [database.md](docs/database.md) — drivers, DSNs, instrumented connections
7. [openapi.md](docs/openapi.md) — operation options, security schemes, servers
8. [middleware.md](docs/middleware.md) — writing middleware and using CORS
9. [auth.md](docs/auth.md) — JWT, social providers, avatars, `Auth` / `Trace`
10. [telemetry.md](docs/telemetry.md) — OTel SDK, exporters, custom spans
11. [hooks.md](docs/hooks.md) — `BindHook` / `RenderHook` / `ErrorHook` / `ExecHook`

---

## Status & versioning

mkfst is **pre-1.0**. Expect breaking changes between minor versions.
The most stable surface is the handler contract
(`func(*gin.Context, *sql.DB[, *Input]) ([Output,] error)`); the
configuration, telemetry and auth surfaces may move.

---

## Acknowledgements

mkfst is glue around superb open-source work, primarily:

- [Gin](https://github.com/gin-gonic/gin) — the underlying HTTP router.
- [tonic](https://github.com/loopfz/gadgeto/tree/master/tonic) — the
  reflection-based handler binder. mkfst forks it to inject `*sql.DB`
  as the second parameter of every handler.
- [fizz](https://github.com/wI2L/fizz) — the OpenAPI 3 generator.
- [go-pkgz/auth](https://github.com/go-pkgz/auth) — the social-login
  machinery, ported to a tonic-shaped surface.
- [validator/v10](https://github.com/go-playground/validator) —
  declarative validation rules.
- The OpenTelemetry Go SDK.

---

## License

See [LICENSE](LICENSE).
