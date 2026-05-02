# mkfst examples

Each subdirectory is a self-contained `package main` that demonstrates one
slice of the framework. They share the parent module, so `go run` /
`go build` works out of the box.

## Run any example

From the **repository root**:

```bash
go run ./examples/01-hello
```

Run from the repo root because `service.Run` calls `LoadHTMLGlob("docs/*")`
to load the Swagger UI template — that path is resolved relative to the
process's working directory. The repo root contains `docs/index.tmpl`; if
you `cd` into an example folder, the template will not be found and the
`/api/docs` route will 500.

## What each example shows

| Folder              | Topic                                                      |
| ------------------- | ---------------------------------------------------------- |
| `01-hello`          | Smallest possible mkfst server (no DB, single route)       |
| `02-routing`        | Groups, nested groups, per-group middleware                |
| `03-binding`        | Path / query / header / body binding + `validate` tags     |
| `04-database`       | SQLite CRUD with `service.GetDB`                           |
| `05-cors`           | Bundled CORS middleware                                    |
| `06-auth`           | Dev OAuth provider + protected `/me` endpoint              |
| `07-telemetry`      | OpenTelemetry stdout exporters and a manual span           |
| `08-openapi`        | Rich OpenAPI metadata, custom responses, `x-codeSamples`   |

## Once a server is running

The framework mounts these paths automatically:

- `/api/docs`        — interactive Swagger UI
- `/openapi.json`    — generated OpenAPI 3 spec
- `/openapi.yaml`    — same, YAML
- `/status`          — built-in liveness probe (`OK`)

Each example's README highlights the routes it adds on top.

## Common gotchas

- **`docs/index.tmpl` not found** — you launched from somewhere other than
  the repo root.
- **Port already in use** — every example listens on a different port to
  let you run two side-by-side; check the example's README.
- **`gcc` not found** — `go-sqlite3` is CGO. Install build-essential or
  set `SkipDB: true`.
