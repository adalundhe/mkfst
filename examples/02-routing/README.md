# 02 — Routing & middleware

Shows how to build groups outside `main`, nest them, and attach middleware
at every level. Watch the log output to see the execution order.

```bash
go run ./examples/02-routing
```

| Method | Path                      | Behaviour                                |
| ------ | ------------------------- | ---------------------------------------- |
| GET    | `/users/`                 | Returns `["alice", "bob"]`.              |
| GET    | `/users/:name`            | Returns `"user=alice"` for `:name=alice`.|
| DELETE | `/users/admin/:name`      | Requires `X-Admin: true`, otherwise 403. |

## Try it

```bash
curl -s http://localhost:8082/users/ | jq
curl -s http://localhost:8082/users/alice
curl -i -X DELETE http://localhost:8082/users/admin/alice
# HTTP/1.1 403 Forbidden ...
curl -i -X DELETE -H 'X-Admin: true' http://localhost:8082/users/admin/alice
# HTTP/1.1 204 No Content
```

The server log shows the middleware firing in order:

```
[service]      DELETE /users/admin/alice
[users]        DELETE /users/admin/alice
[users/admin]  DELETE /users/admin/alice
hard-deleting alice
```

## What this example shows

- `router.CreateGroup` defines a group outside `main`; `svc.AddGroup`
  attaches it.
- `group.Group(...)` nests; the child path is `/users/admin`.
- Middleware order is **service → outer group → inner group → handler**.
- A middleware that returns `nil, nil` lets the request continue;
  `ctx.AbortWithStatusJSON(...)` short-circuits.
