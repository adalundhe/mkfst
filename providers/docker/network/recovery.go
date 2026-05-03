package network

import (
	"context"
	"fmt"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// === crash recovery ===
//
// On engine restart, prior-process resources are tagged with the
// previous EngineID. They show up in `docker network ls` and
// `docker ps` as labeled but unowned. Recovery has two modes:
//
//   - Adopt: rebuild Stack handles from the labels and resume
//     management. Requires the same EngineID to be passed via
//     EngineOpts so we know which resources to claim.
//   - Reap: bulk-remove every labeled resource matching a filter
//     and walk away.
//
// Either mode is opt-in. By default a fresh engine ignores prior
// resources (they keep running, just unmanaged).

// AdoptOpts configures Engine.Adopt.
type AdoptOpts struct {
	// EngineID filters which engine's resources to adopt. Empty =
	// adopt every mkfst-managed resource regardless of engine ID
	// (useful when the prior process was started with a random
	// EngineID we no longer have).
	EngineID string
	// StackID, if non-empty, narrows adoption to a single stack.
	StackID string
}

// AdoptResult reports what Adopt found.
type AdoptResult struct {
	Stacks     []*Stack
	Networks   int
	Containers int
}

// Adopt discovers labeled resources from a prior engine and
// rebuilds Stack handles. The returned Stacks are in StackUp state
// (assuming their containers are still running) and can be
// inspected/Down'd as if the current engine had created them.
//
// Adoption rebuilds a minimal Stack: the network handle and the
// container map. Service definitions are NOT recovered (they live
// in Go code, not docker labels) — adopted stacks are only useful
// for shutdown / introspection, not for restart.
func (e *Engine) Adopt(ctx context.Context, opts AdoptOpts) (AdoptResult, error) {
	res := AdoptResult{}

	// 1. Find labeled networks.
	netFilter := map[string]string{LabelManagedBy: "mkfst-network"}
	if opts.EngineID != "" {
		netFilter[LabelEngineID] = opts.EngineID
	}
	if opts.StackID != "" {
		netFilter[LabelStackID] = opts.StackID
	}
	netws, err := List(ctx, e.cli, netFilter)
	if err != nil {
		return res, fmt.Errorf("Adopt: list networks: %w", err)
	}
	res.Networks = len(netws)

	for _, n := range netws {
		stackID := n.Labels()[LabelStackID]
		stackName := n.Labels()[LabelStackName]
		if stackID == "" {
			continue // not a stack-owned network
		}
		// Already registered? Skip.
		if _, exists := e.stacks.Load(stackID); exists {
			continue
		}
		s := newStack(e, stackID, stackName)
		nCopy := n
		s.network = &nCopy

		// 2. Find containers belonging to this stack.
		ctrs, err := e.listContainersByLabel(ctx, opts.EngineID, stackID, "")
		if err != nil {
			continue
		}
		res.Containers += len(ctrs)
		for _, c := range ctrs {
			svcName := c.Labels[LabelService]
			if svcName == "" {
				continue
			}
			rep := 0
			fmt.Sscanf(c.Labels[LabelReplica], "%d", &rep)
			role := c.Labels[LabelRole]
			inst := containerInstance{
				id:      c.ID,
				name:    firstName(c.Names),
				replica: rep,
				role:    role,
			}
			s.containers[svcName] = append(s.containers[svcName], inst)
		}
		s.state.Store(int32(StackUp))
		e.stacks.Store(stackID, s)
		res.Stacks = append(res.Stacks, s)
	}
	return res, nil
}

// Reap removes every mkfst-network-labeled resource that matches the
// filter. Use with care — this destroys containers and networks
// belonging to other engine instances.
func (e *Engine) Reap(ctx context.Context, opts AdoptOpts) error {
	// Containers first (so the network can be removed afterward).
	ctrs, err := e.listContainersByLabel(ctx, opts.EngineID, opts.StackID, "")
	if err != nil {
		return fmt.Errorf("Reap: list containers: %w", err)
	}
	for _, c := range ctrs {
		_ = e.cli.ContainerRemove(ctx, c.ID, dockercontainer.RemoveOptions{Force: true})
	}
	// Networks.
	netFilter := map[string]string{LabelManagedBy: "mkfst-network"}
	if opts.EngineID != "" {
		netFilter[LabelEngineID] = opts.EngineID
	}
	if opts.StackID != "" {
		netFilter[LabelStackID] = opts.StackID
	}
	netws, err := List(ctx, e.cli, netFilter)
	if err != nil {
		return fmt.Errorf("Reap: list networks: %w", err)
	}
	for _, n := range netws {
		_ = n.Remove(ctx)
	}
	return nil
}

func (e *Engine) listContainersByLabel(ctx context.Context, engineID, stackID, service string) ([]dockercontainer.Summary, error) {
	all, err := e.cli.ContainerList(ctx, dockercontainer.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := []dockercontainer.Summary{}
	for _, c := range all {
		if c.Labels[LabelManagedBy] != "mkfst-network" {
			continue
		}
		if engineID != "" && c.Labels[LabelEngineID] != engineID {
			continue
		}
		if stackID != "" && c.Labels[LabelStackID] != stackID {
			continue
		}
		if service != "" && c.Labels[LabelService] != service {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// keep the docker client import alive in case future helpers move here
var _ = client.NewClientWithOpts
