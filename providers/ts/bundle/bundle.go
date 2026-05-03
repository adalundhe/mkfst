// Package bundle is mkfst's TypeScript-to-JavaScript build pipeline
// for user-submitted workflow files. It uses esbuild (Go-native) to
// transpile + bundle, walks the import graph against an operator-
// configured allowlist, and AST-validates the result for forbidden
// constructs before handing the bundle to the runtime.
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	esbuild "github.com/evanw/esbuild/pkg/api"
)

// === Result ===

// Result is the output of a successful bundle.
type Result struct {
	// JS is the bundled, transpiled JavaScript ready for the
	// runtime. ES2020 syntax target.
	JS []byte
	// SourceMap is the inline map (or empty if disabled).
	SourceMap []byte
	// Imports lists every (module, version) pair that was pulled
	// in, including transitive deps. Used for audit + lockfile
	// verification.
	Imports []ImportRecord
	// SHA256 is the content hash of the JS output. Recorded with
	// the workflow definition; runtime refuses mismatches.
	SHA256 string
	// SizeKB is JS output size in kilobytes.
	SizeKB int
}

// ImportRecord is one resolved import.
type ImportRecord struct {
	Module    string // npm name (e.g. "mkfst-redis", "@mkfst/sdk")
	Version   string // resolved version (from server lockfile)
	ImporterPath string // filesystem path of the file that imported it
}

// === Opts ===

// Opts configures Build.
type Opts struct {
	// Source is the TS source bytes. Required. (Single-file
	// submissions; multi-file workflows can use bundle entrypoint
	// resolution by setting EntrypointPath instead.)
	Source []byte

	// SourceFilename is the name shown in error messages (e.g.
	// "smoketest.workflow.ts"). Default "<workflow>.ts".
	SourceFilename string

	// EntrypointPath, when set, overrides Source — bundles the file
	// at this path on the server's filesystem and lets esbuild walk
	// the local import graph.
	EntrypointPath string

	// Allowlist is the operator-curated module allow set. Imports
	// outside of this set (and their transitive deps) are rejected
	// at build time. Required.
	Allowlist *Allowlist

	// MaxSourceBytes caps the raw TS source. 0 = use
	// DefaultMaxSourceBytes (256 KiB). DOS defense: large source
	// is rejected before esbuild ever sees it.
	MaxSourceBytes int

	// MaxSizeBytes caps the bundle output. 0 = use
	// DefaultMaxBundleBytes (1 MiB).
	MaxSizeBytes int

	// SourceMap controls whether to embed an inline source map.
	SourceMap bool
}

// === Build ===

// Build runs the full pipeline: source-size check → bundle →
// allowlist check → AST validate → bundle-size check → hash.
// Returns the Result or the first failure.
func Build(opts Opts) (*Result, error) {
	if opts.Allowlist == nil {
		return nil, errors.New("bundle.Build: Allowlist is required")
	}
	if opts.SourceFilename == "" {
		opts.SourceFilename = "<workflow>.ts"
	}

	// Pre-bundle source-size cap. esbuild reads stdin into memory
	// in one shot; bounding here avoids handing it gigabytes.
	maxSrc := opts.MaxSourceBytes
	if maxSrc == 0 {
		maxSrc = DefaultMaxSourceBytes
	}
	if len(opts.Source) > maxSrc {
		return nil, fmt.Errorf("bundle: source %d bytes exceeds %d-byte cap (DOS defense)", len(opts.Source), maxSrc)
	}

	// Bundle-output cap default.
	if opts.MaxSizeBytes == 0 {
		opts.MaxSizeBytes = DefaultMaxBundleBytes
	}

	// Build with esbuild. We use stdin entrypoint when Source is
	// supplied; file entrypoint otherwise.
	buildOpts := esbuild.BuildOptions{
		Bundle:       true,
		Write:        false,
		Format:       esbuild.FormatIIFE,
		GlobalName:   "__mkfst_default_export",
		Target:       esbuild.ES2020,
		Platform:     esbuild.PlatformNeutral,
		LegalComments: esbuild.LegalCommentsNone,
		Sourcemap:    sourcemapMode(opts.SourceMap),
		Loader: map[string]esbuild.Loader{
			".ts":  esbuild.LoaderTS,
			".tsx": esbuild.LoaderTSX,
			".js":  esbuild.LoaderJS,
			".mjs": esbuild.LoaderJS,
			".json": esbuild.LoaderJSON,
		},
		Plugins: []esbuild.Plugin{allowlistPlugin(opts.Allowlist)},
		// Resolve from the operator's modules cache as the npm
		// node_modules root. The plugin (above) has the final say.
		NodePaths: opts.Allowlist.NodePaths(),
	}

	if opts.EntrypointPath != "" {
		buildOpts.EntryPoints = []string{opts.EntrypointPath}
	} else {
		buildOpts.Stdin = &esbuild.StdinOptions{
			Contents:   string(opts.Source),
			Loader:     esbuild.LoaderTS,
			Sourcefile: opts.SourceFilename,
			ResolveDir: opts.Allowlist.ResolveDir(),
		}
	}

	result := esbuild.Build(buildOpts)

	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("bundle: esbuild: %s", formatMessages(result.Errors))
	}

	var jsBytes, mapBytes []byte
	for _, f := range result.OutputFiles {
		switch {
		case strings.HasSuffix(f.Path, ".map"):
			mapBytes = f.Contents
		default:
			// stdin entry produces "<stdout>"; file entry produces
			// a real path. Either way, the .js bytes are here.
			jsBytes = f.Contents
		}
	}
	if len(jsBytes) == 0 {
		return nil, errors.New("bundle: empty JS output")
	}
	if opts.MaxSizeBytes > 0 && len(jsBytes) > opts.MaxSizeBytes {
		return nil, fmt.Errorf("bundle: output %d bytes exceeds %d cap", len(jsBytes), opts.MaxSizeBytes)
	}

	// AST validation of forbidden constructs.
	if err := ValidateAST(jsBytes); err != nil {
		return nil, fmt.Errorf("bundle: ast: %w", err)
	}

	// Collect import records from esbuild metafile (if any).
	imports := collectImports(result, opts.Allowlist)

	sum := sha256.Sum256(jsBytes)
	return &Result{
		JS:        jsBytes,
		SourceMap: mapBytes,
		Imports:   imports,
		SHA256:    hex.EncodeToString(sum[:]),
		SizeKB:    (len(jsBytes) + 1023) / 1024,
	}, nil
}

func sourcemapMode(enable bool) esbuild.SourceMap {
	if enable {
		return esbuild.SourceMapInline
	}
	return esbuild.SourceMapNone
}

func formatMessages(msgs []esbuild.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Location != nil {
			fmt.Fprintf(&b, "%s:%d:%d: %s\n",
				m.Location.File, m.Location.Line, m.Location.Column, m.Text)
		} else {
			fmt.Fprintf(&b, "%s\n", m.Text)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// collectImports walks the resolved imports out of esbuild's
// metafile. We don't enable Metafile when the bundle was tiny;
// fall back to the allowlist's tracking.
func collectImports(_ esbuild.BuildResult, al *Allowlist) []ImportRecord {
	return al.Resolved()
}

// === path helpers ===

// IsBareSpecifier reports whether p is an npm-style bare module
// specifier (e.g. "mkfst-redis", "@scope/pkg/sub") as opposed to a
// relative or absolute path.
func IsBareSpecifier(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return false
	}
	if filepath.IsAbs(p) {
		return false
	}
	return true
}
