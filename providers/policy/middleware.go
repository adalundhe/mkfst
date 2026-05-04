package policy

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"mkfst/auth/token"
)

// === HTTP middleware ===
//
// Two pieces:
//
//   - InjectSubject: a tonic-shape middleware that converts the
//     authenticated user (from the existing auth/token package)
//     into a policy.Subject and attaches it to the request ctx.
//     Mount once at the service / group level.
//
//   - Require: returns a tonic-shape middleware that aborts the
//     request with 403 if the authenticated subject lacks the
//     required permission (optionally scoped). Use per-route or
//     per-group.

// SubjectFromUser maps an authenticated User → Subject. The default
// reads `roles` from the user's slice attribute; an admin user gets
// every permission via the synthetic admin.* role.
func SubjectFromUser(u token.User) Subject {
	roles := u.SliceAttr("roles")
	if u.IsAdmin() {
		// Promote admins to a synthetic role with admin.*; the
		// enforcer treats AdminAll as a master grant.
		roles = append([]string{"__admin"}, roles...)
	}
	return Subject{
		ID:    u.ID,
		Roles: roles,
		Tags:  map[string]string{"name": u.Name, "email": u.Email},
	}
}

// InjectSubject is a middleware that pulls the authenticated user
// off the request, builds a Subject, and stashes it on the ctx so
// every downstream handler / provider can find it.
//
// Returns a tonic-shape middleware (matches mkfst's
// service.Middleware contract).
func InjectSubject(e *Enforcer) func(*gin.Context, *sql.DB) (any, error) {
	return func(g *gin.Context, _ *sql.DB) (any, error) {
		u, err := token.GetUserInfo(g.Request)
		if err != nil {
			// Unauthenticated requests get an empty subject;
			// downstream Require() will deny if the route needs
			// a permission. This lets some routes be public
			// without forcing every middleware path to assert
			// auth.
			ctx := WithSubject(g.Request.Context(), Subject{})
			g.Request = g.Request.WithContext(ctx)
			return nil, nil
		}
		// Auto-register the synthetic admin role if missing.
		if e != nil && e.Role("__admin") == nil {
			_ = e.AddRole(&Role{Name: "__admin", Permissions: []Permission{PermAdminAll}})
		}
		s := SubjectFromUser(u)
		ctx := WithSubject(g.Request.Context(), s)
		g.Request = g.Request.WithContext(ctx)
		return nil, nil
	}
}

// Require returns a per-route middleware that aborts with 403 if
// the authenticated subject lacks perm. resourceFromCtx, when
// non-nil, computes the resource name from the gin context (e.g.,
// `g.Param("name")` for a stack-name path param) so the
// Enforcer's resource-scope check fires.
func Require(e *Enforcer, perm Permission, resourceFromCtx func(*gin.Context) string) func(*gin.Context, *sql.DB) (any, error) {
	if e == nil {
		// Nil enforcer = no auth; pass-through. Useful for tests
		// + dev. Production should always supply one.
		return func(*gin.Context, *sql.DB) (any, error) { return nil, nil }
	}
	return func(g *gin.Context, _ *sql.DB) (any, error) {
		subj := SubjectFromContext(g.Request.Context())
		var resource string
		if resourceFromCtx != nil {
			resource = resourceFromCtx(g)
		}
		if err := e.Require(subj, perm, resource); err != nil {
			g.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":      "forbidden",
				"permission": string(perm),
				"resource":   resource,
				"reason":     denialReason(err),
			})
			return nil, err
		}
		return nil, nil
	}
}

// ResourceFromPath is a helper for the common case where the
// resource name is a path parameter:
//
//	policy.Require(e, policy.PermStackUp, policy.ResourceFromPath("name"))
func ResourceFromPath(param string) func(*gin.Context) string {
	return func(g *gin.Context) string {
		return g.Param(param)
	}
}

func denialReason(err error) string {
	if err == nil {
		return ""
	}
	// Strip the "policy: permission denied: " prefix for cleaner UX.
	msg := err.Error()
	prefix := "policy: permission denied: "
	if i := indexOf(msg, prefix); i == 0 {
		return msg[len(prefix):]
	}
	return msg
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// silence unused-import warning when token package is in
// dependency mode but not directly referenced by tests.
var _ = errors.New
