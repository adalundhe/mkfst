package bundle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	esbuild "github.com/evanw/esbuild/pkg/api"
)

// === Allowlist ===
//
// Allowlist is the set of npm-style modules the operator has
// approved for user submissions. The bundler resolves every import
// through an esbuild plugin that consults this list:
//
//   - Top-level allowed: explicit user-importable modules
//     (e.g. "mkfst-k6", "mkfst-redis", "@mkfst/sdk", "zod").
//   - Transitively allowed: deps of allowed modules, resolved at
//     module-add time and recorded in the lockfile.
//
// On import resolution:
//   - Bare specifier in the allowed set → resolve to local module path
//   - Bare specifier transitively allowed → resolve to local path
//   - Bare specifier neither → reject with structured error
//   - Relative/absolute path within an allowed module → allowed
//   - Relative/absolute path outside an allowed module → reject

// Allowlist is the operator-curated module set with resolution
// metadata.
type Allowlist struct {
	mu sync.RWMutex

	// modulesDir is the root of the server's modules cache. Each
	// allowed module lives at modulesDir/<name>/ (with @scope/foo
	// at modulesDir/@scope/foo/).
	modulesDir string

	// resolveDir is the working directory esbuild uses when no
	// explicit one is supplied (typically a server-managed temp
	// dir or modulesDir).
	resolveDir string

	// allowed is the set of approved module names.
	allowed map[string]ModuleEntry

	// transitiveAllowed is computed: every dep listed in any
	// allowed module's lockfile.
	transitiveAllowed map[string]struct{}

	// resolved tracks what was actually pulled in during the
	// current build. Reset per-build via NewBuildSession.
	resolvedMu sync.Mutex
	resolved   map[string]ImportRecord // module → record
}

// ModuleEntry is one approved module's metadata.
type ModuleEntry struct {
	Name    string
	Version string
	// Path is the absolute path on the server's filesystem where
	// the module's package.json lives.
	Path string
	// TransitiveDeps is the set of dep names this module pulls in
	// (resolved at module-add time, recorded in the operator's
	// lockfile). All are auto-allowed.
	TransitiveDeps []string
}

// NewAllowlist constructs an empty Allowlist rooted at the given
// modules cache directory.
func NewAllowlist(modulesDir string) *Allowlist {
	return &Allowlist{
		modulesDir:        modulesDir,
		resolveDir:        modulesDir,
		allowed:           map[string]ModuleEntry{},
		transitiveAllowed: map[string]struct{}{},
		resolved:          map[string]ImportRecord{},
	}
}

// ModulesDir returns the configured root.
func (a *Allowlist) ModulesDir() string { return a.modulesDir }

// ResolveDir returns the directory esbuild uses for stdin entry
// resolution.
func (a *Allowlist) ResolveDir() string { return a.resolveDir }

// SetResolveDir overrides the working dir.
func (a *Allowlist) SetResolveDir(p string) { a.resolveDir = p }

// NodePaths returns paths esbuild searches for `node_modules` style
// resolution. We just return the modulesDir; the plugin enforces
// the actual allowlist.
func (a *Allowlist) NodePaths() []string {
	return []string{a.modulesDir}
}

// Add registers an approved module. transitiveDeps may be nil.
func (a *Allowlist) Add(entry ModuleEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry.Name == "" {
		return errors.New("Allowlist.Add: empty name")
	}
	if entry.Path == "" {
		// Default to modulesDir/<name>.
		entry.Path = filepath.Join(a.modulesDir, entry.Name)
	}
	a.allowed[entry.Name] = entry
	for _, d := range entry.TransitiveDeps {
		a.transitiveAllowed[d] = struct{}{}
	}
	return nil
}

// IsAllowed reports whether a bare specifier is approved (top-level
// or transitive). The private bridge SDK `@mkfst/host` is always
// allowed: the bundler synthesizes a per-importer virtual module
// for it (see allowlistPlugin), so user workflows still cannot
// import it directly — only blessed modules' import statements
// resolve.
func (a *Allowlist) IsAllowed(spec string) bool {
	if rootPackageName(spec) == "@mkfst/host" {
		return true
	}
	root := rootPackageName(spec)
	a.mu.RLock()
	defer a.mu.RUnlock()
	if _, ok := a.allowed[root]; ok {
		return true
	}
	if _, ok := a.transitiveAllowed[root]; ok {
		return true
	}
	return false
}

// Resolve maps a bare specifier to a filesystem path under
// modulesDir. Returns ("", err) if not allowed or not present.
func (a *Allowlist) Resolve(spec string) (string, error) {
	if !a.IsAllowed(spec) {
		return "", fmt.Errorf("module %q is not on the allowlist (root=%q)", spec, rootPackageName(spec))
	}
	root := rootPackageName(spec)
	subpath := strings.TrimPrefix(spec, root)
	subpath = strings.TrimPrefix(subpath, "/")

	a.mu.RLock()
	entry, ok := a.allowed[root]
	a.mu.RUnlock()

	var modPath string
	if ok && entry.Path != "" {
		modPath = entry.Path
	} else {
		modPath = filepath.Join(a.modulesDir, root)
	}
	if subpath != "" {
		modPath = filepath.Join(modPath, subpath)
	}
	// If the resolved path is a regular file, use it directly.
	if info, err := os.Stat(modPath); err == nil && !info.IsDir() {
		return modPath, nil
	}
	// If it's a directory, find the entry file. We honor a small
	// fixed list of candidates rather than fully parsing
	// package.json's "main"/"module"/"types"/"exports" fields —
	// blessed modules follow predictable layouts.
	candidates := []string{
		filepath.Join(modPath, "src", "index.ts"),
		filepath.Join(modPath, "src", "index.tsx"),
		filepath.Join(modPath, "src", "index.js"),
		filepath.Join(modPath, "dist", "index.js"),
		filepath.Join(modPath, "dist", "index.mjs"),
		filepath.Join(modPath, "index.ts"),
		filepath.Join(modPath, "index.js"),
		filepath.Join(modPath, "index.mjs"),
		modPath + ".ts",
		modPath + ".js",
		modPath + ".mjs",
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	// Fall back to the bare path; esbuild's package.json resolver
	// gets a chance.
	return modPath, nil
}

// recordResolution tracks which modules have been pulled in for the
// current build (for audit + lockfile checks).
func (a *Allowlist) recordResolution(spec, version string, importerPath string) {
	root := rootPackageName(spec)
	a.resolvedMu.Lock()
	defer a.resolvedMu.Unlock()
	if _, dup := a.resolved[root]; dup {
		return
	}
	a.resolved[root] = ImportRecord{Module: root, Version: version, ImporterPath: importerPath}
}

// Resolved returns a snapshot of what was pulled in during the
// current build session.
func (a *Allowlist) Resolved() []ImportRecord {
	a.resolvedMu.Lock()
	defer a.resolvedMu.Unlock()
	out := make([]ImportRecord, 0, len(a.resolved))
	for _, r := range a.resolved {
		out = append(out, r)
	}
	return out
}

// ResetSession clears the per-build resolved record.
func (a *Allowlist) ResetSession() {
	a.resolvedMu.Lock()
	a.resolved = map[string]ImportRecord{}
	a.resolvedMu.Unlock()
}

// === esbuild plugin ===

// allowlistPlugin returns an esbuild plugin that enforces a's
// resolution rules. Imports that aren't allowed produce errors
// with a clear "module not on allowlist" message.
//
// The plugin also implements per-module capability threading:
// when a blessed module imports `@mkfst/host`, we synthesize a
// per-importer virtual module that wraps the real `@mkfst/host`
// and tags every dispatch call with the importing module's name.
// The bridge dispatcher then knows which module is calling, so
// capability checks can scope properly.
func allowlistPlugin(a *Allowlist) esbuild.Plugin {
	return esbuild.Plugin{
		Name: "mkfst-allowlist",
		Setup: func(b esbuild.PluginBuild) {
			// Per-module @mkfst/host wrappers live under a
			// synthetic namespace. Each importer gets its own.
			b.OnResolve(esbuild.OnResolveOptions{Filter: ".*"},
				func(args esbuild.OnResolveArgs) (esbuild.OnResolveResult, error) {
					if !IsBareSpecifier(args.Path) {
						return esbuild.OnResolveResult{}, nil
					}
					if !a.IsAllowed(args.Path) {
						return esbuild.OnResolveResult{
								Errors: []esbuild.Message{{
									Text: fmt.Sprintf(
										"module %q is not on the allowlist (root package %q). "+
											"Ask the operator to add it via `mkfst module add`.",
										args.Path, rootPackageName(args.Path)),
								}},
							},
							nil
					}
					// Special-case @mkfst/host: thread caller identity.
					// Only blessed modules may import @mkfst/host; user
					// workflow files cannot.
					if args.Path == "@mkfst/host" {
						importerModule := importerRootModule(args.Importer, a)
						if importerModule == "user" {
							return esbuild.OnResolveResult{
									Errors: []esbuild.Message{{
										Text: "@mkfst/host is private — only blessed modules may import it. " +
											"Use the high-level wrappers (mkfst-stack, mkfst-redis, ...) instead.",
									}},
								},
								nil
						}
						return esbuild.OnResolveResult{
							Path:      "mkfst-host:" + importerModule,
							Namespace: "mkfst-host-virtual",
						}, nil
					}
					resolved, err := a.Resolve(args.Path)
					if err != nil {
						return esbuild.OnResolveResult{
								Errors: []esbuild.Message{{Text: err.Error()}},
							},
							nil
					}
					a.recordResolution(args.Path, "", args.Importer)
					return esbuild.OnResolveResult{Path: resolved}, nil
				})

			// Generate a per-caller wrapper around the real @mkfst/host
			// surface. Each call goes through __mkfst_dispatch with
			// the importing module's name as a third argument.
			b.OnLoad(esbuild.OnLoadOptions{
				Filter:    ".*",
				Namespace: "mkfst-host-virtual",
			}, func(args esbuild.OnLoadArgs) (esbuild.OnLoadResult, error) {
				const prefix = "mkfst-host:"
				if !strings.HasPrefix(args.Path, prefix) {
					return esbuild.OnLoadResult{}, fmt.Errorf("unexpected virtual path %q", args.Path)
				}
				moduleName := args.Path[len(prefix):]
				code := generateHostShim(moduleName)
				loader := esbuild.LoaderJS
				return esbuild.OnLoadResult{
					Contents: &code,
					Loader:   loader,
				}, nil
			})
		},
	}
}

// importerRootModule returns the npm root package name that owns
// the file at importerPath, by finding the longest allowed-module
// path that's a prefix of importerPath. Falls back to "user" when
// the importer is the user's submitted workflow file.
func importerRootModule(importerPath string, a *Allowlist) string {
	if importerPath == "" {
		return "user"
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	bestName, bestLen := "", 0
	for name, entry := range a.allowed {
		if entry.Path == "" {
			continue
		}
		// Match either entry.Path itself or any subpath.
		if strings.HasPrefix(importerPath, entry.Path) && len(entry.Path) > bestLen {
			bestName = name
			bestLen = len(entry.Path)
		}
	}
	if bestName == "" {
		return "user"
	}
	return bestName
}

// generateHostShim emits the per-module @mkfst/host wrapper. Every
// host call goes through __mkfst_dispatch(op, argsJSON,
// callingModule); the third arg is what enables capability scoping.
func generateHostShim(moduleName string) string {
	// We escape the moduleName as a JSON string literal so any
	// special characters are safe in JS source.
	escaped := jsStringLiteral(moduleName)
	return `// auto-generated per-module @mkfst/host wrapper for ` + moduleName + `
const __MODULE = ` + escaped + `;

function __dispatch(op, args) {
	if (typeof globalThis.__mkfst_dispatch !== "function") {
		throw new Error("@mkfst/host: bridge not installed");
	}
	const raw = globalThis.__mkfst_dispatch(op, JSON.stringify(args == null ? {} : args), __MODULE);
	if (raw === "" || raw == null) return undefined;
	const parsed = JSON.parse(raw);
	if (parsed && typeof parsed === "object" && parsed.__error) {
		const e = new Error(parsed.__error.message || "host error");
		e.code = parsed.__error.code || "HOST";
		throw e;
	}
	return parsed;
}

export class HostError extends Error {
	constructor(opts) {
		super(opts && opts.message);
		this.name = "HostError";
		this.code = (opts && opts.code) || "HOST";
	}
}

export function stack(name) {
	return {
		runOneShot: (opts) => __dispatch("stack.runOneShot", Object.assign({ stack: name }, opts || {})),
		exec: (service, replica, opts) => __dispatch("stack.exec", Object.assign({ stack: name, service: service, replica: replica }, opts || {})),
		address: (service) => __dispatch("stack.address", { stack: name, service: service }),
		waitHealthy: (service, timeoutSec) => __dispatch("stack.waitHealthy", { stack: name, service: service, timeoutSec: timeoutSec }),
	};
}

export const log = {
	debug: (msg, fields) => __dispatch("log", { level: "debug", msg: msg, fields: fields }),
	info:  (msg, fields) => __dispatch("log", { level: "info",  msg: msg, fields: fields }),
	warn:  (msg, fields) => __dispatch("log", { level: "warn",  msg: msg, fields: fields }),
	error: (msg, fields) => __dispatch("log", { level: "error", msg: msg, fields: fields }),
};

export const host = { stack, log };
`
}

// jsStringLiteral encodes s as a JSON-style JS string literal.
func jsStringLiteral(s string) string {
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, []byte(string(r))...)
		}
	}
	out = append(out, '"')
	return string(out)
}

// rootPackageName extracts the npm root package name from a
// specifier ("mkfst-k6/dist/foo" → "mkfst-k6"; "@scope/pkg/sub" →
// "@scope/pkg").
func rootPackageName(spec string) string {
	if strings.HasPrefix(spec, "@") {
		// Scoped: @scope/name[/...]
		parts := strings.SplitN(spec, "/", 3)
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return spec
	}
	if i := strings.Index(spec, "/"); i >= 0 {
		return spec[:i]
	}
	return spec
}
