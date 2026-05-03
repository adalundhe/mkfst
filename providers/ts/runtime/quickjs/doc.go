// Package quickjs is mkfst's first-party QuickJS binding running on
// wazero. It vendors a prebuilt QuickJS-NG WebAssembly module (under
// MIT, see QUICKJS_WASM_LICENSE) and exposes only the QuickJS C API
// surface mkfst needs.
//
// Why first-party: the TypeScript task subsystem is load-bearing
// infrastructure for mkfst. Depending on a third-party Go binding
// for an engine this critical means our cancellation semantics,
// promise behavior, and capability dispatch are at the mercy of an
// external project. By owning the binding we control:
//
//   - exact wazero / WASM lifecycle and pooling
//   - how Go-side context.Context cancellation maps to JS promise
//     rejection
//   - which JSValues are pinned vs. eligible for QuickJS GC
//   - how we recover from out-of-memory, stack overflow, interrupt
//   - the exact shape of Go-callable functions registered into JS
//
// Why this WASM: it's the same QuickJS-NG build distributed by
// fastschema/qjs, shipped with a small C helper layer (`QJS_*`
// exports) that flattens QuickJS's NaN-boxed JSValue into a single
// uint64 across the WASM boundary. We vendor the binary; we do
// not depend on their Go module.
//
// Layout:
//   - quickjs.wasm                    the engine
//   - QUICKJS_WASM_LICENSE            MIT, retained per-binary
//   - module.go                       wazero compile + module pool
//   - runtime.go                      Runtime + Context lifecycle
//   - value.go                        Value alloc / convert / free
//   - eval.go                         script + module evaluation
//   - call.go                         JS function invocation
//   - props.go                        property get/set
//   - error.go                        exception capture + throw
//   - host.go                         Go-callable function registration
//   - mem.go                          WASM memory marshaling helpers
//   - lock.go                         deletes eval/Function/dynamic-import
//
// The package exports only what callers need: NewRuntime, Runtime,
// Context, Value, HostFunc. Internal WASM call wrappers are
// unexported.
package quickjs
