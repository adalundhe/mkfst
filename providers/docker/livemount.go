package docker

import (
	"context"
	"sync"
	"time"

	"mkfst/providers/vfs"
)

// LiveMountOptions tunes a single LiveMount registration.
type LiveMountOptions struct {
	// SubtreeRoot scopes which part of the host VFS the container
	// sees. Empty or "/" exposes the whole tree at TargetPath.
	SubtreeRoot string
	// PollIntervalMin overrides the engine default for this tenant —
	// floor on how often we'll check the container for changes.
	PollIntervalMin time.Duration
	// PollIntervalMax overrides the engine default — ceiling for the
	// adaptive backoff when the container is idle.
	PollIntervalMax time.Duration
	// Engine, if non-nil, registers this LiveMount with a specific
	// engine instance instead of the process-wide default. Useful when
	// the application wants per-purpose engines (different
	// concurrency caps, separate lifecycles).
	Engine *SyncEngine
}

// LiveMount mirrors a host VFS subtree into the container at targetPath
// with bidirectional synchronization. Host writes propagate to the
// container in ~ms (event-driven); container writes propagate back to
// the host within the configured poll interval (default 10ms→1s
// adaptive).
//
// Cost & correctness model:
//   - No host setup needed (no /etc/fuse.conf, no SYS_ADMIN, no rootless
//     daemon required). Uses only standard Docker remote-API calls.
//   - VFS data stays in our process RAM; container side lives in the
//     container's writable layer (or tmpfs if the user pairs LiveMount
//     with Tmpfs(targetPath, ...)).
//   - Last-writer-wins by mtime on conflicting writes during a sync
//     window. Document-and-move-on; advanced users layer their own
//     resolver on top via Tree.Subscribe.
//
// Combine with Tmpfs to keep the in-container path RAM-only too:
//
//	client.Run(ctx, "alpine",
//	    docker.Tmpfs("/vfs", "size=64M"),
//	    docker.LiveMount(tree, "/vfs"),
//	    docker.Cmd("..."),
//	)
func LiveMount(tree *vfs.Tree, targetPath string, opts ...LiveMountOption) RunOption {
	cfg := LiveMountOptions{}
	for _, o := range opts {
		o(&cfg)
	}
	return func(s *runState) {
		// Prestart: hydrate the target path with VFS contents BEFORE
		// the entrypoint runs. This is the moment the daemon will
		// accept CopyToContainer calls (rootfs exists post-Create)
		// but the entrypoint hasn't started yet (pre-Start). Without
		// this ordering, the entrypoint would race the hydrate and
		// see a missing /vfs.
		s.prestart = append(s.prestart, func(ctx context.Context, c *Client, containerID string) error {
			engine := cfg.Engine
			if engine == nil {
				engine = defaultEngine(c)
			}
			return engine.Hydrate(ctx, LiveMountSpec{
				ContainerID:             containerID,
				Tree:                    tree,
				SubtreeRoot:             cfg.SubtreeRoot,
				TargetPath:              targetPath,
				PollIntervalMinOverride: cfg.PollIntervalMin,
				PollIntervalMaxOverride: cfg.PollIntervalMax,
			})
		})
		// Poststart: start the bidirectional sync goroutines. Needs
		// the container to be running because the container→host
		// direction does ContainerArchive reads against a live
		// filesystem.
		s.poststart = append(s.poststart, func(ctx context.Context, c *Client, containerID string) error {
			engine := cfg.Engine
			if engine == nil {
				engine = defaultEngine(c)
			}
			return engine.AttachLoops(containerID)
		})
	}
}

// LiveMountOption mutates a LiveMountOptions before LiveMount fires.
type LiveMountOption func(*LiveMountOptions)

// LiveMountSubtree scopes the host VFS view to a subtree.
func LiveMountSubtree(root string) LiveMountOption {
	return func(o *LiveMountOptions) { o.SubtreeRoot = root }
}

// LiveMountPollInterval overrides the engine defaults for this
// container's container→host poll cadence.
func LiveMountPollInterval(min, max time.Duration) LiveMountOption {
	return func(o *LiveMountOptions) {
		o.PollIntervalMin = min
		o.PollIntervalMax = max
	}
}

// LiveMountEngine pins this LiveMount to a specific SyncEngine. By
// default LiveMounts attach to a process-wide default engine.
func LiveMountEngine(e *SyncEngine) LiveMountOption {
	return func(o *LiveMountOptions) { o.Engine = e }
}

// === default-engine bookkeeping ===

var (
	defaultEngineMu sync.Mutex
	defaultEngines  = make(map[string]*SyncEngine) // keyed by daemon host
)

// defaultEngine returns (lazily creating) a process-wide SyncEngine for
// the given client's daemon. Each distinct daemon host gets its own
// engine so trees can't accidentally cross-talk between unrelated
// daemons in a multi-daemon process.
func defaultEngine(c *Client) *SyncEngine {
	defaultEngineMu.Lock()
	defer defaultEngineMu.Unlock()
	if e, ok := defaultEngines[c.Host()]; ok {
		return e
	}
	e := NewSyncEngine(c)
	defaultEngines[c.Host()] = e
	return e
}

// StopDefaultEngines tears down every process-wide default SyncEngine.
// Useful in test cleanup and at process shutdown.
func StopDefaultEngines() {
	defaultEngineMu.Lock()
	engines := make([]*SyncEngine, 0, len(defaultEngines))
	for _, e := range defaultEngines {
		engines = append(engines, e)
	}
	defaultEngines = make(map[string]*SyncEngine)
	defaultEngineMu.Unlock()
	for _, e := range engines {
		e.Stop()
	}
}

// UnregisterLiveMount detaches a container from its SyncEngine. Safe
// to call after the container exits — the engine also notices via
// "container gone" errors and self-cleans, but explicit unregister is
// faster and frees resources sooner.
func UnregisterLiveMount(c *Client, containerID string) {
	defaultEngineMu.Lock()
	e, ok := defaultEngines[c.Host()]
	defaultEngineMu.Unlock()
	if ok {
		e.Unregister(containerID)
	}
}

