package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// === Bridge ===
//
// Bridge is the host-call dispatcher: a registry of named host
// functions that JS-side blessed modules invoke via __mkfst_dispatch.
// Each call carries the name of the calling module so capability
// enforcement can scope.

// Bridge is the central host dispatcher. Construct via NewBridge.
type Bridge struct {
	mu       sync.RWMutex
	handlers map[string]BridgeHandler // op name → handler
	policy   PolicyChecker            // capability gate; nil = allow all
}

// BridgeHandler is a typed host call. Args are decoded from JS's
// Uint8Array (msgpack/json depending on caller convention); result
// is encoded the same way.
type BridgeHandler func(ctx BridgeCtx, args []byte) ([]byte, error)

// BridgeCtx carries per-call invocation context.
type BridgeCtx struct {
	Ctx        context.Context
	ModuleName string // calling blessed-module name, for capability check
	WorkflowID string // optional: workflow instance the call is for
	NodeName   string // optional: workflow node the call is for
	// BoundStack is the stack this workflow is scoped to. Calls
	// referencing any other stack must be denied. Empty = no host
	// access (pure compute).
	BoundStack string
}

// PolicyChecker decides whether a given module is allowed to call
// the given op with the given args. Policy implementations live in
// providers/ts/config; this interface keeps the runtime free of
// config concerns.
type PolicyChecker interface {
	Check(moduleName, op string, args []byte) error
}

// AllowAll is a no-op PolicyChecker for tests / single-tenant.
type AllowAll struct{}

func (AllowAll) Check(string, string, []byte) error { return nil }

// NewBridge returns a fresh Bridge. policy may be nil → allow all.
func NewBridge(policy PolicyChecker) *Bridge {
	if policy == nil {
		policy = AllowAll{}
	}
	return &Bridge{
		handlers: map[string]BridgeHandler{},
		policy:   policy,
	}
}

// Register binds a handler to an op name. Returns an error on dup.
func (b *Bridge) Register(op string, h BridgeHandler) error {
	if op == "" {
		return errors.New("bridge.Register: empty op")
	}
	if h == nil {
		return errors.New("bridge.Register: nil handler")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, dup := b.handlers[op]; dup {
		return fmt.Errorf("bridge.Register: %q already registered", op)
	}
	b.handlers[op] = h
	return nil
}

// Dispatch looks up op, runs the policy check, invokes the handler,
// returns the encoded result.
func (b *Bridge) Dispatch(ctx BridgeCtx, op string, args []byte) ([]byte, error) {
	b.mu.RLock()
	h := b.handlers[op]
	policy := b.policy
	b.mu.RUnlock()
	if h == nil {
		return nil, fmt.Errorf("bridge: no handler for %q", op)
	}
	if err := policy.Check(ctx.ModuleName, op, args); err != nil {
		return nil, fmt.Errorf("bridge: policy denied %s.%s: %w", ctx.ModuleName, op, err)
	}
	return h(ctx, args)
}

// === wiring helpers ===

// JSON is a tiny generic helper to wire a typed handler.
//
//	bridge.Register("log", runtime.JSON(func(ctx BridgeCtx, in LogArgs) (LogResult, error) {
//	    ...
//	}))
func JSON[In any, Out any](fn func(BridgeCtx, In) (Out, error)) BridgeHandler {
	return func(ctx BridgeCtx, raw []byte) ([]byte, error) {
		var in In
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("decode args: %w", err)
			}
		}
		out, err := fn(ctx, in)
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	}
}
