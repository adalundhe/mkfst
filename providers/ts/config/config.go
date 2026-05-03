// Package config parses mkfst.yaml — the operator's single source
// of truth for what stacks exist, which modules are allowed, and
// what limits apply.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// === Top-level config ===

// Config is the parsed mkfst.yaml.
type Config struct {
	Server  ServerConfig                  `yaml:"server"`
	Modules ModulesConfig                 `yaml:"modules"`
	Stacks  map[string]StackConfig        `yaml:"stacks"`
	Limits  LimitsConfig                  `yaml:"limits"`
	// Capabilities is operator-narrowed module capability overrides:
	//   capabilities:
	//     "mkfst-k6":
	//       stack.runOneShot:
	//         imageAllowList: ["myco/k6:*"]
	Capabilities map[string]map[string]map[string]any `yaml:"capabilities"`
}

// ServerConfig holds the HTTP server settings.
type ServerConfig struct {
	Listen string    `yaml:"listen"`
	TLS    TLSConfig `yaml:"tls"`
}

// TLSConfig holds TLS material paths.
type TLSConfig struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// ModulesConfig holds the npm allowlist + cache location.
type ModulesConfig struct {
	Allow []string `yaml:"allow"`
	Cache string   `yaml:"cache"`
}

// StackConfig is one declared stack.
type StackConfig struct {
	Services map[string]StackServiceConfig `yaml:"services"`
	Probes   map[string]StackProbeConfig   `yaml:"probes"`
}

// StackServiceConfig is one service in a stack.
type StackServiceConfig struct {
	Image     string            `yaml:"image"`
	Cmd       []string          `yaml:"cmd"`
	Env       map[string]string `yaml:"env"`
	Port      int               `yaml:"port"`
	Replicas  int               `yaml:"replicas"`
	DependsOn []string          `yaml:"dependsOn"`
}

// StackProbeConfig is one probe spec.
type StackProbeConfig struct {
	HTTP *HTTPProbe `yaml:"http"`
	TCP  *TCPProbe  `yaml:"tcp"`
	UDP  *UDPProbe  `yaml:"udp"`
	Mode string     `yaml:"mode"` // "readiness" | "liveness"
}

// HTTPProbe is an HTTP probe spec.
type HTTPProbe struct {
	Path string `yaml:"path"`
	Port int    `yaml:"port"`
}

// TCPProbe is a TCP probe spec.
type TCPProbe struct {
	Port int `yaml:"port"`
}

// UDPProbe is a UDP probe spec.
type UDPProbe struct {
	Port int    `yaml:"port"`
	Send string `yaml:"send"`
}

// LimitsConfig holds resource caps.
type LimitsConfig struct {
	MaxConcurrentOneShots int    `yaml:"maxConcurrentOneShots"`
	CPUMillicores         int    `yaml:"cpuMillicores"`
	MemoryMB              int    `yaml:"memoryMB"`
	WorkflowDuration      string `yaml:"workflowDuration"`
	BundleSizeKB          int    `yaml:"bundleSizeKB"`
}

// === parsing ===

// Load reads + parses a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config.Load: %w", err)
	}
	return Parse(b, filepath.Dir(path))
}

// Parse parses the bytes of a config file. baseDir is used to
// resolve relative paths (e.g., modules.cache).
func Parse(b []byte, baseDir string) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config.Parse: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	c.resolvePaths(baseDir)
	return &c, nil
}

func (c *Config) validate() error {
	if c.Modules.Cache == "" && len(c.Modules.Allow) > 0 {
		return errors.New("modules.cache is required when modules.allow is non-empty")
	}
	for name, svc := range c.Stacks {
		for sname, s := range svc.Services {
			if s.Image == "" {
				return fmt.Errorf("stack %q service %q: image is required", name, sname)
			}
		}
	}
	return nil
}

func (c *Config) resolvePaths(baseDir string) {
	if c.Modules.Cache != "" && !filepath.IsAbs(c.Modules.Cache) {
		c.Modules.Cache = filepath.Join(baseDir, c.Modules.Cache)
	}
}
