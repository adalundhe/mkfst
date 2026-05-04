package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/registry"
)

// BuildOption mutates the assembled build request just before submission.
// Every flag the daemon understands has a corresponding BuildOption — no
// escape hatches, no "drop into the SDK" moments. Discover them via
// `docker.<TAB>` autocomplete.
type BuildOption func(*buildState)

// buildState is the accumulator the BuildOptions write into. Translated to
// build.ImageBuildOptions inside Client.Build.
type buildState struct {
	opts build.ImageBuildOptions
}

// Build submits an image build to the daemon. source is the build context
// (typically a *vfs.Tree wrapped in VFSSource — the in-memory default).
// Options are applied in order; later options override earlier ones for
// scalar fields and append for collections.
//
// Returns a channel of Events that closes when the build completes (either
// EventDone or EventError as the final emission). The Event with Kind=Aux
// near the end of a successful build carries the new image ID — use
// Event.ImageID() to extract.
//
// Goroutine ownership: a single streamer goroutine is spawned to decode
// the daemon's jsonmessage stream and feed the returned channel. It exits
// cleanly when the stream completes, when ctx is cancelled, or when the
// caller stops draining (via DrainEvents or a manual loop with a ctx
// check). Callers MUST do one of: drain to channel-close, cancel ctx,
// or both. Failure to drain AND failure to cancel will leak the streamer
// + the underlying HTTP response body.
func (c *Client) Build(ctx context.Context, source Source, opts ...BuildOption) (<-chan Event, error) {
	if source == nil {
		return nil, fmt.Errorf("docker.Build: source is nil")
	}

	state := &buildState{
		opts: build.ImageBuildOptions{
			// Match docker CLI defaults — clean up intermediate containers
			// after a successful build, leave them on failure for debugging.
			Remove: true,
		},
	}
	for _, opt := range opts {
		opt(state)
	}

	tarStream, err := source.Tar(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker.Build: source.Tar: %w", err)
	}

	state.opts.Context = tarStream

	resp, err := c.api.ImageBuild(ctx, tarStream, state.opts)
	if err != nil {
		_ = tarStream.Close()
		return nil, fmt.Errorf("docker.Build: %w", err)
	}

	events := make(chan Event, 16)
	go func() {
		defer tarStream.Close()
		defer resp.Body.Close()
		streamJSONMessages(ctx, resp.Body, events)
	}()
	return events, nil
}

// === Tagging ===

// Tag adds a tag to the resulting image. May be called multiple times to
// produce a multi-tagged image in a single build.
func Tag(tag string) BuildOption {
	return func(s *buildState) { s.opts.Tags = append(s.opts.Tags, tag) }
}

// Tags adds many tags at once. Equivalent to chaining Tag.
func Tags(tags ...string) BuildOption {
	return func(s *buildState) { s.opts.Tags = append(s.opts.Tags, tags...) }
}

// === Build context ===

// Dockerfile sets the path (within the build context) to the Dockerfile.
// Default "Dockerfile". Useful for monorepos that keep multiple Dockerfiles
// alongside a single context (e.g. Dockerfile.api, Dockerfile.web).
func Dockerfile(path string) BuildOption {
	return func(s *buildState) { s.opts.Dockerfile = path }
}

// RemoteContext tells the daemon to fetch the build context from a URL or
// git reference instead of reading the supplied tar. Use sparingly — it
// bypasses everything VFSSource gives you (in-memory speed, deterministic
// input, host-overlay union).
func RemoteContext(url string) BuildOption {
	return func(s *buildState) { s.opts.RemoteContext = url }
}

// === Build args ===

// Arg sets a build-time variable (--build-arg). The arg must also be
// declared in the Dockerfile via ARG to take effect.
func Arg(name, value string) BuildOption {
	return func(s *buildState) {
		if s.opts.BuildArgs == nil {
			s.opts.BuildArgs = make(map[string]*string)
		}
		v := value
		s.opts.BuildArgs[name] = &v
	}
}

// ArgFromEnv passes a build-time variable through from the daemon's
// environment without specifying a value. The daemon resolves the value
// from its own environment at build time. (This is what `docker build
// --build-arg NAME` without a value does.)
func ArgFromEnv(name string) BuildOption {
	return func(s *buildState) {
		if s.opts.BuildArgs == nil {
			s.opts.BuildArgs = make(map[string]*string)
		}
		s.opts.BuildArgs[name] = nil
	}
}

// === Labels ===

// Label sets a single label on the resulting image.
func Label(key, value string) BuildOption {
	return func(s *buildState) {
		if s.opts.Labels == nil {
			s.opts.Labels = make(map[string]string)
		}
		s.opts.Labels[key] = value
	}
}

// === Build behavior ===

// NoCache forces every step to re-execute, ignoring layer cache.
func NoCache() BuildOption {
	return func(s *buildState) { s.opts.NoCache = true }
}

// Pull forces a fresh pull of the base image even if it's locally cached.
// Mirrors `docker build --pull`.
func Pull() BuildOption {
	return func(s *buildState) { s.opts.PullParent = true }
}

// Squash collapses the resulting image's layers into a single layer over
// the parent. Requires the daemon to be configured with experimental
// features.
func Squash() BuildOption {
	return func(s *buildState) { s.opts.Squash = true }
}

// SuppressOutput suppresses verbose build step output, leaving only the
// final image ID in the event stream.
func SuppressOutput() BuildOption {
	return func(s *buildState) { s.opts.SuppressOutput = true }
}

// KeepIntermediate retains the intermediate containers from each build
// step. Useful for post-mortem debugging of failed builds. Default behavior
// (without this option) removes them.
func KeepIntermediate() BuildOption {
	return func(s *buildState) { s.opts.Remove = false }
}

// ForceRemoveIntermediate removes intermediate containers even if the
// build fails. Default keeps them on failure for debugging.
func ForceRemoveIntermediate() BuildOption {
	return func(s *buildState) { s.opts.ForceRemove = true }
}

// === Multi-stage / cache ===

// Target builds only up to the named stage in a multi-stage Dockerfile.
// Mirrors `docker build --target`.
func Target(stage string) BuildOption {
	return func(s *buildState) { s.opts.Target = stage }
}

// CacheFrom names an image to use as an additional cache source. Common
// pattern: pull a previously-built image and feed it back as cache for the
// next build to skip already-built layers.
func CacheFrom(images ...string) BuildOption {
	return func(s *buildState) { s.opts.CacheFrom = append(s.opts.CacheFrom, images...) }
}

// === Platform / cross-build ===

// BuildPlatform pins the target platform (e.g. "linux/amd64",
// "linux/arm64"). Without this, the daemon picks based on its own arch.
// For multi-arch builds, use BuildKit + buildx; this option targets a
// single platform.
func BuildPlatform(platform string) BuildOption {
	return func(s *buildState) { s.opts.Platform = platform }
}

// === Networking ===

// BuildNetwork sets the network mode for build steps (RUN instructions).
// Common values: "default", "host", "none". Mirrors `docker build
// --network`.
func BuildNetwork(mode string) BuildOption {
	return func(s *buildState) { s.opts.NetworkMode = mode }
}

// ExtraHost adds a host:ip mapping to /etc/hosts inside build steps.
// Mirrors `docker build --add-host`.
func ExtraHost(hostIP string) BuildOption {
	return func(s *buildState) { s.opts.ExtraHosts = append(s.opts.ExtraHosts, hostIP) }
}

// === Resource limits ===

// Memory caps build container memory in bytes. 0 disables (default).
func Memory(bytes int64) BuildOption {
	return func(s *buildState) { s.opts.Memory = bytes }
}

// MemorySwap caps total memory + swap in bytes. -1 disables swap; 0 means
// unlimited swap.
func MemorySwap(bytes int64) BuildOption {
	return func(s *buildState) { s.opts.MemorySwap = bytes }
}

// CPUShares sets the relative CPU weight for build containers. Default
// 1024 (one CPU's worth); 512 means half a CPU's worth in contention.
func CPUShares(shares int64) BuildOption {
	return func(s *buildState) { s.opts.CPUShares = shares }
}

// CPUQuota and CPUPeriod together cap CPU usage. CPUPeriod is the window
// (microseconds, default 100000 = 100ms); CPUQuota is the max time the
// container can run during one period.
func CPUQuota(microseconds int64) BuildOption {
	return func(s *buildState) { s.opts.CPUQuota = microseconds }
}

// CPUPeriod sets the CFS period (microseconds). See CPUQuota.
func CPUPeriod(microseconds int64) BuildOption {
	return func(s *buildState) { s.opts.CPUPeriod = microseconds }
}

// CPUSetCPUs pins the build to specific CPU cores ("0-2" or "0,2,4").
func CPUSetCPUs(set string) BuildOption {
	return func(s *buildState) { s.opts.CPUSetCPUs = set }
}

// CPUSetMems pins the build to specific NUMA memory nodes.
func CPUSetMems(set string) BuildOption {
	return func(s *buildState) { s.opts.CPUSetMems = set }
}

// CgroupParent sets the cgroup parent under which the build runs. Useful
// for resource-class isolation in shared hosts.
func CgroupParent(parent string) BuildOption {
	return func(s *buildState) { s.opts.CgroupParent = parent }
}

// ShmSize sets the size (bytes) of the /dev/shm tmpfs mount. Some
// build steps (chrome headless tests, scientific builds) need more than
// the 64 MiB default.
func ShmSize(bytes int64) BuildOption {
	return func(s *buildState) { s.opts.ShmSize = bytes }
}

// Ulimit defines a single resource limit ("nofile", "nproc", etc.) for the
// build. Both soft and hard caps are required.
func Ulimit(name string, soft, hard int64) BuildOption {
	return func(s *buildState) {
		s.opts.Ulimits = append(s.opts.Ulimits, &container.Ulimit{
			Name: name,
			Soft: soft,
			Hard: hard,
		})
	}
}

// === Security ===

// SecurityOpt adds a security option ("apparmor=unconfined",
// "seccomp=unconfined", etc.). Repeatable.
func SecurityOpt(opt string) BuildOption {
	return func(s *buildState) { s.opts.SecurityOpt = append(s.opts.SecurityOpt, opt) }
}

// Isolation chooses the container runtime isolation mode. "default",
// "process", "hyperv". Windows-specific in practice; harmless on Linux
// where only "default" is valid.
func Isolation(mode string) BuildOption {
	return func(s *buildState) { s.opts.Isolation = container.Isolation(mode) }
}

// === Auth (for pulling private base images) ===

// AuthConfig is a friendly registry-credential record. Username + Password
// is the common case; Token-based auth uses RegistryToken; IdentityToken
// is for OIDC-style flows.
type AuthConfig struct {
	Username      string
	Password      string
	Email         string
	Auth          string // base64-encoded "user:pass" — alt to Username/Password
	IdentityToken string
	RegistryToken string
	ServerAddress string
}

// RegistryAuth registers credentials for a specific registry hostname.
// Used to pull private base images during a build (and to push intermediate
// outputs in BuildKit). Hostname examples: "index.docker.io",
// "ghcr.io", "registry.example.com:5000".
func RegistryAuth(hostname string, auth AuthConfig) BuildOption {
	return func(s *buildState) {
		if s.opts.AuthConfigs == nil {
			s.opts.AuthConfigs = make(map[string]registry.AuthConfig)
		}
		s.opts.AuthConfigs[hostname] = registry.AuthConfig{
			Username:      auth.Username,
			Password:      auth.Password,
			Email:         auth.Email,
			Auth:          auth.Auth,
			IdentityToken: auth.IdentityToken,
			RegistryToken: auth.RegistryToken,
			ServerAddress: auth.ServerAddress,
		}
	}
}

// === BuildKit-specific ===

// BuilderVersion selects the build engine: BuilderV1 (legacy) or
// BuilderBuildKit (modern, parallel, secret-aware). Default is the
// daemon's configured default.
type BuilderVersion = build.BuilderVersion

const (
	BuilderV1       = build.BuilderV1
	BuilderBuildKit = build.BuilderBuildKit
)

// Builder pins the build engine. See BuilderVersion constants.
func Builder(version BuilderVersion) BuildOption {
	return func(s *buildState) { s.opts.Version = version }
}

// SessionID associates the build with an existing BuildKit session
// (created via the docker SDK's session package). Required for some
// BuildKit features like secret mounts.
func SessionID(id string) BuildOption {
	return func(s *buildState) { s.opts.SessionID = id }
}

// BuildID tags the build for later cancellation via Client.CancelBuild.
// Only meaningful with BuildKit.
func BuildID(id string) BuildOption {
	return func(s *buildState) { s.opts.BuildID = id }
}

// Output configures a BuildKit export target (registry, local tar,
// container image, OCI layout). See docker BuildKit docs for the exact
// shape of Type and Attrs.
type Output struct {
	Type  string
	Attrs map[string]string
}

// AddOutput appends a BuildKit output target. Repeatable for multi-output
// builds (e.g., one local tar + one registry push).
func AddOutput(out Output) BuildOption {
	return func(s *buildState) {
		s.opts.Outputs = append(s.opts.Outputs, build.ImageBuildOutput{
			Type:  out.Type,
			Attrs: out.Attrs,
		})
	}
}

// CancelBuild gracefully cancels an in-flight build identified by id (set
// via BuildID at submission time). Only meaningful for BuildKit builds.
func (c *Client) CancelBuild(ctx context.Context, id string) error {
	if err := c.api.BuildCancel(ctx, id); err != nil {
		return fmt.Errorf("docker.CancelBuild: %w", err)
	}
	return nil
}
