// Package policy is mkfst's RBAC layer. The framework defines the
// canonical set of Permissions; operators define Roles (named sets
// of permissions, optionally scoped to specific resource names);
// every provider asks the same Enforcer "is this subject allowed
// to do this thing to this resource?"
//
// Why permissions are fixed and roles are user-defined:
//   - Permissions describe what the system actually does. They
//     belong to the framework and are stable across deployments.
//   - Roles describe how an organization wants to slice access.
//     They belong to the operator and vary by deployment.
//
// A user can't invent a new permission ("stack.frobulate") because
// no provider checks it. They CAN invent a new role
// ("staging-deployer") that combines existing permissions however
// they want.
package policy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
)

// === Permissions ===
//
// Add new permissions ONLY when adding new provider operations.
// Each permission's string value is stable — it appears in role
// definitions, audit logs, and error messages.

type Permission string

const (
	// === Stacks (providers/docker/network) ===
	PermStackCreate     Permission = "stack.create"
	PermStackDelete     Permission = "stack.delete"
	PermStackRead       Permission = "stack.read"
	PermStackUp         Permission = "stack.up"
	PermStackDown       Permission = "stack.down"
	PermStackRunOneShot Permission = "stack.run_oneshot"
	PermStackExec       Permission = "stack.exec"
	PermStackMonitor    Permission = "stack.monitor"
	PermStackIngress    Permission = "stack.ingress"

	// === Workflows (providers/workflows) ===
	PermWorkflowRegister Permission = "workflow.register"
	PermWorkflowSubmit   Permission = "workflow.submit"
	PermWorkflowInspect  Permission = "workflow.inspect"
	PermWorkflowCancel   Permission = "workflow.cancel"

	// === TS workflows (providers/ts) ===
	PermTSSubmit Permission = "ts.submit"
	PermTSRun    Permission = "ts.run"

	// === Containers (providers/docker, raw) ===
	PermContainerRun     Permission = "container.run"
	PermContainerExec    Permission = "container.exec"
	PermContainerLogs    Permission = "container.logs"
	PermContainerInspect Permission = "container.inspect"
	PermContainerRemove  Permission = "container.remove"

	// === VFS (providers/vfs, providers/files) ===
	PermVFSRead   Permission = "vfs.read"
	PermVFSWrite  Permission = "vfs.write"
	PermVFSList   Permission = "vfs.list"
	PermVFSDelete Permission = "vfs.delete"

	// === Cache (providers/cache) ===
	PermCacheRead   Permission = "cache.read"
	PermCacheWrite  Permission = "cache.write"
	PermCacheDelete Permission = "cache.delete"

	// === Tasks (providers/tasks) ===
	PermTaskEnqueue Permission = "task.enqueue"
	PermTaskCancel  Permission = "task.cancel"
	PermTaskInspect Permission = "task.inspect"

	// === Operator-only ===
	PermModuleAdd    Permission = "module.add"
	PermModuleRemove Permission = "module.remove"
	PermConfigReload Permission = "config.reload"
	PermAdminAll     Permission = "admin.*" // grants every other permission
)

// AllPermissions is the canonical list. Used by the YAML/JSON
// validator to reject role definitions referencing unknown perms.
func AllPermissions() []Permission {
	return []Permission{
		PermStackCreate, PermStackDelete, PermStackRead,
		PermStackUp, PermStackDown,
		PermStackRunOneShot, PermStackExec, PermStackMonitor, PermStackIngress,
		PermWorkflowRegister, PermWorkflowSubmit, PermWorkflowInspect, PermWorkflowCancel,
		PermTSSubmit, PermTSRun,
		PermContainerRun, PermContainerExec, PermContainerLogs, PermContainerInspect, PermContainerRemove,
		PermVFSRead, PermVFSWrite, PermVFSList, PermVFSDelete,
		PermCacheRead, PermCacheWrite, PermCacheDelete,
		PermTaskEnqueue, PermTaskCancel, PermTaskInspect,
		PermModuleAdd, PermModuleRemove, PermConfigReload,
		PermAdminAll,
	}
}

// IsKnown reports whether p is a framework-defined permission.
func IsKnown(p Permission) bool {
	for _, k := range AllPermissions() {
		if k == p {
			return true
		}
	}
	return false
}

// === Role ===

// Role is a named set of permissions. Operator-defined. Each
// permission may optionally be scoped to a list of resource-name
// patterns (filepath.Match globs); when scoped, the permission
// only applies to resources whose name matches one of the patterns.
type Role struct {
	Name        string
	Permissions []Permission
	// ResourceScopes narrows permissions. Example:
	//
	//   {
	//     "stack.up":   ["dev-*", "staging-*"],
	//     "stack.exec": ["dev-*"],
	//   }
	//
	// means the role allows Up on stacks dev-* and staging-*, but
	// only Exec on dev-*. Permissions absent from this map are
	// unscoped (apply to any resource).
	ResourceScopes map[Permission][]string
}

// Has reports whether the role lists the permission (without
// considering scope).
func (r *Role) Has(p Permission) bool {
	for _, q := range r.Permissions {
		if q == p || q == PermAdminAll {
			return true
		}
	}
	return false
}

// AppliesTo reports whether the role's permission p covers the
// given resource name. Unscoped permissions apply to any name;
// scoped permissions match against their pattern list.
func (r *Role) AppliesTo(p Permission, resourceName string) bool {
	if !r.Has(p) {
		return false
	}
	if r.Has(PermAdminAll) {
		return true
	}
	patterns, scoped := r.ResourceScopes[p]
	if !scoped || resourceName == "" {
		return true
	}
	for _, pat := range patterns {
		if pat == "*" {
			return true
		}
		if match, _ := filepath.Match(pat, resourceName); match {
			return true
		}
	}
	return false
}

// === Enforcer ===

// Subject identifies who is asking. Sourced from the HTTP middleware
// (auth/token.User) or set explicitly for non-HTTP code paths.
type Subject struct {
	ID    string
	Roles []string
	// Tags is opaque key/value metadata an operator may use for
	// custom audit / logging.
	Tags map[string]string
}

// Enforcer is the central policy decision point. Construct with
// NewEnforcer; register roles via AddRole; consult via Allow.
type Enforcer struct {
	mu    sync.RWMutex
	roles map[string]*Role
	// AuditFn, if non-nil, is called for every Allow decision.
	// Use for security audit logging. The decision is what was
	// returned to the caller; reason is human-readable diagnostic.
	AuditFn func(s Subject, perm Permission, resource string, allowed bool, reason string)
}

// NewEnforcer returns a fresh enforcer with no roles. Add roles
// via AddRole or LoadRoles before checking.
func NewEnforcer() *Enforcer {
	return &Enforcer{roles: map[string]*Role{}}
}

// AddRole registers a role. Validates that every permission listed
// is framework-known.
func (e *Enforcer) AddRole(r *Role) error {
	if r == nil || r.Name == "" {
		return errors.New("policy.AddRole: role and name required")
	}
	for _, p := range r.Permissions {
		if !IsKnown(p) {
			return fmt.Errorf("policy.AddRole: %s: unknown permission %q", r.Name, p)
		}
	}
	for p := range r.ResourceScopes {
		if !IsKnown(p) {
			return fmt.Errorf("policy.AddRole: %s: unknown scoped permission %q", r.Name, p)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.roles[r.Name] = r
	return nil
}

// LoadRoles bulk-registers roles. Returns the first validation error.
func (e *Enforcer) LoadRoles(roles []*Role) error {
	for _, r := range roles {
		if err := e.AddRole(r); err != nil {
			return err
		}
	}
	return nil
}

// Roles returns a snapshot of all registered roles.
func (e *Enforcer) Roles() []*Role {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*Role, 0, len(e.roles))
	for _, r := range e.roles {
		out = append(out, r)
	}
	return out
}

// Role returns the named role or nil.
func (e *Enforcer) Role(name string) *Role {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.roles[name]
}

// Allow reports whether subject has permission, optionally scoped
// to resourceName. Returns (allowed, reason). reason is populated
// for both allows and denies — useful for audit + structured
// errors.
func (e *Enforcer) Allow(s Subject, perm Permission, resourceName string) (bool, string) {
	if !IsKnown(perm) {
		return e.audit(s, perm, resourceName, false, "unknown permission")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(s.Roles) == 0 {
		return e.audit(s, perm, resourceName, false, "subject has no roles")
	}
	for _, name := range s.Roles {
		role := e.roles[name]
		if role == nil {
			continue
		}
		if role.AppliesTo(perm, resourceName) {
			return e.audit(s, perm, resourceName, true, "matched role "+name)
		}
	}
	return e.audit(s, perm, resourceName, false, "no role grants this permission/scope")
}

func (e *Enforcer) audit(s Subject, perm Permission, resource string, allowed bool, reason string) (bool, string) {
	if e.AuditFn != nil {
		e.AuditFn(s, perm, resource, allowed, reason)
	}
	return allowed, reason
}

// Require returns ErrDenied (wrapped with reason) when Allow is false.
func (e *Enforcer) Require(s Subject, perm Permission, resourceName string) error {
	ok, reason := e.Allow(s, perm, resourceName)
	if ok {
		return nil
	}
	return fmt.Errorf("%w: %s on %q: %s", ErrDenied, perm, resourceName, reason)
}

// ErrDenied is returned by Require when the subject lacks the perm.
// Wrapped with permission + reason for diagnostics.
var ErrDenied = errors.New("policy: permission denied")

// === Subject context plumbing ===
//
// HTTP middleware sets the Subject on the request ctx; provider
// code reads it back via SubjectFromContext.

type subjectKey struct{}

// WithSubject returns a derived context carrying the subject.
func WithSubject(ctx context.Context, s Subject) context.Context {
	return context.WithValue(ctx, subjectKey{}, s)
}

// SubjectFromContext returns the subject set by upstream middleware.
// The zero Subject is returned when none is set; callers can detect
// this via len(s.Roles) == 0.
func SubjectFromContext(ctx context.Context) Subject {
	if v, ok := ctx.Value(subjectKey{}).(Subject); ok {
		return v
	}
	return Subject{}
}

// === Checker — what providers consume ===
//
// Providers don't depend on the HTTP layer. They consume a Checker
// to ask "is this op allowed for whoever is in the ctx?" Allowing
// nil = pass-through (operator opted out of RBAC for that
// provider).

// Checker is the narrow interface providers depend on.
type Checker interface {
	Check(ctx context.Context, perm Permission, resourceName string) error
}

// EnforcerChecker adapts an Enforcer to the Checker interface, using
// the subject pulled from the ctx.
type EnforcerChecker struct{ E *Enforcer }

// Check implements Checker. Returns nil when allowed,
// fmt.Errorf-wrapped ErrDenied when denied.
func (c EnforcerChecker) Check(ctx context.Context, perm Permission, resourceName string) error {
	if c.E == nil {
		return nil
	}
	subj := SubjectFromContext(ctx)
	return c.E.Require(subj, perm, resourceName)
}

// AllowAllChecker is a no-op Checker that permits everything. The
// default for providers that haven't been wired with a real
// Checker.
type AllowAllChecker struct{}

// Check always returns nil.
func (AllowAllChecker) Check(_ context.Context, _ Permission, _ string) error { return nil }
