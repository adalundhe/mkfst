package network

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Service is a definition of one container-backed program in a Stack.
// Build via Stack.AddService(name, opts...). The Service is pure data
// at definition time; runtime state lives on the Stack.
type Service struct {
	mu sync.RWMutex

	name     string
	image    string
	cmd      []string
	entrypoint []string
	env      map[string]string
	workDir  string
	user     string
	ports    []ServicePort
	mounts   []ServiceMount
	depends  []string

	// Replicas is the number of identical container instances that
	// run for this service. Default 1. When >1, the service is
	// reachable by name and Docker DNS round-robins; mkfst's
	// gateway also load-balances across replicas with health
	// awareness.
	replicas int

	probe     *Probe
	probeMode ProbeMode

	restart RestartPolicy

	resources ResourceLimits

	role string

	// Hooks run at lifecycle phases. preStart fires after Create
	// but before Start (image is hydrated). postStart fires after
	// Start succeeds. preStop fires before container Stop.
	preStart  []Hook
	postStart []Hook
	preStop   []Hook

	// secrets are referenced by name; the Stack resolves to the
	// concrete tmpfs/named-pipe injection at Up time.
	secrets []SecretRef

	// egress controls what external endpoints this service may
	// reach. Nil = unrestricted.
	egress *EgressPolicy

	// initBefore lists init-container service names that must run
	// to completion before this service starts. Init containers
	// share the stack's network and are removed after they exit
	// successfully.
	initBefore []string

	// labels merged into the container's docker labels.
	extraLabels map[string]string

	// Linux capabilities, security options, etc., kept minimal for
	// v1 — callers wanting full control can drop down to the
	// underlying providers/docker.RunOption surface (escape hatch
	// at Stack.AddRawService).
	capAdd  []string
	capDrop []string

	// stopTimeout is how long the daemon waits between SIGTERM and
	// SIGKILL during Stop. Default 10s.
	stopTimeout time.Duration
}

// ServicePort declares a container-internal port. Internal ports are
// NOT host-published — that's the gateway's job via Ingress. This
// struct tells the gateway which container port to forward to when
// an ingress is attached.
type ServicePort struct {
	// Port is the container's listening port.
	Port int
	// Protocol is "tcp" (default), "udp", or "sctp".
	Protocol string
}

// ServiceMount is a per-service volume mount (named volume or
// bind-mount). Bind mounts are discouraged but supported for
// scenarios where a host path must be exposed.
type ServiceMount struct {
	// Type is "volume" or "bind".
	Type string
	// Source is the volume name (Type=volume) or absolute host
	// path (Type=bind).
	Source string
	// Target is the container path.
	Target string
	// ReadOnly mounts the source read-only.
	ReadOnly bool
}

// SecretRef is a service's reference to a Secret defined on the Stack
// via Stack.AddSecret. The Stack materializes the secret as a
// tmpfs-backed file (Linux) or named-pipe (Windows) and mounts it
// at MountPath inside the container; the Service reads from
// MountPath at runtime.
type SecretRef struct {
	Name      string // matches Stack.AddSecret(name, ...)
	MountPath string // path inside the container (e.g. "/run/secrets/db_password")
	Mode      uint32 // file permission (default 0400)
}

// Hook runs at a specific service lifecycle moment. ctx is bounded
// by the Stack's operation deadline. containerID identifies the
// concrete container (mostly relevant for replicated services).
type Hook func(ctx HookCtx) error

// HookCtx carries everything a hook might need. Decoupled from the
// raw context.Context so the field set can grow without breaking
// existing hooks.
type HookCtx struct {
	StackID     string
	StackName   string
	ServiceName string
	ContainerID string
	Replica     int
	Stop        <-chan struct{} // closed when the stack is shutting down
}

// === restart policy ===

// RestartPolicyKind enumerates the auto-restart modes.
type RestartPolicyKind int

const (
	// RestartNever leaves a stopped container stopped. Mkfst will
	// surface the failure via Stack.Status; the user decides what
	// to do.
	RestartNever RestartPolicyKind = iota
	// RestartOnFailure restarts only on non-zero exit; bounded by
	// MaxAttempts.
	RestartOnFailure
	// RestartAlways restarts unconditionally; bounded by
	// MaxAttempts.
	RestartAlways
	// RestartUnlessStopped restarts unconditionally except when
	// the user explicitly stopped the service via Stack.StopService.
	RestartUnlessStopped
)

// RestartPolicy controls how mkfst (NOT the docker daemon's restart
// policy) handles container exits. We don't use the daemon's
// restart-policy because:
//
//   - It bypasses our probe + state machine — a container could
//     restart-loop while we think the service is healthy.
//   - It doesn't surface restart events through our monitor channel.
//   - It's not consistent across rootful / rootless / Docker Desktop.
//
// mkfst's reaper observes container state and applies the policy
// in-process.
type RestartPolicy struct {
	Kind RestartPolicyKind
	// MaxAttempts caps total restart attempts before the service
	// is marked permanently failed. 0 = unbounded.
	MaxAttempts int
	// Backoff returns the delay before attempt N (1-indexed). nil
	// uses defaultRestartBackoff (full-jitter exponential, capped
	// at 30s).
	Backoff func(attempt int) time.Duration
}

// === resource limits ===

// ResourceLimits caps per-container resource usage. All zero =
// unlimited. mkfst applies these via docker host config.
type ResourceLimits struct {
	// CPUShares is the relative CPU weight (default 1024). Used by
	// CFS share scheduling — shares only matter under contention.
	CPUShares int64
	// CPUPercent caps absolute CPU time (0..100; 100 = one full
	// core). Translated to CFS quota/period.
	CPUPercent float64
	// MemoryBytes caps RSS. OOM-killer enforces; container is
	// killed if it tries to exceed this.
	MemoryBytes int64
	// MemoryReservationBytes is a soft limit — under host pressure
	// the kernel reclaims down to this number first.
	MemoryReservationBytes int64
	// PidsLimit caps the number of processes/threads. Defends
	// against fork bombs.
	PidsLimit int64
}

// === service options ===

// ServiceOption is the functional-option form for AddService.
type ServiceOption func(*Service)

// Image sets the container image (REQUIRED).
func Image(image string) ServiceOption {
	return func(s *Service) { s.image = image }
}

// Cmd sets the container command (overrides the image's default CMD).
func Cmd(args ...string) ServiceOption {
	return func(s *Service) {
		s.cmd = append([]string(nil), args...)
	}
}

// Entrypoint overrides the image's ENTRYPOINT.
func Entrypoint(args ...string) ServiceOption {
	return func(s *Service) {
		s.entrypoint = append([]string(nil), args...)
	}
}

// Env adds one environment variable.
func Env(key, value string) ServiceOption {
	return func(s *Service) {
		if s.env == nil {
			s.env = map[string]string{}
		}
		s.env[key] = value
	}
}

// EnvMap adds many environment variables at once.
func EnvMap(vars map[string]string) ServiceOption {
	return func(s *Service) {
		if s.env == nil {
			s.env = map[string]string{}
		}
		for k, v := range vars {
			s.env[k] = v
		}
	}
}

// WorkDir sets the container's working directory.
func WorkDir(dir string) ServiceOption {
	return func(s *Service) { s.workDir = dir }
}

// User sets the UID:GID (or username) the entrypoint runs as.
func User(user string) ServiceOption {
	return func(s *Service) { s.user = user }
}

// Port declares an internal container port. Internal ports are
// reachable from other services in the same stack via the service
// name. To expose the port to outside the stack, attach an Ingress
// to it via Stack.Ingress.
func Port(port int, protocol ...string) ServiceOption {
	proto := "tcp"
	if len(protocol) > 0 && protocol[0] != "" {
		proto = protocol[0]
	}
	return func(s *Service) {
		s.ports = append(s.ports, ServicePort{Port: port, Protocol: proto})
	}
}

// Volume mounts a named docker volume into the container.
func Volume(name, target string, readOnly ...bool) ServiceOption {
	ro := false
	if len(readOnly) > 0 {
		ro = readOnly[0]
	}
	return func(s *Service) {
		s.mounts = append(s.mounts, ServiceMount{
			Type: "volume", Source: name, Target: target, ReadOnly: ro,
		})
	}
}

// Bind mounts a host path into the container. Discouraged — bind
// mounts couple stack lifecycle to host filesystem state — but
// supported for cases where the host path is genuinely the data
// source (config files, sockets).
func Bind(hostPath, target string, readOnly ...bool) ServiceOption {
	ro := false
	if len(readOnly) > 0 {
		ro = readOnly[0]
	}
	return func(s *Service) {
		s.mounts = append(s.mounts, ServiceMount{
			Type: "bind", Source: hostPath, Target: target, ReadOnly: ro,
		})
	}
}

// DependsOn declares one or more services this service waits for.
// "Wait for" means: dependency's probe must pass (or, if no probe
// is set, the dependency's container must reach Started state)
// before this service starts.
func DependsOn(services ...string) ServiceOption {
	return func(s *Service) {
		s.depends = append(s.depends, services...)
	}
}

// Replicas sets the number of container instances for this service.
// Multiple replicas are reachable by the same service name via DNS
// round-robin (Docker embedded DNS) and load-balanced by the
// gateway with health awareness.
func Replicas(n int) ServiceOption {
	return func(s *Service) {
		if n < 1 {
			n = 1
		}
		s.replicas = n
	}
}

// WithProbe attaches a probe. mode is Readiness (one-shot, gates
// dependents) or Liveness (continuous, restarts container on
// failure). Mutually exclusive — most-recent call wins.
func WithProbe(p *Probe, mode ProbeMode) ServiceOption {
	return func(s *Service) {
		s.probe = p
		s.probeMode = mode
	}
}

// Restart sets the restart policy for this service.
func Restart(p RestartPolicy) ServiceOption {
	return func(s *Service) { s.restart = p }
}

// Resources sets per-container resource limits.
func Resources(r ResourceLimits) ServiceOption {
	return func(s *Service) { s.resources = r }
}

// PreStart registers a hook to run after Create, before Start.
func PreStart(h Hook) ServiceOption {
	return func(s *Service) { s.preStart = append(s.preStart, h) }
}

// PostStart registers a hook to run after Start succeeds.
func PostStart(h Hook) ServiceOption {
	return func(s *Service) { s.postStart = append(s.postStart, h) }
}

// PreStop registers a hook to run before container Stop.
func PreStop(h Hook) ServiceOption {
	return func(s *Service) { s.preStop = append(s.preStop, h) }
}

// UseSecret references a stack-level secret by name and mounts it at
// mountPath inside the container.
func UseSecret(name, mountPath string, mode ...uint32) ServiceOption {
	m := uint32(0o400)
	if len(mode) > 0 {
		m = mode[0]
	}
	return func(s *Service) {
		s.secrets = append(s.secrets, SecretRef{
			Name: name, MountPath: mountPath, Mode: m,
		})
	}
}

// Egress sets the egress policy for this service. nil clears it.
func Egress(p *EgressPolicy) ServiceOption {
	return func(s *Service) { s.egress = p }
}

// InitContainer marks this service as an init container — runs
// once to completion before any service that lists it in
// AfterInit(...) starts.
func InitContainer() ServiceOption {
	return func(s *Service) { s.role = RoleInit }
}

// AfterInit declares init containers that must run to completion
// before this service starts. The named services must themselves
// be marked InitContainer().
func AfterInit(initServiceNames ...string) ServiceOption {
	return func(s *Service) {
		s.initBefore = append(s.initBefore, initServiceNames...)
	}
}

// CapAdd grants a Linux capability (e.g. "NET_ADMIN").
func CapAdd(cap string) ServiceOption {
	return func(s *Service) { s.capAdd = append(s.capAdd, cap) }
}

// CapDrop revokes a Linux capability.
func CapDrop(cap string) ServiceOption {
	return func(s *Service) { s.capDrop = append(s.capDrop, cap) }
}

// StopTimeout overrides the SIGTERM-to-SIGKILL grace period.
func StopTimeout(d time.Duration) ServiceOption {
	return func(s *Service) { s.stopTimeout = d }
}

// ServiceLabel attaches one extra docker label to the container.
func ServiceLabel(key, value string) ServiceOption {
	return func(s *Service) {
		if s.extraLabels == nil {
			s.extraLabels = map[string]string{}
		}
		s.extraLabels[key] = value
	}
}

// === read-only accessors (for engine internals + tests) ===

// Name returns the service's name within the stack.
func (s *Service) Name() string { return s.name }

// Image returns the container image.
func (s *Service) Image() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.image
}

// Replicas returns the configured replica count.
func (s *Service) Replicas() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.replicas <= 0 {
		return 1
	}
	return s.replicas
}

// Probe returns the configured probe (may be nil).
func (s *Service) Probe() (*Probe, ProbeMode) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.probe, s.probeMode
}

// DependsOn returns the (immutable) list of service names this
// service depends on.
func (s *Service) DependsOn() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.depends...)
}

// Role returns RoleService / RoleInit / RoleSidecar.
func (s *Service) Role() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.role == "" {
		return RoleService
	}
	return s.role
}

// === validation ===

// validate checks the service in isolation (no cross-service
// concerns — the Stack does the cross-service validation). Returns
// the first error or nil.
func (s *Service) validate() error {
	if s.name == "" {
		return errors.New("service: name is empty")
	}
	if s.image == "" {
		return fmt.Errorf("service %q: %w: image is required", s.name, ErrInvalidConfig)
	}
	for _, p := range s.ports {
		if p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("service %q: %w: invalid port %d", s.name, ErrInvalidConfig, p.Port)
		}
		switch p.Protocol {
		case "tcp", "udp", "sctp":
		default:
			return fmt.Errorf("service %q: %w: unknown protocol %q", s.name, ErrInvalidConfig, p.Protocol)
		}
	}
	for _, m := range s.mounts {
		if m.Target == "" {
			return fmt.Errorf("service %q: %w: mount missing target", s.name, ErrInvalidConfig)
		}
		if m.Source == "" {
			return fmt.Errorf("service %q: %w: mount missing source", s.name, ErrInvalidConfig)
		}
		switch m.Type {
		case "volume", "bind":
		default:
			return fmt.Errorf("service %q: %w: unknown mount type %q", s.name, ErrInvalidConfig, m.Type)
		}
	}
	for _, sec := range s.secrets {
		if sec.Name == "" || sec.MountPath == "" {
			return fmt.Errorf("service %q: %w: secret ref missing name or mount path", s.name, ErrInvalidConfig)
		}
	}
	if s.probe != nil {
		if err := s.probe.validate(); err != nil {
			return fmt.Errorf("service %q: probe: %w", s.name, err)
		}
		switch s.probeMode {
		case ProbeReadiness, ProbeLiveness:
		default:
			return fmt.Errorf("service %q: %w: invalid probe mode", s.name, ErrInvalidConfig)
		}
	}
	if s.replicas < 0 {
		return fmt.Errorf("service %q: %w: replicas must be ≥ 1", s.name, ErrInvalidConfig)
	}
	if s.role != "" && s.role != RoleService && s.role != RoleInit && s.role != RoleSidecar {
		return fmt.Errorf("service %q: %w: invalid role %q", s.name, ErrInvalidConfig, s.role)
	}
	if s.role == RoleInit && s.replicas > 1 {
		return fmt.Errorf("service %q: %w: init containers must have replicas=1", s.name, ErrInvalidConfig)
	}
	return nil
}

// dependsOnSorted returns DependsOn in stable sorted order — handy
// for deterministic topological sort and cycle messages.
func (s *Service) dependsOnSorted() []string {
	d := s.DependsOn()
	sort.Strings(d)
	return d
}
