# Authentication

`mkfst/middleware/auth` is a fork of [`go-pkgz/auth`](https://github.com/go-pkgz/auth)
that swaps the Chi router for Gin and exposes its handlers in the tonic
shape. It provides:

- JWT issuance and verification (cookie + header + query).
- Social-login providers: GitHub, Google, Facebook, Microsoft, Yandex,
  Twitter, Battle.net, Patreon, Apple, Telegram.
- Direct username/password providers and verification-link providers.
- A Dev provider for local testing without real OAuth credentials.
- An avatar proxy with pluggable storage (local FS, BoltDB, MongoDB GridFS,
  no-op).
- `Auth` and `Trace` middleware (the latter populates user info if present
  but never rejects the request).

## Setting up the service

```go
import (
    "time"

    "mkfst/auth/avatar"
    "mkfst/auth/token"
    "mkfst/middleware/auth"
)

opts := auth.Opts{
    SecretReader: token.SecretFunc(func(aud string) (string, error) {
        return os.Getenv("JWT_SECRET"), nil
    }),
    TokenDuration:  5 * time.Minute,    // JWT lifetime
    CookieDuration: 30 * 24 * time.Hour,
    Issuer:         "users-api",
    URL:            "https://api.example.com",
    AvatarStore:    avatar.NewLocalFS("/var/lib/avatars"),
    Validator: token.ValidatorFunc(func(_ string, c token.Claims) bool {
        return c.User != nil && c.User.Email != ""
    }),
    SecureCookies: true,
}

authSvc := auth.NewService(opts)
authSvc.AddProvider("github", os.Getenv("GH_CID"), os.Getenv("GH_CSECRET"))
authSvc.AddProvider("google", os.Getenv("GG_CID"), os.Getenv("GG_CSECRET"))
```

Key fields on `Opts` (the rest are documented in
[`middleware/auth/auth.go`](../middleware/auth/auth.go)):

| Field             | Notes                                                                |
| ----------------- | -------------------------------------------------------------------- |
| `SecretReader`    | Returns the HMAC key for signing. Required.                          |
| `Validator`       | Hook to reject otherwise-valid tokens (e.g. ban list).               |
| `URL`             | Public URL of the API; used to build OAuth callbacks. Required.      |
| `AvatarStore`     | Anything implementing `avatar.Store`. Use `avatar.NewNoOp()` to skip. |
| `TokenDuration`   | JWT TTL. Refreshed automatically on every request that hits `Auth`.  |
| `CookieDuration`  | Cookie TTL (forces re-login).                                        |
| `SecureCookies`   | Set `Secure` on the cookie. Required for browsers over HTTPS.        |
| `SameSiteCookie`  | `http.SameSiteLaxMode` is a sensible default.                        |
| `JWTHeaderKey`    | Override the default `X-JWT` header name.                            |
| `DisableXSRF`     | Disable XSRF token enforcement (testing only).                       |

## Mounting the routes

`AuthService.Handlers()` returns two tonic handlers:

```go
authRoute, avatarRoute := authSvc.Handlers()
mw := authSvc.Middleware()

api := router.CreateGroup("/api/v1", "v1", "v1 API")
api.Route("GET", "/auth",   200, nil, authRoute)   // /login, /logout, /callback, /user, /status
api.Route("GET", "/avatar", 200, nil, avatarRoute) // serves cached avatars

protected := api.Group("/me", "Me", "Authenticated user routes")
protected.Middleware(mw.Auth) // 401 if no valid JWT
protected.Route("GET", "/", 200, nil, getMe)
```

The single `/auth` endpoint dispatches based on `?action=`:

| Query param       | Effect                                                    |
| ----------------- | --------------------------------------------------------- |
| `?action=list`    | Returns `{"providers": [...]}`                            |
| `?action=login&using=github`  | Redirects to GitHub for OAuth.                |
| `?action=callback&using=github` | OAuth callback URL.                          |
| `?action=user`    | Returns the current claims.                                |
| `?action=status`  | Returns `{"status": "Logged in", "user": "..."}`          |
| `?action=logout`  | Clears the JWT cookie.                                    |

## Reading the user inside a handler

```go
import "mkfst/auth/token"

func getMe(ctx *gin.Context, _ *sql.DB) (token.User, error) {
    u, err := token.GetUserInfo(ctx.Request)
    if err != nil {
        return token.User{}, err
    }
    return u, nil
}
```

`token.User` carries `ID`, `Name`, `Email`, `Picture`, `Audience` and a
free-form `Attributes map[string]interface{}`. There are helpers
(`SetBoolAttr`, `IsAdmin`, `SetAdmin`, `SliceAttr`, …) for the common
shapes.

## Two middlewares: `Auth` vs `Trace`

- `mw.Auth`  — rejects the request with 401 if there is no valid token.
  Use it on routes that *must* have a user.
- `mw.Trace` — extracts the user info if it exists, but lets the request
  through either way. Useful when an endpoint has both anonymous and
  authenticated paths.

## Direct (username + password) providers

When you don't want OAuth at all:

```go
authSvc.AddDirectProvider("local",
    provider.CredCheckerFunc(func(user, pwd string) (bool, error) {
        return checkPassword(db, user, pwd)
    }))
```

Clients then `POST /auth/local/login` with form fields `user` and `passwd`,
and `mkfst` issues a JWT cookie + header.

## The Dev provider

For local development you can spin up an in-process OAuth2 server that
never validates anything:

```go
authSvc.AddDevProvider("localhost", 8084)
go func() {
    devServer, _ := authSvc.DevAuth()
    devServer.Run(context.Background())
}()
```

Sign in by visiting `http://localhost:8080/api/v1/auth?action=login&using=dev`.
The dev server presents a tiny form; whatever name you type becomes the
authenticated user. Never enable in production.

## Avatars

Implementations of `avatar.Store` shipped with mkfst:

- `avatar.NewLocalFS(dir)` — files on disk
- `avatar.NewBoltDB(path, bucket)` — embedded Bolt store
- `avatar.NewGridFS(...)` — MongoDB GridFS
- `avatar.NewNoOp()` — disables avatar caching entirely

Avatars are fetched from the OAuth provider on first login and cached. A
sha-1 of the URL becomes the avatar ID. Mount with `avatarRoute` (returned
by `Handlers()`); the route serves `/avatar/<id>.image`.

## End-to-end example

See [`../examples/06-auth`](../examples/06-auth) for a working server with
the Dev provider, the `Auth` middleware and a protected `/me` endpoint.
