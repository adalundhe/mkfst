package network

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"

	"mkfst/providers/policy"
)

// === policy enforcement at the Stack API surface ===
//
// These tests prove that Stack.Up / Stack.Down / Stack.RunOneShot /
// Stack.Exec invoke the configured policy.Checker BEFORE they do
// any docker work. We don't need a real docker daemon — when the
// checker denies, the methods short-circuit before reaching the SDK.

// fakeChecker is a hand-rolled stub that records every (perm,
// resource) check and returns a configurable verdict. Equivalent
// to mockery's Checker mock but inlined here for readability.
type fakeChecker struct {
	calls []struct {
		Perm     policy.Permission
		Resource string
	}
	deny bool
}

func (c *fakeChecker) Check(_ context.Context, p policy.Permission, r string) error {
	c.calls = append(c.calls, struct {
		Perm     policy.Permission
		Resource string
	}{p, r})
	if c.deny {
		return errors.New("policy: denied")
	}
	return nil
}

func newStackWithChecker(name string, checker *fakeChecker) *Stack {
	eng := &Engine{opts: EngineOpts{Policy: checker}}
	return newStack(eng, "stack-id", name)
}

func TestStack_Up_FiresPolicyCheck(t *testing.T) {
	checker := &fakeChecker{deny: true}
	s := newStackWithChecker("my-stack", checker)
	err := s.Up(context.Background())
	if err == nil {
		t.Fatal("expected policy denial")
	}
	if len(checker.calls) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checker.calls))
	}
	if checker.calls[0].Perm != policy.PermStackUp {
		t.Fatalf("wrong perm: %v", checker.calls[0].Perm)
	}
	if checker.calls[0].Resource != "my-stack" {
		t.Fatalf("wrong resource: %v", checker.calls[0].Resource)
	}
}

func TestStack_Down_FiresPolicyCheck(t *testing.T) {
	checker := &fakeChecker{deny: true}
	s := newStackWithChecker("my-stack", checker)
	s.state.Store(int32(StackUp))
	err := s.Down(context.Background())
	if err == nil {
		t.Fatal("expected policy denial")
	}
	if checker.calls[0].Perm != policy.PermStackDown {
		t.Fatalf("wrong perm: %v", checker.calls[0].Perm)
	}
}

func TestStack_RunOneShot_FiresPolicyCheck(t *testing.T) {
	checker := &fakeChecker{deny: true}
	s := newStackWithChecker("my-stack", checker)
	s.state.Store(int32(StackUp))
	_, err := s.RunOneShot(context.Background(), OneShotOpts{Image: "alpine"})
	if err == nil {
		t.Fatal("expected policy denial")
	}
	if checker.calls[0].Perm != policy.PermStackRunOneShot {
		t.Fatalf("wrong perm: %v", checker.calls[0].Perm)
	}
}

func TestStack_Exec_FiresPolicyCheck(t *testing.T) {
	checker := &fakeChecker{deny: true}
	s := newStackWithChecker("my-stack", checker)
	s.state.Store(int32(StackUp))
	_, err := s.Exec(context.Background(), "svc", 0, ExecOpts{Cmd: []string{"echo"}})
	if err == nil {
		t.Fatal("expected policy denial")
	}
	if checker.calls[0].Perm != policy.PermStackExec {
		t.Fatalf("wrong perm: %v", checker.calls[0].Perm)
	}
}

// silence unused-import for mock — kept available for future tests
// that want testify expectations on the Checker.
var _ = mock.Anything
