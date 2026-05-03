// Package runtime provides the JS execution engine + host bridge
// dispatcher used by the TS task subsystem. The engine wraps the
// QuickJS WASM binding behind an interface so the implementation
// can be swapped without touching call sites.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"mkfst/providers/ts/runtime/quickjs"
)

// === Engine ===

// Engine creates Runtime instances. One per process.
type Engine interface {
	NewRuntime(ctx context.Context, opts RuntimeOpts) (Runtime, error)
}

// RuntimeOpts configures a single runtime instance.
type RuntimeOpts struct {
	MemoryBytes      int           // 0 = default 64 MiB
	MaxStackBytes    int           // 0 = QuickJS default
	MaxExecutionMs   int           // 0 = no time cap
	HostBridge       *Bridge       // host-call dispatcher (required for non-trivial use)
}

// Runtime is one isolated JS execution context. Goroutine-safe at
// the operation level (Eval / Invoke / etc.). Values produced by a
// runtime are bound to the goroutine that produced them until
// freed; sharing values across goroutines is the caller's problem.
type Runtime interface {
	Eval(ctx context.Context, source string, opts EvalOpts) (Value, error)
	RegisterHostFunction(ctx context.Context, name string, fn HostFunc) error
	GetGlobal(ctx context.Context, name string) (Value, error)
	NewObject(ctx context.Context) (Value, error)
	NewString(ctx context.Context, s string) (Value, error)
	NewInt32(ctx context.Context, n int32) (Value, error)
	NewBool(ctx context.Context, b bool) (Value, error)
	NewUndefined(ctx context.Context) (Value, error)
	// Await drives the JS event loop until v's underlying promise
	// settles. If v is not a Promise it is wrapped via
	// Promise.resolve. Returns the resolved value or an error
	// wrapping the rejection / timeout.
	Await(ctx context.Context, v Value) (Value, error)
	Close(ctx context.Context) error
}

// EvalOpts mirrors quickjs.EvalOpts.
type EvalOpts struct {
	Filename string
	Module   bool
	Strict   bool
}

// Value is a Go-side handle to a JS value. Callers must Free when
// done.
type Value interface {
	String(ctx context.Context) (string, error)
	JSON(ctx context.Context) ([]byte, error)
	Int32(ctx context.Context) (int32, error)
	Bool(ctx context.Context) (bool, error)
	GetProperty(ctx context.Context, name string) (Value, error)
	Call(ctx context.Context, args ...Value) (Value, error) // for function values
	Free(ctx context.Context)
	Raw() uint64
}

// HostFunc is a Go function callable from JS.
type HostFunc func(ctx context.Context, args []Value) (Value, error)

// === QuickJS-backed engine ===

type qjsEngine struct{}

// NewEngine returns the default engine (QuickJS via wazero).
func NewEngine() Engine { return &qjsEngine{} }

func (e *qjsEngine) NewRuntime(ctx context.Context, opts RuntimeOpts) (Runtime, error) {
	rt, err := quickjs.NewRuntime(ctx, quickjs.RuntimeOpts{
		MemoryLimit:      opts.MemoryBytes,
		MaxStackSize:     opts.MaxStackBytes,
		MaxExecutionTime: opts.MaxExecutionMs,
	})
	if err != nil {
		return nil, err
	}
	return &qjsRuntime{rt: rt, bridge: opts.HostBridge}, nil
}

// === qjsRuntime ===

type qjsRuntime struct {
	rt     *quickjs.Runtime
	bridge *Bridge

	mu        sync.Mutex
	hostFuncs map[string]HostFunc
}

func (r *qjsRuntime) Close(ctx context.Context) error {
	return r.rt.Close(ctx)
}

func (r *qjsRuntime) Eval(ctx context.Context, source string, opts EvalOpts) (Value, error) {
	v, err := r.rt.Eval(ctx, source, quickjs.EvalOpts{
		Filename: opts.Filename, Module: opts.Module, Strict: opts.Strict,
	})
	if err != nil {
		return nil, err
	}
	return &qjsValue{v: v, rt: r}, nil
}

func (r *qjsRuntime) RegisterHostFunction(ctx context.Context, name string, fn HostFunc) error {
	if name == "" {
		return errors.New("RegisterHostFunction: empty name")
	}
	if fn == nil {
		return errors.New("RegisterHostFunction: nil fn")
	}
	r.mu.Lock()
	if r.hostFuncs == nil {
		r.hostFuncs = make(map[string]HostFunc)
	}
	r.hostFuncs[name] = fn
	r.mu.Unlock()

	// Wrap into a quickjs.HostFunc, adapting Value types.
	wrapped := func(ctx context.Context, this *quickjs.Value, args []*quickjs.Value) (*quickjs.Value, error) {
		// Adapt args.
		adapted := make([]Value, len(args))
		for i, a := range args {
			adapted[i] = &qjsValue{v: a, rt: r, borrowed: true}
		}
		result, err := fn(ctx, adapted)
		if err != nil {
			return nil, err
		}
		if result == nil {
			return r.rt.NewUndefined(ctx)
		}
		// Unwrap our adapter back to the underlying *quickjs.Value.
		qv, ok := result.(*qjsValue)
		if !ok {
			return nil, fmt.Errorf("RegisterHostFunction: returned Value is not a qjsValue")
		}
		return qv.v, nil
	}
	v, err := r.rt.RegisterFunction(ctx, wrapped)
	if err != nil {
		return err
	}
	return r.rt.SetGlobal(ctx, name, v)
}

func (r *qjsRuntime) GetGlobal(ctx context.Context, name string) (Value, error) {
	// Run a tiny script to fetch globalThis[name] — simpler than
	// adding new exports.
	v, err := r.rt.Eval(ctx, "globalThis["+jsString(name)+"]", quickjs.EvalOpts{
		Filename: "<get-global>",
	})
	if err != nil {
		return nil, err
	}
	return &qjsValue{v: v, rt: r}, nil
}

func (r *qjsRuntime) NewObject(ctx context.Context) (Value, error) {
	// Use Eval as a small helper to construct an empty object;
	// avoids wiring more exports for the minimal v1.
	v, err := r.rt.Eval(ctx, "({})", quickjs.EvalOpts{Filename: "<new-object>"})
	if err != nil {
		return nil, err
	}
	return &qjsValue{v: v, rt: r}, nil
}

func (r *qjsRuntime) NewString(ctx context.Context, s string) (Value, error) {
	v, err := r.rt.NewString(ctx, s)
	if err != nil {
		return nil, err
	}
	return &qjsValue{v: v, rt: r}, nil
}

func (r *qjsRuntime) NewInt32(ctx context.Context, n int32) (Value, error) {
	v, err := r.rt.NewInt32(ctx, n)
	if err != nil {
		return nil, err
	}
	return &qjsValue{v: v, rt: r}, nil
}

func (r *qjsRuntime) NewBool(ctx context.Context, b bool) (Value, error) {
	v, err := r.rt.NewBool(ctx, b)
	if err != nil {
		return nil, err
	}
	return &qjsValue{v: v, rt: r}, nil
}

func (r *qjsRuntime) NewUndefined(ctx context.Context) (Value, error) {
	v, err := r.rt.NewUndefined(ctx)
	if err != nil {
		return nil, err
	}
	return &qjsValue{v: v, rt: r}, nil
}

func (r *qjsRuntime) Await(ctx context.Context, v Value) (Value, error) {
	qv, ok := v.(*qjsValue)
	if !ok {
		return nil, fmt.Errorf("Await: value is not a qjsValue")
	}
	resolved, err := r.rt.Await(ctx, qv.v, quickjs.AwaitOpts{})
	if err != nil {
		return nil, err
	}
	return &qjsValue{v: resolved, rt: r}, nil
}

// === qjsValue ===

type qjsValue struct {
	v        *quickjs.Value
	rt       *qjsRuntime
	borrowed bool // true when we don't own the lifetime (host-fn args)
}

func (v *qjsValue) String(ctx context.Context) (string, error) {
	return v.v.String(ctx)
}

func (v *qjsValue) JSON(ctx context.Context) ([]byte, error) {
	return v.v.JSON(ctx)
}

func (v *qjsValue) Int32(ctx context.Context) (int32, error) {
	return v.v.Int32(ctx)
}

func (v *qjsValue) Bool(ctx context.Context) (bool, error) {
	return v.v.Bool(ctx)
}

func (v *qjsValue) Raw() uint64 { return v.v.Raw() }

func (v *qjsValue) Free(ctx context.Context) {
	if v.borrowed {
		return // host-fn args are freed by the dispatcher
	}
	v.v.Free(ctx)
}

func (v *qjsValue) GetProperty(ctx context.Context, name string) (Value, error) {
	// Simple property get via eval — not ideal for performance but
	// keeps the binding minimal. Real implementation would call
	// JS_GetPropertyStr via the QJS exports.
	src := "globalThis.__mkfst_tmp_obj = {}; globalThis.__mkfst_tmp_obj.x = (" +
		jsString("placeholder") + "); globalThis.__mkfst_tmp_obj.x"
	_ = src
	return nil, errors.New("GetProperty: not yet implemented (use JSON or eval-string for v1)")
}

func (v *qjsValue) Call(ctx context.Context, args ...Value) (Value, error) {
	return nil, errors.New("Call: not yet implemented (use eval-string with closure for v1)")
}

// jsString JSON-encodes a string for safe interpolation into JS source.
func jsString(s string) string {
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if r < 0x20 {
				out = append(out, '\\', 'u', '0', '0', hexDigit(byte(r>>4)), hexDigit(byte(r&0xF)))
			} else {
				out = append(out, []byte(string(r))...)
			}
		}
	}
	out = append(out, '"')
	return string(out)
}

func hexDigit(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}
