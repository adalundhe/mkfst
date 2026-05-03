package config

import (
	"context"
	"fmt"
	"time"

	"mkfst/providers/docker/network"
)

// === stack apply ===

// ApplyStacks materializes the stacks declared in the config as
// live network.Stack instances bound to the given engine. Stacks
// already up are left alone (idempotent). Stacks present in the
// engine but absent from the config are NOT torn down — Down is
// an explicit operator action via `mkfst stack down`.
func ApplyStacks(ctx context.Context, c *Config, eng *network.Engine) ([]*network.Stack, error) {
	if c == nil {
		return nil, fmt.Errorf("ApplyStacks: nil config")
	}
	out := make([]*network.Stack, 0, len(c.Stacks))
	for name, sc := range c.Stacks {
		// Skip if already up under the same name.
		if existing := findStackByName(eng, name); existing != nil {
			out = append(out, existing)
			continue
		}
		s, err := buildStack(eng, name, sc)
		if err != nil {
			return out, fmt.Errorf("apply stack %q: %w", name, err)
		}
		upCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err = s.Up(upCtx)
		cancel()
		if err != nil {
			return out, fmt.Errorf("apply stack %q: Up: %w", name, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func findStackByName(eng *network.Engine, name string) *network.Stack {
	for _, s := range eng.Stacks() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func buildStack(eng *network.Engine, name string, sc StackConfig) (*network.Stack, error) {
	s, err := eng.NewStack(name)
	if err != nil {
		return nil, err
	}
	for svcName, svc := range sc.Services {
		opts := []network.ServiceOption{
			network.Image(svc.Image),
		}
		if len(svc.Cmd) > 0 {
			opts = append(opts, network.Cmd(svc.Cmd...))
		}
		for k, v := range svc.Env {
			opts = append(opts, network.Env(k, v))
		}
		if svc.Port > 0 {
			opts = append(opts, network.Port(svc.Port))
		}
		if svc.Replicas > 1 {
			opts = append(opts, network.Replicas(svc.Replicas))
		}
		if len(svc.DependsOn) > 0 {
			opts = append(opts, network.DependsOn(svc.DependsOn...))
		}
		if probe := buildProbe(sc.Probes[svcName]); probe != nil {
			mode := network.ProbeReadiness
			if sc.Probes[svcName].Mode == "liveness" {
				mode = network.ProbeLiveness
			}
			opts = append(opts, network.WithProbe(probe, mode))
		}
		if _, err := s.AddService(svcName, opts...); err != nil {
			return nil, fmt.Errorf("AddService %q: %w", svcName, err)
		}
	}
	return s, nil
}

func buildProbe(pc StackProbeConfig) *network.Probe {
	switch {
	case pc.HTTP != nil && pc.HTTP.Port > 0:
		return network.HTTPProbe(pc.HTTP.Port, pathOrDefault(pc.HTTP.Path)).
			WithFailureThreshold(40)
	case pc.TCP != nil && pc.TCP.Port > 0:
		return network.TCPProbe(pc.TCP.Port).WithFailureThreshold(40)
	case pc.UDP != nil && pc.UDP.Port > 0:
		return network.UDPProbe(pc.UDP.Port, []byte(pc.UDP.Send)).WithFailureThreshold(40)
	}
	return nil
}

func pathOrDefault(p string) string {
	if p == "" {
		return "/"
	}
	return p
}
