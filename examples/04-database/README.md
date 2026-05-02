# 04 — SQLite CRUD

A full create / read / list / delete cycle backed by the bundled SQLite
connection. The first run creates a `04-database.sqlite` file in the
working directory.

```bash
go run ./examples/04-database
```

| Method | Path           | What it does                              |
| ------ | -------------- | ----------------------------------------- |
| GET    | `/users/`      | List all users.                           |
| POST   | `/users/`      | Create a user. `name` + `email` required. |
| GET    | `/users/:id`   | Fetch a user by ID; 404 if missing.       |
| DELETE | `/users/:id`   | Delete a user (idempotent, always 204).   |

## Try it

```bash
# create
curl -s -XPOST http://localhost:8084/users/ \
  -H 'Content-Type: application/json' \
  -d '{"name":"alice","email":"alice@example.com"}' | jq

# list
curl -s http://localhost:8084/users/ | jq

# get one
curl -s http://localhost:8084/users/1 | jq

# delete
curl -i -XDELETE http://localhost:8084/users/1
```

## What this example shows

- Letting the framework open SQLite via `Database.Type = "SQLITE"` and
  `Database.Host = "<filename>"`.
- `service.GetDB()` for migrating schema before `Run`.
- `db.QueryContext` / `db.ExecContext` with `ctx.Request.Context()` so
  request cancellation reaches the driver.
- Aborting from a handler with `ctx.AbortWithStatus(http.StatusNotFound)`
  while still returning an error (so the error hook records it).
- Adding extra response codes to the OpenAPI spec via
  `fizz.Response("404", ...)` and `fizz.Response("409", ...)`.
