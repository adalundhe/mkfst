# 06 — Authentication

Spins up the social-login machinery with the bundled **Dev** OAuth
provider, mounts the public `/auth` and `/avatar` endpoints, and
protects an `/me` endpoint with the `Auth` middleware.

```bash
go run ./examples/06-auth
```

Two ports are used:

| Port  | Service                                |
| ----- | -------------------------------------- |
| 8086  | The mkfst app under test.              |
| 8084  | The in-process Dev OAuth server.       |

### Why two servers?

OAuth2 is a *redirect* protocol — the user's browser must visit the
provider on a different origin to consent, then come back. The Dev
provider deliberately models a real OAuth2 server (GitHub / Google /…)
so the same code paths run; that means it lives on its own port and
serves the equivalents of `/login/oauth/authorize`, `/access_token`
and `/userinfo`.

Flow per login:

1. Browser → 8086: `/api/v1/auth?action=login&using=dev`
2. 8086 → browser: 302 to `http://localhost:8084/login/oauth/authorize?…`
3. 8084: renders the username form, then 302s back to
   `http://localhost:8086/api/v1/auth?action=callback&using=dev&code=…`
4. 8086 → 8084 (server-to-server): exchange code for token, fetch user.
5. 8086 → browser: set JWT cookie + 302 home.

When you swap `AddDevProvider` for `AddProvider("github", …)`, port
8084 becomes `github.com` — same architecture, zero code changes.

> **Never enable the Dev provider in production.** It accepts any name
> with no real authentication.

## Try it

In a browser:

1. Open http://localhost:8086/api/v1/auth?action=login&using=dev
2. The dev OAuth server presents a tiny form. Type any username and submit.
3. You are redirected back to `localhost:8086` with a JWT cookie set.
4. Call `http://localhost:8086/api/v1/me/` — it returns the authenticated
   user as JSON.

Or with curl (note the cookie jar):

```bash
# Trigger login. The dev server auto-confirms when you POST a name.
curl -s -c jar.txt -L "http://localhost:8086/api/v1/auth?action=login&using=dev"

# Once a JWT is in jar.txt, the protected route returns the user.
curl -s -b jar.txt http://localhost:8086/api/v1/me/ | jq
# {"id":"dev_xxx","name":"dev_user", ...}
```

Other useful endpoints under `/auth`:

```bash
curl -s 'http://localhost:8086/api/v1/auth?action=list' | jq
# {"providers":["dev"]}

curl -s -b jar.txt 'http://localhost:8086/api/v1/auth?action=user' | jq
curl -s -b jar.txt 'http://localhost:8086/api/v1/auth?action=status' | jq
curl -s -b jar.txt 'http://localhost:8086/api/v1/auth?action=logout'
```

## What this example shows

- Wiring `auth.NewService` with a `SecretReader`, a `Validator`, an
  `AvatarStore` and a TTL for tokens.
- Adding the Dev provider with `authSvc.AddDevProvider`, then running its
  side-car HTTP server in a goroutine.
- Mounting `Handlers()` (returns `authRoute, avatarRoute`).
- Using `mw.Auth` as a tonic middleware to gate `/me`.
- Reading the authenticated user inside a handler with
  `token.GetUserInfo(ctx.Request)`.

For OAuth against real providers (GitHub / Google / …), swap
`AddDevProvider` for `AddProvider("github", clientID, clientSecret)` —
everything else stays the same.

## A note on shutdown

The dev OAuth server runs in a goroutine. We tie it to a cancellable
context with `signal.NotifyContext(..., SIGINT, SIGTERM)` so a single
Ctrl+C cancels the context, `DevAuthServer.Run` returns, the goroutine
exits, and `Shutdown` is called automatically — no leak.

The framework's own `service.Run` does **not** currently expose a
graceful-shutdown hook (it calls `http.Server.ListenAndServe` directly),
so a second Ctrl+C is needed to terminate the main listener. The dev
server has already shut down cleanly by that point.
