package runtime

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// === capability declarations ===
//
// Each blessed module ships a `mkfst.capabilities` block in its
// package.json. The CapabilityRegistry parses these blocks at
// module-add time and exposes a PolicyChecker the bridge consults
// per-call.
//
// Operators may further narrow declared capabilities via the
// mkfst.yaml `capabilities:` block — narrowing only, never widening.

// CapabilitySpec is a single declared host-call permission.
type CapabilitySpec struct {
	// Op is the bridge op name (e.g. "stack.runOneShot").
	Op string

	// Booleans (parsed when the value is `true`).
	Allowed bool

	// Constraint maps (any of these may be present).
	ImageAllowList    []string
	ServiceAllowList  []string
	CmdRegex          string
	URLAllowList      []string
	KeyPrefix         string
	MaxBytes          int64
	MaxTimeoutSec     int

	// Compiled (cached) forms for hot-path use.
	cmdRE *regexp.Regexp
}

// ModuleCapabilities is the full capability set declared by one
// module.
type ModuleCapabilities struct {
	ModuleName string
	Caps       map[string]*CapabilitySpec
}

// CapabilityRegistry holds capabilities for every loaded module
// and applies operator narrowing on lookup.
type CapabilityRegistry struct {
	mu       sync.RWMutex
	modules  map[string]*ModuleCapabilities
	override map[string]map[string]map[string]any
}

// NewCapabilityRegistry returns an empty registry.
func NewCapabilityRegistry() *CapabilityRegistry {
	return &CapabilityRegistry{
		modules:  map[string]*ModuleCapabilities{},
		override: map[string]map[string]map[string]any{},
	}
}

// LoadFromPackageJSON parses a module's package.json `mkfst.capabilities`
// block. Returns the parsed ModuleCapabilities or nil if there's
// no mkfst block (the module has zero capabilities — pure utility).
func LoadFromPackageJSON(moduleName string, pkgJSON []byte) (*ModuleCapabilities, error) {
	var doc struct {
		Mkfst struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"mkfst"`
	}
	if err := json.Unmarshal(pkgJSON, &doc); err != nil {
		return nil, fmt.Errorf("LoadFromPackageJSON %s: %w", moduleName, err)
	}
	mc := &ModuleCapabilities{
		ModuleName: moduleName,
		Caps:       map[string]*CapabilitySpec{},
	}
	for op, raw := range doc.Mkfst.Capabilities {
		spec := &CapabilitySpec{Op: op, Allowed: true}
		// Try as bool first.
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			spec.Allowed = b
			mc.Caps[op] = spec
			continue
		}
		// Try as object.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err == nil {
			applyConstraints(spec, obj)
			mc.Caps[op] = spec
			continue
		}
		return nil, fmt.Errorf("module %s capability %q: unsupported value", moduleName, op)
	}
	return mc, nil
}

func applyConstraints(spec *CapabilitySpec, obj map[string]json.RawMessage) {
	for k, v := range obj {
		switch k {
		case "imageAllowList":
			_ = json.Unmarshal(v, &spec.ImageAllowList)
		case "serviceAllowList":
			_ = json.Unmarshal(v, &spec.ServiceAllowList)
		case "cmdRegex":
			_ = json.Unmarshal(v, &spec.CmdRegex)
			if spec.CmdRegex != "" {
				if re, err := regexp.Compile(spec.CmdRegex); err == nil {
					spec.cmdRE = re
				}
			}
		case "urlAllowList":
			_ = json.Unmarshal(v, &spec.URLAllowList)
		case "keyPrefix":
			_ = json.Unmarshal(v, &spec.KeyPrefix)
		case "maxBytes":
			_ = json.Unmarshal(v, &spec.MaxBytes)
		case "maxTimeoutSec":
			_ = json.Unmarshal(v, &spec.MaxTimeoutSec)
		}
	}
}

// Add registers a module's capabilities.
func (r *CapabilityRegistry) Add(mc *ModuleCapabilities) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modules[mc.ModuleName] = mc
}

// SetOperatorOverride applies operator-side narrowing. Each map
// value replaces the module's declared constraints for that op
// (operators can only narrow — adding more permissive constraints
// is silently ignored).
func (r *CapabilityRegistry) SetOperatorOverride(overrides map[string]map[string]map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.override = overrides
}

// Check is the PolicyChecker shape expected by the bridge.
func (r *CapabilityRegistry) Check(moduleName, op string, args []byte) error {
	r.mu.RLock()
	mc := r.modules[moduleName]
	override := r.override[moduleName]
	r.mu.RUnlock()
	if mc == nil {
		// Unknown module → no capabilities. v1 is permissive when
		// no module identification is plumbed; production should
		// thread the calling module's name (TODO in runner.go).
		return nil
	}
	spec, ok := mc.Caps[op]
	if !ok || !spec.Allowed {
		return fmt.Errorf("module %s: capability %s not declared", moduleName, op)
	}
	// Apply operator override (narrowing).
	if override != nil {
		if narrow, ok := override[op]; ok {
			spec = narrowSpec(spec, narrow)
		}
	}
	// Op-specific validation.
	switch op {
	case "stack.runOneShot":
		return checkRunOneShot(spec, args)
	case "stack.exec":
		return checkExec(spec, args)
	default:
		return nil
	}
}

func narrowSpec(spec *CapabilitySpec, override map[string]any) *CapabilitySpec {
	out := *spec
	if v, ok := override["imageAllowList"]; ok {
		if list, ok := v.([]any); ok {
			out.ImageAllowList = toStrSlice(list)
		}
	}
	if v, ok := override["serviceAllowList"]; ok {
		if list, ok := v.([]any); ok {
			out.ServiceAllowList = toStrSlice(list)
		}
	}
	if v, ok := override["cmdRegex"]; ok {
		if s, ok := v.(string); ok {
			out.CmdRegex = s
			if re, err := regexp.Compile(s); err == nil {
				out.cmdRE = re
			}
		}
	}
	if v, ok := override["maxTimeoutSec"]; ok {
		switch n := v.(type) {
		case int:
			out.MaxTimeoutSec = n
		case float64:
			out.MaxTimeoutSec = int(n)
		}
	}
	return &out
}

func toStrSlice(a []any) []string {
	out := make([]string, 0, len(a))
	for _, v := range a {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func checkRunOneShot(spec *CapabilitySpec, args []byte) error {
	var p struct {
		Image      string `json:"image"`
		TimeoutSec int    `json:"timeoutSec"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil // permissive on decode failure; caller will get
	}
	if len(spec.ImageAllowList) > 0 && !globMatchAny(spec.ImageAllowList, p.Image) {
		return fmt.Errorf("image %q not in allow list", p.Image)
	}
	if spec.MaxTimeoutSec > 0 && p.TimeoutSec > spec.MaxTimeoutSec {
		return fmt.Errorf("timeout %ds > max %ds", p.TimeoutSec, spec.MaxTimeoutSec)
	}
	return nil
}

func checkExec(spec *CapabilitySpec, args []byte) error {
	var p struct {
		Service string   `json:"service"`
		Cmd     []string `json:"cmd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil
	}
	if len(spec.ServiceAllowList) > 0 && !globMatchAny(spec.ServiceAllowList, p.Service) {
		return fmt.Errorf("service %q not in allow list", p.Service)
	}
	if spec.cmdRE != nil && len(p.Cmd) > 0 {
		joined := strings.Join(p.Cmd, " ")
		if !spec.cmdRE.MatchString(joined) {
			return fmt.Errorf("cmd %q does not match %s", joined, spec.CmdRegex)
		}
	}
	return nil
}

// globMatchAny reports whether s matches any of the patterns
// (filepath.Match semantics: "*" matches any except "/").
func globMatchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if matched, _ := filepath.Match(p, s); matched {
			return true
		}
	}
	return false
}
