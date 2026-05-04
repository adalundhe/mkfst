package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	dockermount "github.com/docker/docker/api/types/mount"
	dockernetwork "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
)

// === one-shot containers ===
//
// RunOneShot creates an ephemeral container attached to this
// stack's bridge network, runs it to completion, captures
// stdout/stderr/exit code, and removes it. Used by workflow tasks
// that need to drive the stack (load tests, smoke tests,
// migrations, custom test scripts).
//
// Containers are labeled mkfst.role=oneshot so crash recovery
// finds and reaps them on engine restart. Concurrent invocations
// are bounded by the stack's MaxConcurrentOneShots cap.

// OneShotOpts configures RunOneShot.
type OneShotOpts struct {
	Image      string            // required
	Cmd        []string          // overrides image CMD
	Entrypoint []string          // overrides ENTRYPOINT
	Env        map[string]string
	WorkDir    string
	User       string
	Mounts     []ServiceMount
	Aliases    []string
	Timeout    time.Duration
	Name       string            // optional; auto-generated otherwise

	// Stdin: bounded byte payload piped to the container's stdin.
	// Mutually exclusive with StdinReader. For inputs > 16 MiB
	// prefer StdinReader to avoid full in-memory buffering.
	Stdin []byte

	// StdinReader: streaming stdin source. Bounded by
	// StdinMaxBytes (default 16 MiB) — reads past that are
	// truncated with an error logged via the monitor.
	StdinReader io.Reader

	// StdinMaxBytes caps reads from StdinReader. 0 = default 16 MiB.
	StdinMaxBytes int64

	// MaxOutputBytes caps the in-memory capture of stdout/stderr.
	// 0 = default 10 MiB. Bytes past the cap are SPOOLED to a per-
	// stack docker volume (see Stack.SpoolMount()), not silently
	// dropped — the result records the spool path so callers can
	// retrieve the full output post-hoc.
	MaxOutputBytes int64

	// LeaveContainer disables the post-run container removal so
	// operators can inspect a failed one-shot. The container is
	// labeled mkfst.debug=true and discoverable via
	// Engine.ListDebugContainers.
	LeaveContainer bool

	Labels map[string]string // user labels merged on top of mkfst's
}

// OneShotResult is the captured outcome.
type OneShotResult struct {
	ContainerID string
	ExitCode    int

	// Stdout / Stderr are the in-memory capture, capped at
	// MaxOutputBytes. When the container produced more, the bytes
	// past the cap are written to the spool path (below) and the
	// truncated flag is set.
	Stdout []byte
	Stderr []byte

	StdoutTruncated bool
	StderrTruncated bool

	// SpoolPath, when non-empty, is the host filesystem path
	// where the full untruncated output lives. The path is under
	// the per-stack spool directory and removed when the operator
	// calls Stack.SweepSpool. Empty when no spooling occurred.
	SpoolStdoutPath string
	SpoolStderrPath string

	Duration time.Duration
}

// ExecOpts configures Exec.
type ExecOpts struct {
	Cmd     []string
	Env     map[string]string
	User    string
	WorkDir string
	Stdin   []byte
	Timeout time.Duration
}

// ExecResult is one Exec invocation outcome.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Duration time.Duration
}

// SetMaxConcurrentOneShots caps in-flight RunOneShot calls.
func (s *Stack) SetMaxConcurrentOneShots(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oneShotSem == nil || cap(s.oneShotSem) != n {
		s.oneShotSem = make(chan struct{}, n)
	}
}

// RunOneShot creates + runs + removes an ephemeral container on
// this stack's network.
func (s *Stack) RunOneShot(ctx context.Context, opts OneShotOpts) (*OneShotResult, error) {
	if s.State() != StackUp {
		return nil, fmt.Errorf("RunOneShot: stack not up (state=%s)", s.State())
	}
	if opts.Image == "" {
		return nil, fmt.Errorf("%w: image is required", ErrInvalidConfig)
	}
	// Policy check — scoped to the stack name so operators can grant
	// `stack.run_oneshot` on a per-stack basis.
	if err := s.engine.opts.Policy.Check(ctx, "stack.run_oneshot", s.name); err != nil {
		return nil, err
	}
	// Resolve image to digest-pinned form (and reject if pinning
	// is required but the ref isn't pinned).
	resolvedImage, err := s.resolveImage(opts.Image)
	if err != nil {
		return nil, err
	}
	opts.Image = resolvedImage

	// Concurrency cap.
	s.mu.RLock()
	sem := s.oneShotSem
	s.mu.RUnlock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	netName := s.network.Name()
	engineID := s.engine.opts.EngineID

	// Build config.
	envSlice := make([]string, 0, len(opts.Env))
	for k, v := range opts.Env {
		envSlice = append(envSlice, k+"="+v)
	}
	labels := stackLabels(engineID, s.id, s.name, KindService)
	labels[LabelRole] = "oneshot"
	for k, v := range opts.Labels {
		labels[k] = v
	}
	cfg := &dockercontainer.Config{
		Image:      opts.Image,
		Env:        envSlice,
		WorkingDir: opts.WorkDir,
		User:       opts.User,
		Labels:     labels,
		Tty:        false,
		OpenStdin:  len(opts.Stdin) > 0,
		StdinOnce:  len(opts.Stdin) > 0,
		AttachStdin: len(opts.Stdin) > 0,
	}
	if len(opts.Cmd) > 0 {
		cfg.Cmd = strslice.StrSlice(opts.Cmd)
	}
	if len(opts.Entrypoint) > 0 {
		cfg.Entrypoint = strslice.StrSlice(opts.Entrypoint)
	}

	host := &dockercontainer.HostConfig{
		AutoRemove:  false,
		NetworkMode: dockercontainer.NetworkMode(netName),
	}
	for _, m := range opts.Mounts {
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
	endpoint := &dockernetwork.EndpointSettings{
		Aliases: opts.Aliases,
	}
	netCfg := &dockernetwork.NetworkingConfig{
		EndpointsConfig: map[string]*dockernetwork.EndpointSettings{
			netName: endpoint,
		},
	}

	// Name.
	name := opts.Name
	if name == "" {
		id, _ := newID()
		name = "mkfst-oneshot-" + s.id + "-" + id[:8]
	}

	// Tag debug containers so operators can find them later.
	if opts.LeaveContainer {
		labels["mkfst.debug"] = "true"
	}

	startedAt := time.Now()
	created, err := s.engine.cli.ContainerCreate(ctx, cfg, host, netCfg, nil, name)
	if err != nil {
		return nil, fmt.Errorf("RunOneShot create: %w", err)
	}
	if !opts.LeaveContainer {
		defer func() {
			rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = s.engine.cli.ContainerRemove(rmCtx, created.ID, dockercontainer.RemoveOptions{Force: true})
		}()
	}

	// Decide stdin path: bounded []byte vs streaming io.Reader.
	hasStdin := len(opts.Stdin) > 0 || opts.StdinReader != nil
	if len(opts.Stdin) > 0 && opts.StdinReader != nil {
		return nil, fmt.Errorf("%w: Stdin and StdinReader are mutually exclusive", ErrInvalidConfig)
	}

	// Attach for stdin / log capture.
	attachCtx, attachCancel := context.WithCancel(ctx)
	defer attachCancel()
	hijack, err := s.engine.cli.ContainerAttach(attachCtx, created.ID, dockercontainer.AttachOptions{
		Stream: true,
		Stdin:  hasStdin,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("RunOneShot attach: %w", err)
	}
	defer hijack.Close()

	if err := s.engine.cli.ContainerStart(ctx, created.ID, dockercontainer.StartOptions{}); err != nil {
		return nil, fmt.Errorf("RunOneShot start: %w", err)
	}

	// Pipe stdin.
	if hasStdin {
		stdinCap := opts.StdinMaxBytes
		if stdinCap <= 0 {
			stdinCap = 16 << 20 // 16 MiB default
		}
		if len(opts.Stdin) > 0 {
			if int64(len(opts.Stdin)) > stdinCap {
				return nil, fmt.Errorf("RunOneShot: Stdin %d bytes > cap %d", len(opts.Stdin), stdinCap)
			}
			if _, werr := hijack.Conn.Write(opts.Stdin); werr != nil {
				return nil, fmt.Errorf("RunOneShot: stdin write: %w", werr)
			}
		} else {
			limited := newLimitedStdin(opts.StdinReader, stdinCap)
			if _, werr := io.Copy(hijack.Conn, limited); werr != nil &&
				!errors.Is(werr, ErrSpoolFull) && !errors.Is(werr, io.EOF) {
				return nil, fmt.Errorf("RunOneShot: stdin stream: %w", werr)
			}
		}
		_ = hijack.CloseWrite()
	}

	// Bounded capture-then-spool for stdout/stderr.
	maxOut := opts.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = 10 << 20 // 10 MiB
	}
	spoolDir := s.spoolDirFor(name)
	hardCap := s.spoolHardCap()
	stdoutW := newCapSpoolWriter(maxOut, spoolDir, "stdout", hardCap)
	stderrW := newCapSpoolWriter(maxOut, spoolDir, "stderr", hardCap)

	logsDone := make(chan error, 1)
	go func() {
		_, derr := demuxDockerStream(stdoutW, stderrW, hijack.Reader)
		_ = stdoutW.Close()
		_ = stderrW.Close()
		logsDone <- derr
	}()
	joinLogs := func() {
		select {
		case <-logsDone:
		case <-time.After(5 * time.Second):
		}
	}
	defer joinLogs()

	// Wait for exit.
	statusCh, errCh := s.engine.cli.ContainerWait(ctx, created.ID, dockercontainer.WaitConditionNotRunning)
	var exitCode int
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errCh:
		if err != nil {
			return nil, fmt.Errorf("RunOneShot wait: %w", err)
		}
	case status := <-statusCh:
		exitCode = int(status.StatusCode)
		if status.Error != nil {
			return nil, fmt.Errorf("RunOneShot container error: %s", status.Error.Message)
		}
	}

	return &OneShotResult{
		ContainerID:     created.ID,
		ExitCode:        exitCode,
		Stdout:          stdoutW.Bytes(),
		Stderr:          stderrW.Bytes(),
		StdoutTruncated: stdoutW.Truncated(),
		StderrTruncated: stderrW.Truncated(),
		SpoolStdoutPath: stdoutW.SpoolPath(),
		SpoolStderrPath: stderrW.SpoolPath(),
		Duration:        time.Since(startedAt),
	}, nil
}

// Exec runs cmd inside the named service's replica. Captures
// stdout/stderr/exit. Honors ExecOpts.Timeout via ctx.
func (s *Stack) Exec(ctx context.Context, serviceName string, replica int, opts ExecOpts) (*ExecResult, error) {
	if s.State() != StackUp {
		return nil, fmt.Errorf("Exec: stack not up (state=%s)", s.State())
	}
	if len(opts.Cmd) == 0 {
		return nil, fmt.Errorf("%w: cmd required", ErrInvalidConfig)
	}
	if err := s.engine.opts.Policy.Check(ctx, "stack.exec", s.name); err != nil {
		return nil, err
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	insts := s.containerByService(serviceName)
	if len(insts) == 0 {
		return nil, fmt.Errorf("Exec: service %q has no containers", serviceName)
	}
	if replica < 0 || replica >= len(insts) {
		return nil, fmt.Errorf("Exec: replica %d out of range [0,%d)", replica, len(insts))
	}
	inst := insts[replica]

	envSlice := make([]string, 0, len(opts.Env))
	for k, v := range opts.Env {
		envSlice = append(envSlice, k+"="+v)
	}

	startedAt := time.Now()
	exec, err := s.engine.cli.ContainerExecCreate(ctx, inst.id, dockercontainer.ExecOptions{
		Cmd:          opts.Cmd,
		Env:          envSlice,
		User:         opts.User,
		WorkingDir:   opts.WorkDir,
		AttachStdin:  len(opts.Stdin) > 0,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("Exec create: %w", err)
	}
	resp, err := s.engine.cli.ContainerExecAttach(ctx, exec.ID, dockercontainer.ExecStartOptions{})
	if err != nil {
		return nil, fmt.Errorf("Exec attach: %w", err)
	}
	defer resp.Close()
	if len(opts.Stdin) > 0 {
		_, _ = resp.Conn.Write(opts.Stdin)
		_ = resp.CloseWrite()
	}
	var stdout, stderr bytes.Buffer
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		_, _ = demuxDockerStream(&stdout, &stderr, resp.Reader)
	}()
	select {
	case <-doneCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	insp, err := s.engine.cli.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return nil, fmt.Errorf("Exec inspect: %w", err)
	}
	return &ExecResult{
		ExitCode: insp.ExitCode,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		Duration: time.Since(startedAt),
	}, nil
}

// === docker stream demux ===
//
// docker exec/attach streams use 8-byte frame headers when not in
// TTY mode:
//
//   [0]    1=stdout, 2=stderr (0=stdin, never seen in our reads)
//   [1..3] padding
//   [4..7] payload length, big-endian uint32
//   [8..]  payload

func demuxDockerStream(stdout, stderr io.Writer, src io.Reader) (int64, error) {
	var hdr [8]byte
	var total int64
	for {
		_, err := io.ReadFull(src, hdr[:])
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			// Some streams (TTY mode) are unframed; copy whatever's
			// left to stdout and bail.
			n, _ := io.Copy(stdout, src)
			return total + n, nil
		}
		size := int64(uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7]))
		if size < 0 {
			return total, errors.New("demuxDockerStream: negative size")
		}
		if size == 0 {
			continue
		}
		var dst io.Writer = stdout
		if hdr[0] == 2 {
			dst = stderr
		}
		n, err := io.CopyN(dst, src, size)
		total += n
		if err != nil {
			return total, err
		}
	}
}

// === unused-symbol guards ===
var (
	_ = strconv.Itoa
	_ = sync.Once{}
)
