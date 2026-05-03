package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	dockermount "github.com/docker/docker/api/types/mount"
	dockernetwork "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"golang.org/x/sync/errgroup"
)

// strconvI is a tiny int-to-string helper to avoid pulling strconv
// purely for nat.NewPort calls.
func strconvI(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	pos := len(b)
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// === stack state machine ===

// StackState enumerates the lifecycle states of a Stack. The state
// machine is strictly ordered:
//
//	Down → Creating → Created → Starting → Up
//	Up   → Stopping → Down
//	*    → Failed   (terminal until explicit Down)
//
// Invalid transitions return ErrIllegalStateTransition. The only
// caller-visible transitions are Up()/Down(); intermediate states
// surface via Status() for observability.
type StackState int32

const (
	StackDown StackState = iota
	StackCreating
	StackCreated
	StackStarting
	StackUp
	StackStopping
	StackFailed
)

func (s StackState) String() string {
	switch s {
	case StackDown:
		return "down"
	case StackCreating:
		return "creating"
	case StackCreated:
		return "created"
	case StackStarting:
		return "starting"
	case StackUp:
		return "up"
	case StackStopping:
		return "stopping"
	case StackFailed:
		return "failed"
	}
	return "unknown"
}

// ErrIllegalStateTransition is returned when an operation is
// attempted from a state that doesn't permit it.
var ErrIllegalStateTransition = errors.New("network: illegal stack state transition")

// === Stack ===

// Stack is a named bundle of Services on a private bridge network.
// Construct via Engine.NewStack; configure via AddService /
// AddSecret / Ingress; lifecycle via Up / Down.
type Stack struct {
	id     string
	name   string
	engine *Engine

	mu       sync.RWMutex
	services map[string]*Service
	order    []string
	secrets  map[string]Secret
	ingress  []*Ingress

	// stopTimeoutDefault applies to Down() when individual services
	// don't override.
	stopTimeoutDefault time.Duration

	// drainTimeout caps how long Down waits for in-flight ingress
	// connections before forcing close. 0 = forever (until ctx).
	drainTimeout time.Duration

	// internalNetwork makes the network internal (no NAT) — only
	// for hermetic stacks; default false.
	internalNetwork bool

	// disableICC disables inter-container communication on the
	// bridge. Default false (services in the same stack can talk).
	disableICC bool

	// state is the current StackState. Read with stateLoad / write
	// with stateCAS — direct atomic.Int32 ops to avoid taking mu
	// just for state reads.
	state atomic.Int32

	// runtime state populated during Up:
	network    *Network
	containers map[string][]containerInstance // service → list of replicas
	monitor    *Monitor
	gateway    *Gateway

	// per-service runtime egress holders (atomic snapshots).
	egress map[string]*egressHolder

	// per-service runtime probe state (one per replica).
	probes map[string][]*replicaProbeState

	// per-stack stop signal closed by Down to notify hooks and
	// background goroutines.
	stopCh chan struct{}

	// background goroutines (probe runners, restart watchers,
	// monitor emitter) tracked here for join-on-Down.
	bg errgroup.Group

	// secret materialization paths (populated by materializeSecrets).
	cachedSecretsDir string
	secretPaths      map[string]string

	// dns is the per-stack DNS resolver (best-effort; nil if bind
	// failed and we're running without DNS-layer egress
	// enforcement).
	dns *dnsResolver

	// oneShotSem caps concurrent RunOneShot calls. nil = unlimited.
	oneShotSem chan struct{}
}

// containerInstance holds a created container's identity + per-instance
// state. One entry per replica.
type containerInstance struct {
	id       string
	name     string
	replica  int
	role     string
	hostPort string // loopback-published "127.0.0.1:NNN" for the service's primary port; "" if none
}

// === stack construction (called by Engine) ===

func newStack(engine *Engine, id, name string) *Stack {
	s := &Stack{
		id:                 id,
		name:               name,
		engine:             engine,
		services:           map[string]*Service{},
		secrets:            map[string]Secret{},
		stopTimeoutDefault: 10 * time.Second,
		drainTimeout:       30 * time.Second,
		containers:         map[string][]containerInstance{},
		egress:             map[string]*egressHolder{},
		probes:             map[string][]*replicaProbeState{},
		stopCh:             make(chan struct{}),
	}
	s.state.Store(int32(StackDown))
	return s
}

// === public configuration API ===

// ID returns the stack's unique identifier (used as the
// LabelStackID on every owned resource).
func (s *Stack) ID() string { return s.id }

// Name returns the human-readable stack name.
func (s *Stack) Name() string { return s.name }

// State returns the current state. Cheap (atomic read).
func (s *Stack) State() StackState { return StackState(s.state.Load()) }

// AddService declares a service. Returns the new *Service so callers
// can introspect; modifications via the option set should happen
// before Up — modifications after Up have undefined effect and may
// be rejected.
//
// Returns ErrInvalidConfig on empty/duplicate names. Errors are
// fail-fast at startup; service definition is a call-once-per-name
// operation.
func (s *Stack) AddService(name string, opts ...ServiceOption) (*Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		return nil, fmt.Errorf("%w: service name must not be empty", ErrInvalidConfig)
	}
	if _, dup := s.services[name]; dup {
		return nil, fmt.Errorf("%w: service %q already added", ErrInvalidConfig, name)
	}
	svc := &Service{name: name}
	for _, opt := range opts {
		opt(svc)
	}
	s.services[name] = svc
	s.order = append(s.order, name)
	return svc, nil
}

// MustAddService is the panic-on-error variant for code paths
// where definition is statically known correct. Use sparingly —
// AddService is the safer default.
func (s *Stack) MustAddService(name string, opts ...ServiceOption) *Service {
	svc, err := s.AddService(name, opts...)
	if err != nil {
		panic(err)
	}
	return svc
}

// AddSecret registers a secret on the stack. Services reference it
// via UseSecret(name, mountPath). The secret is materialized as a
// tmpfs-backed file (Linux) or named-pipe (Windows) at Up time and
// removed at Down.
//
// Returns ErrInvalidConfig on empty/duplicate names.
func (s *Stack) AddSecret(name string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		return fmt.Errorf("%w: secret name must not be empty", ErrInvalidConfig)
	}
	if _, dup := s.secrets[name]; dup {
		return fmt.Errorf("%w: secret %q already added", ErrInvalidConfig, name)
	}
	s.secrets[name] = Secret{Name: name, Value: append([]byte(nil), value...)}
	return nil
}

// SetStopTimeout overrides the default SIGTERM-to-SIGKILL grace
// period for services that don't set their own.
func (s *Stack) SetStopTimeout(d time.Duration) { s.stopTimeoutDefault = d }

// SetDrainTimeout caps how long Down waits for in-flight ingress
// connections to finish.
func (s *Stack) SetDrainTimeout(d time.Duration) { s.drainTimeout = d }

// Internal makes the stack's network internal (no outbound NAT).
// Use with care — services can no longer reach external Valkey/DBs.
func (s *Stack) Internal(b bool) { s.internalNetwork = b }

// DisableInterContainerComm prevents containers in this stack's
// network from talking to each other directly. Useful for fully
// isolated single-service stacks.
func (s *Stack) DisableInterContainerComm(b bool) { s.disableICC = b }

// Monitor returns the per-stack event channel. Callers should drain
// it; the channel is buffered, and over-full events are dropped
// (with a counter) rather than blocking the gateway. Subscribe
// before Up to avoid missing early events.
func (s *Stack) Monitor() *Monitor {
	s.mu.Lock()
	if s.monitor == nil {
		s.monitor = newMonitor(s.id, s.name, s.engine.opts.MonitorBuffer)
	}
	mon := s.monitor
	s.mu.Unlock()
	return mon
}

// === lifecycle ===

// Up brings the stack up: validates, creates the network, then
// runs init containers in dependency order, then services in
// dependency order, waiting per-service for the configured probe
// (Readiness or Liveness first-success). Two-phase atomic: any
// failure rolls back to Down with all created resources removed.
//
// Idempotent: calling Up while already Up returns nil. Calling Up
// while in any non-Down state returns ErrIllegalStateTransition.
func (s *Stack) Up(ctx context.Context) error {
	if !s.stateCAS(StackDown, StackCreating) {
		// Allow re-entry from Up (idempotent yes-noop) and Failed
		// (caller may want to retry).
		switch s.State() {
		case StackUp:
			return nil
		case StackFailed:
			// Reset stopCh and try again.
			s.resetStopCh()
			s.state.Store(int32(StackCreating))
		default:
			return fmt.Errorf("Up: %w (current=%s)", ErrIllegalStateTransition, s.State())
		}
	}

	// Make sure we surface a sane state on any error.
	rollbackOnErr := func(failure error) error {
		s.state.Store(int32(StackFailed))
		// Best-effort teardown; don't shadow the original error.
		teardownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = s.tearDown(teardownCtx, true)
		return failure
	}

	// === pre-flight ===
	if err := s.validate(); err != nil {
		return rollbackOnErr(err)
	}

	// === phase: create network ===
	netName := "mkfst-stack-" + s.id
	netOpts := []CreateOption{
		Driver("bridge"),
		Option("com.docker.network.bridge.enable_icc", boolStr(!s.disableICC)),
	}
	if s.internalNetwork {
		netOpts = append(netOpts, Internal())
	}
	netw, err := Create(ctx, s.engine.cli, s.engine.opts.EngineID, s.id, s.name, netName, netOpts...)
	if err != nil {
		return rollbackOnErr(fmt.Errorf("create stack network: %w", err))
	}
	s.mu.Lock()
	s.network = netw
	s.mu.Unlock()

	// === phase: materialize secrets ===
	if err := s.materializeSecrets(ctx); err != nil {
		return rollbackOnErr(fmt.Errorf("materialize secrets: %w", err))
	}

	// === phase: create containers (init + services) ===
	s.state.Store(int32(StackCreating))
	order, err := s.topologicalOrder()
	if err != nil {
		return rollbackOnErr(err)
	}

	if err := s.createAll(ctx, order); err != nil {
		return rollbackOnErr(err)
	}
	s.state.Store(int32(StackCreated))

	// === phase: start (init first, then services in dep order) ===
	s.state.Store(int32(StackStarting))
	if err := s.startInitContainers(ctx, order); err != nil {
		return rollbackOnErr(err)
	}
	if err := s.startServices(ctx, order); err != nil {
		return rollbackOnErr(err)
	}

	// === phase: bring up gateway listeners ===
	if err := s.startGateway(ctx); err != nil {
		return rollbackOnErr(err)
	}

	// === phase: per-stack DNS resolver (best-effort) ===
	s.startDNSResolver(ctx)

	// === phase: liveness probes start running ===
	s.startLivenessLoops()

	// === phase: restart watchers ===
	s.startRestartWatchers()

	s.state.Store(int32(StackUp))
	return nil
}

// Down tears the stack down: stops gateway acceptors, drains
// in-flight connections (up to drainTimeout), stops services in
// reverse dependency order, removes all containers, removes the
// network, removes materialized secrets. Idempotent.
func (s *Stack) Down(ctx context.Context) error {
	switch s.State() {
	case StackDown:
		return nil
	case StackCreating, StackCreated, StackStarting, StackUp, StackFailed:
		// proceed
	case StackStopping:
		// already in progress; wait for it to finish.
		return s.waitState(ctx, StackDown)
	default:
		return fmt.Errorf("Down: %w (current=%s)", ErrIllegalStateTransition, s.State())
	}
	s.state.Store(int32(StackStopping))
	err := s.tearDown(ctx, false)
	s.state.Store(int32(StackDown))
	return err
}

// Status returns a read-only snapshot of the stack and its services.
func (s *Stack) Status(ctx context.Context) StackStatus {
	st := StackStatus{
		ID:    s.id,
		Name:  s.name,
		State: s.State(),
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st.Services = make(map[string]ServiceStatus, len(s.services))
	for name, svc := range s.services {
		ss := ServiceStatus{
			Name:     name,
			Image:    svc.image,
			Replicas: svc.Replicas(),
			Role:     svc.Role(),
		}
		if instances, ok := s.containers[name]; ok {
			for _, inst := range instances {
				ss.Containers = append(ss.Containers, ContainerStatus{
					ID:      inst.id,
					Name:    inst.name,
					Replica: inst.replica,
				})
			}
		}
		if probes, ok := s.probes[name]; ok {
			ss.Healthy = true
			for _, p := range probes {
				rs := p.snapshot()
				ss.Probes = append(ss.Probes, rs)
				if !rs.Healthy {
					ss.Healthy = false
				}
			}
		} else {
			ss.Healthy = true
		}
		st.Services[name] = ss
	}
	return st
}

// === read-only types ===

// StackStatus is the read-only view from Status().
type StackStatus struct {
	ID       string
	Name     string
	State    StackState
	Services map[string]ServiceStatus
}

// ServiceStatus is per-service runtime info.
type ServiceStatus struct {
	Name       string
	Image      string
	Replicas   int
	Role       string
	Containers []ContainerStatus
	Probes     []ProbeStatus
	Healthy    bool
}

// ContainerStatus is per-container info.
type ContainerStatus struct {
	ID      string
	Name    string
	Replica int
}

// === state helpers ===

func (s *Stack) stateCAS(from, to StackState) bool {
	return s.state.CompareAndSwap(int32(from), int32(to))
}

func (s *Stack) waitState(ctx context.Context, want StackState) error {
	const interval = 25 * time.Millisecond
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if s.State() == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (s *Stack) resetStopCh() {
	s.mu.Lock()
	s.stopCh = make(chan struct{})
	s.mu.Unlock()
}

// === pre-flight validation ===

func (s *Stack) validate() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.services) == 0 {
		return fmt.Errorf("%w: stack has no services", ErrInvalidConfig)
	}
	for _, svc := range s.services {
		if err := svc.validate(); err != nil {
			return err
		}
		// All declared dependencies must exist as services in the stack.
		for _, dep := range svc.depends {
			if _, ok := s.services[dep]; !ok {
				return fmt.Errorf("%w: service %q depends on unknown service %q",
					ErrInvalidConfig, svc.name, dep)
			}
		}
		// All declared init containers must exist and be RoleInit.
		for _, init := range svc.initBefore {
			dep, ok := s.services[init]
			if !ok {
				return fmt.Errorf("%w: service %q AfterInit references unknown service %q",
					ErrInvalidConfig, svc.name, init)
			}
			if dep.Role() != RoleInit {
				return fmt.Errorf("%w: service %q AfterInit references %q which is not an init container",
					ErrInvalidConfig, svc.name, init)
			}
		}
		// Secrets referenced must exist on the stack.
		for _, sr := range svc.secrets {
			if _, ok := s.secrets[sr.Name]; !ok {
				return fmt.Errorf("%w: service %q references unknown secret %q",
					ErrInvalidConfig, svc.name, sr.Name)
			}
		}
	}
	// Cycle detection on the dep graph.
	if err := s.detectDependencyCycle(); err != nil {
		return err
	}
	// Each ingress must point at an existing service + port.
	for _, ing := range s.ingress {
		svc, ok := s.services[ing.serviceName]
		if !ok {
			return fmt.Errorf("%w: ingress %q targets unknown service %q",
				ErrInvalidConfig, ing.name, ing.serviceName)
		}
		found := false
		for _, p := range svc.ports {
			if p.Port == ing.servicePort {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: ingress %q targets service %q port %d which the service doesn't declare",
				ErrInvalidConfig, ing.name, ing.serviceName, ing.servicePort)
		}
	}
	return nil
}

// === dependency / topological order ===

// detectDependencyCycle uses standard 3-color DFS.
func (s *Stack) detectDependencyCycle() error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var visit func(name string) error
	visit = func(name string) error {
		color[name] = gray
		stack = append(stack, name)
		svc := s.services[name]
		for _, dep := range svc.depends {
			switch color[dep] {
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			case gray:
				idx := 0
				for i, n := range stack {
					if n == dep {
						idx = i
						break
					}
				}
				cycle := append([]string{}, stack[idx:]...)
				cycle = append(cycle, dep)
				return fmt.Errorf("%w: dependency cycle: %v",
					ErrInvalidConfig, cycle)
			}
		}
		color[name] = black
		stack = stack[:len(stack)-1]
		return nil
	}
	for _, name := range s.order {
		if color[name] == white {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// topologicalOrder returns service names in dependency order
// (parents before dependents), with init containers grouped first
// in their own dependency order.
func (s *Stack) topologicalOrder() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	in := map[string]int{}
	for _, name := range s.order {
		in[name] = 0
	}
	for _, name := range s.order {
		for _, dep := range s.services[name].depends {
			in[name]++
			_ = dep
		}
		// AfterInit also contributes to the wait edge.
		for _, init := range s.services[name].initBefore {
			in[name]++
			_ = init
		}
	}
	// Kahn's algorithm with deterministic tie-breaker (sort).
	ready := []string{}
	for n, deg := range in {
		if deg == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	out := make([]string, 0, len(s.order))
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		for _, child := range s.order {
			deps := s.services[child].depends
			inits := s.services[child].initBefore
			for _, d := range deps {
				if d == n {
					in[child]--
				}
			}
			for _, d := range inits {
				if d == n {
					in[child]--
				}
			}
			if in[child] == 0 {
				// don't double-add
				already := false
				for _, x := range out {
					if x == child {
						already = true
						break
					}
				}
				if !already {
					alreadyReady := false
					for _, x := range ready {
						if x == child {
							alreadyReady = true
							break
						}
					}
					if !alreadyReady {
						ready = append(ready, child)
						sort.Strings(ready)
					}
				}
			}
		}
	}
	if len(out) != len(s.order) {
		return nil, fmt.Errorf("%w: dependency graph not a DAG", ErrInvalidConfig)
	}
	return out, nil
}

// === phase: create containers ===

// createAll creates the docker containers (without starting) for
// every service, in topological order. Sets s.containers and
// s.egress for each. Aborts on first error so the caller can
// rollback.
func (s *Stack) createAll(ctx context.Context, order []string) error {
	s.mu.Lock()
	netName := s.network.Name()
	engineID := s.engine.opts.EngineID
	stackID := s.id
	stackName := s.name
	s.mu.Unlock()

	for _, name := range order {
		s.mu.RLock()
		svc := s.services[name]
		s.mu.RUnlock()

		holder, err := s.compileServiceEgress(svc)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.egress[name] = holder
		s.mu.Unlock()

		instances := make([]containerInstance, 0, svc.Replicas())
		probes := make([]*replicaProbeState, 0, svc.Replicas())
		for r := 0; r < svc.Replicas(); r++ {
			cfg, host, netCfg, err := s.buildContainerConfig(svc, r, netName, engineID, stackID, stackName)
			if err != nil {
				return fmt.Errorf("build config for %s[%d]: %w", name, r, err)
			}
			ctrName := fmt.Sprintf("mkfst-%s-%s-%d", s.id, name, r)
			created, err := retryWithResult(ctx, RetryOpts{IsRetryable: IsRetryableDocker},
				func(ctx context.Context) (dockercontainer.CreateResponse, error) {
					return s.engine.cli.ContainerCreate(ctx, cfg, host, netCfg, nil, ctrName)
				},
			)
			if err != nil {
				return fmt.Errorf("ContainerCreate %s[%d]: %w", name, r, err)
			}
			inst := containerInstance{
				id:      created.ID,
				name:    ctrName,
				replica: r,
				role:    svc.Role(),
			}
			instances = append(instances, inst)
			probes = append(probes, newReplicaProbeState(name, r, created.ID))
		}
		s.mu.Lock()
		s.containers[name] = instances
		s.probes[name] = probes
		s.mu.Unlock()
	}
	return nil
}

// buildContainerConfig translates a Service + replica into the
// docker SDK config triple. Pure function — no side effects.
func (s *Stack) buildContainerConfig(svc *Service, replica int, netName, engineID, stackID, stackName string) (
	*dockercontainer.Config, *dockercontainer.HostConfig, *dockernetwork.NetworkingConfig, error,
) {
	envSlice := make([]string, 0, len(svc.env))
	for k, v := range svc.env {
		envSlice = append(envSlice, k+"="+v)
	}
	sort.Strings(envSlice) // deterministic ordering for testing

	labels := withServiceLabels(
		stackLabels(engineID, stackID, stackName, KindService),
		svc.name, replica, svc.Role(),
	)
	for k, v := range svc.extraLabels {
		labels[k] = v
	}

	cfg := &dockercontainer.Config{
		Image:      svc.image,
		Env:        envSlice,
		WorkingDir: svc.workDir,
		User:       svc.user,
		Labels:     labels,
		Hostname:   svc.name,
	}
	if len(svc.cmd) > 0 {
		cfg.Cmd = strslice.StrSlice(svc.cmd)
	}
	if len(svc.entrypoint) > 0 {
		cfg.Entrypoint = strslice.StrSlice(svc.entrypoint)
	}

	host := &dockercontainer.HostConfig{
		AutoRemove: false,
		CapAdd:     svc.capAdd,
		CapDrop:    svc.capDrop,
		Resources: dockercontainer.Resources{
			CPUShares:         svc.resources.CPUShares,
			Memory:            svc.resources.MemoryBytes,
			MemoryReservation: svc.resources.MemoryReservationBytes,
			PidsLimit:         ifNonZero(svc.resources.PidsLimit),
		},
	}
	if svc.resources.CPUPercent > 0 {
		// Translate percent to CFS quota with period 100000us (100ms).
		host.CPUPeriod = 100000
		host.CPUQuota = int64(svc.resources.CPUPercent * 1000)
	}

	// Mounts.
	for _, m := range svc.mounts {
		switch m.Type {
		case "volume":
			host.Mounts = append(host.Mounts, dockermount.Mount{
				Type: dockermount.TypeVolume, Source: m.Source, Target: m.Target,
				ReadOnly: m.ReadOnly,
			})
		case "bind":
			host.Mounts = append(host.Mounts, dockermount.Mount{
				Type: dockermount.TypeBind, Source: m.Source, Target: m.Target,
				ReadOnly: m.ReadOnly,
			})
		}
	}
	// Secret mounts (tmpfs-backed bind from the secret directory
	// the Stack materialized at Up).
	for _, sec := range svc.secrets {
		hostFile, ok := s.secretHostPath(sec.Name)
		if !ok {
			return nil, nil, nil, fmt.Errorf("secret %q not materialized", sec.Name)
		}
		host.Mounts = append(host.Mounts, dockermount.Mount{
			Type:     dockermount.TypeBind,
			Source:   hostFile,
			Target:   sec.MountPath,
			ReadOnly: true,
		})
	}

	// Expose + publish ports. Each declared Service.Port is:
	//  - Exposed on the container so the daemon's port machinery
	//    routes to it.
	//  - Published to 127.0.0.1:0 (loopback ephemeral) so the
	//    in-process gateway can reach it cross-platform without
	//    requiring host-routable container IPs (which don't work
	//    on rootless / Mac / Windows).
	if len(svc.ports) > 0 {
		cfg.ExposedPorts = nat.PortSet{}
		host.PortBindings = nat.PortMap{}
		for _, p := range svc.ports {
			natPort, err := nat.NewPort(p.Protocol, strconvI(p.Port))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("nat.NewPort: %w", err)
			}
			cfg.ExposedPorts[natPort] = struct{}{}
			host.PortBindings[natPort] = []nat.PortBinding{{
				HostIP:   "127.0.0.1",
				HostPort: "", // empty = ephemeral
			}}
		}
	}

	// Networking: connect to the stack network with the service
	// name as primary alias. For replicated services, every replica
	// shares the alias so Docker DNS round-robins.
	endpoint := &dockernetwork.EndpointSettings{
		NetworkID: "", // resolved by name
		Aliases:   []string{svc.name},
	}
	netCfg := &dockernetwork.NetworkingConfig{
		EndpointsConfig: map[string]*dockernetwork.EndpointSettings{
			netName: endpoint,
		},
	}
	host.NetworkMode = dockercontainer.NetworkMode(netName)

	// Stop timeout.
	if svc.stopTimeout > 0 {
		t := int(svc.stopTimeout.Seconds())
		cfg.StopTimeout = &t
	}

	return cfg, host, netCfg, nil
}

// === phase: start init containers ===

func (s *Stack) startInitContainers(ctx context.Context, order []string) error {
	for _, name := range order {
		s.mu.RLock()
		svc := s.services[name]
		s.mu.RUnlock()
		if svc.Role() != RoleInit {
			continue
		}
		// Init containers always have replicas=1 (validated).
		s.mu.RLock()
		insts := s.containers[name]
		s.mu.RUnlock()
		if len(insts) == 0 {
			continue
		}
		inst := insts[0]
		if err := s.runInitToCompletion(ctx, svc, inst); err != nil {
			return fmt.Errorf("init container %q: %w", name, err)
		}
	}
	return nil
}

func (s *Stack) runInitToCompletion(ctx context.Context, svc *Service, inst containerInstance) error {
	if err := s.fireHooks(ctx, svc.preStart, svc, inst); err != nil {
		return fmt.Errorf("preStart: %w", err)
	}
	err := retry(ctx, RetryOpts{IsRetryable: IsRetryableDocker}, func(ctx context.Context) error {
		return s.engine.cli.ContainerStart(ctx, inst.id, dockercontainer.StartOptions{})
	})
	if err != nil {
		return fmt.Errorf("ContainerStart: %w", err)
	}
	if err := s.fireHooks(ctx, svc.postStart, svc, inst); err != nil {
		return fmt.Errorf("postStart: %w", err)
	}
	// Wait for the init container to exit.
	statusCh, errCh := s.engine.cli.ContainerWait(ctx, inst.id, dockercontainer.WaitConditionNotRunning)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("wait: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			return fmt.Errorf("init container exited with code %d", status.StatusCode)
		}
		if status.Error != nil {
			return fmt.Errorf("init container error: %s", status.Error.Message)
		}
	}
	return nil
}

// === phase: start services ===

func (s *Stack) startServices(ctx context.Context, order []string) error {
	for _, name := range order {
		s.mu.RLock()
		svc := s.services[name]
		insts := s.containers[name]
		s.mu.RUnlock()
		if svc.Role() == RoleInit {
			continue // already done
		}
		// Wait for declared dependencies' probes (or "started" if no
		// probe).
		if err := s.waitForDeps(ctx, svc); err != nil {
			return fmt.Errorf("wait deps for %q: %w", name, err)
		}
		// Fire preStart hooks for every replica.
		for _, inst := range insts {
			if err := s.fireHooks(ctx, svc.preStart, svc, inst); err != nil {
				return fmt.Errorf("preStart %s[%d]: %w", name, inst.replica, err)
			}
		}
		// Start every replica in parallel.
		startCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		grp, gCtx := errgroup.WithContext(startCtx)
		for _, inst := range insts {
			inst := inst
			grp.Go(func() error {
				return retry(gCtx, RetryOpts{IsRetryable: IsRetryableDocker}, func(ctx context.Context) error {
					return s.engine.cli.ContainerStart(ctx, inst.id, dockercontainer.StartOptions{})
				})
			})
		}
		if err := grp.Wait(); err != nil {
			cancel()
			return fmt.Errorf("ContainerStart %s: %w", name, err)
		}
		cancel()

		// Fire postStart hooks.
		for _, inst := range insts {
			if err := s.fireHooks(ctx, svc.postStart, svc, inst); err != nil {
				return fmt.Errorf("postStart %s[%d]: %w", name, inst.replica, err)
			}
		}

		// Run readiness/first-success probe gating.
		if svc.probe != nil {
			if err := s.gateOnFirstSuccess(ctx, svc); err != nil {
				return fmt.Errorf("probe gate %q: %w", name, err)
			}
		} else {
			// No probe configured — mark every replica healthy so
			// the gateway's load balancer routes to them. The
			// service is "healthy" by virtue of the container
			// reaching Started state (which we just confirmed).
			s.mu.RLock()
			probes := s.probes[name]
			s.mu.RUnlock()
			for _, p := range probes {
				p.markHealthy()
			}
		}
	}
	return nil
}

func (s *Stack) waitForDeps(ctx context.Context, svc *Service) error {
	for _, dep := range svc.depends {
		if err := s.waitForServiceReady(ctx, dep); err != nil {
			return fmt.Errorf("dep %q: %w", dep, err)
		}
	}
	return nil
}

// waitForServiceReady blocks until depName's probes report healthy.
// If depName has no probe, "ready" means containers reached running
// state (which is already true if we started them in topo order).
func (s *Stack) waitForServiceReady(ctx context.Context, depName string) error {
	s.mu.RLock()
	probes := s.probes[depName]
	s.mu.RUnlock()
	if len(probes) == 0 {
		return nil
	}
	for _, p := range probes {
		if err := p.waitHealthy(ctx); err != nil {
			return err
		}
	}
	return nil
}

// gateOnFirstSuccess runs the probe synchronously per replica until
// every replica has at least one success. Used at startup before
// dependent services may start. For Liveness mode, the continuous
// loop is later started by startLivenessLoops.
func (s *Stack) gateOnFirstSuccess(ctx context.Context, svc *Service) error {
	s.mu.RLock()
	probes := s.probes[svc.name]
	insts := s.containers[svc.name]
	s.mu.RUnlock()
	for i, p := range probes {
		if err := s.engine.probeSched.runUntilSuccess(ctx, s, svc, insts[i], p); err != nil {
			return err
		}
	}
	return nil
}

// startLivenessLoops launches the continuous probing goroutines
// for every Liveness-mode service. Each goroutine is tracked via
// s.bg so Down can wait deterministically.
func (s *Stack) startLivenessLoops() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name, svc := range s.services {
		if svc.probe == nil || svc.probeMode != ProbeLiveness {
			continue
		}
		probes := s.probes[name]
		insts := s.containers[name]
		for i, p := range probes {
			i, p := i, p
			inst := insts[i]
			svc := svc
			s.bg.Go(func() error {
				s.engine.probeSched.runLiveness(s, svc, inst, p, s.stopCh)
				return nil
			})
		}
	}
}

// === DNS resolver ===

// startDNSResolver attempts to bind a per-stack DNS server on the
// bridge gateway IP. Best-effort: failure (rootless port-53 bind,
// missing privileges, gateway IP not bind-eligible on Mac/Win) is
// logged via OnError-style channel and the stack continues without
// DNS-layer egress enforcement.
func (s *Stack) startDNSResolver(ctx context.Context) {
	netw := s.network
	if netw == nil {
		return
	}
	insp, err := netw.Inspect(ctx)
	if err != nil {
		return
	}
	var gateway string
	for _, ipam := range insp.IPAM.Config {
		if ipam.Gateway != "" {
			gateway = ipam.Gateway
			break
		}
	}
	if gateway == "" {
		return
	}
	r := newDNSResolver(s)
	if err := r.start(gateway); err != nil {
		// Bind failed (port 53 needs CAP_NET_BIND_SERVICE on Linux,
		// or gateway IP isn't bindable on Mac/Win). Continue without
		// DNS — emit a monitor event so it's visible.
		if s.monitor != nil {
			s.monitor.emit(Event{
				Kind:    EventConnectionDenied,
				At:      time.Now(),
				Service: "[dns]",
				Error:   err.Error(),
			})
		}
		return
	}
	s.mu.Lock()
	s.dns = r
	s.mu.Unlock()
}

// AllowsEgress reports whether the named service is permitted by
// its egress policy to resolve / connect to `target` (a hostname or
// IP). Useful for application code that wants to gate outbound
// calls without relying on the DNS-layer enforcement (which may not
// be active on every platform).
func (s *Stack) AllowsEgress(serviceName, target string) bool {
	s.mu.RLock()
	holder, ok := s.egress[serviceName]
	s.mu.RUnlock()
	if !ok || holder == nil {
		return true
	}
	c := holder.load()
	if c == nil {
		return true
	}
	if ip := net.ParseIP(target); ip != nil {
		return c.AllowsIP(ip)
	}
	return c.AllowsName(target)
}

// === gateway ===

func (s *Stack) startGateway(ctx context.Context) error {
	s.mu.Lock()
	hasIngress := len(s.ingress) > 0
	mon := s.monitor
	if hasIngress && mon == nil {
		mon = newMonitor(s.id, s.name, s.engine.opts.MonitorBuffer)
		s.monitor = mon
	}
	s.mu.Unlock()

	if !hasIngress {
		return nil
	}
	gw, err := newGateway(s, mon)
	if err != nil {
		return err
	}
	// gw.start calls back into the stack (containerByService etc.)
	// to resolve backends. Release s.mu before calling so the
	// callbacks can acquire it.
	if err := gw.start(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.gateway = gw
	s.mu.Unlock()
	return nil
}

// === phase: tear down ===

// tearDown is the worker for Down. preserveState=true means the
// caller will set the final state itself (used by rollback during
// Up failure to leave Failed visible until cleanup completes).
func (s *Stack) tearDown(ctx context.Context, preserveState bool) error {
	// 1. Stop accepting new ingress connections, drain in-flight.
	s.mu.RLock()
	gw := s.gateway
	dns := s.dns
	s.mu.RUnlock()
	if gw != nil {
		gw.stop(ctx, s.drainTimeout)
	}
	if dns != nil {
		dns.stop()
	}

	// 2. Signal background goroutines (liveness loops) to exit.
	s.mu.Lock()
	close(s.stopCh)
	s.mu.Unlock()
	_ = s.bg.Wait()

	// 3. Stop containers in reverse order. PreStop hooks first.
	s.mu.RLock()
	order, _ := s.topologicalOrderLocked()
	s.mu.RUnlock()
	for i := len(order) - 1; i >= 0; i-- {
		name := order[i]
		s.mu.RLock()
		svc := s.services[name]
		insts := s.containers[name]
		s.mu.RUnlock()
		for _, inst := range insts {
			_ = s.fireHooks(ctx, svc.preStop, svc, inst) // best-effort
			t := svc.stopTimeout
			if t <= 0 {
				t = s.stopTimeoutDefault
			}
			seconds := int(t.Seconds())
			_ = retry(ctx, RetryOpts{IsRetryable: IsRetryableDocker}, func(ctx context.Context) error {
				err := s.engine.cli.ContainerStop(ctx, inst.id, dockercontainer.StopOptions{Timeout: &seconds})
				if err != nil && isNotFoundError(err) {
					return nil
				}
				return err
			})
		}
	}

	// 4. Remove containers.
	for _, name := range order {
		s.mu.RLock()
		insts := s.containers[name]
		s.mu.RUnlock()
		for _, inst := range insts {
			_ = retry(ctx, RetryOpts{IsRetryable: IsRetryableDocker}, func(ctx context.Context) error {
				err := s.engine.cli.ContainerRemove(ctx, inst.id, dockercontainer.RemoveOptions{Force: true, RemoveVolumes: false})
				if err != nil && isNotFoundError(err) {
					return nil
				}
				return err
			})
		}
	}

	// 5. Remove the network.
	s.mu.Lock()
	netw := s.network
	mon := s.monitor
	s.network = nil
	s.containers = map[string][]containerInstance{}
	s.probes = map[string][]*replicaProbeState{}
	s.egress = map[string]*egressHolder{}
	s.gateway = nil
	s.dns = nil
	s.monitor = nil
	s.mu.Unlock()
	if netw != nil {
		_ = netw.Remove(ctx)
	}
	// Stop the monitor — closes both the in-channel and joins the
	// serializer goroutine. Subscribers see the out-channel close
	// as the signal that the stack is gone.
	if mon != nil {
		mon.stop()
	}

	// 6. Remove materialized secrets.
	_ = s.cleanupSecrets()

	if !preserveState {
		s.state.Store(int32(StackDown))
	}
	return nil
}

// topologicalOrderLocked is a Stack.mu-held variant. Caller holds
// the read lock. We use the same algorithm as topologicalOrder but
// don't take the lock again.
func (s *Stack) topologicalOrderLocked() ([]string, error) {
	in := map[string]int{}
	for _, name := range s.order {
		in[name] = 0
	}
	for _, name := range s.order {
		for _, dep := range s.services[name].depends {
			in[name]++
			_ = dep
		}
		for _, init := range s.services[name].initBefore {
			in[name]++
			_ = init
		}
	}
	ready := []string{}
	for n, deg := range in {
		if deg == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	out := make([]string, 0, len(s.order))
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		for _, child := range s.order {
			deps := s.services[child].depends
			inits := s.services[child].initBefore
			for _, d := range deps {
				if d == n {
					in[child]--
				}
			}
			for _, d := range inits {
				if d == n {
					in[child]--
				}
			}
			if in[child] == 0 {
				already := false
				for _, x := range out {
					if x == child {
						already = true
						break
					}
				}
				if !already {
					alreadyReady := false
					for _, x := range ready {
						if x == child {
							alreadyReady = true
							break
						}
					}
					if !alreadyReady {
						ready = append(ready, child)
						sort.Strings(ready)
					}
				}
			}
		}
	}
	if len(out) != len(s.order) {
		return out, fmt.Errorf("not a DAG")
	}
	return out, nil
}

// === hooks ===

func (s *Stack) fireHooks(ctx context.Context, hooks []Hook, svc *Service, inst containerInstance) error {
	if len(hooks) == 0 {
		return nil
	}
	hctx := HookCtx{
		StackID:     s.id,
		StackName:   s.name,
		ServiceName: svc.name,
		ContainerID: inst.id,
		Replica:     inst.replica,
		Stop:        s.stopCh,
	}
	_ = ctx
	for _, h := range hooks {
		if err := h(hctx); err != nil {
			return err
		}
	}
	return nil
}

// === egress compilation ===

func (s *Stack) compileServiceEgress(svc *Service) (*egressHolder, error) {
	c, err := compileEgress(svc.egress)
	if err != nil {
		return nil, err
	}
	h := &egressHolder{}
	h.store(c)
	return h, nil
}

// === misc helpers ===

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func ifNonZero(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

// containerByService returns a copy of the in-memory container list
// for a service. Used by the gateway's load balancer.
func (s *Stack) containerByService(name string) []containerInstance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	insts := s.containers[name]
	out := make([]containerInstance, len(insts))
	copy(out, insts)
	return out
}

// probesByService returns the per-replica probe state for a service.
func (s *Stack) probesByService(name string) []*replicaProbeState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	probes := s.probes[name]
	out := make([]*replicaProbeState, len(probes))
	copy(out, probes)
	return out
}

// === forward declarations satisfied by other files ===
// secret materialization → secrets.go
// gateway / monitor      → gateway.go, monitor.go
// probe scheduler        → probe_scheduler.go
//
// This file deliberately avoids depending on the dockerprov package
// to keep the Stack type a leaf in the providers/docker dep graph.

// ensure unused-import safety while files come up incrementally
var _ = client.NewClientWithOpts
