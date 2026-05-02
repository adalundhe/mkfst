# Getting Started

## Requirements

- Go 1.21 or newer (the module declares `go 1.21.6`)
- A C toolchain — `go-sqlite3` is a CGO package, so the default SQLite backend
  needs `gcc` (or `clang`) on `$PATH`. Set `CGO_ENABLED=1` if you have
  disabled it globally.

If you do not need SQLite (or any database at all), set `SkipDB: true` on the
config and you can build with `CGO_ENABLED=0`.

## Install

mkfst is a regular Go module. Add it to your project with:

```bash
go get mkfst@latest
```

Or, while developing inside this repository, just import the local packages
(`mkfst/service`, `mkfst/router`, …) — the module is named `mkfst` in
[`go.mod`](../go.mod).

## A minimal server

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
        SkipDB: true, // no database for this example
        Spec: openapi.Info{
            Title:   "Hello API",
            Version: "v1.0.0",
        },
    })

    svc.Route("GET", "/hello", 200, nil,
        func(ctx *gin.Context, db *sql.DB) (string, error) {
            return "hello, world", nil
        },
    )

    svc.Run()
}
```

Run it from the repo root:

```bash
go run ./examples/01-hello
```

then open:

- `http://localhost:8080/hello`        — your handler
- `http://localhost:8080/api/docs`     — Swagger UI
- `http://localhost:8080/openapi.json` — generated OpenAPI 3 spec
- `http://localhost:8080/openapi.yaml` — same, as YAML
- `http://localhost:8080/status`       — built-in liveness probe

## Why "from the repo root"?

`service.Run` calls `LoadHTMLGlob("docs/*")` to load the Swagger UI template
from [`docs/index.tmpl`](index.tmpl). The path is relative to the working
directory, so the binary must be launched from a directory that contains a
`docs/` folder with `index.tmpl` in it. Either:

- run from the repo root, or
- copy `docs/index.tmpl` next to your binary at deploy time, or
- skip `service.Run` and wire the Fizz engine yourself (see
  [architecture.md](architecture.md)).

## Project layout suggestion

mkfst does not impose a structure, but a layout that scales well is:

```
your-app/
├── cmd/
│   └── api/main.go            # service.Create + Run
├── internal/
│   ├── users/                 # one router.Group per domain
│   │   ├── handlers.go
│   │   ├── store.go
│   │   └── types.go
│   └── billing/...
├── docs/
│   └── index.tmpl             # mkfst's Swagger UI template
└── go.mod
```

The example apps under [`../examples/`](../examples/) collapse this into a
single file each, which is fine for a tutorial but not for production.

## Next steps

Read [architecture.md](architecture.md) to understand how a request flows
through the framework, then [handlers.md](handlers.md) for the binding rules.
