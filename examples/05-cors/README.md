# 05 — CORS

Mounts the bundled CORS middleware with a non-trivial config: explicit
origins, a wildcard subdomain, custom exposed headers and credentials.

```bash
go run ./examples/05-cors
```

| Method | Path  | What it does                              |
| ------ | ----- | ----------------------------------------- |
| GET    | `/api`| Echoes the `Origin` header back as JSON.  |

## Try it

A successful preflight from an allowed origin:

```bash
curl -is -XOPTIONS http://localhost:8085/api \
  -H 'Origin: https://app.example.com' \
  -H 'Access-Control-Request-Method: POST' \
  -H 'Access-Control-Request-Headers: content-type'
# Access-Control-Allow-Origin: https://app.example.com
# Access-Control-Allow-Methods: GET,POST,PATCH,DELETE
# Access-Control-Allow-Headers: Origin,Content-Type,Authorization
# Access-Control-Allow-Credentials: true
# Access-Control-Max-Age: 43200
```

A wildcard match:

```bash
curl -is http://localhost:8085/api -H 'Origin: https://staging.example.com'
# Access-Control-Allow-Origin: https://staging.example.com
```

A disallowed origin (no `Access-Control-Allow-Origin` returned):

```bash
curl -is http://localhost:8085/api -H 'Origin: https://evil.example.org'
```

## What this example shows

- `cors.CORS(cors.Config{...})` returns a tonic-shaped middleware,
  registered just like any other.
- `AllowWildcard: true` enables `https://*.example.com` matching.
- `cors.Default()` is also available (allows everything — dev only).
