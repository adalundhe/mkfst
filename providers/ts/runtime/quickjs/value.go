package quickjs

import (
	"context"
	"fmt"
	"sync/atomic"
)

// === Value ===
//
// Value is a Go-side handle to a JS value living in the QuickJS
// heap. The underlying QuickJS JSValue is NaN-boxed as a single
// uint64 across the WASM boundary (this build is configured with
// JS_NAN_BOXING). We keep the raw uint64 plus a back-pointer to the
// owning Runtime; freeing happens via Runtime.QJS_FreeValue.
//
// Values are NOT goroutine-safe. They are produced inside an
// r.mu-locked region (eval, call, prop, etc.) and should be either
// immediately converted to a Go-native type and freed, or used
// briefly within the same logical operation.

type Value struct {
	rt   *Runtime
	raw  uint64
	freed atomic.Bool
}

// IsExceptionRaw is the QuickJS NaN-boxed sentinel that means
// "exception". Helper functions that may throw return this when
// they fail; we then fetch the actual exception via JS_GetException.
const isExceptionRaw uint64 = 0x6 // JS_TAG_EXCEPTION marker tag

// IsException reports whether the raw value is the exception sentinel.
func (v *Value) IsException() bool {
	// NaN-boxed: the tag is encoded in the high bits. The exception
	// tag value is checked via JS_IsException — we mirror that test
	// by inspecting the low 4 bits of the tag.
	return v.raw == 0xFFFFFFFF00000006 || (v.raw>>32)&0xFFFF == 0xFFF6
}

// Free releases the value back to QuickJS. Idempotent.
func (v *Value) Free(ctx context.Context) {
	if v == nil || v.freed.Load() {
		return
	}
	if !v.freed.CompareAndSwap(false, true) {
		return
	}
	if fn := v.rt.mod.ExportedFunction("QJS_FreeValue"); fn != nil {
		_, _ = fn.Call(ctx, uint64(v.rt.ctxPtr), v.raw)
	}
}

// Raw returns the underlying NaN-boxed uint64. Internal use only.
func (v *Value) Raw() uint64 { return v.raw }

// === conversion ===

// String renders the value as a Go string by calling QJS_ToCString
// on the QuickJS value. Strings, numbers, booleans, objects (via
// toString) all stringify; functions render as "function …".
func (v *Value) String(ctx context.Context) (string, error) {
	r := v.rt
	fn := r.mod.ExportedFunction("QJS_ToCString")
	if fn == nil {
		return "", fmt.Errorf("QJS_ToCString export missing")
	}
	res, err := fn.Call(ctx, uint64(r.ctxPtr), v.raw)
	if err != nil {
		return "", err
	}
	pkg := uint32(res[0])
	if pkg == 0 {
		return "", nil
	}
	b, err := r.unpackPtr(ctx, pkg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// JSON returns the QuickJS-side JSON.stringify of the value. Useful
// for round-tripping objects from JS to Go without writing a custom
// converter for every shape.
func (v *Value) JSON(ctx context.Context) ([]byte, error) {
	r := v.rt
	fn := r.mod.ExportedFunction("QJS_JSONStringify")
	if fn == nil {
		return nil, fmt.Errorf("QJS_JSONStringify export missing")
	}
	res, err := fn.Call(ctx, uint64(r.ctxPtr), v.raw)
	if err != nil {
		return nil, err
	}
	pkg := uint32(res[0])
	if pkg == 0 {
		return nil, nil
	}
	return r.unpackPtr(ctx, pkg)
}

// Int32 reads the value as int32 (truncating + coercing per ECMA).
func (v *Value) Int32(ctx context.Context) (int32, error) {
	r := v.rt
	fn := r.mod.ExportedFunction("QJS_ToInt32")
	if fn == nil {
		return 0, fmt.Errorf("QJS_ToInt32 export missing")
	}
	res, err := fn.Call(ctx, uint64(r.ctxPtr), v.raw)
	if err != nil {
		return 0, err
	}
	return int32(res[0]), nil
}

// Bool reads the value as bool (per ECMA truthiness).
func (v *Value) Bool(ctx context.Context) (bool, error) {
	r := v.rt
	// QuickJS C: JS_ToBool returns -1 on error, else 0/1.
	fn := r.mod.ExportedFunction("JS_ToBool")
	if fn == nil {
		// Helper layer doesn't always export JS_ToBool — fall back
		// to JSON which serializes booleans.
		fn = r.mod.ExportedFunction("QJS_JSONStringify")
		if fn == nil {
			return false, fmt.Errorf("no boolean conversion export")
		}
		res, err := fn.Call(ctx, uint64(r.ctxPtr), v.raw)
		if err != nil {
			return false, err
		}
		b, _ := r.unpackPtr(ctx, uint32(res[0]))
		return string(b) == "true", nil
	}
	res, err := fn.Call(ctx, uint64(r.ctxPtr), v.raw)
	if err != nil {
		return false, err
	}
	v32 := int32(res[0])
	return v32 == 1, nil
}

// === construction (called from other files in this package) ===

func (r *Runtime) newValue(raw uint64) *Value {
	return &Value{rt: r, raw: raw}
}

// callExport0to1 is a tiny helper for "call WASM export with 1 arg
// (the context ptr) and capture single i64 return". Useful for
// create-undefined / create-null shapes.
func (r *Runtime) callExport0to1(ctx context.Context, name string) (*Value, error) {
	fn := r.mod.ExportedFunction(name)
	if fn == nil {
		return nil, fmt.Errorf("%s export missing", name)
	}
	res, err := fn.Call(ctx)
	if err != nil {
		return nil, err
	}
	return r.newValue(res[0]), nil
}

// NewUndefined returns the JS undefined value.
func (r *Runtime) NewUndefined(ctx context.Context) (*Value, error) {
	return r.callExport0to1(ctx, "JS_NewUndefined")
}

// NewNull returns the JS null value.
func (r *Runtime) NewNull(ctx context.Context) (*Value, error) {
	return r.callExport0to1(ctx, "JS_NewNull")
}

// NewBool returns a JS boolean. Safe to call from inside a host
// function dispatch (no lock — caller must serialize across
// goroutines themselves).
func (r *Runtime) NewBool(ctx context.Context, b bool) (*Value, error) {
	fn := r.mod.ExportedFunction("QJS_NewBool")
	if fn == nil {
		return nil, fmt.Errorf("QJS_NewBool export missing")
	}
	v := uint64(0)
	if b {
		v = 1
	}
	res, err := fn.Call(ctx, uint64(r.ctxPtr), v)
	if err != nil {
		return nil, err
	}
	return r.newValue(res[0]), nil
}

// NewInt32 returns a JS number from int32.
func (r *Runtime) NewInt32(ctx context.Context, n int32) (*Value, error) {
	fn := r.mod.ExportedFunction("QJS_NewInt32")
	if fn == nil {
		return nil, fmt.Errorf("QJS_NewInt32 export missing")
	}
	res, err := fn.Call(ctx, uint64(r.ctxPtr), uint64(uint32(n)))
	if err != nil {
		return nil, err
	}
	return r.newValue(res[0]), nil
}

// NewString returns a JS string.
func (r *Runtime) NewString(ctx context.Context, s string) (*Value, error) {
	fn := r.mod.ExportedFunction("QJS_NewString")
	if fn == nil {
		return nil, fmt.Errorf("QJS_NewString export missing")
	}
	cstr, err := r.writeCString(ctx, s)
	if err != nil {
		return nil, err
	}
	defer r.free(ctx, cstr)
	res, err := fn.Call(ctx, uint64(r.ctxPtr), uint64(cstr))
	if err != nil {
		return nil, err
	}
	return r.newValue(res[0]), nil
}
