# Routing

mkfst's router is *declarative*: every `Route`, `Group` and `Middleware`
call is buffered and only flushed to Gin during `service.Run` →
`router.Build`. That means you can compose groups freely, return them from
functions, and assemble the final tree in `main` without worrying about
ordering side-effects.

## Three levels of attachment

```
service ── Route, Middleware, Group, AddGroup ──▶  Router
                                                    │
                Router ── Group, Middleware ─────▶  Group
                                                    │
                          Group ── Group, Route ──▶ nested Group
```

You can call `Route` or `Middleware` on any of:
- the **service** (`service.Route`, `service.Middleware`),
- a **router** (`service.Group(...).Route` or the value returned from those calls),
- a **group** (`router.Group(...)` / `router.CreateGroup(...)` / `group.Group(...)`).

All four shapes accept the same handler contract documented in
[handlers.md](handlers.md).

## Method + helpers

`Route` is verb-agnostic; pass the HTTP method as a string. There are no
`GET`, `POST` shorthands at the router/group level — use the underlying
`fizz.RouterGroup` (via `group.Base`) if you want them.

```go
group.Route("GET",    "/users", 200, nil, handler)
group.Route("POST",   "/users", 201, nil, handler)
group.Route("PATCH",  "/users/:id", 200, nil, handler)
group.Route("DELETE", "/users/:id", 204, nil, handler)
```

## Detached groups: `router.CreateGroup`

`router.CreateGroup` is the idiomatic way to define a group **outside**
`main` and attach it later. The returned group has `Base == nil`; the base
is filled in when you pass it to `service.AddGroup`.

```go
func usersGroup() router.Group {
    g := router.CreateGroup("/users", "Users", "User CRUD")
    g.Route("GET",  "/",   200, nil, listUsers)
    g.Route("POST", "/",   201, nil, createUser)
    g.Route("GET",  "/:id", 200, nil, getUser)
    return *g
}

func main() {
    svc := service.Create(...)
    svc.AddGroup(usersGroup())
    svc.Run()
}
```

> **Note:** `AddGroup` takes a `Group` *value*, not a pointer. Idiomatic
> usage is `svc.AddGroup(*g)` or returning `*g` from your factory.

## Nested groups

`group.Group(child, name, desc)` creates a child group. At `Build` time the
child's path is concatenated (`parent.path + child.path`) and the parent's
middleware is copied onto the child.

```go
api := router.CreateGroup("/api/v1", "v1 API", "v1 API root")

users := api.Group("/users", "Users", "User CRUD")
users.Route("GET", "/", 200, nil, listUsers)

admin := users.Group("/admin", "Admin", "Admin-only endpoints")
admin.Middleware(requireAdmin)              // chained on top of /users middleware
admin.Route("DELETE", "/:id", 204, nil, hardDelete)
```

After build, the routes resolve to:

- `GET    /api/v1/users`
- `DELETE /api/v1/users/admin/:id` (admin and any /users middleware run first)

## Middleware on each level

`Middleware(...)` accepts variadic `interface{}`. Each value must be a tonic
handler — same signature constraints as a route. At build time, every
middleware is wrapped with `tonic.Handler(mw, db, 200)`.

A middleware passes the request through by simply returning `nil, nil`. To
short-circuit, write the response yourself or call `ctx.AbortWithStatus`.

```go
func requireAdmin(ctx *gin.Context, db *sql.DB) (any, error) {
    user, _ := token.GetUserInfo(ctx.Request)
    if !user.IsAdmin() {
        ctx.AbortWithStatus(http.StatusForbidden)
        return nil, nil
    }
    return nil, nil
}
```

### Middleware execution order

For request `/api/v1/users/admin/42`:

1. Router-level middleware (in registration order)
2. `/api/v1` group middleware
3. `/api/v1/users` group middleware
4. `/api/v1/users/admin` group middleware
5. The `DELETE /:id` handler

Middleware for a parent group is automatically inherited by children
because of the copy in [`router.go:266`](../router/router.go).

## Mounting third-party Gin middleware

Anything that satisfies `gin.HandlerFunc` can also be mounted, but you have
to drop down to the underlying engine:

```go
engine := svc.Router.Base.Engine() // *gin.Engine
engine.Use(gin.Recovery())
engine.Use(gin.Logger())
```

The bundled CORS middleware in [`middleware/cors`](../middleware/cors/) is
already a tonic handler, so use it through `Middleware(...)` like everything
else.

## Status codes & defaults

The `status` argument to `Route` is the **default** status for the happy
path. The render hook honours it unless you (or a middleware) have already
written to the response. `tonic.Description`, `tonic.Summary` and friends
let you change the OpenAPI documentation for the same status.

## What you cannot do (yet)

- Handler chaining: each route registers exactly one user handler. To
  combine logic, use middleware on the group or compose helpers inside the
  handler.
- Per-route customisation of the `BindHook` / `ErrorHook` — these are
  package globals on `tonic`. See [hooks.md](hooks.md).
- Method-routing the same path to different handlers in one call. Use
  multiple `Route(...)` calls instead.
