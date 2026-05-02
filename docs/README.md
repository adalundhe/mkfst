# mkfst Documentation

`mkfst` (Make Fast) is a batteries-included Go web framework that stitches
together [Gin](https://github.com/gin-gonic/gin) for routing, a fork of
[loopfz/gadgeto/tonic](https://github.com/loopfz/gadgeto) for typed handlers,
and [wI2L/fizz](https://github.com/wI2L/fizz) for OpenAPI 3 generation, then
adds opinionated layers for configuration, database connections, social
authentication and OpenTelemetry tracing.

The point of mkfst is that **the handler signature is the source of truth**:
write an idiomatic Go function, and you get request binding, validation,
JSON/YAML rendering, an OpenAPI operation, an interactive Swagger UI and
sensible error responses for free.

```go
service.Route("GET", "/users/:id", 200, nil,
    func(ctx *gin.Context, db *sql.DB, in *struct {
        ID int `path:"id" validate:"required,min=1"`
    }) (User, error) {
        return loadUser(db, in.ID)
    },
)
```

## Documentation map

Read these in roughly the order below; each builds on the previous.

| File                                       | Topic                                                      |
| ------------------------------------------ | ---------------------------------------------------------- |
| [getting-started.md](getting-started.md)   | Install, first server, project layout                      |
| [architecture.md](architecture.md)         | How `service`, `router`, `tonic`, `fizz`, `db` fit together |
| [configuration.md](configuration.md)       | The `config.Config` struct and environment overrides       |
| [handlers.md](handlers.md)                 | Handler signatures, binding tags, validation, errors       |
| [routing.md](routing.md)                   | `Service`, `Router`, `Group`; nesting and middleware       |
| [database.md](database.md)                 | SQLite / MySQL / PostgreSQL connection management          |
| [openapi.md](openapi.md)                   | Operation options, Swagger UI, customising the spec        |
| [middleware.md](middleware.md)             | Writing middleware and using the bundled CORS middleware    |
| [auth.md](auth.md)                         | JWT, social providers, avatars, the `Auth` middleware      |
| [telemetry.md](telemetry.md)               | OpenTelemetry traces and metrics                           |
| [hooks.md](hooks.md)                       | `tonic` bind / render / error / exec hooks                 |

## Runnable examples

Every concept in the docs is exercised in [`../examples/`](../examples/).
Examples are numbered by complexity:

- [`01-hello`](../examples/01-hello)         — minimal server, no DB
- [`02-routing`](../examples/02-routing)     — groups, nested groups, per-group middleware
- [`03-binding`](../examples/03-binding)     — path / query / header / body binding and validation
- [`04-database`](../examples/04-database)   — full CRUD on SQLite
- [`05-cors`](../examples/05-cors)           — bundled CORS middleware
- [`06-auth`](../examples/06-auth)           — dev OAuth provider + protected routes
- [`07-telemetry`](../examples/07-telemetry) — stdout OTel exporters
- [`08-openapi`](../examples/08-openapi)     — rich OpenAPI metadata, custom responses, examples

Run any example from the **repository root** (see
[examples/README.md](../examples/README.md) for the reason why):

```bash
go run ./examples/01-hello
```

## Status / stability

mkfst is pre-1.0. Expect breaking changes between minor versions. The handler
contract (`func(*gin.Context, *sql.DB[, *Input]) ([Output,] error)`) is the
oldest and most stable part of the surface; everything else may move.
