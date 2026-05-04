package docker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// RunOption mutates the assembled run request just before submission.
type RunOption func(*runState)

// runState is the accumulator for RunOption mutations. Translated to the
// SDK's create/start args inside Client.Run.
type runState struct {
	config   *container.Config
	host     *container.HostConfig
	network  *network.NetworkingConfig
	platform *ocispec.Platform
	name     string
	detach   bool
	wait     bool

	// prestart hooks fire after ContainerCreate but BEFORE
	// ContainerStart. The container's rootfs exists and is writable
	// via CopyToContainer at this point, but the entrypoint hasn't
	// run yet — the right moment to hydrate VFS data into a target
	// path so the entrypoint sees it ready.
	prestart []func(ctx context.Context, c *Client, containerID string) error
	// poststart hooks fire after ContainerStart succeeds. Used to
	// start anything that needs the container to actually be running
	// (e.g. sync goroutines that immediately exec into it).
	poststart []func(ctx context.Context, c *Client, containerID string) error
}

func newRunState(image string) *runState {
	return &runState{
		config: &container.Config{
			Image: image,
		},
		host: &container.HostConfig{
			LogConfig: container.LogConfig{},
		},
		network: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{},
		},
		detach: true, // default: framework code typically wants to fire-and-move-on
	}
}

// RunResult is returned by Run.
//
//   - ContainerID is always populated on success (i.e. the container was
//     created and started).
//   - ExitCode is meaningful only when the run was synchronous (Detach=false
//     or WaitForExit() was set). Otherwise it's 0 and should be ignored.
type RunResult struct {
	ContainerID string
	ExitCode    int
}

// Run creates and starts a container. By default it returns as soon as the
// container is running (detached). Pass WaitForExit() to block until the
// container exits and surface the exit code on the result.
//
// image is the image to run, in standard reference form ("alpine:3.19",
// "ghcr.io/owner/img:sha-abc"). The image must already be present locally —
// pull it first via Client.Pull if needed.
func (c *Client) Run(ctx context.Context, image string, opts ...RunOption) (*RunResult, error) {
	state := newRunState(image)
	for _, opt := range opts {
		opt(state)
	}

	created, err := c.api.ContainerCreate(ctx, state.config, state.host, state.network, state.platform, state.name)
	if err != nil {
		return nil, fmt.Errorf("docker.Run: create: %w", err)
	}

	// Pre-start hooks: hydrate VFS data into a target path, etc.
	// Fires after Create (rootfs exists) but before Start (so the
	// entrypoint sees the data ready). Failures tear down the
	// container so we don't leak.
	for _, hook := range state.prestart {
		if err := hook(ctx, c, created.ID); err != nil {
			_ = c.api.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
			return nil, fmt.Errorf("docker.Run: prestart: %w", err)
		}
	}

	if err := c.api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// Best-effort cleanup of the half-created container.
		_ = c.api.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("docker.Run: start: %w", err)
	}

	// Post-start hooks: things that need the container actually
	// running (sync goroutines, etc).
	for _, hook := range state.poststart {
		if err := hook(ctx, c, created.ID); err != nil {
			_ = c.api.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
			return nil, fmt.Errorf("docker.Run: poststart: %w", err)
		}
	}

	result := &RunResult{ContainerID: created.ID}
	if state.wait || !state.detach {
		statusCh, errCh := c.api.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
		select {
		case err := <-errCh:
			return result, fmt.Errorf("docker.Run: wait: %w", err)
		case status := <-statusCh:
			result.ExitCode = int(status.StatusCode)
			if status.Error != nil {
				return result, fmt.Errorf("docker.Run: container error: %s", status.Error.Message)
			}
			return result, nil
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	return result, nil
}

// === Identity & lifecycle ===

// Name sets a human-readable name for the container. Without this, the
// daemon assigns a random name like "boring_einstein".
func Name(name string) RunOption {
	return func(s *runState) { s.name = name }
}

// Detach (the default) returns from Run as soon as the container is
// running. Pass WaitForExit() to override.
func Detach() RunOption {
	return func(s *runState) { s.detach = true }
}

// WaitForExit makes Run block until the container exits, populating
// RunResult.ExitCode. Equivalent to docker run without -d.
func WaitForExit() RunOption {
	return func(s *runState) { s.wait = true; s.detach = false }
}

// AutoRemove removes the container automatically when it exits — equivalent
// to docker run --rm.
func AutoRemove() RunOption {
	return func(s *runState) { s.host.AutoRemove = true }
}

// RunPlatform pins the container's target platform (multi-arch images
// only). Same shape as BuildPlatform/PullPlatform: "linux/amd64",
// "linux/arm64", etc.
func RunPlatform(platform string) RunOption {
	return func(s *runState) {
		os, arch, variant := parsePlatform(platform)
		s.platform = &ocispec.Platform{OS: os, Architecture: arch, Variant: variant}
	}
}

// === Process & command ===

// Cmd overrides the image's default CMD. Variadic for ergonomic calls
// (Cmd("echo", "hello")). Pass an empty Cmd() to unset.
func Cmd(args ...string) RunOption {
	return func(s *runState) { s.config.Cmd = strslice.StrSlice(args) }
}

// Entrypoint overrides the image's ENTRYPOINT.
func Entrypoint(args ...string) RunOption {
	return func(s *runState) { s.config.Entrypoint = strslice.StrSlice(args) }
}

// Shell sets the shell used for shell-form RUN/CMD/ENTRYPOINT. Default
// platform-dependent.
func Shell(args ...string) RunOption {
	return func(s *runState) { s.config.Shell = strslice.StrSlice(args) }
}

// User sets the user (and optional group) the container's processes run
// as. Forms: "uid", "uid:gid", "username", "username:groupname".
func User(user string) RunOption {
	return func(s *runState) { s.config.User = user }
}

// WorkDir sets the working directory for the container's processes.
func WorkDir(dir string) RunOption {
	return func(s *runState) { s.config.WorkingDir = dir }
}

// Hostname sets the container's hostname (visible to its own processes).
func Hostname(host string) RunOption {
	return func(s *runState) { s.config.Hostname = host }
}

// Domainname sets the container's domain name.
func Domainname(domain string) RunOption {
	return func(s *runState) { s.config.Domainname = domain }
}

// === Environment & metadata ===

// Env sets a single environment variable. Repeatable.
func Env(key, value string) RunOption {
	return func(s *runState) {
		s.config.Env = append(s.config.Env, key+"="+value)
	}
}

// EnvMap sets a batch of environment variables.
func EnvMap(vars map[string]string) RunOption {
	return func(s *runState) {
		for k, v := range vars {
			s.config.Env = append(s.config.Env, k+"="+v)
		}
	}
}

// RunLabel sets a single container label. Distinct from Build's Label
// (which decorates the image); this decorates the container.
func RunLabel(key, value string) RunOption {
	return func(s *runState) {
		if s.config.Labels == nil {
			s.config.Labels = make(map[string]string)
		}
		s.config.Labels[key] = value
	}
}

// === Stdio ===

// Tty allocates a pseudo-TTY for the container (docker run -t).
func Tty() RunOption {
	return func(s *runState) { s.config.Tty = true }
}

// Interactive keeps stdin open even if not attached (docker run -i).
func Interactive() RunOption {
	return func(s *runState) { s.config.OpenStdin = true }
}

// AttachStdio attaches stdin/stdout/stderr — useful for interactive runs
// driven through the SDK's HijackedResponse.
func AttachStdio() RunOption {
	return func(s *runState) {
		s.config.AttachStdin = true
		s.config.AttachStdout = true
		s.config.AttachStderr = true
	}
}

// === Ports ===

// PortMap is the friendly port-binding type. Host="" means "all
// interfaces" (0.0.0.0); HostPort=0 means "let the daemon pick".
type PortMap struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string // "tcp" (default) or "udp"
}

// Port adds a port binding. The container port is the only required field;
// host-side defaults to all-interfaces, host-picks-port. Repeatable.
func Port(p PortMap) RunOption {
	return func(s *runState) {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		key := nat.Port(strconv.Itoa(p.ContainerPort) + "/" + proto)
		if s.config.ExposedPorts == nil {
			s.config.ExposedPorts = nat.PortSet{}
		}
		s.config.ExposedPorts[key] = struct{}{}
		if s.host.PortBindings == nil {
			s.host.PortBindings = nat.PortMap{}
		}
		s.host.PortBindings[key] = append(s.host.PortBindings[key], nat.PortBinding{
			HostIP:   p.HostIP,
			HostPort: hostPortString(p.HostPort),
		})
	}
}

// PublishAllPorts publishes every EXPOSEd port to a random host port.
// Mirrors docker run -P.
func PublishAllPorts() RunOption {
	return func(s *runState) { s.host.PublishAllPorts = true }
}

func hostPortString(p int) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(p)
}

// === Mounts ===

// MountType discriminates the kind of mount. Constants below.
type MountType string

const (
	MountTypeBind   MountType = MountType(mount.TypeBind)
	MountTypeVolume MountType = MountType(mount.TypeVolume)
	MountTypeTmpfs  MountType = MountType(mount.TypeTmpfs)
	MountTypeNpipe  MountType = MountType(mount.TypeNamedPipe) // Windows
	MountTypeImage  MountType = MountType(mount.TypeImage)
)

// MountSpec is the friendly mount-binding type. For bind mounts, Source is
// a host path; for volume mounts, Source is a volume name; for tmpfs,
// Source is ignored.
type MountSpec struct {
	Type     MountType
	Source   string
	Target   string
	ReadOnly bool

	// BindPropagation is the propagation mode for bind mounts. Empty means
	// daemon default. Common values: "private", "rprivate", "shared",
	// "rshared", "slave", "rslave".
	BindPropagation string

	// TmpfsSize caps the tmpfs at this many bytes (Type=Tmpfs only).
	TmpfsSize int64
	// TmpfsMode is the file mode for the tmpfs root (Type=Tmpfs only).
	TmpfsMode uint32
}

// Mount adds a mount. Repeatable. For most cases prefer this over
// the legacy Bind() option.
func Mount(m MountSpec) RunOption {
	return func(s *runState) {
		spec := mount.Mount{
			Type:     mount.Type(m.Type),
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		}
		if m.BindPropagation != "" {
			spec.BindOptions = &mount.BindOptions{Propagation: mount.Propagation(m.BindPropagation)}
		}
		if m.Type == MountTypeTmpfs && (m.TmpfsSize > 0 || m.TmpfsMode != 0) {
			spec.TmpfsOptions = &mount.TmpfsOptions{
				SizeBytes: m.TmpfsSize,
				Mode:      0,
			}
			if m.TmpfsMode != 0 {
				spec.TmpfsOptions.Mode = 0
			}
		}
		s.host.Mounts = append(s.host.Mounts, spec)
	}
}

// Bind adds a bind mount in the legacy "/host:/container[:ro]" string
// form. Prefer Mount(MountSpec{...}) for new code; this option exists for
// docker-compose-style configs that already speak the string form.
func Bind(spec string) RunOption {
	return func(s *runState) { s.host.Binds = append(s.host.Binds, spec) }
}

// VolumesFrom inherits all the mounts from another container.
func VolumesFrom(containerName string) RunOption {
	return func(s *runState) { s.host.VolumesFrom = append(s.host.VolumesFrom, containerName) }
}

// Tmpfs mounts a tmpfs at containerPath with optional size/mode. Empty opts
// uses daemon defaults. Repeatable.
func Tmpfs(containerPath, opts string) RunOption {
	return func(s *runState) {
		if s.host.Tmpfs == nil {
			s.host.Tmpfs = make(map[string]string)
		}
		s.host.Tmpfs[containerPath] = opts
	}
}

// AnonymousVolume declares an anonymous volume mount at containerPath. The
// daemon manages the underlying volume.
func AnonymousVolume(containerPath string) RunOption {
	return func(s *runState) {
		if s.config.Volumes == nil {
			s.config.Volumes = make(map[string]struct{})
		}
		s.config.Volumes[containerPath] = struct{}{}
	}
}

// === Networking ===

// Network sets the container's network mode. Common values: "bridge"
// (default), "host", "none", "container:<name>", "<custom-network>".
func Network(mode string) RunOption {
	return func(s *runState) { s.host.NetworkMode = container.NetworkMode(mode) }
}

// AttachNetwork attaches the container to an additional network at create
// time. Repeatable for multi-network attachment.
func AttachNetwork(networkName string) RunOption {
	return func(s *runState) {
		if s.network.EndpointsConfig == nil {
			s.network.EndpointsConfig = make(map[string]*network.EndpointSettings)
		}
		s.network.EndpointsConfig[networkName] = &network.EndpointSettings{}
	}
}

// AttachNetworkWith attaches the container to an additional network with
// fully-customized endpoint settings (aliases, IP address, link-local).
func AttachNetworkWith(networkName string, settings *network.EndpointSettings) RunOption {
	return func(s *runState) {
		if s.network.EndpointsConfig == nil {
			s.network.EndpointsConfig = make(map[string]*network.EndpointSettings)
		}
		s.network.EndpointsConfig[networkName] = settings
	}
}

// DNS adds a DNS server.
func DNS(server string) RunOption {
	return func(s *runState) { s.host.DNS = append(s.host.DNS, server) }
}

// DNSOption adds a /etc/resolv.conf option ("ndots:5", "rotate", etc.).
func DNSOption(opt string) RunOption {
	return func(s *runState) { s.host.DNSOptions = append(s.host.DNSOptions, opt) }
}

// DNSSearch adds a DNS search domain.
func DNSSearch(domain string) RunOption {
	return func(s *runState) { s.host.DNSSearch = append(s.host.DNSSearch, domain) }
}

// AddHost appends a host:ip pair to /etc/hosts inside the container.
// Mirrors docker run --add-host.
func AddHost(hostIP string) RunOption {
	return func(s *runState) { s.host.ExtraHosts = append(s.host.ExtraHosts, hostIP) }
}

// Link adds a legacy network link "name:alias". Bridge-network only.
func Link(spec string) RunOption {
	return func(s *runState) { s.host.Links = append(s.host.Links, spec) }
}

// === Security & isolation ===

// Privileged drops most security restrictions. Avoid in production unless
// you know exactly what's being unblocked.
func Privileged() RunOption {
	return func(s *runState) { s.host.Privileged = true }
}

// ReadonlyRootfs makes the container's root filesystem read-only.
func ReadonlyRootfs() RunOption {
	return func(s *runState) { s.host.ReadonlyRootfs = true }
}

// CapAdd grants a Linux capability (e.g. "NET_ADMIN", "SYS_PTRACE").
func CapAdd(capability string) RunOption {
	return func(s *runState) { s.host.CapAdd = append(s.host.CapAdd, capability) }
}

// CapDrop revokes a Linux capability. Useful baseline: drop ALL then add
// just what's needed.
func CapDrop(capability string) RunOption {
	return func(s *runState) { s.host.CapDrop = append(s.host.CapDrop, capability) }
}

// RunSecurityOpt adds a security option ("seccomp=unconfined",
// "apparmor=docker-default", "label=disable", …). Distinct from build's
// SecurityOpt (which applies during build steps).
func RunSecurityOpt(opt string) RunOption {
	return func(s *runState) { s.host.SecurityOpt = append(s.host.SecurityOpt, opt) }
}

// GroupAdd appends a supplementary group ID to the container's processes.
func GroupAdd(group string) RunOption {
	return func(s *runState) { s.host.GroupAdd = append(s.host.GroupAdd, group) }
}

// IpcMode sets the IPC namespace mode ("none", "private", "shareable",
// "host", "container:<name>").
func IpcMode(mode string) RunOption {
	return func(s *runState) { s.host.IpcMode = container.IpcMode(mode) }
}

// PidMode sets the PID namespace mode ("host", "container:<name>").
func PidMode(mode string) RunOption {
	return func(s *runState) { s.host.PidMode = container.PidMode(mode) }
}

// UTSMode sets the UTS namespace mode ("host" or empty for private).
func UTSMode(mode string) RunOption {
	return func(s *runState) { s.host.UTSMode = container.UTSMode(mode) }
}

// UsernsMode sets the user namespace mode ("host" disables remapping).
func UsernsMode(mode string) RunOption {
	return func(s *runState) { s.host.UsernsMode = container.UsernsMode(mode) }
}

// CgroupnsMode sets the cgroup namespace mode ("host", "private").
func CgroupnsMode(mode string) RunOption {
	return func(s *runState) { s.host.CgroupnsMode = container.CgroupnsMode(mode) }
}

// RunIsolation sets the container isolation mode (Windows: "default",
// "process", "hyperv"). No-op on Linux.
func RunIsolation(mode string) RunOption {
	return func(s *runState) { s.host.Isolation = container.Isolation(mode) }
}

// Sysctl sets a sysctl in the container's namespaces. Repeatable.
func Sysctl(key, value string) RunOption {
	return func(s *runState) {
		if s.host.Sysctls == nil {
			s.host.Sysctls = make(map[string]string)
		}
		s.host.Sysctls[key] = value
	}
}

// Runtime selects a non-default OCI runtime ("nvidia", "kata", "youki").
// Daemon must be configured with the runtime registered.
func Runtime(name string) RunOption {
	return func(s *runState) { s.host.Runtime = name }
}

// === Resource limits ===

// MemoryLimit caps container memory in bytes. 0 disables (unlimited).
// Distinct from build's Memory (which caps build steps).
func MemoryLimit(bytes int64) RunOption {
	return func(s *runState) { s.host.Memory = bytes }
}

// MemoryReservation sets a soft memory limit (best-effort floor under
// pressure). Should be <= MemoryLimit.
func MemoryReservation(bytes int64) RunOption {
	return func(s *runState) { s.host.MemoryReservation = bytes }
}

// MemorySwapLimit caps memory + swap. -1 disables swap entirely.
func MemorySwapLimit(bytes int64) RunOption {
	return func(s *runState) { s.host.MemorySwap = bytes }
}

// MemorySwappiness controls how aggressively the kernel swaps anonymous
// pages (0-100). nil leaves the daemon default.
func MemorySwappiness(value int64) RunOption {
	return func(s *runState) { s.host.MemorySwappiness = &value }
}

// CPUs caps CPU usage as a fraction of cores ("1.5" = 1.5 cores). Mirrors
// docker run --cpus.
func CPUs(cores float64) RunOption {
	return func(s *runState) { s.host.NanoCPUs = int64(cores * 1e9) }
}

// CPUSharesLimit sets relative CPU weight (default 1024).
func CPUSharesLimit(shares int64) RunOption {
	return func(s *runState) { s.host.CPUShares = shares }
}

// CPUQuotaLimit caps CPU time per period (microseconds).
func CPUQuotaLimit(microseconds int64) RunOption {
	return func(s *runState) { s.host.CPUQuota = microseconds }
}

// CPUPeriodLimit sets the CFS period (microseconds, default 100000).
func CPUPeriodLimit(microseconds int64) RunOption {
	return func(s *runState) { s.host.CPUPeriod = microseconds }
}

// CPUSet pins the container to specific CPU cores ("0-2" or "0,2,4").
func CPUSet(cpus string) RunOption {
	return func(s *runState) { s.host.CpusetCpus = cpus }
}

// MemSet pins the container to specific NUMA memory nodes.
func MemSet(mems string) RunOption {
	return func(s *runState) { s.host.CpusetMems = mems }
}

// PidsLimit caps the number of processes the container can spawn. -1
// unlimited.
func PidsLimit(limit int64) RunOption {
	return func(s *runState) { s.host.PidsLimit = &limit }
}

// RunUlimit defines a single resource limit for the container. Same shape
// as build's Ulimit.
func RunUlimit(name string, soft, hard int64) RunOption {
	return func(s *runState) {
		s.host.Ulimits = append(s.host.Ulimits, &container.Ulimit{
			Name: name,
			Soft: soft,
			Hard: hard,
		})
	}
}

// OomScoreAdj tunes the OOM-killer's bias for this container (-1000..1000).
// More negative = less likely to be killed.
func OomScoreAdj(adj int) RunOption {
	return func(s *runState) { s.host.OomScoreAdj = adj }
}

// OomKillDisable prevents the OOM killer from killing this container's
// processes. Use with caution — paired with no MemoryLimit, can deadlock
// the host.
func OomKillDisable() RunOption {
	return func(s *runState) { val := true; s.host.OomKillDisable = &val }
}

// RunCgroupParent sets the cgroup parent for this container's resources.
func RunCgroupParent(parent string) RunOption {
	return func(s *runState) { s.host.CgroupParent = parent }
}

// RunShmSize sets the size of /dev/shm in bytes. Default 64 MiB.
func RunShmSize(bytes int64) RunOption {
	return func(s *runState) { s.host.ShmSize = bytes }
}

// === Lifecycle policy ===

// RestartPolicy controls how the daemon handles container exits.
type RestartPolicy struct {
	// Name is one of: "no" (default), "always", "on-failure",
	// "unless-stopped".
	Name string
	// MaxRetries caps the retries for "on-failure" mode. 0 = unlimited.
	MaxRetries int
}

// Restart sets the container's restart policy.
func Restart(policy RestartPolicy) RunOption {
	return func(s *runState) {
		s.host.RestartPolicy = container.RestartPolicy{
			Name:              container.RestartPolicyMode(policy.Name),
			MaximumRetryCount: policy.MaxRetries,
		}
	}
}

// StopSignal sets the signal sent to the main process when the container
// is stopped (default SIGTERM).
func StopSignal(sig string) RunOption {
	return func(s *runState) { s.config.StopSignal = sig }
}

// StopTimeout sets the grace period (seconds) before SIGKILL after the
// stop signal.
func StopTimeout(seconds int) RunOption {
	return func(s *runState) { s.config.StopTimeout = &seconds }
}

// UseInit runs a small init process inside the container to reap zombies
// and forward signals. Recommended for processes that don't handle SIGCHLD.
func UseInit() RunOption {
	return func(s *runState) { val := true; s.host.Init = &val }
}

// === Logging ===

// LogDriver configures the log driver and its per-driver options.
// Common drivers: "json-file" (default), "syslog", "journald", "none",
// "fluentd", "awslogs".
func LogDriver(driver string, options map[string]string) RunOption {
	return func(s *runState) {
		s.host.LogConfig = container.LogConfig{
			Type:   driver,
			Config: options,
		}
	}
}

// === Health check ===

// HealthCheck specifies a health probe for the container.
type HealthCheck struct {
	// Test is the command. The first element is one of:
	//   "NONE"     — disable inherited check
	//   "CMD"      — exec the rest as args
	//   "CMD-SHELL"— wrap the rest in /bin/sh -c
	Test []string
	// Interval between checks (default 30s).
	Interval time.Duration
	// Timeout for each check (default 30s).
	Timeout time.Duration
	// StartPeriod is the warmup window before failures count (default 0s).
	StartPeriod time.Duration
	// StartInterval is the interval used during the start period (Docker
	// 25.0+).
	StartInterval time.Duration
	// Retries before the container is considered unhealthy (default 3).
	Retries int
}

// HealthProbe attaches a healthcheck to the container.
func HealthProbe(hc HealthCheck) RunOption {
	return func(s *runState) {
		s.config.Healthcheck = &container.HealthConfig{
			Test:          hc.Test,
			Interval:      hc.Interval,
			Timeout:       hc.Timeout,
			StartPeriod:   hc.StartPeriod,
			StartInterval: hc.StartInterval,
			Retries:       hc.Retries,
		}
	}
}

// DisableHealthCheck disables an inherited HEALTHCHECK without specifying
// a replacement. Equivalent to docker run --no-healthcheck.
func DisableHealthCheck() RunOption {
	return func(s *runState) {
		s.config.Healthcheck = &container.HealthConfig{Test: []string{"NONE"}}
	}
}

// DisableNetworking disables network access entirely.
func DisableNetworking() RunOption {
	return func(s *runState) { s.config.NetworkDisabled = true }
}

// === Lifecycle ops on existing containers ===

// Stop sends the stop signal to a running container, then SIGKILL after
// timeout. timeout=nil uses the container's StopTimeout (or 10s default).
func (c *Client) Stop(ctx context.Context, containerID string, timeout *time.Duration) error {
	opts := container.StopOptions{}
	if timeout != nil {
		secs := int(timeout.Seconds())
		opts.Timeout = &secs
	}
	if err := c.api.ContainerStop(ctx, containerID, opts); err != nil {
		return fmt.Errorf("docker.Stop: %w", err)
	}
	return nil
}

// Kill sends a signal to a container's main process. Use Stop for graceful
// shutdown; Kill for SIGKILL or specific signals (e.g. SIGUSR1 to trigger
// a config reload).
func (c *Client) Kill(ctx context.Context, containerID, signal string) error {
	if err := c.api.ContainerKill(ctx, containerID, signal); err != nil {
		return fmt.Errorf("docker.Kill: %w", err)
	}
	return nil
}

// Restart stops and starts a container in one call.
func (c *Client) Restart(ctx context.Context, containerID string, timeout *time.Duration) error {
	opts := container.StopOptions{}
	if timeout != nil {
		secs := int(timeout.Seconds())
		opts.Timeout = &secs
	}
	if err := c.api.ContainerRestart(ctx, containerID, opts); err != nil {
		return fmt.Errorf("docker.Restart: %w", err)
	}
	return nil
}

// RemoveOpts controls Container removal.
type RemoveOpts struct {
	// Force kills the container if it's running before removal.
	Force bool
	// RemoveVolumes also removes volumes that the container's mounts
	// reference (only anonymous volumes; named volumes are left alone).
	RemoveVolumes bool
	// RemoveLinks removes link associations rather than the container.
	RemoveLinks bool
}

// Remove deletes a container. Idempotent for already-removed containers.
func (c *Client) Remove(ctx context.Context, containerID string, opts RemoveOpts) error {
	if err := c.api.ContainerRemove(ctx, containerID, container.RemoveOptions{
		Force:         opts.Force,
		RemoveVolumes: opts.RemoveVolumes,
		RemoveLinks:   opts.RemoveLinks,
	}); err != nil {
		return fmt.Errorf("docker.Remove: %w", err)
	}
	return nil
}

// Wait blocks until the container reaches the target state and returns
// the exit code. condition is one of: "not-running" (default), "next-exit",
// "removed".
func (c *Client) Wait(ctx context.Context, containerID, condition string) (int, error) {
	cond := container.WaitConditionNotRunning
	switch condition {
	case "not-running", "":
		cond = container.WaitConditionNotRunning
	case "next-exit":
		cond = container.WaitConditionNextExit
	case "removed":
		cond = container.WaitConditionRemoved
	}
	statusCh, errCh := c.api.ContainerWait(ctx, containerID, cond)
	select {
	case err := <-errCh:
		return 0, fmt.Errorf("docker.Wait: %w", err)
	case status := <-statusCh:
		if status.Error != nil {
			return int(status.StatusCode), fmt.Errorf("docker.Wait: %s", status.Error.Message)
		}
		return int(status.StatusCode), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Inspect returns the daemon's full record for a container — config, host
// config, network settings, state, mounts, etc. Returned struct is the
// docker SDK's ContainerJSON unchanged.
func (c *Client) Inspect(ctx context.Context, containerID string) (container.InspectResponse, error) {
	resp, err := c.api.ContainerInspect(ctx, containerID)
	if err != nil {
		return container.InspectResponse{}, fmt.Errorf("docker.Inspect: %w", err)
	}
	return resp, nil
}

// List returns containers visible to the daemon. By default lists only
// running containers; pass WithListAll() to include stopped ones.
func (c *Client) List(ctx context.Context, opts ...ListOption) ([]container.Summary, error) {
	state := container.ListOptions{}
	for _, opt := range opts {
		opt(&state)
	}
	containers, err := c.api.ContainerList(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("docker.List: %w", err)
	}
	return containers, nil
}

// ListOption mutates the assembled list request.
type ListOption func(*container.ListOptions)

// WithListAll includes stopped containers in List output.
func WithListAll() ListOption { return func(o *container.ListOptions) { o.All = true } }

// WithListLatest limits to the most recently created container.
func WithListLatest() ListOption { return func(o *container.ListOptions) { o.Latest = true } }

// WithListLimit caps the number of containers returned.
func WithListLimit(n int) ListOption { return func(o *container.ListOptions) { o.Limit = n } }

// WithListSize includes container size info (slower).
func WithListSize() ListOption { return func(o *container.ListOptions) { o.Size = true } }

// === Helpers ===

// parsePlatform splits "linux/amd64", "linux/arm64/v8", etc. into the
// (os, arch, variant) triple needed by the OCI spec.
func parsePlatform(p string) (string, string, string) {
	var os, arch, variant string
	parts := splitN(p, "/", 3)
	if len(parts) > 0 {
		os = parts[0]
	}
	if len(parts) > 1 {
		arch = parts[1]
	}
	if len(parts) > 2 {
		variant = parts[2]
	}
	return os, arch, variant
}

// splitN is a tiny helper to avoid pulling in strings.SplitN at package
// init for one call site. Idiomatic enough at this scale.
func splitN(s, sep string, n int) []string {
	out := []string{}
	for len(s) > 0 && len(out) < n-1 {
		i := indexOf(s, sep)
		if i < 0 {
			break
		}
		out = append(out, s[:i])
		s = s[i+len(sep):]
	}
	if len(s) > 0 || (len(out) == 0 && s == "") {
		out = append(out, s)
	}
	return out
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
