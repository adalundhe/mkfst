package quickjs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero/api"
)

// === HostFunc + registry ===
//
// HostFunc is a Go function callable from JS. Signature mirrors the
// shape of a JS function call: receives a slice of *Value arguments,
// returns a *Value result OR an error (which becomes a JS throw).
//
// Resource ownership: arguments are borrowed — the runtime frees
// them after the call. The returned *Value is consumed by the
// runtime (passed to JS) and freed by JS-side GC.

type HostFunc func(ctx context.Context, this *Value, args []*Value) (*Value, error)

// HostRegistry maps integer fnIDs to HostFuncs. Each Runtime owns
// one. The dispatcher uses (modKey + fnID) lookup so multiple
// runtimes can independently register functions with overlapping IDs
// without collision.
type HostRegistry struct {
	mu      sync.RWMutex
	fns     map[uint64]HostFunc
	nextID  atomic.Uint64
}

// NewHostRegistry returns an empty registry.
func NewHostRegistry() *HostRegistry {
	return &HostRegistry{fns: make(map[uint64]HostFunc)}
}

// Register stores fn under a freshly-minted ID.
func (h *HostRegistry) Register(fn HostFunc) uint64 {
	id := h.nextID.Add(1)
	h.mu.Lock()
	h.fns[id] = fn
	h.mu.Unlock()
	return id
}

func (h *HostRegistry) lookup(id uint64) HostFunc {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.fns[id]
}

// === Runtime API ===

// RegisterFunction creates a JS-callable wrapper around fn and
// returns it as a *Value. Caller decides where to install it
// (typically as a global property: SetGlobal("name", v)).
func (r *Runtime) RegisterFunction(ctx context.Context, fn HostFunc) (*Value, error) {
	if r.closed.Load() {
		return nil, ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	createFn := r.mod.ExportedFunction("QJS_CreateFunctionProxy")
	if createFn == nil {
		return nil, errors.New("QJS_CreateFunctionProxy export missing")
	}
	fnID := r.hostRegistry.Register(fn)
	// ctxID is intentionally fixed to 0; we use the per-Runtime
	// registry keyed by module pointer rather than a separate
	// per-Context ID. The C wrapper accepts any value here; we
	// retrieve r via runtimeFromModule in the dispatcher.
	res, err := createFn.Call(ctx,
		uint64(r.ctxPtr),
		fnID,
		0, // ctxID
		0, // is_async (sync for v1; async added later)
	)
	if err != nil {
		return nil, fmt.Errorf("CreateFunctionProxy: %w", err)
	}
	return r.newValue(res[0]), nil
}

// SetGlobal installs v under name on the JS global object.
// Convenience wrapper around JS_SetPropertyStr(globalThis, name, v).
func (r *Runtime) SetGlobal(ctx context.Context, name string, v *Value) error {
	if r.closed.Load() {
		return ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	getGlobal := r.mod.ExportedFunction("JS_GetGlobalObject")
	setProp := r.mod.ExportedFunction("JS_SetPropertyStr")
	if getGlobal == nil || setProp == nil {
		return errors.New("missing global property exports")
	}
	gres, err := getGlobal.Call(ctx, uint64(r.ctxPtr))
	if err != nil {
		return err
	}
	gobj := r.newValue(gres[0])
	defer gobj.Free(ctx)
	cstr, err := r.writeCString(ctx, name)
	if err != nil {
		return err
	}
	defer r.free(ctx, cstr)
	// JS_SetPropertyStr consumes the value reference; we pass the
	// raw and DON'T Free v after this call.
	_, err = setProp.Call(ctx, uint64(r.ctxPtr), gobj.raw, uint64(cstr), v.raw)
	if err != nil {
		return err
	}
	v.freed.Store(true) // ownership transferred
	return nil
}

// === dispatchHostCall ===
//
// Called from wazero when JS invokes a registered Go function. The
// JS-side closure (built by QJS_CreateFunctionProxy) prepends three
// ID args: [fnID, ctxID, isAsync, promise(undefined for sync), ...userArgs].
// We extract fnID, look up the Go fn, call it, and return the
// result as a NaN-boxed JSValue uint64.

func dispatchHostCall(ctx context.Context, mod api.Module, jsCtxPtr int32, thisVal uint64, argc int32, argvPtr int32) (uint64, error) {
	rt := runtimeFromModule(mod)
	if rt == nil {
		return 0, errors.New("dispatchHostCall: no runtime registered for module")
	}
	if argc < 4 {
		// fnID + ctxID + isAsync + promise = 4 prepended; userArgs follow.
		return 0, errors.New("dispatchHostCall: argc < 4 (proxy header missing)")
	}
	// Read the argv array from WASM memory: argc * 8 bytes (each
	// JSValue is a uint64 LE).
	argvBytes, ok := rt.memory.Read(uint32(argvPtr), uint32(argc)*8)
	if !ok {
		return 0, errors.New("dispatchHostCall: argv oob")
	}
	read64 := func(i int32) uint64 {
		off := i * 8
		return uint64(argvBytes[off]) |
			uint64(argvBytes[off+1])<<8 |
			uint64(argvBytes[off+2])<<16 |
			uint64(argvBytes[off+3])<<24 |
			uint64(argvBytes[off+4])<<32 |
			uint64(argvBytes[off+5])<<40 |
			uint64(argvBytes[off+6])<<48 |
			uint64(argvBytes[off+7])<<56
	}
	fnID := read64(0)
	// ctxID := read64(1)  // unused for now
	// isAsync := read64(2)
	// promise := read64(3) // undefined for sync
	hostFn := rt.hostRegistry.lookup(fnID)
	if hostFn == nil {
		return 0, fmt.Errorf("dispatchHostCall: no fn registered for id=%d", fnID)
	}
	userArgs := make([]*Value, argc-4)
	for i := int32(4); i < argc; i++ {
		userArgs[i-4] = rt.newValue(read64(i))
	}
	thisV := rt.newValue(thisVal)
	defer thisV.Free(ctx)
	defer func() {
		for _, a := range userArgs {
			a.Free(ctx)
		}
	}()
	result, err := hostFn(ctx, thisV, userArgs)
	if err != nil {
		// Throw a JS Error and return the exception sentinel.
		return throwJSError(ctx, rt, err.Error())
	}
	if result == nil {
		// Treat as undefined.
		und, uerr := rt.callExport0to1(ctx, "JS_NewUndefined")
		if uerr != nil {
			return 0, uerr
		}
		return und.raw, nil
	}
	// Caller transfers ownership of result to JS — don't free.
	result.freed.Store(true)
	return result.raw, nil
}

func throwJSError(ctx context.Context, rt *Runtime, msg string) (uint64, error) {
	throwFn := rt.mod.ExportedFunction("QJS_ThrowInternalError")
	if throwFn == nil {
		// Some helper builds expose differently-named throws; try
		// type error as a fallback.
		throwFn = rt.mod.ExportedFunction("QJS_ThrowTypeError")
	}
	if throwFn == nil {
		return 0, errors.New("no throw export available")
	}
	cstr, err := rt.writeCString(ctx, msg)
	if err != nil {
		return 0, err
	}
	defer rt.free(ctx, cstr)
	res, err := throwFn.Call(ctx, uint64(rt.ctxPtr), uint64(cstr))
	if err != nil {
		return 0, err
	}
	return res[0], nil
}
