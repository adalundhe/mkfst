package bundle

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Locate the sdk dir relative to this test file.
func sdkDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	// providers/ts/bundle/bundle_test.go → providers/ts/sdk
	return filepath.Join(filepath.Dir(thisFile), "..", "sdk")
}

func TestBundle_HelloWorldWithSDK(t *testing.T) {
	al := NewAllowlist(sdkDir(t))
	if err := al.Add(ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(sdkDir(t), "mkfst-sdk"),
	}); err != nil {
		t.Fatal(err)
	}

	src := []byte(`
import { defineTask, defineDAG } from "@mkfst/sdk";

const hello = defineTask({
  name: "hello",
  run: () => "world",
});

export default defineDAG("greet", (b) => {
  b.add(hello);
});
`)
	res, err := Build(Opts{
		Source:         src,
		SourceFilename: "greet.workflow.ts",
		Allowlist:      al,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(string(res.JS), "defineTask") {
		t.Fatal("bundle missing defineTask reference")
	}
	if !strings.Contains(string(res.JS), `"greet"`) {
		t.Fatal("bundle missing dag name")
	}
	if res.SHA256 == "" {
		t.Fatal("missing sha256")
	}
	if res.SizeKB <= 0 {
		t.Fatal("zero size")
	}
}

func TestBundle_RejectsUnapprovedImport(t *testing.T) {
	al := NewAllowlist(sdkDir(t))
	_ = al.Add(ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(sdkDir(t), "mkfst-sdk"),
	})

	src := []byte(`
import { defineTask } from "@mkfst/sdk";
import * as evil from "totally-not-allowed";
defineTask({ name: "x", run: () => evil });
`)
	_, err := Build(Opts{
		Source:         src,
		SourceFilename: "bad.ts",
		Allowlist:      al,
	})
	if err == nil {
		t.Fatal("expected allowlist rejection")
	}
	if !strings.Contains(err.Error(), "totally-not-allowed") {
		t.Fatalf("error didn't mention offending import: %v", err)
	}
}

func TestBundle_ASTRejectsEval(t *testing.T) {
	al := NewAllowlist(sdkDir(t))
	_ = al.Add(ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(sdkDir(t), "mkfst-sdk"),
	})

	src := []byte(`
import { defineTask } from "@mkfst/sdk";
defineTask({
  name: "evil",
  run: () => eval("1+1"),
});
`)
	_, err := Build(Opts{
		Source:         src,
		SourceFilename: "evil.ts",
		Allowlist:      al,
	})
	if err == nil || !strings.Contains(err.Error(), "eval") {
		t.Fatalf("expected eval rejection, got %v", err)
	}
}

func TestBundle_RejectsPrototypePollution(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "__proto__ assignment",
			src:  `import { defineDAG } from "@mkfst/sdk"; ({}).__proto__ = {x: 1}; export default defineDAG("x", b => {});`,
			want: "__proto__",
		},
		{
			name: "Object.setPrototypeOf",
			src:  `import { defineDAG } from "@mkfst/sdk"; Object.setPrototypeOf({}, null); export default defineDAG("x", b => {});`,
			want: "setPrototypeOf",
		},
		{
			// esbuild itself rejects `with` in ESM modules — our
			// AST check is defense in depth and doesn't fire here.
			name: "with statement",
			src:  `import { defineDAG } from "@mkfst/sdk"; with (Math) { } export default defineDAG("x", b => {});`,
			want: "With statements",
		},
		{
			name: "constructor reach-around",
			src:  `import { defineDAG } from "@mkfst/sdk"; (()=>{}).constructor("return 1"); export default defineDAG("x", b => {});`,
			want: "constructor",
		},
		{
			// "fs" isn't allowlisted, so the allowlist plugin fires
			// before our AST check ever runs. Either rejection
			// path is acceptable; we assert allowlist's wording.
			name: "require call",
			src:  `import { defineDAG } from "@mkfst/sdk"; require("fs"); export default defineDAG("x", b => {});`,
			want: "allowlist",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			al := NewAllowlist(sdkDir(t))
			_ = al.Add(ModuleEntry{
				Name: "@mkfst/sdk",
				Path: filepath.Join(sdkDir(t), "mkfst-sdk"),
			})
			_, err := Build(Opts{
				Source:         []byte(tc.src),
				SourceFilename: "evil.ts",
				Allowlist:      al,
			})
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error didn't mention %q: %v", tc.want, err)
			}
		})
	}
}

func TestBundle_RejectsOversizeSource(t *testing.T) {
	al := NewAllowlist(sdkDir(t))
	_ = al.Add(ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(sdkDir(t), "mkfst-sdk"),
	})
	// 300 KiB of source — over the 256 KiB default.
	big := make([]byte, 300*1024)
	for i := range big {
		big[i] = '/' // valid filler
	}
	big[0] = '/'
	big[1] = '/'
	big[2] = '\n'
	_, err := Build(Opts{
		Source:         big,
		SourceFilename: "big.ts",
		Allowlist:      al,
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size cap rejection, got %v", err)
	}
}

func TestBundle_HashStable(t *testing.T) {
	// Same source must produce the same SHA — ensures the
	// hash-verify tripwire in runner.Submit doesn't false-fire on
	// re-submit of identical code.
	al := NewAllowlist(sdkDir(t))
	_ = al.Add(ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(sdkDir(t), "mkfst-sdk"),
	})
	src := []byte(`
import { defineTask, defineDAG } from "@mkfst/sdk";
const t = defineTask({ name: "x", run: () => 1 });
export default defineDAG("x", b => { b.add(t); });
`)
	r1, err := Build(Opts{Source: src, SourceFilename: "x.ts", Allowlist: al})
	if err != nil {
		t.Fatal(err)
	}
	al.ResetSession()
	r2, err := Build(Opts{Source: src, SourceFilename: "x.ts", Allowlist: al})
	if err != nil {
		t.Fatal(err)
	}
	if r1.SHA256 != r2.SHA256 {
		t.Fatalf("hashes differ across builds: %s vs %s", r1.SHA256, r2.SHA256)
	}
	if r1.SHA256 == "" {
		t.Fatal("missing sha256")
	}
}

func TestBundle_SourceMapInline(t *testing.T) {
	al := NewAllowlist(sdkDir(t))
	_ = al.Add(ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(sdkDir(t), "mkfst-sdk"),
	})
	src := []byte(`
import { defineDAG } from "@mkfst/sdk";
export default defineDAG("y", b => {});
`)
	r, err := Build(Opts{
		Source:         src,
		SourceFilename: "y.ts",
		Allowlist:      al,
		SourceMap:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.JS), "sourceMappingURL=data:application/json;base64,") {
		t.Fatal("inline source map not embedded")
	}
}

func TestBundle_ASTRejectsDynamicImport(t *testing.T) {
	al := NewAllowlist(sdkDir(t))
	_ = al.Add(ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(sdkDir(t), "mkfst-sdk"),
	})

	// Dynamic import with a runtime-computed specifier — esbuild
	// can't statically resolve these, so they pass through to the
	// runtime where our AST validator catches them.
	src := []byte(`
import { defineTask } from "@mkfst/sdk";
defineTask({
  name: "dyn",
  run: async () => {
    const m = "modname";
    return await import(m);
  },
});
`)
	_, err := Build(Opts{
		Source:         src,
		SourceFilename: "dyn.ts",
		Allowlist:      al,
	})
	if err == nil || !strings.Contains(err.Error(), "dynamic-import") {
		t.Fatalf("expected dynamic-import rejection, got %v", err)
	}
}
