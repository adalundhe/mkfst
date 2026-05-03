# Cache

`providers/cache` is mkfst's pluggable key-value cache: one
`Cache` interface, three backends (memory, Redis/Valkey, SQL).

## Interface

```go
type Cache interface {
    Get(ctx context.Context, key string) ([]byte, bool, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    Delete(ctx context.Context, key string) error
    DeletePrefix(ctx context.Context, prefix string) (int, error)
    Close() error
}
```

`Get` returns `(value, found, error)` — a miss is not an error,
backend failure is. Pass `ttl=0` to `Set` for no expiry.

## Backends

### Memory (single-process LRU + TTL)

```go
c := cache.NewMemoryCache(cache.MemoryOpts{
    MaxBytes: 256 << 20, // 256 MiB cap
})
defer c.Close()
```

Pure Go, no external deps. Best for response caches, computed-value
caches, anything that doesn't need to survive a restart.

### Redis / Valkey

```go
c, err := cache.NewRedisCache(cache.RedisOpts{
    Addr:     "127.0.0.1:6379",
    Password: "", // optional
    DB:       0,
})
```

Uses `go-redis/v9`. `DeletePrefix` walks via `SCAN` + `UNLINK` (non-
blocking server-side).

### SQL (Postgres / MySQL / SQLite)

```go
import mkfstdb "mkfst/db"

conn := &mkfstdb.Connection{Conn: rawDB, Config: mkfstdb.ConnectionInfo{Type: "POSTGRESQL"}}
c, err := cache.NewSQLCache(conn, cache.SQLOpts{
    TablePrefix:    "myapp_cache_",
    SweepInterval:  5 * time.Minute, // background expired-row sweeper
})
```

Uses your existing app DB (no extra infrastructure). On first construction
it runs `CREATE TABLE IF NOT EXISTS`; subsequent constructions reuse the
existing table. Stale rows are filtered on `Get` and physically deleted
by the background sweeper.

## Usage with the HTTP server

A response-cache middleware is a few lines:

```go
svc.Use(func(c *gin.Context) {
    if c.Request.Method != "GET" {
        c.Next()
        return
    }
    key := "resp:" + c.Request.URL.RequestURI()
    if body, ok, _ := myCache.Get(c.Request.Context(), key); ok {
        c.Data(200, "application/json", body)
        c.Abort()
        return
    }
    rec := &recordingWriter{ResponseWriter: c.Writer}
    c.Writer = rec
    c.Next()
    if rec.code == 200 {
        _ = myCache.Set(c.Request.Context(), key, rec.body.Bytes(), 60*time.Second)
    }
})
```

See [`examples/09-cache`](../examples/09-cache) for a runnable version.

## When the cache writes are best-effort

- Setting after a slow request: drop on cancellation rather than
  blocking the response.
- DeletePrefix: in Redis, may lag if concurrent Sets land mid-scan.
  Authoritative for SQL (single transaction).
- Memory cache LRU eviction: silent. Don't store anything you can't
  recompute.

## Conformance

Every backend passes the same conformance suite (`providers/cache/conformance_test.go`).
Switching from memory to Redis to SQL is a one-line constructor change.
