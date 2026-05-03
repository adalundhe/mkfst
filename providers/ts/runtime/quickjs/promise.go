package quickjs

import (
	"context"
	"errors"
	"fmt"
)

// === Promise resolution ===
//
// QuickJS executes synchronous JS to completion, then queues
// promise reactions in the job queue. To get the eventual value of
// a promise, we install resolve/reject handlers that store the
// outcome in a JS-side global, then drain the job queue until the
// global is populated.

// AwaitOpts configures Await. Reserved for future tuning;
// js_std_await drains the queue internally so no caller knobs are
// needed for v1.
type AwaitOpts struct{}

// Await drives the QuickJS job queue until the value's underlying
// promise resolves (or rejects). Uses QuickJS-libc's js_std_await
// which internally pumps the microtask queue until the promise
// settles, then returns the resolved value (or throws on
// rejection).
//
// If v is not a Promise, js_std_await returns it unchanged.
// Returns the resolved value (caller frees) or an error wrapping
// the rejection / context cancellation.
func (r *Runtime) Await(ctx context.Context, v *Value, opts AwaitOpts) (*Value, error) {
	if r.closed.Load() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	fn := r.mod.ExportedFunction("js_std_await")
	if fn == nil {
		return nil, errors.New("Await: js_std_await export missing")
	}
	res, err := fn.Call(ctx, uint64(r.ctxPtr), v.raw)
	if err != nil {
		return nil, fmt.Errorf("Await: js_std_await: %w", err)
	}
	resolved := r.newValue(res[0])
	// Mark the input value as freed — js_std_await consumes a
	// reference (similar to JS_SetPropertyStr semantics).
	v.freed.Store(true)
	// Check for a pending exception left by a rejected promise.
	hasExc, err := r.hasExceptionLocked(ctx)
	if err != nil {
		resolved.Free(ctx)
		return nil, err
	}
	if hasExc {
		resolved.Free(ctx)
		return nil, r.captureExceptionLocked(ctx)
	}
	return resolved, nil
}

