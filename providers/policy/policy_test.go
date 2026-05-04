package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRoleHas(t *testing.T) {
	r := &Role{
		Name:        "stack-admin",
		Permissions: []Permission{PermStackUp, PermStackDown},
	}
	if !r.Has(PermStackUp) {
		t.Fatal("expected stack.up")
	}
	if r.Has(PermStackExec) {
		t.Fatal("did not expect stack.exec")
	}
}

func TestRoleAppliesTo_Unscoped(t *testing.T) {
	r := &Role{Permissions: []Permission{PermStackUp}}
	if !r.AppliesTo(PermStackUp, "any-name") {
		t.Fatal("unscoped permission should apply to any resource")
	}
}

func TestRoleAppliesTo_Scoped(t *testing.T) {
	r := &Role{
		Permissions: []Permission{PermStackUp, PermStackExec},
		ResourceScopes: map[Permission][]string{
			PermStackUp:   {"dev-*", "staging-*"},
			PermStackExec: {"dev-*"},
		},
	}
	if !r.AppliesTo(PermStackUp, "dev-foo") {
		t.Fatal("dev-foo should match dev-*")
	}
	if r.AppliesTo(PermStackUp, "prod-foo") {
		t.Fatal("prod-foo should not match scoped allow")
	}
	if !r.AppliesTo(PermStackExec, "dev-bar") {
		t.Fatal("dev-bar should match exec scope")
	}
	if r.AppliesTo(PermStackExec, "staging-bar") {
		t.Fatal("staging-bar should not match exec scope (only Up scope includes it)")
	}
}

func TestRoleAppliesTo_AdminAll(t *testing.T) {
	r := &Role{Permissions: []Permission{PermAdminAll}}
	if !r.AppliesTo(PermStackUp, "anything") {
		t.Fatal("admin.* should grant everything")
	}
	if !r.AppliesTo(PermContainerRemove, "x") {
		t.Fatal("admin.* should grant container.remove too")
	}
}

func TestEnforcer_AddRole_RejectsUnknownPerm(t *testing.T) {
	e := NewEnforcer()
	if err := e.AddRole(&Role{Name: "bad", Permissions: []Permission{"frob.bar"}}); err == nil {
		t.Fatal("expected rejection of unknown permission")
	}
}

func TestEnforcer_Allow_NoRoles(t *testing.T) {
	e := NewEnforcer()
	ok, reason := e.Allow(Subject{ID: "u1"}, PermStackUp, "x")
	if ok {
		t.Fatal("subject without roles should be denied")
	}
	if !strings.Contains(reason, "no roles") {
		t.Fatalf("expected 'no roles' reason, got %q", reason)
	}
}

func TestEnforcer_Allow_HappyPath(t *testing.T) {
	e := NewEnforcer()
	_ = e.AddRole(&Role{
		Name:        "deployer",
		Permissions: []Permission{PermStackUp},
		ResourceScopes: map[Permission][]string{
			PermStackUp: {"dev-*"},
		},
	})
	subj := Subject{ID: "u1", Roles: []string{"deployer"}}
	if ok, _ := e.Allow(subj, PermStackUp, "dev-foo"); !ok {
		t.Fatal("dev-foo should be allowed")
	}
	if ok, _ := e.Allow(subj, PermStackUp, "prod-foo"); ok {
		t.Fatal("prod-foo should be denied")
	}
	if ok, _ := e.Allow(subj, PermStackDown, "dev-foo"); ok {
		t.Fatal("Down permission not granted to deployer")
	}
}

func TestEnforcer_Require_ReturnsErrDenied(t *testing.T) {
	e := NewEnforcer()
	_ = e.AddRole(&Role{Name: "viewer", Permissions: []Permission{PermStackRead}})
	subj := Subject{ID: "u1", Roles: []string{"viewer"}}
	err := e.Require(subj, PermStackUp, "x")
	if err == nil || !errors.Is(err, ErrDenied) {
		t.Fatalf("expected ErrDenied, got %v", err)
	}
}

func TestEnforcer_Audit_Fires(t *testing.T) {
	e := NewEnforcer()
	_ = e.AddRole(&Role{Name: "v", Permissions: []Permission{PermStackRead}})
	var seen []string
	e.AuditFn = func(s Subject, p Permission, r string, allowed bool, reason string) {
		seen = append(seen, string(p)+":"+boolStr(allowed))
	}
	_, _ = e.Allow(Subject{Roles: []string{"v"}}, PermStackRead, "x")
	_, _ = e.Allow(Subject{Roles: []string{"v"}}, PermStackUp, "x")
	if len(seen) != 2 || seen[0] != "stack.read:true" || seen[1] != "stack.up:false" {
		t.Fatalf("audit log unexpected: %v", seen)
	}
}

func TestEnforcerChecker_PassThrough(t *testing.T) {
	e := NewEnforcer()
	_ = e.AddRole(&Role{Name: "ok", Permissions: []Permission{PermStackUp}})
	c := EnforcerChecker{E: e}

	ctx := WithSubject(context.Background(), Subject{Roles: []string{"ok"}})
	if err := c.Check(ctx, PermStackUp, "x"); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}

	ctxDenied := WithSubject(context.Background(), Subject{Roles: []string{"ok"}})
	if err := c.Check(ctxDenied, PermStackDown, "x"); err == nil {
		t.Fatal("expected denial")
	}
}

func TestAllowAllChecker(t *testing.T) {
	c := AllowAllChecker{}
	if err := c.Check(context.Background(), PermStackUp, "x"); err != nil {
		t.Fatalf("AllowAllChecker should pass everything: %v", err)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
