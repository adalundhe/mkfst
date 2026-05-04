package policy

import (
	"context"
	"errors"
	"testing"
)

// TestProviderIntegration_StackOps proves the Checker contract works
// end-to-end against the standard Stack-style permission set, using
// EnforcerChecker as the bridge.
func TestProviderIntegration_StackOps(t *testing.T) {
	e := NewEnforcer()
	_ = e.AddRole(&Role{
		Name: "deployer",
		Permissions: []Permission{
			PermStackUp, PermStackDown, PermStackRead,
		},
		ResourceScopes: map[Permission][]string{
			PermStackUp:   {"dev-*", "staging-*"},
			PermStackDown: {"dev-*", "staging-*"},
			// Read is unscoped (any stack).
		},
	})
	_ = e.AddRole(&Role{
		Name: "operator",
		Permissions: []Permission{
			PermStackUp, PermStackDown, PermStackExec, PermStackRunOneShot,
			PermStackRead, PermAdminAll,
		},
	})

	checker := EnforcerChecker{E: e}

	// "deployer" can Up dev-foo but not prod-foo.
	devCtx := WithSubject(context.Background(), Subject{
		ID:    "alice",
		Roles: []string{"deployer"},
	})
	if err := checker.Check(devCtx, PermStackUp, "dev-foo"); err != nil {
		t.Fatalf("deployer should Up dev-*: %v", err)
	}
	if err := checker.Check(devCtx, PermStackUp, "prod-foo"); err == nil {
		t.Fatal("deployer must NOT Up prod-*")
	}

	// "deployer" can NOT exec or runOneShot at all (not in role).
	if err := checker.Check(devCtx, PermStackExec, "dev-foo"); err == nil {
		t.Fatal("deployer must NOT have stack.exec")
	}

	// "operator" with admin.* can do anything anywhere.
	opCtx := WithSubject(context.Background(), Subject{
		ID:    "bob",
		Roles: []string{"operator"},
	})
	if err := checker.Check(opCtx, PermContainerRemove, "anything"); err != nil {
		t.Fatalf("admin.* should grant container.remove: %v", err)
	}

	// Errors carry ErrDenied for typed branches.
	err := checker.Check(devCtx, PermStackUp, "prod-foo")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("expected ErrDenied, got %v", err)
	}
}

// TestProviderIntegration_TSWorkflow proves the same checker works
// for ts.submit / ts.run with stack-name scoping — the actual
// integration point used by providers/ts/runner.go.
func TestProviderIntegration_TSWorkflow(t *testing.T) {
	e := NewEnforcer()
	_ = e.AddRole(&Role{
		Name: "ts-author",
		Permissions: []Permission{
			PermTSSubmit, PermTSRun,
		},
		ResourceScopes: map[Permission][]string{
			PermTSSubmit: {"smoketest"},
			PermTSRun:    {"smoke*"},
		},
	})
	checker := EnforcerChecker{E: e}

	// Author bound to "smoketest" stack.
	ctx := WithSubject(context.Background(), Subject{
		ID:    "alice",
		Roles: []string{"ts-author"},
	})

	// Submit OK for the right stack.
	if err := checker.Check(ctx, PermTSSubmit, "smoketest"); err != nil {
		t.Fatal(err)
	}
	// Submit denied for a different stack.
	if err := checker.Check(ctx, PermTSSubmit, "production"); err == nil {
		t.Fatal("submit to other stack should deny")
	}
	// Run scoped by workflow name (which the runner passes as resource).
	if err := checker.Check(ctx, PermTSRun, "smoketest-load"); err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(ctx, PermTSRun, "production-load"); err == nil {
		t.Fatal("run on prod-* workflow should deny")
	}
}
