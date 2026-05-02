# Handlers

A mkfst handler is a regular Go function whose shape is enforced by `tonic`
through reflection. Get the shape right and binding, validation, response
rendering, errors and OpenAPI all come for free.

## The accepted signatures

```go
// Minimum: just the context and the database handle.
func(*gin.Context, *sql.DB) error

// With a typed response.
func(*gin.Context, *sql.DB) (Output, error)

// With a typed input.
func(*gin.Context, *sql.DB, *Input) error

// With both.
func(*gin.Context, *sql.DB, *Input) (Output, error)
```

Rules enforced by `tonic.input` / `tonic.output`
([`tonic/handler.go`](../tonic/handler.go)):

- The first parameter is `*gin.Context` — anything else panics on registration.
- The second parameter is `*sql.DB` — even if you don't use it. Pass `nil`
  is fine, but the type must match.
- The third parameter, if present, must be a **pointer to a struct**.
- The last return value must implement `error`.
- There can be at most one extra return (the response payload).

If a signature is wrong, `tonic.Handler` panics at startup, never at request
time.

## Inputs: where each field comes from

mkfst supports four sources, controlled by struct tags:

| Tag        | Source                              | Example URL fragment            |
| ---------- | ----------------------------------- | ------------------------------- |
| `path`     | URL path parameter (`:name`)        | `/users/:id`                    |
| `query`    | URL query string                    | `?limit=10&order=desc`          |
| `header`   | HTTP header                         | `Authorization: Bearer …`       |
| `json`/`yaml` (no special tonic tag) | Request body via `BindHook` | `POST /users` body              |

Every field outside a body uses `tonic`'s reflection-driven binder. Body
binding goes through Gin's standard `binding.JSON` (or yaml binding for
`Content-Type: application/yaml`).

### Worked example

```go
type ListUsersInput struct {
    // path
    OrgID int `path:"org_id" validate:"required"`

    // query
    Limit  int    `query:"limit" default:"20" validate:"min=1,max=200"`
    Cursor string `query:"cursor"`
    Order  string `query:"order" enum:"asc,desc" default:"asc"`

    // header
    TraceID string `header:"X-Trace-Id"`
}

svc.Group("/orgs", "Orgs", "Org management").Route(
    "GET", "/:org_id/users", 200, nil,
    func(ctx *gin.Context, db *sql.DB, in *ListUsersInput) ([]User, error) {
        return store.List(db, in.OrgID, in.Limit, in.Cursor, in.Order)
    },
)
```

### Body binding

When the handler input has fields without a `path|query|header` tag, `tonic`
treats them as the body. Use the standard `json` (or `yaml`) tags:

```go
type CreateUserInput struct {
    Name  string `json:"name"  validate:"required,min=1,max=80"`
    Email string `json:"email" validate:"required,email"`
}
```

Tonic uses Gin's binding under the hood, so the same JSON field rules apply
(`omitempty`, embedded structs, `,string`, etc.).

## Tags reference

| Tag        | Meaning                                                                                  |
| ---------- | ---------------------------------------------------------------------------------------- |
| `path`     | Bind from URL path.                                                                       |
| `query`    | Bind from query string. Supports comma-separated lists for `[]T` fields.                   |
| `header`   | Bind from HTTP header.                                                                    |
| `default`  | Default value if the source has no value. For lists, comma-separated.                      |
| `enum`     | Comma-separated whitelist. Rejected with `binding error` if the value is outside the set. |
| `validate` | Forwarded to [validator/v10](https://pkg.go.dev/github.com/go-playground/validator/v10). |
| `explode`  | `false` to interpret a single comma-separated query value as a list. Defaults to `true`. |
| `required` | **Deprecated** — use `validate:"required"`.                                              |

Validation tags use the standard `go-playground/validator` rule set:
`required`, `min`, `max`, `len`, `email`, `url`, `uuid`, `oneof`, regex,
cross-field `eqfield`/`gtfield`, etc.

## Outputs

The non-error return value can be:

- `nil` — produces an empty body with the configured status code.
- A struct, slice, map, primitive — JSON-encoded by the default `RenderHook`.
- `gin.H` — the standard map alias for ad-hoc objects.

The status code defaults to whatever you passed to `Route`. To use a
different code for one path (for example, `204 No Content`), return `nil`
and the framework writes the empty body with the route's default status.

### Streaming or custom responses

If you need to write the response yourself (CSV, file download, server-sent
events…), write directly to `ctx.Writer` and return `nil, nil`. The
`RenderHook` checks `c.Writer.Written()` and will not overwrite it.

```go
func download(ctx *gin.Context, db *sql.DB) (any, error) {
    ctx.Header("Content-Type", "text/csv")
    ctx.Writer.WriteHeader(200)
    _, err := io.Copy(ctx.Writer, csvSource())
    return nil, err
}
```

## Errors

A handler signals failure by returning a non-nil error. The error is fed to
the active `ErrorHook`. The default hook returns `400` with a body of
`{"error": "<msg>"}`.

You usually want richer errors. Either install a custom `ErrorHook` (see
[hooks.md](hooks.md)) or wrap errors so the hook can switch on them:

```go
type httpErr struct {
    code int
    msg  string
}

func (e httpErr) Error() string { return e.msg }

tonic.SetErrorHook(func(c *gin.Context, err error) (int, interface{}) {
    var he httpErr
    if errors.As(err, &he) {
        return he.code, gin.H{"error": he.msg}
    }
    var be tonic.BindError
    if errors.As(err, &be) {
        return 422, gin.H{
            "error":  be.Error(),
            "fields": be.ValidationErrors(),
        }
    }
    return 500, gin.H{"error": err.Error()}
})
```

## Aborting from a handler

You can `ctx.AbortWithStatus(http.StatusUnauthorized)` from inside a handler
or middleware, then return an error or `nil, nil`. Once `ctx.Writer` has
been written to, the render hook leaves it alone.

## Handler lifecycle (one-liner)

> mkfst calls `BindHook → query/path/header binders → validator → your code
> → RenderHook` (or `ErrorHook` on the unhappy path).

For per-stage detail, see [hooks.md](hooks.md).
