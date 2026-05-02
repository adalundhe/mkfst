# 01 — Hello World

The smallest mkfst server: one route, no database, JSON response.

```bash
go run ./examples/01-hello
```

| Path           | What it does                              |
| -------------- | ----------------------------------------- |
| `/hello`       | Returns the JSON string `"hello, world"`. |
| `/api/docs`    | Swagger UI for everything below.          |
| `/openapi.json`| The generated spec.                       |
| `/status`      | Built-in liveness probe (`"OK"`).         |

## Try it

```bash
curl -s http://localhost:8081/hello
# "hello, world"

curl -s http://localhost:8081/openapi.json | jq '.paths."/hello"'
# Generated automatically from the handler.
```

## What this example shows

- `service.Create(config.Config{...})` — the entry point.
- `SkipDB: true` — works without sqlite installed.
- `service.Route(method, path, status, docs, handler)` — registering a
  single route on the root router (no group required).
- A handler shaped `func(*gin.Context, *sql.DB) (string, error)` — the
  minimum viable signature with a typed response.
