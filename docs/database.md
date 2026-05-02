# Database

mkfst opens a single `*sql.DB` at boot and threads it through every
handler. The pool is lazy — `sql.Open` does not actually dial the database;
the first query does.

## Picking a driver

Set `Database.Type` (or `DB_TYPE`) to one of:

- `""` or `SQLITE` — uses `github.com/mattn/go-sqlite3` (CGO).
- `MYSQL`         — uses `github.com/go-sql-driver/mysql` (you import it).
- `POSTGRESQL`    — uses `pgx` via `github.com/jackc/pgx/v5/stdlib` (you import it).

mkfst itself only blank-imports `mattn/go-sqlite3` from
[`main.go`](../main.go). For MySQL or Postgres your application has to
register the driver:

```go
import (
    _ "github.com/go-sql-driver/mysql"  // for DB_TYPE=MYSQL
    _ "github.com/jackc/pgx/v5/stdlib"  // for DB_TYPE=POSTGRESQL
)
```

## Connection info

```go
type ConnectionInfo struct {
    Type     string  // SQLITE | MYSQL | POSTGRESQL
    Host     string
    Port     string
    Username string
    Password string
    Database string
    UseSSL   bool
}
```

| Field    | Env          | Default (SQLITE) | Default (MYSQL/POSTGRES) |
| -------- | ------------ | ---------------- | ------------------------ |
| Type     | `DB_TYPE`    | `SQLITE`         | —                        |
| Host     | `DB_HOST`    | `app.db`         | `localhost`              |
| Port     | `DB_PORT`    | (empty)          | `3306` / `5432`          |
| Username | `DB_USERNAME`| (empty)          | required (`log.Fatal`)   |
| Password | `DB_PASSWORD`| (empty)          | required (`log.Fatal`)   |
| Database | `DB_NAME`    | `app`            | `app`                    |
| UseSSL   | `DB_USE_SSL` | false            | false                    |

> **SQLite-specific:** `Host` is the *file path*. mkfst checks whether the
> path exists and creates an empty file if it does not. This means the file
> is created relative to the process's working directory, so if you launch
> from somewhere other than your project root, the database can land in an
> unexpected place.

## DSN strings (for reference)

```
SQLITE       sqlite3       <Host>
MYSQL        mysql         <User>:<Pass>@tcp(<Host>:<Port>)/<DB>
POSTGRESQL   pgx           postgres://<User>:<Pass>@<Host>:<Port>/<DB>?sslmode=disable
```

The Postgres DSN currently hard-codes `sslmode=disable` — `UseSSL` is
parsed but not yet wired into the DSN. If you need TLS today, pre-build
the DSN yourself and call `db.Create` directly, then assign the connection
to the router (see "Bring your own connection" below).

## Using the database in a handler

```go
func listUsers(ctx *gin.Context, db *sql.DB) ([]User, error) {
    rows, err := db.QueryContext(ctx.Request.Context(),
        "SELECT id, name FROM users ORDER BY id")
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var out []User
    for rows.Next() {
        var u User
        if err := rows.Scan(&u.ID, &u.Name); err != nil {
            return nil, err
        }
        out = append(out, u)
    }
    return out, rows.Err()
}
```

Always pass `ctx.Request.Context()` to `QueryContext` / `ExecContext` so
that client cancellation propagates and OpenTelemetry spans get attached
to the parent request span.

### Migrations / schema setup

mkfst does not bundle migrations. Either:

- run a separate migration tool (golang-migrate, atlas, goose, …) before
  starting the service, or
- run `db.Exec("CREATE TABLE IF NOT EXISTS …")` once after `service.Create`
  and before `service.Run` (this is what `main.go` does).

## Skipping the database

If your service is stateless, set `SkipDB: true` and the second handler
parameter will always be `nil`. Do not dereference it.

```go
svc := service.Create(config.Config{
    Port:   8080,
    SkipDB: true,
})

svc.Route("GET", "/health", 200, nil,
    func(ctx *gin.Context, _ *sql.DB) (string, error) {
        return "OK", nil
    },
)
```

## Bring your own connection

If you need a custom DSN, connection pooling settings or PgBouncer-style
prepared-statement disabling, build the connection yourself and replace
`router.Db.Conn`:

```go
custom, err := sql.Open("pgx",
    "postgres://user:pass@host:5432/app?sslmode=verify-full")
if err != nil {
    log.Fatal(err)
}
custom.SetMaxOpenConns(50)
custom.SetMaxIdleConns(10)
custom.SetConnMaxLifetime(5 * time.Minute)

svc := service.Create(config.Config{Port: 8080})
svc.Router.Db.Conn = custom
```

(`Router.Db` and `.Conn` are exported, so you can swap them post-`Create`.)
