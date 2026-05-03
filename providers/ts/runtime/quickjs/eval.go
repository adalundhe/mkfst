package quickjs

import (
	"context"
	"errors"
	"fmt"
)

// EvalOpts configures Eval.
type EvalOpts struct {
	// Filename is the source name in stack traces. Default "<eval>".
	Filename string

	// Module: when true, source is parsed as an ES module
	// (`import`/`export` allowed). Default false (script mode).
	Module bool

	// Strict: when true, evaluate in strict mode. Default false
	// (script mode follows the source's "use strict"; module mode
	// is implicitly strict).
	Strict bool

	// Global: when true, treat top-level `var` declarations as
	// globals. Default true (script mode); ignored for modules.
	Global bool
}

// Eval feature flags matching qjs.h.
const (
	evalFlagGlobal      = 0
	evalFlagModule      = 1
	evalFlagDirect      = 2
	evalFlagIndirect    = 3
	evalFlagMaskKind    = 0xff
	evalFlagStrict      = 1 << 8
	evalFlagCompileOnly = 1 << 11
)

// Eval compiles and runs JS source. The returned Value is the
// completion value of the script (last expression result for
// scripts; module namespace object for modules). Caller must Free
// the result.
//
// On a JS exception, returns ErrEval wrapping the formatted
// exception message; the QuickJS-side exception is consumed.
func (r *Runtime) Eval(ctx context.Context, source string, opts EvalOpts) (*Value, error) {
	if r.closed.Load() {
		return nil, ErrClosed
	}
	if opts.Filename == "" {
		opts.Filename = "<eval>"
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	createOpt := r.mod.ExportedFunction("QJS_CreateEvalOption")
	evalFn := r.mod.ExportedFunction("QJS_Eval")
	if createOpt == nil || evalFn == nil {
		return nil, errors.New("quickjs: missing eval exports")
	}

	srcPtr, err := r.writeCString(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("write source: %w", err)
	}
	defer r.free(ctx, srcPtr)

	filenamePtr, err := r.writeCString(ctx, opts.Filename)
	if err != nil {
		return nil, fmt.Errorf("write filename: %w", err)
	}
	defer r.free(ctx, filenamePtr)

	flags := evalFlagGlobal
	if opts.Module {
		flags = evalFlagModule
	}
	if opts.Strict {
		flags |= evalFlagStrict
	}

	// QJS_CreateEvalOption(buf, bytecode_buf, bytecode_len, filename, flags) -> opts*
	res, err := createOpt.Call(ctx,
		uint64(srcPtr),
		0, // bytecode_buf
		0, // bytecode_len
		uint64(filenamePtr),
		uint64(flags),
	)
	if err != nil {
		return nil, fmt.Errorf("CreateEvalOption: %w", err)
	}
	optsPtr := uint32(res[0])
	if optsPtr == 0 {
		return nil, errors.New("CreateEvalOption returned null")
	}
	defer r.free(ctx, optsPtr)

	// QJS_Eval(ctx, opts) -> JSValue
	res, err = evalFn.Call(ctx, uint64(r.ctxPtr), uint64(optsPtr))
	if err != nil {
		return nil, fmt.Errorf("QJS_Eval: %w", err)
	}
	v := r.newValue(res[0])
	// QuickJS sets a context-side pending-exception flag whenever
	// an op throws. We check that, not bit-pattern of v.raw, since
	// NaN-boxing varies across QuickJS builds.
	hasExc, herr := r.hasExceptionLocked(ctx)
	if herr != nil {
		v.Free(ctx)
		return nil, herr
	}
	if hasExc {
		v.Free(ctx)
		return nil, r.captureExceptionLocked(ctx)
	}
	return v, nil
}

// hasExceptionLocked queries the context for a pending exception.
// Caller holds r.mu.
func (r *Runtime) hasExceptionLocked(ctx context.Context) (bool, error) {
	fn := r.mod.ExportedFunction("JS_HasException")
	if fn == nil {
		return false, errors.New("JS_HasException export missing")
	}
	res, err := fn.Call(ctx, uint64(r.ctxPtr))
	if err != nil {
		return false, err
	}
	return res[0] != 0, nil
}

// captureExceptionLocked reads the pending JS exception, formats it
// as ErrEval, and returns. Caller must hold r.mu.
func (r *Runtime) captureExceptionLocked(ctx context.Context) error {
	getExc := r.mod.ExportedFunction("JS_GetException")
	if getExc == nil {
		return ErrEval
	}
	res, err := getExc.Call(ctx, uint64(r.ctxPtr))
	if err != nil {
		return fmt.Errorf("%w: GetException: %v", ErrEval, err)
	}
	exc := r.newValue(res[0])
	defer exc.Free(ctx)

	// Try to extract .message and .stack.
	msg, _ := r.getPropertyStrLocked(ctx, exc, "message")
	var msgStr string
	if msg != nil {
		s, _ := msg.String(ctx)
		msgStr = s
		msg.Free(ctx)
	}
	if msgStr == "" {
		// Fall back to toString()ing the exception itself.
		s, _ := exc.String(ctx)
		msgStr = s
	}
	stack, _ := r.getPropertyStrLocked(ctx, exc, "stack")
	if stack != nil {
		s, _ := stack.String(ctx)
		if s != "" {
			msgStr = msgStr + "\n" + s
		}
		stack.Free(ctx)
	}
	return fmt.Errorf("%w: %s", ErrEval, msgStr)
}

// getPropertyStrLocked is a small helper used by exception capture.
// Caller holds r.mu. Returns nil + nil error if the property is
// missing (vs. an actual fetch error).
func (r *Runtime) getPropertyStrLocked(ctx context.Context, v *Value, name string) (*Value, error) {
	getProp := r.mod.ExportedFunction("JS_GetPropertyStr")
	if getProp == nil {
		return nil, errors.New("JS_GetPropertyStr missing")
	}
	cstr, err := r.writeCString(ctx, name)
	if err != nil {
		return nil, err
	}
	defer r.free(ctx, cstr)
	res, err := getProp.Call(ctx, uint64(r.ctxPtr), v.raw, uint64(cstr))
	if err != nil {
		return nil, err
	}
	prop := r.newValue(res[0])
	if prop.IsException() {
		prop.Free(ctx)
		return nil, nil
	}
	return prop, nil
}
