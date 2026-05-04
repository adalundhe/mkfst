# Policy (RBAC)

`providers/policy` is mkfst's authorization layer. It plugs into the
existing `auth/` (JWT + social providers) to gate every privileged
provider operation: stack ops, container ops, VFS reads/writes, cache
ops, task enqueues, TS workflow submits, module + config changes.

## Two-layer model

| Layer | Defined by | Stable across deployments? |
|---|---|---|
| **Permission** | mkfst (framework) | Yes — adding a new permission requires a new provider operation |
| **Role** | Operator (you) | No — operators define roles however their org slices access |

Permissions describe **what the system actually does**. Roles describe
**how an organization wants to slice access**. A user can't invent a
new permission ("stack.frobulate") because no provider checks it.
A user CAN invent a new role ("staging-deployer") that combines
existing permissions however they want.

## Permissions

Framework-defined enum (full list in `providers/policy/policy.go`):

| Group | Permissions |
|---|---|
| Stacks | `stack.create`, `stack.delete`, `stack.read`, `stack.up`, `stack.down`, `stack.run_oneshot`, `stack.exec`, `stack.monitor`, `stack.ingress` |
| Workflows (Go) | `workflow.register`, `workflow.submit`, `workflow.inspect`, `workflow.cancel` |
| TS Workflows | `ts.submit`, `ts.run` |
| Containers | `container.run`, `container.exec`, `container.logs`, `container.inspect`, `container.remove` |
| VFS | `vfs.read`, `vfs.write`, `vfs.list`, `vfs.delete` |
| Cache | `cache.read`, `cache.write`, `cache.delete` |
| Tasks | `task.enqueue`, `task.cancel`, `task.inspect` |
| Operator | `module.add`, `module.remove`, `config.reload` |
| Master | `admin.*` (grants every other permission) |

## Roles

A `Role` is a named set of permissions, optionally **scoped** to
specific resource-name patterns (filepath.Match globs):

```go
role := &policy.Role{
    Name: "dev-deployer",
    Permissions: []policy.Permission{
        policy.PermStackUp, policy.PermStackDown,
        policy.PermStackExec, policy.PermStackRunOneShot,
        policy.PermTSSubmit, policy.PermTSRun,
    },
    ResourceScopes: map[policy.Permission][]string{
        // Up/Down only on dev/staging stacks.
        policy.PermStackUp:   {"dev-*", "staging-*"},
        policy.PermStackDown: {"dev-*", "staging-*"},
        // Exec only on dev (operators handle staging exec).
        policy.PermStackExec: {"dev-*"},
        // TS submit only against dev stacks.
        policy.PermTSSubmit:  {"dev-*"},
    },
}
```

A subject (= authenticated user) holds **role names**. The Enforcer
resolves names → roles → permission/scope checks.

## Wiring up

```go
import (
    "mkfst/providers/policy"
    "mkfst/providers/docker/network"
    "mkfst/providers/ts"
)

// 1. Build the enforcer + register operator-defined roles.
e := policy.NewEnforcer()
_ = e.AddRole(&policy.Role{ Name: "dev-deployer", Permissions: [...] })
_ = e.AddRole(&policy.Role{ Name: "viewer",       Permissions: [...] })

// 2. (Optional) audit hook fires on every Allow decision.
e.AuditFn = func(s policy.Subject, p policy.Permission, r string, allowed bool, reason string) {
    log.Printf("[audit] subj=%s perm=%s resource=%s allowed=%v reason=%s",
        s.ID, p, r, allowed, reason)
}

// 3. Hand the enforcer to providers via the Checker interface.
checker := policy.EnforcerChecker{E: e}

netEng, _ := network.NewEngine(cli.SDK(), network.EngineOpts{
    Policy: checker, // checked at Up/Down/RunOneShot/Exec
})

tsEng, _ := ts.NewEngine(ts.EngineOpts{
    WorkflowEngine: wfEng,
    Allowlist:      al,
    Policy:         checker, // checked at SubmitWith/Run
})

// 4. Mount the HTTP middleware (auth + subject injection).
svc.Middleware(policy.InjectSubject(e))
```

## Per-route enforcement (HTTP)

Use `policy.Require` on individual routes for explicit gates:

```go
svc.Route("POST", "/stacks/:name/up", 200,
    nil,
    policy.Require(e, policy.PermStackUp, policy.ResourceFromPath("name")),
    func(g *gin.Context, _ *sql.DB, in *struct {
        Name string `path:"name"`
    }) (struct{ State string }, error) {
        // ... already gated; the middleware aborted with 403 if denied
        return struct{ State string }{State: "up"}, nil
    },
)
```

When the subject lacks the permission, `Require` aborts with HTTP 403:

```json
{
  "error":      "forbidden",
  "permission": "stack.up",
  "resource":   "production-foo",
  "reason":     "no role grants this permission/scope"
}
```

## Provider-level defense in depth

Even if HTTP middleware is misconfigured, providers re-check via
`Engine.opts.Policy` on the request ctx. The Subject is plumbed
through `policy.WithSubject(ctx, subj)` and read back via
`policy.SubjectFromContext(ctx)`. If both layers are wired, you have
two independent enforcement points; if only one is, that one suffices.

## Subject identification

`Subject` is built from the existing `auth/token.User`:

- `ID` ← `User.ID`
- `Roles` ← `User.SliceAttr("roles")`
- Admin promotion: `User.IsAdmin() == true` → synthetic `__admin`
  role (which holds `admin.*`) prepended automatically

Set roles on a user via the auth layer when issuing JWTs:

```go
u := token.User{ID: "u-123", Name: "alice"}
u.SetSliceAttr("roles", []string{"dev-deployer", "viewer"})
jwt := token.SignJWT(u, secret, ...)
```

## Audit trail

Every `Enforcer.Allow` call (allow OR deny) fires `AuditFn` if set.
Use this to feed an audit log, an OTEL span, a SIEM, or whatever your
compliance posture demands. The reason string is human-readable
("matched role admin", "no role grants this permission/scope",
"unknown permission") and stable enough to alert on.

## Operator playbook

A typical mkfst.yaml fragment (config loader supports this; see
`providers/ts/config`):

```yaml
roles:
  - name: viewer
    permissions:
      - stack.read
      - workflow.inspect
      - task.inspect

  - name: dev-deployer
    permissions:
      - stack.up
      - stack.down
      - stack.run_oneshot
      - stack.exec
      - ts.submit
      - ts.run
    resourceScopes:
      stack.up:           ["dev-*", "staging-*"]
      stack.down:         ["dev-*", "staging-*"]
      stack.run_oneshot:  ["dev-*"]
      stack.exec:         ["dev-*"]
      ts.submit:          ["dev-*"]
      ts.run:             ["dev-*"]

  - name: prod-operator
    permissions:
      - admin.*  # everything
```

## See also

- [providers.md](providers.md) — the provider map
- [auth.md](auth.md) — authentication that produces the Subject
- [`examples/13-ts-workflows`](../examples/13-ts-workflows) — TS submission with stack scoping (combines well with policy)
