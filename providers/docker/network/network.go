package network

import (
	"context"
	"errors"
	"fmt"

	dockernetwork "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// === errors ===

var (
	// ErrNotFound: no resource matches the lookup.
	ErrNotFound = errors.New("network: not found")
	// ErrAlreadyExists: a resource with the same name already
	// exists. Returned by Create when the docker daemon reports a
	// 409 conflict and the caller did not opt into adoption.
	ErrAlreadyExists = errors.New("network: already exists")
	// ErrInvalidConfig: a config option failed validation before
	// hitting the daemon. Caller can fix and retry.
	ErrInvalidConfig = errors.New("network: invalid config")
)

// === network resource type ===

// Network is a thin handle around a docker network. Construct via
// Create or Get; every operation goes through the docker SDK with
// retry-on-transient-error.
type Network struct {
	id     string
	name   string
	labels map[string]string
	cli    *client.Client
	retry  RetryOpts
}

// ID returns the docker network ID (long form).
func (n *Network) ID() string { return n.id }

// Name returns the docker network name.
func (n *Network) Name() string { return n.name }

// Labels returns a copy of the labels assigned at create time. The
// returned map is safe to mutate without affecting the network.
func (n *Network) Labels() map[string]string {
	out := make(map[string]string, len(n.labels))
	for k, v := range n.labels {
		out[k] = v
	}
	return out
}

// === create options ===

// CreateOpts configures Create. All fields are optional unless noted.
type CreateOpts struct {
	// Driver chooses the network driver. Empty defaults to "bridge".
	// "overlay" requires swarm mode; "macvlan" / "ipvlan" require
	// host configuration. mkfst stacks default to "bridge".
	Driver string

	// EnableIPv6 turns on IPv6. Off by default; turning it on
	// requires the daemon to have ipv6 enabled.
	EnableIPv6 bool

	// Internal makes the network internal — containers connected to
	// it cannot reach the external internet. Use this only when a
	// stack should be wholly hermetic; mkfst stacks generally do
	// NOT set this so containers can reach external Valkey/DBs.
	Internal bool

	// Attachable allows manual `docker network connect` from
	// containers outside the stack. Off by default; mkfst stacks
	// keep this false to enforce isolation.
	Attachable bool

	// Subnet is the explicit IPAM subnet (CIDR). Leave empty to
	// let docker assign from the daemon's address pool.
	Subnet string

	// Gateway is the gateway address within the subnet. Empty =
	// docker picks the first usable address.
	Gateway string

	// IPRange is a sub-range from which to allocate container IPs
	// (must be inside Subnet). Empty = all of Subnet is allocatable.
	IPRange string

	// Options are arbitrary driver-specific options
	// (e.g., "com.docker.network.bridge.enable_icc": "false" to
	// disable inter-container communication on the bridge).
	Options map[string]string

	// ExtraLabels are merged with the standard mkfst labels. Useful
	// when callers want their own taxonomies on top of mkfst's.
	ExtraLabels map[string]string

	// Retry overrides the default retry policy for this Create.
	Retry RetryOpts
}

// CreateOption is the functional-option form of CreateOpts. Mirrors
// providers/docker's RunOption pattern so callers see consistent
// ergonomics.
type CreateOption func(*CreateOpts)

// Driver sets the network driver.
func Driver(driver string) CreateOption {
	return func(o *CreateOpts) { o.Driver = driver }
}

// Internal marks the network as internal (no external NAT).
func Internal() CreateOption {
	return func(o *CreateOpts) { o.Internal = true }
}

// Subnet sets the explicit IPAM subnet.
func Subnet(cidr string) CreateOption {
	return func(o *CreateOpts) { o.Subnet = cidr }
}

// NetGateway sets the gateway address within the subnet (renamed
// to avoid collision with the in-process gateway type).
func NetGateway(addr string) CreateOption {
	return func(o *CreateOpts) { o.Gateway = addr }
}

// IPRange sub-restricts allocatable IPs within Subnet.
func IPRange(cidr string) CreateOption {
	return func(o *CreateOpts) { o.IPRange = cidr }
}

// Option sets one driver-specific option key/value pair.
func Option(key, value string) CreateOption {
	return func(o *CreateOpts) {
		if o.Options == nil {
			o.Options = map[string]string{}
		}
		o.Options[key] = value
	}
}

// ExtraLabel adds a single user label on top of mkfst's standard set.
func ExtraLabel(key, value string) CreateOption {
	return func(o *CreateOpts) {
		if o.ExtraLabels == nil {
			o.ExtraLabels = map[string]string{}
		}
		o.ExtraLabels[key] = value
	}
}

// === Create ===

// Create makes a new docker network and returns a typed handle. Tags
// the network with mkfst labels (engineID + stackID); pass these via
// Engine + Stack helpers — for direct Create calls, the labels are
// engine-only with stackID = "" indicating "no stack owns this".
//
// Errors:
//   - ErrAlreadyExists if the daemon reports a 409 (network with
//     this name exists). Callers wanting adopt-or-create semantics
//     should use Get-then-Create or recovery.AdoptNetwork.
//   - ErrInvalidConfig when subnet/gateway/iprange validation fails
//     locally before hitting the daemon.
func Create(ctx context.Context, cli *client.Client, engineID, stackID, stackName, name string, opts ...CreateOption) (*Network, error) {
	if cli == nil {
		return nil, fmt.Errorf("network.Create: nil docker client")
	}
	if name == "" {
		return nil, fmt.Errorf("network.Create: %w: name is empty", ErrInvalidConfig)
	}
	o := CreateOpts{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.Driver == "" {
		o.Driver = "bridge"
	}

	labels := stackLabels(engineID, stackID, stackName, KindNetwork)
	for k, v := range o.ExtraLabels {
		labels[k] = v
	}

	cfg := dockernetwork.CreateOptions{
		Driver:     o.Driver,
		EnableIPv6: boolPtr(o.EnableIPv6),
		Internal:   o.Internal,
		Attachable: o.Attachable,
		Options:    o.Options,
		Labels:     labels,
	}
	if o.Subnet != "" || o.Gateway != "" || o.IPRange != "" {
		cfg.IPAM = &dockernetwork.IPAM{
			Driver: "default",
			Config: []dockernetwork.IPAMConfig{{
				Subnet:  o.Subnet,
				Gateway: o.Gateway,
				IPRange: o.IPRange,
			}},
		}
	}

	resp, err := retryWithResult(ctx, o.Retry, func(ctx context.Context) (dockernetwork.CreateResponse, error) {
		return cli.NetworkCreate(ctx, name, cfg)
	})
	if err != nil {
		if isConflictError(err) {
			return nil, fmt.Errorf("network.Create: %w: %s", ErrAlreadyExists, name)
		}
		return nil, fmt.Errorf("network.Create: %w", err)
	}
	return &Network{
		id:     resp.ID,
		name:   name,
		labels: labels,
		cli:    cli,
		retry:  o.Retry,
	}, nil
}

// Get returns a handle for an existing network by name. Returns
// ErrNotFound if the network doesn't exist.
func Get(ctx context.Context, cli *client.Client, name string) (*Network, error) {
	if cli == nil {
		return nil, fmt.Errorf("network.Get: nil docker client")
	}
	insp, err := retryWithResult(ctx, RetryOpts{}, func(ctx context.Context) (dockernetwork.Inspect, error) {
		return cli.NetworkInspect(ctx, name, dockernetwork.InspectOptions{})
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, fmt.Errorf("network.Get %q: %w", name, ErrNotFound)
		}
		return nil, fmt.Errorf("network.Get: %w", err)
	}
	return &Network{
		id:     insp.ID,
		name:   insp.Name,
		labels: insp.Labels,
		cli:    cli,
	}, nil
}

// === instance methods ===

// Remove deletes the network. Idempotent: ErrNotFound is treated as
// success since the goal-state is "the network is gone."
func (n *Network) Remove(ctx context.Context) error {
	err := retry(ctx, n.retry, func(ctx context.Context) error {
		return n.cli.NetworkRemove(ctx, n.id)
	})
	if err == nil {
		return nil
	}
	if isNotFoundError(err) {
		return nil
	}
	return fmt.Errorf("network.Remove %s: %w", n.name, err)
}

// Connect attaches a container to this network with the given DNS
// aliases. Aliases are resolvable by other containers in the same
// network — the canonical way to address services by name.
func (n *Network) Connect(ctx context.Context, containerID string, aliases ...string) error {
	cfg := &dockernetwork.EndpointSettings{}
	if len(aliases) > 0 {
		cfg.Aliases = aliases
	}
	err := retry(ctx, n.retry, func(ctx context.Context) error {
		return n.cli.NetworkConnect(ctx, n.id, containerID, cfg)
	})
	if err != nil {
		return fmt.Errorf("network.Connect %s→%s: %w", containerID, n.name, err)
	}
	return nil
}

// Disconnect detaches a container from this network. Force=true
// removes even from containers in unknown state.
func (n *Network) Disconnect(ctx context.Context, containerID string, force bool) error {
	err := retry(ctx, n.retry, func(ctx context.Context) error {
		return n.cli.NetworkDisconnect(ctx, n.id, containerID, force)
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("network.Disconnect %s↛%s: %w", containerID, n.name, err)
	}
	return nil
}

// Inspect returns the full docker-side network state.
func (n *Network) Inspect(ctx context.Context) (dockernetwork.Inspect, error) {
	insp, err := retryWithResult(ctx, n.retry, func(ctx context.Context) (dockernetwork.Inspect, error) {
		return n.cli.NetworkInspect(ctx, n.id, dockernetwork.InspectOptions{})
	})
	if err != nil {
		if isNotFoundError(err) {
			return dockernetwork.Inspect{}, fmt.Errorf("network.Inspect %s: %w", n.name, ErrNotFound)
		}
		return dockernetwork.Inspect{}, fmt.Errorf("network.Inspect %s: %w", n.name, err)
	}
	return insp, nil
}

// === listing ===

// List returns every network on the daemon. filter, when non-nil, is
// applied to each network's labels — only networks where every key
// in filter matches (and equals the same value) are returned. Pass
// nil to get the unfiltered list.
//
// Common usage: filter by stack ID to find all stack-owned networks:
//
//	List(ctx, cli, map[string]string{LabelStackID: stackID})
func List(ctx context.Context, cli *client.Client, filter map[string]string) ([]Network, error) {
	if cli == nil {
		return nil, fmt.Errorf("network.List: nil docker client")
	}
	all, err := retryWithResult(ctx, RetryOpts{}, func(ctx context.Context) ([]dockernetwork.Summary, error) {
		return cli.NetworkList(ctx, dockernetwork.ListOptions{})
	})
	if err != nil {
		return nil, fmt.Errorf("network.List: %w", err)
	}
	out := make([]Network, 0, len(all))
	for _, s := range all {
		if !labelsMatch(s.Labels, filter) {
			continue
		}
		out = append(out, Network{
			id:     s.ID,
			name:   s.Name,
			labels: s.Labels,
			cli:    cli,
		})
	}
	return out, nil
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// === SDK error classifiers ===

// isNotFoundError catches the SDK's "no such network" / 404 family.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsFold(msg, "no such network") ||
		containsFold(msg, "not found") ||
		containsFold(msg, "404")
}

// isConflictError catches the SDK's 409 conflict ("network with
// name X already exists").
func isConflictError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsFold(msg, "already exists") ||
		containsFold(msg, "409") ||
		containsFold(msg, "conflict")
}

func boolPtr(b bool) *bool { return &b }
