package quickjs

import (
	"context"
	"strings"
	"testing"
)

func TestQuickJS_EvalSimple(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, RuntimeOpts{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	v, err := rt.Eval(ctx, "1 + 2 * 3", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	defer v.Free(ctx)
	n, err := v.Int32(ctx)
	if err != nil {
		t.Fatalf("Int32: %v", err)
	}
	if n != 7 {
		t.Fatalf("got %d, want 7", n)
	}
}

func TestQuickJS_EvalString(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, RuntimeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)

	v, err := rt.Eval(ctx, `"hello, " + "world"`, EvalOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free(ctx)
	s, err := v.String(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s != "hello, world" {
		t.Fatalf("got %q", s)
	}
}

func TestQuickJS_ModernES(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, RuntimeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)

	// Optional chaining + nullish coalescing + arrow + spread + template lits.
	src := `
		const obj = { a: { b: { c: 42 } } };
		const x = obj?.a?.b?.c ?? 0;
		const y = [1, 2, 3].reduce((s, n) => s + n, 0);
		const z = ` + "`" + `x=${x}, y=${y}` + "`" + `;
		z
	`
	v, err := rt.Eval(ctx, src, EvalOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free(ctx)
	s, _ := v.String(ctx)
	if s != "x=42, y=6" {
		t.Fatalf("got %q", s)
	}
}

func TestQuickJS_Exception(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, RuntimeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)

	_, err = rt.Eval(ctx, `throw new Error("nope")`, EvalOpts{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error didn't mention 'nope': %v", err)
	}
}

func TestQuickJS_HostFunc(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, RuntimeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)

	called := 0
	fn := func(ctx context.Context, this *Value, args []*Value) (*Value, error) {
		called++
		if len(args) != 2 {
			return nil, nil
		}
		a, _ := args[0].Int32(ctx)
		b, _ := args[1].Int32(ctx)
		return rt.NewInt32(ctx, a+b)
	}
	v, err := rt.RegisterFunction(ctx, fn)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.SetGlobal(ctx, "myAdd", v); err != nil {
		t.Fatal(err)
	}

	res, err := rt.Eval(ctx, "myAdd(40, 2)", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	defer res.Free(ctx)
	n, _ := res.Int32(ctx)
	if n != 42 {
		t.Fatalf("got %d, want 42; called=%d", n, called)
	}
	if called != 1 {
		t.Fatalf("host fn called %d times, want 1", called)
	}
}

func TestQuickJS_HostFuncThrow(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, RuntimeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)

	fn := func(ctx context.Context, this *Value, args []*Value) (*Value, error) {
		return nil, errAdHoc("boom")
	}
	v, _ := rt.RegisterFunction(ctx, fn)
	_ = rt.SetGlobal(ctx, "boomer", v)

	_, err = rt.Eval(ctx, `try { boomer(); "unreached" } catch (e) { e.message }`, EvalOpts{})
	if err != nil {
		t.Fatalf("eval failed unexpectedly: %v", err)
	}
}

type errAdHoc string

func (e errAdHoc) Error() string { return string(e) }
