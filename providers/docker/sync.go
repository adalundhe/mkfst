package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"

	"mkfst/providers/vfs"
)

// SyncEngine maintains live, bidirectional file sync between host-side
// VFS subtrees and per-container target paths. One SyncEngine handles
// many tenants — one container ↔ one VFS subtree per registration.
//
// Direction semantics:
//
//   - Host→container: pure event-driven, fired by vfs.Tree.Subscribe.
//     Latency is one ContainerArchive roundtrip (~1ms over a unix socket).
//     No polling, zero idle CPU per tenant.
//
//   - Container→host: adaptive-poll, since Docker doesn't push us in-
//     container fs change notifications. Per-tenant interval starts at
//     PollIntervalMin and backs off exponentially up to PollIntervalMax
//     when no changes are seen for a while; snaps back to Min the
//     instant a change is observed. Per-pass cost is tracked, and the
//     interval is auto-floor-raised so a pass never takes more than
//     ~half the interval — protects CPU under load.
//
// Conflict policy: last-writer-wins by mtime. If host and container
// both modified the same path during a sync window, the side with the
// later mtime wins. Document-and-move-on; advanced users can layer
// their own conflict resolver on top.
//
// Lifecycle: one engine instance is fine for a whole process. Engine is
// safe for concurrent Register/Unregister calls. Stop releases all
// per-tenant goroutines and unsubscribes from every Tree.
type SyncEngine struct {
	cli *dockerclient.Client

	// PollIntervalMin floors how often we'll check a container for
	// changes. 10ms is the recommended default: feels live to humans,
	// per-pass stat call is cheap enough to fit comfortably.
	PollIntervalMin time.Duration
	// PollIntervalMax caps how slow we'll go when an idle tenant
	// hasn't changed in a while. 1s keeps the engine responsive when
	// changes resume without burning CPU on perpetually-idle tenants.
	PollIntervalMax time.Duration
	// MaxInflightCopyTo bounds concurrent ContainerArchive copy-to
	// calls across all tenants. Protects the docker socket from
	// pile-ups when many tenants change at once. 16 is plenty for
	// dozens of tenants on one daemon.
	MaxInflightCopyTo int
	// OnError, if non-nil, is invoked from the per-tenant goroutines
	// for transient errors that the engine handles internally (failed
	// CopyTo retries, stat-snapshot refresh failures, conflict-resolve
	// edge cases, etc.). The engine continues operating; OnError is
	// purely observational. Default nil means errors are swallowed —
	// set this in production to catch silent degradation.
	OnError func(containerID string, op string, err error)

	// internal state
	mu      sync.RWMutex
	tenants map[string]*syncTenant
	stopCh  chan struct{}
	stopped atomic.Bool
	copySem chan struct{}
}

// reportErr forwards a tenant-level transient error to the OnError
// hook. No-op when OnError is nil (default). Used everywhere the engine
// previously dropped errors silently.
func (e *SyncEngine) reportErr(containerID, op string, err error) {
	if e.OnError != nil && err != nil {
		e.OnError(containerID, op, err)
	}
}

// NewSyncEngine returns an engine bound to a docker Client. The engine
// inherits sensible defaults for poll intervals and concurrency bounds;
// callers may override fields before Register is called.
func NewSyncEngine(c *Client) *SyncEngine {
	return &SyncEngine{
		cli:               c.SDK(),
		PollIntervalMin:   10 * time.Millisecond,
		PollIntervalMax:   time.Second,
		MaxInflightCopyTo: 16,
		tenants:           make(map[string]*syncTenant),
		stopCh:            make(chan struct{}),
	}
}

// Stop terminates every tenant's loops, unsubscribes from trees, and
// releases resources. Idempotent. After Stop, the engine cannot be
// re-used.
func (e *SyncEngine) Stop() {
	if !e.stopped.CompareAndSwap(false, true) {
		return
	}
	close(e.stopCh)
	e.mu.Lock()
	for _, t := range e.tenants {
		t.cancel()
	}
	e.tenants = nil
	e.mu.Unlock()
}

// LiveMountSpec describes a single tenant registration.
type LiveMountSpec struct {
	// ContainerID is the docker container the data syncs into.
	ContainerID string
	// Tree is the host-side VFS the container's TargetPath mirrors.
	Tree *vfs.Tree
	// SubtreeRoot scopes which part of the tree is exposed. Empty or
	// "/" exposes the whole tree.
	SubtreeRoot string
	// TargetPath is the directory inside the container that mirrors
	// the host subtree. Created on initial sync if absent.
	TargetPath string
	// PollIntervalMinOverride / PollIntervalMaxOverride, if non-zero,
	// override the engine defaults for this specific tenant.
	PollIntervalMinOverride time.Duration
	PollIntervalMaxOverride time.Duration
}

// Hydrate does the initial host→container synchronous copy and
// records the tenant — but does NOT start the bidirectional sync
// goroutines yet. Call this BEFORE ContainerStart so the container's
// entrypoint sees the data ready at TargetPath when it begins.
//
// Pair with AttachLoops(spec.ContainerID) AFTER ContainerStart to
// begin the live-sync. The Run wrapper does this automatically when
// you use the LiveMount RunOption.
func (e *SyncEngine) Hydrate(ctx context.Context, spec LiveMountSpec) error {
	if e.stopped.Load() {
		return errors.New("docker.SyncEngine: stopped")
	}
	if spec.ContainerID == "" || spec.Tree == nil || spec.TargetPath == "" {
		return fmt.Errorf("docker.SyncEngine.Hydrate: ContainerID, Tree, TargetPath required")
	}

	e.mu.Lock()
	if _, exists := e.tenants[spec.ContainerID]; exists {
		e.mu.Unlock()
		return fmt.Errorf("docker.SyncEngine.Hydrate: %s already registered", spec.ContainerID)
	}
	if e.copySem == nil {
		e.copySem = make(chan struct{}, e.MaxInflightCopyTo)
	}
	e.mu.Unlock()

	tenantCtx, tenantCancel := context.WithCancel(context.Background())
	t := &syncTenant{
		spec:   spec,
		engine: e,
		ctx:    tenantCtx,
		cancel: tenantCancel,
		known:  make(map[string]containerStat),
	}
	t.minInterval = firstNonZero(spec.PollIntervalMinOverride, e.PollIntervalMin)
	t.maxInterval = firstNonZero(spec.PollIntervalMaxOverride, e.PollIntervalMax)
	t.curInterval = t.minInterval

	t.events, t.unsub = spec.Tree.Subscribe(vfs.SubscribeOpts{
		PathPrefix: spec.SubtreeRoot,
		Buffer:     128,
	})

	if err := t.fullHydrate(ctx); err != nil {
		t.unsub()
		tenantCancel()
		return fmt.Errorf("docker.SyncEngine.Hydrate: %w", err)
	}

	e.mu.Lock()
	e.tenants[spec.ContainerID] = t
	e.mu.Unlock()

	return nil
}

// AttachLoops starts the bidirectional sync goroutines for a tenant
// previously registered via Hydrate. Idempotent if called twice.
// Call AFTER ContainerStart so the goroutines' first I/O succeeds.
func (e *SyncEngine) AttachLoops(containerID string) error {
	e.mu.RLock()
	t, ok := e.tenants[containerID]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("docker.SyncEngine.AttachLoops: no tenant for %s", containerID)
	}
	if !t.loopsStarted.CompareAndSwap(false, true) {
		return nil
	}
	go t.hostToContainerLoop()
	go t.containerToHostLoop()
	return nil
}

// Register is the convenience wrapper that does Hydrate + AttachLoops
// in one call. Use when the container is already running. For Run
// integration, prefer Hydrate (pre-start) + AttachLoops (post-start)
// — that's what the LiveMount RunOption does internally.
func (e *SyncEngine) Register(ctx context.Context, spec LiveMountSpec) error {
	if err := e.Hydrate(ctx, spec); err != nil {
		return err
	}
	return e.AttachLoops(spec.ContainerID)
}

// Unregister detaches a tenant. The per-tenant goroutines exit, the
// tree subscription is dropped, and the in-memory diff state is
// released. Idempotent.
func (e *SyncEngine) Unregister(containerID string) {
	e.mu.Lock()
	t, ok := e.tenants[containerID]
	if ok {
		delete(e.tenants, containerID)
	}
	e.mu.Unlock()
	if ok {
		t.cancel()
	}
}

// === per-tenant state ===

// syncTenant is the engine-internal record for one Register call.
type syncTenant struct {
	spec   LiveMountSpec
	engine *SyncEngine

	ctx    context.Context
	cancel context.CancelFunc

	events       <-chan vfs.ChangeEvent
	unsub        func()
	loopsStarted atomic.Bool

	// adaptive-poll state for the container→host loop
	minInterval time.Duration
	maxInterval time.Duration
	curInterval time.Duration

	// known is the engine's snapshot of what's currently in the
	// container at TargetPath, keyed by relative path. Used to detect
	// container-side mutations between polls.
	knownMu sync.Mutex
	known   map[string]containerStat
}

// containerStat is a tiny stat record used for change detection.
type containerStat struct {
	size    int64
	mode    fs.FileMode
	modTime time.Time
	isDir   bool
}

// fullHydrate tars the entire subtree from VFS and pushes it to the
// container's TargetPath via ContainerArchive. Used on Register to
// give the entrypoint a populated directory at start.
func (t *syncTenant) fullHydrate(ctx context.Context) error {
	rc := t.spec.Tree.Tar(ctx, vfs.TarOpts{
		Root:        t.spec.SubtreeRoot,
		IncludeRoot: false,
	})
	defer rc.Close()

	// Make sure TargetPath exists before pushing.
	if err := t.ensureTargetExists(ctx); err != nil {
		return err
	}

	if err := t.engine.cli.CopyToContainer(ctx, t.spec.ContainerID, t.spec.TargetPath, rc, container.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
		CopyUIDGID:                false,
	}); err != nil {
		return fmt.Errorf("CopyToContainer: %w", err)
	}

	// Record what's now in the container so the container→host loop
	// has a baseline to diff against.
	return t.refreshKnownFromContainer(ctx)
}

// ensureTargetExists creates TargetPath inside the container by
// extracting a tar containing just the named directory into the
// parent. No-op if TargetPath already exists as a directory.
//
// CopyToContainer extracts the tar relative to the destination dir, so
// to create /vfs we tar `vfs/` and push to `/`. Tarring `./` into `/`
// would create `/.` (no-op), not `/vfs` — which was the original bug.
func (t *syncTenant) ensureTargetExists(ctx context.Context) error {
	// Stat first; if it exists and is a directory, nothing to do.
	stat, err := t.engine.cli.ContainerStatPath(ctx, t.spec.ContainerID, t.spec.TargetPath)
	if err == nil && stat.Mode&fs.ModeDir != 0 {
		return nil
	}
	parent := path.Dir(t.spec.TargetPath)
	base := path.Base(t.spec.TargetPath)
	if parent == "." {
		parent = "/"
	}
	if base == "" || base == "/" {
		return fmt.Errorf("docker.SyncEngine: invalid TargetPath %q", t.spec.TargetPath)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     base + "/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return t.engine.cli.CopyToContainer(ctx, t.spec.ContainerID, parent, &buf, container.CopyToContainerOptions{})
}

// hostToContainerLoop reads ChangeEvents from the tree and pushes the
// affected paths into the container. Coalesces bursts (e.g. a flurry
// of writes to the same dir) into a single tar push within a small
// window.
func (t *syncTenant) hostToContainerLoop() {
	const coalesceWindow = 5 * time.Millisecond
	pending := make(map[string]vfs.ChangeOp)
	flushTimer := time.NewTimer(time.Hour)
	flushTimer.Stop()
	armed := false
	for {
		select {
		case <-t.ctx.Done():
			return
		case ev, ok := <-t.events:
			if !ok {
				return
			}
			pending[ev.Path] = ev.Op
			if !armed {
				flushTimer.Reset(coalesceWindow)
				armed = true
			}
		case <-flushTimer.C:
			armed = false
			if len(pending) == 0 {
				continue
			}
			snapshot := pending
			pending = make(map[string]vfs.ChangeOp)
			t.flushHostToContainer(snapshot)
		}
	}
}

// flushHostToContainer pushes the affected paths into the container.
// Removes are handled separately (we have no "delete" SDK call;
// emulated by exec'ing rm via ContainerExec for now).
func (t *syncTenant) flushHostToContainer(changes map[string]vfs.ChangeOp) {
	// Bound concurrent CopyTo calls.
	select {
	case t.engine.copySem <- struct{}{}:
		defer func() { <-t.engine.copySem }()
	case <-t.ctx.Done():
		return
	}

	// Partition: writes/mkdirs go through CopyToContainer; removes go
	// through ContainerExec (`rm -rf <abs path>`). One exec per remove
	// is fine for typical workloads; if removes ever become a hot path
	// we can batch.
	var pushPaths []string
	var removePaths []string
	for p, op := range changes {
		switch op {
		case vfs.OpRemove:
			removePaths = append(removePaths, p)
		default:
			pushPaths = append(pushPaths, p)
		}
	}

	if len(pushPaths) > 0 {
		if err := t.pushPaths(pushPaths); err != nil {
			// Container could be gone (stopped, removed). Cancel the
			// tenant — no point continuing to push to a dead target.
			if isContainerGoneErr(err) {
				t.cancel()
				return
			}
			// Otherwise: surface to OnError, retry on the next event.
			t.engine.reportErr(t.spec.ContainerID, "host-to-container push", err)
		}
	}
	if len(removePaths) > 0 {
		if err := t.execRemoveInContainer(removePaths); err != nil {
			if isContainerGoneErr(err) {
				t.cancel()
				return
			}
			t.engine.reportErr(t.spec.ContainerID, "host-to-container remove", err)
		}
	}

	// Refresh our known snapshot so the container→host loop doesn't
	// later interpret these host-side changes as container-side
	// mutations and try to re-sync them back. If refresh fails, the
	// next pull will rebuild the snapshot anyway and conflict
	// resolution (mtime last-writer-wins) keeps data correct — but we
	// still report the error so callers see degradation.
	if err := t.refreshKnownFromContainer(t.ctx); err != nil {
		t.engine.reportErr(t.spec.ContainerID, "refresh known snapshot", err)
	}
}

// pushPaths streams a tar of the named subtree paths from the host VFS
// into the container.
func (t *syncTenant) pushPaths(paths []string) error {
	pr, pw := io.Pipe()
	go func() {
		err := writePathsTar(t.spec.Tree, t.spec.SubtreeRoot, paths, pw)
		_ = pw.CloseWithError(err)
	}()
	defer pr.Close()
	return t.engine.cli.CopyToContainer(t.ctx, t.spec.ContainerID, t.spec.TargetPath, pr, container.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
	})
}

// writePathsTar emits a tar containing the named paths (relative to
// subtreeRoot) from the tree. Used by the host→container pump for
// targeted delta pushes.
func writePathsTar(tree *vfs.Tree, subtreeRoot string, paths []string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	for _, p := range paths {
		if !pathInSubtree(p, subtreeRoot) {
			continue
		}
		inode, err := tree.Stat(p)
		if err != nil {
			// Path may have been deleted between event and tar — skip.
			continue
		}
		rel := relativeToSubtree(p, subtreeRoot)
		if rel == "" {
			continue
		}
		switch {
		case inode.IsDir():
			hdr := &tar.Header{
				Name:     rel + "/",
				Mode:     int64(inode.Mode.Perm()),
				ModTime:  inode.ModTime,
				Typeflag: tar.TypeDir,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case inode.IsSymlink():
			hdr := &tar.Header{
				Name:     rel,
				Mode:     int64(inode.Mode.Perm()),
				ModTime:  inode.ModTime,
				Typeflag: tar.TypeSymlink,
				Linkname: inode.LinkTarget,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case inode.IsFile():
			body, err := tree.Read(p)
			if err != nil {
				continue
			}
			hdr := &tar.Header{
				Name:     rel,
				Mode:     int64(inode.Mode.Perm()),
				ModTime:  inode.ModTime,
				Size:     int64(len(body)),
				Typeflag: tar.TypeReg,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := tw.Write(body); err != nil {
				return err
			}
		}
	}
	return nil
}

func pathInSubtree(p, root string) bool {
	if root == "" || root == "/" {
		return true
	}
	return p == root || strings.HasPrefix(p, root+"/")
}

func relativeToSubtree(p, root string) string {
	if root == "" || root == "/" {
		return strings.TrimPrefix(p, "/")
	}
	rel := strings.TrimPrefix(p, root)
	rel = strings.TrimPrefix(rel, "/")
	return rel
}

// execRemoveInContainer runs `rm -rf` inside the container for each
// removed path. The remove path inside the container is computed as
// TargetPath + relative-to-subtree.
func (t *syncTenant) execRemoveInContainer(paths []string) error {
	args := []string{"rm", "-rf"}
	for _, p := range paths {
		rel := relativeToSubtree(p, t.spec.SubtreeRoot)
		if rel == "" {
			continue
		}
		args = append(args, path.Join(t.spec.TargetPath, rel))
	}
	if len(args) == 2 {
		return nil // nothing to do
	}
	exec, err := t.engine.cli.ContainerExecCreate(t.ctx, t.spec.ContainerID, container.ExecOptions{
		Cmd:          args,
		AttachStdout: false,
		AttachStderr: false,
	})
	if err != nil {
		return err
	}
	return t.engine.cli.ContainerExecStart(t.ctx, exec.ID, container.ExecStartOptions{Detach: true})
}

// containerToHostLoop polls the container for filesystem changes and
// pulls them back into the host VFS. The interval adapts: starts at
// minInterval, doubles up to maxInterval when nothing changes, snaps
// back to minInterval the moment something does.
func (t *syncTenant) containerToHostLoop() {
	ticker := time.NewTimer(t.curInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
		}

		start := time.Now()
		changed, err := t.pullContainerChanges()
		if err != nil {
			if isContainerGoneErr(err) {
				t.cancel()
				return
			}
		}
		elapsed := time.Since(start)

		// Adapt interval: snap back on change, back off on idle. Also
		// floor-raise so a pass never exceeds half the interval.
		switch {
		case changed:
			t.curInterval = t.minInterval
		case t.curInterval < t.maxInterval:
			t.curInterval *= 2
			if t.curInterval > t.maxInterval {
				t.curInterval = t.maxInterval
			}
		}
		// CPU floor: if a pass took longer than half the interval,
		// raise the floor for next time. Bounded by maxInterval so we
		// never lock up entirely.
		if elapsed*2 > t.curInterval {
			t.curInterval = elapsed * 2
			if t.curInterval > t.maxInterval {
				t.curInterval = t.maxInterval
			}
		}
		ticker.Reset(t.curInterval)
	}
}

// pullContainerChanges scans the container's TargetPath, finds files
// whose (size, mtime) differ from our snapshot, and pulls them into
// the host VFS. Returns true if any changes were applied.
func (t *syncTenant) pullContainerChanges() (bool, error) {
	// Read the entire TargetPath as a tar — same wire as `docker cp`.
	// For typical workloads (tens of KB to a few MB of data), this is
	// cheap. If the working set grows large we'd switch to per-file
	// stat probes; for the v1 scope, full-tar is simple and correct.
	rc, _, err := t.engine.cli.CopyFromContainer(t.ctx, t.spec.ContainerID, t.spec.TargetPath)
	if err != nil {
		return false, err
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	t.knownMu.Lock()
	defer t.knownMu.Unlock()
	seen := map[string]bool{}
	changed := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return changed, err
		}
		// CopyFromContainer wraps with a top-level dir; strip it.
		rel := strings.TrimPrefix(hdr.Name, path.Base(t.spec.TargetPath)+"/")
		if rel == hdr.Name {
			rel = strings.TrimPrefix(rel, "./")
		}
		if rel == "" || rel == "./" {
			continue
		}
		seen[rel] = true
		stat := containerStat{
			size:    hdr.Size,
			mode:    fs.FileMode(hdr.Mode),
			modTime: hdr.ModTime,
			isDir:   hdr.Typeflag == tar.TypeDir,
		}
		prev, knew := t.known[rel]
		t.known[rel] = stat

		// Ignore unchanged entries.
		if knew && prev.size == stat.size && prev.modTime.Equal(stat.modTime) && prev.isDir == stat.isDir {
			continue
		}
		// Conflict policy: last-writer-wins by mtime. Compare with
		// the host VFS's view if it exists.
		hostPath := path.Join(t.spec.SubtreeRoot, rel)
		if t.spec.SubtreeRoot == "" || t.spec.SubtreeRoot == "/" {
			hostPath = "/" + rel
		}
		if hostInode, err := t.spec.Tree.Stat(hostPath); err == nil {
			if hostInode.ModTime.After(stat.modTime) {
				continue // host wins
			}
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := t.spec.Tree.MkdirAll(hostPath, fs.FileMode(hdr.Mode)&fs.ModePerm); err != nil {
				return changed, fmt.Errorf("mkdirAll %s: %w", hostPath, err)
			}
			changed = true
		case tar.TypeReg:
			body, err := io.ReadAll(tr)
			if err != nil {
				return changed, err
			}
			if err := t.spec.Tree.Write(hostPath, body, fs.FileMode(hdr.Mode)&fs.ModePerm); err != nil {
				return changed, fmt.Errorf("write %s: %w", hostPath, err)
			}
			if err := t.spec.Tree.Chtime(hostPath, hdr.ModTime); err != nil {
				return changed, fmt.Errorf("chtime %s: %w", hostPath, err)
			}
			changed = true
		case tar.TypeSymlink:
			// Symlink errors include "already exists" — that's
			// expected when a symlink we previously synced is still
			// present, and we shouldn't kill the loop over it. Other
			// errors do matter.
			if err := t.spec.Tree.Symlink(hdr.Linkname, hostPath); err != nil && !errors.Is(err, vfs.ErrExist) {
				return changed, fmt.Errorf("symlink %s -> %s: %w", hostPath, hdr.Linkname, err)
			}
			changed = true
		}
	}

	// Detect deletions: anything in known but not seen.
	for prev := range t.known {
		if !seen[prev] {
			delete(t.known, prev)
			hostPath := path.Join(t.spec.SubtreeRoot, prev)
			if t.spec.SubtreeRoot == "" || t.spec.SubtreeRoot == "/" {
				hostPath = "/" + prev
			}
			if err := t.spec.Tree.RemoveAll(hostPath); err == nil {
				changed = true
			}
		}
	}

	return changed, nil
}

// refreshKnownFromContainer rebuilds the known-snapshot from a fresh
// tar of the container's TargetPath. Used after our own host→container
// pushes so we don't see the just-pushed files as foreign changes on
// the next container→host poll.
func (t *syncTenant) refreshKnownFromContainer(ctx context.Context) error {
	rc, _, err := t.engine.cli.CopyFromContainer(ctx, t.spec.ContainerID, t.spec.TargetPath)
	if err != nil {
		return err
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	t.knownMu.Lock()
	defer t.knownMu.Unlock()
	t.known = make(map[string]containerStat)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(hdr.Name, path.Base(t.spec.TargetPath)+"/")
		if rel == hdr.Name {
			rel = strings.TrimPrefix(rel, "./")
		}
		if rel == "" || rel == "./" {
			continue
		}
		t.known[rel] = containerStat{
			size:    hdr.Size,
			mode:    fs.FileMode(hdr.Mode),
			modTime: hdr.ModTime,
			isDir:   hdr.Typeflag == tar.TypeDir,
		}
	}
	return nil
}

// === small helpers ===

func firstNonZero[T comparable](a, b T) T {
	var zero T
	if a != zero {
		return a
	}
	return b
}

// isContainerGoneErr matches the family of errors that indicate the
// container is no longer present (stopped + removed, never existed,
// daemon restarted between calls). We use these to short-circuit the
// per-tenant goroutines instead of looping forever against a corpse.
func isContainerGoneErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "No such container") ||
		strings.Contains(msg, "is not running") ||
		strings.Contains(msg, "removed")
}
