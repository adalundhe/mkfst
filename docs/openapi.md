# OpenAPI & Swagger UI

`fizz` generates an OpenAPI 3.0 document from your tonic-wrapped handlers
on every request to `/openapi.json` (or `/openapi.yaml`). Swagger UI is
served from `/api/docs`.

## What you get for free

- Path, method and status code from the `Route(...)` call.
- Operation ID from the function name (overridable via `fizz.ID`).
- Request body schema from the input struct (with `json:` tags honoured).
- Response schema from the return type.
- Parameter schemas from the `path:`, `query:` and `header:` tags.
- Validation rules (`validate:"required,min=1"`) become OpenAPI
  `required`, `minimum`, `maximum`, etc.
- A best-guess tag from the first segment of the path.

The default-status-code response *always* exists. Other responses must be
added explicitly with `fizz.Response(...)`.

## OperationOption catalogue

Each `Route` call takes a `[]fizz.OperationOption`. The options come from
[`fizz/fizz.go`](../fizz/fizz.go) and produce the OpenAPI metadata for a
single operation.

```go
docs := []fizz.OperationOption{
    fizz.Summary("Create a user"),
    fizz.Description("Creates a new user. The email must be unique."),
    fizz.Deprecated(false),
    fizz.ID("createUser"),
    fizz.Header("X-Trace-Id", "Server-side trace ID", fizz.String),
    fizz.Response("422", "Validation failed", ValidationError{}, nil, nil),
    fizz.Response("409", "Email already exists", APIError{}, nil,
        APIError{Message: "email already exists", Code: "EMAIL_TAKEN"}),
    fizz.XCodeSample(&openapi.XCodeSample{
        Lang:   "curl",
        Label:  "curl",
        Source: `curl -XPOST http://localhost:8080/users -d '{"name":"a","email":"a@b.c"}'`,
    }),
}
```

Quick reference:

| Helper                       | Effect                                                         |
| ---------------------------- | -------------------------------------------------------------- |
| `fizz.Summary(s)`            | Sets `summary`.                                                |
| `fizz.Summaryf(fmt, …)`      | Like `Summary`, with `Sprintf`.                                |
| `fizz.Description(s)`        | Sets `description`.                                            |
| `fizz.Descriptionf(fmt, …)`  | Like `Description`, with `Sprintf`.                            |
| `fizz.Deprecated(b)`         | Sets `deprecated`.                                             |
| `fizz.ID(id)`                | Overrides `operationId` (mkfst sets a UUID by default).        |
| `fizz.StatusDescription(s)`  | Sets the description for the *default* response.              |
| `fizz.Response(code, …)`     | Adds an *additional* response.                                 |
| `fizz.ResponseWithExamples`  | Same as `Response` but with multiple `examples`.               |
| `fizz.Header(name, desc, m)` | Documents a response header.                                   |
| `fizz.InputModel(model)`     | Override the inferred input model (when the handler ignores it).|
| `fizz.XCodeSample(c)`        | Adds an `x-codeSamples` entry (rendered by Redoc).            |
| `fizz.Security(req)`         | Adds a security requirement to this operation.                |
| `fizz.WithOptionalSecurity()`| Adds an empty requirement so the others become optional.       |
| `fizz.WithoutSecurity()`     | Strips top-level security from this operation.                 |
| `fizz.XInternal()`           | Marks the operation `x-internal: true` (useful for filtering).  |

### Primitive helpers

`fizz` exposes prebuilt zero values for documenting headers / response
shapes when you don't have a struct:

```go
fizz.Integer  // int32
fizz.Long     // int64
fizz.Float    // float32
fizz.Double   // float64
fizz.String   // string
fizz.Byte     // []byte
fizz.Binary   // []byte
fizz.Boolean  // bool
fizz.DateTime // time.Time
```

## Customising the spec

### `Info` block

`config.Config.Spec` (`openapi.Info`) is the title/version/description
block. See [configuration.md](configuration.md) for the full shape.

### Servers

The default OpenAPI document contains *no* `servers` entry, which means
clients will use the URL they fetched the spec from. To inject your own:

```go
gen := svc.Router.Base.Generator()
gen.SetServers([]*openapi.Server{
    {URL: "https://api.example.com",  Description: "Production"},
    {URL: "https://staging.example.com", Description: "Staging"},
})
```

Call this **before** `service.Run` (or before the first request to
`/openapi.json`).

### Security schemes

```go
gen := svc.Router.Base.Generator()
gen.SetSecuritySchemes(map[string]*openapi.SecuritySchemeOrRef{
    "bearerAuth": {
        SecurityScheme: &openapi.SecurityScheme{
            Type:         "http",
            Scheme:       "bearer",
            BearerFormat: "JWT",
        },
    },
})
gen.SetSecurityRequirement([]*openapi.SecurityRequirement{
    {"bearerAuth": []string{}},
})
```

`fizz.Security(...)`, `fizz.WithOptionalSecurity()` and
`fizz.WithoutSecurity()` then let you adjust on a per-operation basis.

## Swagger UI vs. Redoc

The bundled Swagger UI lives at `/api/docs` and is rendered by
[`docs/index.tmpl`](index.tmpl). If you prefer Redoc, point Redoc at
`/openapi.json` from your own static page — mkfst's spec uses the
`x-codeSamples` extension that Redoc renders nicely.

## A reasonable convention

Keep operation docs next to the handler:

```go
var createUserDocs = []fizz.OperationOption{
    fizz.Summary("Create a user"),
    fizz.Description("Returns 201 on success, 409 if the email is taken."),
    fizz.Response("409", "Email taken", APIError{}, nil, nil),
}

g.Route("POST", "/users", 201, createUserDocs, createUser)
```

Reusing the docs slice across paths is fine — `Route` deep-copies it, then
appends a fresh UUID `fizz.ID(...)` so operation IDs never collide.
