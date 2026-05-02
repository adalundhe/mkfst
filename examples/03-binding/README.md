# 03 — Binding & validation

Walks through every place a handler can pull data from: path parameters,
query string, headers and the request body. Each field demonstrates a
different feature of the binder.

```bash
go run ./examples/03-binding
```

| Method | Path                        | What it does                              |
| ------ | --------------------------- | ----------------------------------------- |
| GET    | `/orgs/:org_id/search`      | Path + query + header + duration binding. |
| POST   | `/orgs/:org_id/users`       | JSON body binding with `validate` tags.   |

## Try it

```bash
# Happy path
curl -s 'http://localhost:8083/orgs/42/search?q=foo&tag=a,b,c&order=desc&limit=5' \
  -H 'X-Trace-Id: req-123' | jq

# Default value (since=24h kicks in)
curl -s 'http://localhost:8083/orgs/42/search?q=foo' | jq

# Validation: q is required
curl -is 'http://localhost:8083/orgs/42/search'
# HTTP/1.1 400 Bad Request   {"error":"binding error on field 'Query' ..."}

# Enum violation
curl -is 'http://localhost:8083/orgs/42/search?q=foo&order=sideways'
# HTTP/1.1 400 Bad Request   {"error":"binding error on field 'Order' ..."}

# Body binding success
curl -is -XPOST http://localhost:8083/orgs/42/users \
  -H 'Content-Type: application/json' \
  -d '{"name":"alice","email":"a@b.c","age":30}'
# HTTP/1.1 201 Created

# Body validation failure
curl -is -XPOST http://localhost:8083/orgs/42/users \
  -H 'Content-Type: application/json' \
  -d '{"name":"","email":"not-an-email","age":1}'
# HTTP/1.1 400 Bad Request   {"error":"binding error: ... validation ..."}
```

## What this example shows

- `path:`, `query:`, `header:` tags pulling from each source.
- `default:"24h"` populating an unset field, even for `time.Duration`
  (which goes through the `encoding.TextUnmarshaler` path).
- `enum:"asc,desc"` rejecting unknown values.
- `validate:"required,min=1,max=200"` invoking `validator/v10`.
- `explode:"false"` interpreting `?tag=a,b,c` as a list.
- An anonymous embedded struct (`CreateUserBody`) flattening JSON body
  fields into the top-level input.

## See also

- [docs/handlers.md](../../docs/handlers.md) for the full tag reference.
- [docs/hooks.md](../../docs/hooks.md) for replacing the default 400
  response with structured `validator.ValidationErrors`.
