package quickjs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero/api"
)

// === public errors ===

var (
	ErrClosed       = errors.New("quickjs: runtime closed")
	ErrEval         = errors.New("quickjs: eval failed")
	ErrTypeMismatch = errors.New("quickjs: value type mismatch")
)

// === RuntimeOpts ===

// RuntimeOpts configures NewRuntime.
type RuntimeOpts struct {
	// MemoryLimit caps the QuickJS heap (bytes). 0 = no cap.
	// Default 64 MiB when unset.
	MemoryLimit int

	// MaxStackSize caps the JS execution stack (bytes). 0 = QuickJS
	// default (~256 KiB).
	MaxStackSize int

	// MaxExecutionTime caps per-eval wall time (milliseconds).
	// QuickJS checks this cooperatively between bytecodes. 0 = no
	// cap; the surrounding ctx still applies.
	MaxExecutionTime int

	// GCThreshold tunes when QuickJS triggers GC (-1 disables auto
	// GC, 0 = QuickJS default).
	GCThreshold int

	// HostRegistry, if non-nil, replaces the default per-runtime
	// host-function registry. Tests use this to inject mocks.
	HostRegistry *HostRegistry
}

func (o RuntimeOpts) withDefaults() RuntimeOpts {
	if o.MemoryLimit <= 0 {
		o.MemoryLimit = 64 * 1024 * 1024
	}
	return o
}

// === Runtime ===

// Runtime is one isolated QuickJS instance. Each Runtime has its
// own JS heap and global object; values from one Runtime cannot be
// passed to another.
type Runtime struct {
	mod         api.Module
	memory      api.Memory
	mallocFn    api.Function
	freeFn      api.Function
	newQJSFn    api.Function
	getCtxFn    api.Function
	freeQJSFn   api.Function
	updateStack api.Function

	qjsPtr uint32 // *QJSRuntime — also the value passed to QJS_Free
	ctxPtr uint32 // *JSContext — passed to every Context-bound call
	closed atomic.Bool

	// hostRegistry tracks Go functions registered into JS via
	// host.go. Lookup happens on every JS→Go call.
	hostRegistry *HostRegistry

	// mu serializes top-level WASM calls. The QuickJS C runtime is
	// NOT thread-safe; wazero allows concurrent calls but QuickJS
	// will corrupt state.
	//
	// Lock model: mu is taken at the operation entry points
	// (Eval, RegisterFunction, SetGlobal). Value-level methods
	// (Int32, String, JSON, Free, etc.) and constructors
	// (NewInt32, NewString, NewBool) do NOT lock — they're
	// expected to be called either inside a host-function
	// dispatch (which is reentrant from JS, on the same
	// goroutine that already holds mu) or right after an Eval
	// returns (still on the producing goroutine). Sharing values
	// across goroutines is the caller's responsibility.
	mu sync.Mutex
}

// NewRuntime creates a fresh isolated QuickJS runtime. Compile the
// WASM (once per process) and instantiate a per-runtime module
// instance bound to it.
func NewRuntime(ctx context.Context, opts RuntimeOpts) (*Runtime, error) {
	opts = opts.withDefaults()
	mod, err := instantiateInstance(ctx)
	if err != nil {
		return nil, err
	}

	r := &Runtime{
		mod:    mod,
		memory: mod.Memory(),
	}
	r.hostRegistry = opts.HostRegistry
	if r.hostRegistry == nil {
		r.hostRegistry = NewHostRegistry()
	}

	// Required exports. Look them all up up-front so a missing
	// symbol fails at construction, not on first use.
	mustFn := func(name string) (api.Function, error) {
		f := mod.ExportedFunction(name)
		if f == nil {
			return nil, fmt.Errorf("quickjs: WASM missing export %q", name)
		}
		return f, nil
	}
	for _, p := range []struct {
		name string
		dst  *api.Function
	}{
		{"malloc", &r.mallocFn},
		{"free", &r.freeFn},
		{"New_QJS", &r.newQJSFn},
		{"QJS_GetContext", &r.getCtxFn},
		{"QJS_Free", &r.freeQJSFn},
		{"QJS_UpdateStackTop", &r.updateStack},
	} {
		f, err := mustFn(p.name)
		if err != nil {
			_ = mod.Close(ctx)
			return nil, err
		}
		*p.dst = f
	}

	// Build the QJSRuntime + first context.
	res, err := r.newQJSFn.Call(ctx,
		uint64(opts.MemoryLimit),
		uint64(opts.MaxStackSize),
		uint64(opts.MaxExecutionTime),
		uint64(opts.GCThreshold),
	)
	if err != nil {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("quickjs: New_QJS: %w", err)
	}
	r.qjsPtr = uint32(res[0])
	if r.qjsPtr == 0 {
		_ = mod.Close(ctx)
		return nil, errors.New("quickjs: New_QJS returned null")
	}
	res, err = r.getCtxFn.Call(ctx, uint64(r.qjsPtr))
	if err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("quickjs: QJS_GetContext: %w", err)
	}
	r.ctxPtr = uint32(res[0])
	if r.ctxPtr == 0 {
		_ = r.Close(ctx)
		return nil, errors.New("quickjs: QJS_GetContext returned null")
	}
	// Register this Runtime in the per-Module side table so the
	// host-call dispatcher can find it from a wazero callback.
	registerRuntime(mod, r)
	return r, nil
}

// Close releases the QuickJS runtime and the underlying WASM module
// instance. Idempotent.
func (r *Runtime) Close(ctx context.Context) error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	unregisterRuntime(r.mod)
	if r.qjsPtr != 0 && r.freeQJSFn != nil {
		_, _ = r.freeQJSFn.Call(ctx, uint64(r.qjsPtr))
	}
	return r.mod.Close(ctx)
}

// ContextPtr returns the JSContext* pointer — exposed for use by
// other files in this package; not part of the public API.
func (r *Runtime) ContextPtr() uint32 { return r.ctxPtr }

// === per-module → Runtime side table ===
//
// wazero's host-function callback gives us api.Module, but we need
// the *Runtime that owns it (so we can find the HostRegistry, free
// values, etc.). We keep a process-wide map indexed by module
// pointer.

var (
	moduleMap   sync.Map // api.Module → *Runtime
	dummyAPIMod struct{} // satisfy unused import if needed
)

func registerRuntime(mod api.Module, r *Runtime) {
	moduleMap.Store(modKey(mod), r)
}

func unregisterRuntime(mod api.Module) {
	moduleMap.Delete(modKey(mod))
}

func runtimeFromModule(mod api.Module) *Runtime {
	v, ok := moduleMap.Load(modKey(mod))
	if !ok {
		return nil
	}
	return v.(*Runtime)
}

// modKey returns a stable map key for the module. wazero's Module
// is comparable; use it directly.
func modKey(mod api.Module) any { return mod }
