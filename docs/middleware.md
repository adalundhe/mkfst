# Middleware

Middleware in mkfst follows the same shape as a route handler: it is just a
function with the tonic signature, mounted via `Middleware(...)` on the
service, router or any group.

```go
func(*gin.Context, *sql.DB) (any, error)
```

Return `nil, nil` to let the request proceed. To short-circuit:

- write the response yourself (`ctx.JSON`, `ctx.String`, …) and return
  `nil, nil`, or
- call `ctx.AbortWithStatus(...)` and return `nil, nil`, or
- return an error — the configured `ErrorHook` writes the response.

## Writing a middleware

A request-id middleware that sets `X-Request-Id` if missing and stores it
in the Gin context for downstream handlers:

```go
func RequestID(ctx *gin.Context, _ *sql.DB) (any, error) {
    id := ctx.GetHeader("X-Request-Id")
    if id == "" {
        id = uuid.NewString()
    }
    ctx.Header("X-Request-Id", id)
    ctx.Set("request_id", id)
    return nil, nil
}

svc.Middleware(RequestID)
```

A middleware that loads the user from a session and stores them in the
context:

```go
func WithUser(ctx *gin.Context, db *sql.DB) (any, error) {
    sess, err := ctx.Cookie("session")
    if err != nil {
        return nil, nil // anonymous request, that's OK
    }
    var u User
    err = db.QueryRowContext(ctx.Request.Context(),
        "SELECT id, name FROM users WHERE session_id=$1", sess).
        Scan(&u.ID, &u.Name)
    if err == nil {
        ctx.Set("user", &u)
    }
    return nil, nil
}

func RequireUser(ctx *gin.Context, _ *sql.DB) (any, error) {
    if _, ok := ctx.Get("user"); !ok {
        ctx.AbortWithStatus(http.StatusUnauthorized)
    }
    return nil, nil
}

svc.Middleware(WithUser)                    // every route
svc.Group("/admin", "Admin", "...").
    Middleware(RequireUser).
    Route("GET", "/", 200, nil, adminDashboard)
```

## Order of execution

For request `GET /api/v1/users/admin/42`, when `/api/v1`, `/api/v1/users`
and `/api/v1/users/admin` each have middleware:

```
service-level middleware
↓
/api/v1 group middleware
↓
/api/v1/users group middleware
↓
/api/v1/users/admin group middleware
↓
final handler
```

Middleware is run in registration order at each level.

## Bundled CORS middleware

`mkfst/middleware/cors` ships a CORS middleware that mirrors the API of
`github.com/gin-contrib/cors` but exposes a tonic-shaped handler.

### Default (allow everything, dev mode)

```go
import "mkfst/middleware/cors"

svc.Middleware(cors.Default()) // AllowAllOrigins=true
```

### Custom config

```go
svc.Middleware(cors.CORS(cors.Config{
    AllowOrigins: []string{
        "https://app.example.com",
        "https://admin.example.com",
    },
    AllowMethods:     []string{"GET", "POST", "PATCH", "DELETE"},
    AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
    ExposeHeaders:    []string{"X-Request-Id"},
    AllowCredentials: true,
    MaxAge:           12 * time.Hour,
    AllowWildcard:    true,                 // for `https://*.example.com`
}))
```

The full `Config` struct is in
[`middleware/cors/cors.go`](../middleware/cors/cors.go).

### Wildcard origins

Set `AllowWildcard: true` to allow patterns like:

- `http://*.example.com`
- `https://api.*`
- `http://some.*.subdomain.com`

Only one `*` per origin is allowed.

## OpenTelemetry middleware

mkfst ships a Gin-compatible OTel middleware in
`mkfst/middleware/opentel`. It is a regular `gin.HandlerFunc`, not a tonic
handler, so attach it to the underlying Gin engine:

```go
import otelmw "mkfst/middleware/opentel"

engine := svc.Router.Base.Engine()
engine.Use(otelmw.Middleware("users-api"))
```

See [telemetry.md](telemetry.md) for the surrounding tracer setup.

## Auth middleware

`middleware/auth` exposes the social-login `AuthService` and its `Auth` /
`Trace` middlewares, both of which are tonic handlers. Their use is
documented in [auth.md](auth.md).
