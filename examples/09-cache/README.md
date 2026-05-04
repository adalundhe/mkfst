# 09-cache

API server with a response cache backed by `providers/cache`.

## Run

```sh
go run ./examples/09-cache
```

Then:

```sh
# First call: cache miss, ~200ms.
time curl -s http://localhost:8081/expensive/42 -i | head -5

# Second call: cache hit, near-instant.
time curl -s http://localhost:8081/expensive/42 -i | head -5

# Force a recompute.
time curl -s http://localhost:8081/expensive/42 -i -H 'Cache-Bypass: 1' | head -5

# Flush the cache.
curl -X POST http://localhost:8081/cache/clear
```

The `X-Cache: HIT/MISS` header tells you which path served the response.
The `calls_so_far` field in the JSON body shows how many times the handler
actually ran.

## What this demonstrates

- Constructing an in-memory `Cache` with a byte budget.
- Wrapping the response with a small middleware that consults the cache
  on read and stores the body on miss.
- Using `DeletePrefix` to flush a namespace.

Swap `cache.NewMemoryCache` for `cache.NewRedisCache` or
`cache.NewSQLCache` and the rest of the code is unchanged.
