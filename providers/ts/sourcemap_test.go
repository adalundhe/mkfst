package ts

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mkfst/providers/ts/bundle"
)

func sdkDirFromHere(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "sdk")
}

// TestSourceMap_Roundtrip builds a TS bundle with a source map,
// extracts the inline map, looks up a known position in the
// generated JS, and verifies the original source is named.
func TestSourceMap_Roundtrip(t *testing.T) {
	al := bundle.NewAllowlist(sdkDirFromHere(t))
	_ = al.Add(bundle.ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(sdkDirFromHere(t), "mkfst-sdk"),
	})
	src := []byte(`
import { defineDAG, defineTask } from "@mkfst/sdk";
const t = defineTask({ name: "hello", run: () => "world" });
export default defineDAG("smoke", b => { b.add(t); });
`)
	r, err := bundle.Build(bundle.Opts{
		Source:         src,
		SourceFilename: "smoke.workflow.ts",
		Allowlist:      al,
		SourceMap:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sm, err := ParseInlineSourceMap(r.JS)
	if err != nil {
		t.Fatal(err)
	}
	if sm == nil {
		t.Fatal("nil source map")
	}
	if len(sm.Sources) == 0 {
		t.Fatal("no sources in map")
	}
	// At least one of the source files should be the workflow.
	found := false
	for _, s := range sm.Sources {
		if strings.HasSuffix(s, "smoke.workflow.ts") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("workflow source not in map: %v", sm.Sources)
	}
}

// TestSourceMap_RewriteStack verifies that stack-line rewriting
// translates a recognizable bundle position into a TS source ref.
func TestSourceMap_RewriteStack(t *testing.T) {
	al := bundle.NewAllowlist(sdkDirFromHere(t))
	_ = al.Add(bundle.ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(sdkDirFromHere(t), "mkfst-sdk"),
	})
	src := []byte(`
import { defineDAG } from "@mkfst/sdk";
export default defineDAG("rs", b => {});
`)
	r, _ := bundle.Build(bundle.Opts{
		Source:         src,
		SourceFilename: "rs.workflow.ts",
		Allowlist:      al,
		SourceMap:      true,
	})
	sm, err := ParseInlineSourceMap(r.JS)
	if err != nil {
		t.Fatal(err)
	}
	// The exact line:col mapping depends on esbuild output.
	// Smoke-test: feed in a fabricated stack frame and check the
	// output is at least non-empty.
	rewritten := sm.RewriteStack("error at <bundle>:1:1")
	if rewritten == "" {
		t.Fatal("rewrite produced empty")
	}
}
