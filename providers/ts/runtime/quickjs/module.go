package quickjs

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed quickjs.wasm
var quickjsWASM []byte

// compiledModule holds a process-wide compiled module that fresh
// runtimes instantiate against. wazero's compilation step is the
// expensive part (~50ms); instantiation off a compiled module is
// ~5ms. We compile once and reuse.
type compiledModule struct {
	mu       sync.Mutex
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	cfg      wazero.RuntimeConfig
}

var globalModule = &compiledModule{}

// initOnce ensures wazero compilation happens at most once per
// process even under concurrent NewRuntime calls.
func (m *compiledModule) ensureCompiled(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.compiled != nil {
		return nil
	}
	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		// QuickJS uses 64-bit operations heavily; enable when
		// available. Compiler choice (interpreter vs JIT) is left
		// to wazero defaults.
		WithDebugInfoEnabled(false)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return fmt.Errorf("quickjs: WASI instantiate: %w", err)
	}
	// Register the single host import the WASM expects:
	//   env.jsFunctionProxy(ctx i32, this i64, argc i32, argv i32) -> i64
	// All Go-callable JS functions go through this dispatcher, with
	// per-call discrimination via fnID embedded in the JS closure
	// (see host.go).
	_, err := rt.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(hostFunctionProxy),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI64}).
		Export("jsFunctionProxy").
		Instantiate(ctx)
	if err != nil {
		_ = rt.Close(ctx)
		return fmt.Errorf("quickjs: host module: %w", err)
	}
	compiled, err := rt.CompileModule(ctx, quickjsWASM)
	if err != nil {
		_ = rt.Close(ctx)
		return fmt.Errorf("quickjs: compile WASM: %w", err)
	}
	m.rt = rt
	m.compiled = compiled
	m.cfg = cfg
	return nil
}

// closeModule tears down the wazero runtime + compiled module. Test
// only — production keeps the compiled module alive for the process
// lifetime.
func (m *compiledModule) closeModule(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rt == nil {
		return nil
	}
	err := m.rt.Close(ctx)
	m.rt = nil
	m.compiled = nil
	return err
}

// hostFunctionProxy is the Go-side dispatcher for JS-→Go calls.
// Implementation lives in host.go to keep the wazero plumbing here
// minimal.
func hostFunctionProxy(ctx context.Context, mod api.Module, stack []uint64) {
	// Unpack args: ctx i32, this i64, argc i32, argv i32 → i64 ret.
	jsCtxPtr := api.DecodeI32(stack[0])
	thisVal := stack[1]
	argc := api.DecodeI32(stack[2])
	argvPtr := api.DecodeI32(stack[3])

	ret, err := dispatchHostCall(ctx, mod, jsCtxPtr, thisVal, argc, argvPtr)
	if err != nil {
		// Map Go errors to JS-thrown exceptions. The dispatcher is
		// expected to call Throw* on the JS context and return the
		// "exception" sentinel value; if it bubbles an error to us
		// here, we encode an exception by setting bit 32 of the
		// return word (QuickJS exception sentinel).
		stack[0] = uint64(0xFFFFFFFF00000006) // JS_TAG_EXCEPTION marker
		return
	}
	stack[0] = ret
}

// === ensureModuleReady is the single entry point external code uses
// to make sure the WASM is compiled and the host module registered.
// Called from Runtime constructors.
func ensureModuleReady(ctx context.Context) error {
	if err := globalModule.ensureCompiled(ctx); err != nil {
		return err
	}
	if globalModule.compiled == nil {
		return errors.New("quickjs: module not compiled")
	}
	return nil
}

// instantiateInstance creates a fresh module instance bound to the
// given wazero runtime. Used internally by NewRuntime.
func instantiateInstance(ctx context.Context) (api.Module, error) {
	if err := ensureModuleReady(ctx); err != nil {
		return nil, err
	}
	mod, err := globalModule.rt.InstantiateModule(ctx,
		globalModule.compiled,
		wazero.NewModuleConfig().
			WithName("").                  // anonymous so we can instantiate many
			WithStartFunctions("_initialize"), // skip _start; this WASM is a reactor
	)
	if err != nil {
		return nil, fmt.Errorf("quickjs: instantiate: %w", err)
	}
	return mod, nil
}
