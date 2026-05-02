# Configuration

`config.Config` is the single argument to `service.Create`. Anything you do
not set is filled in by, in order: an environment variable, then a built-in
default. Setting the field on the struct beats the environment variable.

## The struct

```go
type Config struct {
    Host     string             // listen host
    Port     int                // listen port
    SkipDB   bool               // skip opening a database
    UseHTTPS bool               // affects the URL printed by the Swagger UI
    Database db.ConnectionInfo  // see database.md
    Spec     openapi.Info       // OpenAPI 3 info block
}
```

## Field-by-field reference

| Field      | Env var          | Default     | Notes                                                                 |
| ---------- | ---------------- | ----------- | --------------------------------------------------------------------- |
| `Host`     | `APP_HOST`       | `"0.0.0.0"` | Hostname/IP to bind. The struct value wins over the env var.          |
| `Port`     | `APP_PORT`       | `8000`      | TCP port. Parsed via `strconv.Atoi`; bad values cause `log.Fatal`.    |
| `UseHTTPS` | `APP_SKIP_HTTPS` | `false`     | Note the env name is misleading — `true` means "render docs as `https://…`". mkfst does not start TLS for you. |
| `SkipDB`   | `APP_SKIP_DB`    | `false`     | When true, no DB is opened and `service.GetDB()` returns nil.         |
| `Database` | (see [database.md](database.md)) | empty `ConnectionInfo` | Per-driver fields each have their own env vars. |
| `Spec`     | —                | empty       | Pure metadata. Title, version, description, contact, license, etc.    |

## Precedence rule (a footgun to know)

The implementation in [`config/config.go`](../config/config.go) reads the env
var **first** and then overwrites it if the struct field is non-zero. That
means:

> **Setting any non-zero value on the struct disables the matching env
> override for that field.**

Practical consequence: if you want to allow operators to override the port
via `APP_PORT`, leave `Port: 0` in your struct. The default of `8000` only
applies when *both* the struct and the env var are absent.

## OpenAPI `Info`

Anything you put in `Spec` is rendered into `info` of the generated OpenAPI
document. The full type is from
[`fizz/openapi.Info`](../fizz/openapi/) — typical usage:

```go
Spec: openapi.Info{
    Title:          "Users API",
    Version:        "v1.2.3",
    Description:    "Users service: create, list, update, delete.",
    TermsOfService: "https://example.com/tos",
    Contact: &openapi.Contact{
        Name:  "Platform Team",
        Email: "platform@example.com",
        URL:   "https://example.com/contact",
    },
    License: &openapi.License{
        Name: "Apache-2.0",
        URL:  "https://www.apache.org/licenses/LICENSE-2.0",
    },
}
```

## Reading values back

`config.Config` does not provide accessors — it is a plain value. After
`service.Create` returns, the resolved config is held inside the service and
is not exported. If you need it elsewhere, build the config separately first:

```go
cfg := config.Create(config.Config{Port: 9001})
fmt.Println(cfg.ToAddress())             // "0.0.0.0:9001"
svc := service.Create(cfg)
```

`(Config).ToAddress()` is the only helper currently exported and returns
`fmt.Sprintf("%s:%d", Host, Port)`.

## Environment-variable cheat sheet

| Var                  | Used by                                |
| -------------------- | -------------------------------------- |
| `APP_HOST`           | service host                           |
| `APP_PORT`           | service port                           |
| `APP_SKIP_HTTPS`     | docs URL scheme (`http` / `https`)     |
| `APP_SKIP_DB`        | skip database opening                  |
| `DB_TYPE`            | `SQLITE` (default), `MYSQL`, `POSTGRESQL` |
| `DB_HOST`            | hostname or sqlite filename            |
| `DB_PORT`            | TCP port (ignored for sqlite)          |
| `DB_USERNAME`        | required for MySQL / PostgreSQL        |
| `DB_PASSWORD`        | required for MySQL / PostgreSQL        |
| `DB_NAME`            | database/schema name (default `app`)   |
| `DB_USE_SSL`         | bool, currently informational          |
