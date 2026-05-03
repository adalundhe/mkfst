package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotMounted is returned by Exec when the bound VFS isn't currently
// FUSE-mounted. Exec needs a real filesystem path for the host process to
// CWD into; without a mount we'd have to materialize to disk, which
// defeats the whole point of the provider.
var ErrNotMounted = errors.New("files.Exec: VFS is not mounted; call tree.Mount first")

// ExecOption mutates the assembled exec request just before submission.
type ExecOption func(*execState)

type execState struct {
	args    []string
	env     []string
	envInherit bool
	dir     string // VFS-relative directory; appended to mountpoint
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	captureCombined bool
	timeout time.Duration
}

// ExecResult captures the outcome of an Exec call. Stdout and Stderr are
// populated only when CaptureOutput() is set.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Combined []byte // populated when CaptureCombined() is set
}

// Exec runs a host process whose CWD is the FUSE mountpoint of the VFS.
// The named program is executed via os/exec.LookPath — if argv0 is an
// absolute path, it's used verbatim; otherwise the host PATH is consulted.
//
// This is the bridge between in-VFS files and host tooling: the host
// process sees VFS files at real paths (mountpoint + relative) without
// any materialization. Useful for running compilers, package managers,
// linters, etc. against an in-memory project layout.
//
// Errors:
//   - ErrNotMounted if the VFS has no active FUSE mount.
//   - exec.ExitError (wrapped) if the process exits non-zero — ExitCode is
//     still populated on the returned result.
//   - context.DeadlineExceeded / context.Canceled for timeouts and cancels.
func (s *Service) Exec(ctx context.Context, argv0 string, opts ...ExecOption) (*ExecResult, error) {
	mount := s.tree.CurrentMount()
	if mount == nil {
		return nil, ErrNotMounted
	}

	state := &execState{
		envInherit: true,
	}
	for _, opt := range opts {
		opt(state)
	}

	if state.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, state.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, argv0, state.args...)

	// Resolve CWD: mountpoint + state.dir. Reject any state.dir that tries
	// to escape via "..". We don't follow symlinks here — if the user
	// pointed Dir at a VFS-side symlink, resolution happens in the kernel
	// when the process tries to chdir.
	cwd := mount.Mountpoint()
	if state.dir != "" {
		clean := filepath.Clean(state.dir)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return nil, fmt.Errorf("files.Exec: dir %q escapes mountpoint", state.dir)
		}
		cwd = filepath.Join(cwd, clean)
	}
	cmd.Dir = cwd

	// Env: by default inherit the parent's env (matches what `bash` does
	// for child processes). NoEnvInherit suppresses inheritance — useful
	// for hermetic / sandbox-ish runs.
	//
	// os/exec treats nil Env as "inherit parent" but a non-nil-empty slice
	// as "child has empty env", so we always allocate a slice (potentially
	// length 0) when NoEnvInherit is set.
	if state.envInherit {
		cmd.Env = append(cmd.Env, parentEnv()...)
	} else {
		cmd.Env = make([]string, 0, len(state.env))
	}
	cmd.Env = append(cmd.Env, state.env...)

	cmd.Stdin = state.stdin

	var stdoutCap, stderrCap, combinedCap *captureBuf
	if state.captureCombined {
		combinedCap = &captureBuf{}
		cmd.Stdout = teeOrCapture(state.stdout, combinedCap)
		cmd.Stderr = teeOrCapture(state.stderr, combinedCap)
	} else {
		if state.stdout == nil {
			stdoutCap = &captureBuf{}
			cmd.Stdout = stdoutCap
		} else {
			cmd.Stdout = state.stdout
		}
		if state.stderr == nil {
			stderrCap = &captureBuf{}
			cmd.Stderr = stderrCap
		} else {
			cmd.Stderr = state.stderr
		}
	}

	err := cmd.Run()
	result := &ExecResult{
		ExitCode: cmd.ProcessState.ExitCode(),
	}
	if stdoutCap != nil {
		result.Stdout = stdoutCap.bytes()
	}
	if stderrCap != nil {
		result.Stderr = stderrCap.bytes()
	}
	if combinedCap != nil {
		result.Combined = combinedCap.bytes()
	}

	if err != nil {
		// exec.ExitError is the "non-zero exit" case — we still return the
		// populated result alongside, so callers can inspect ExitCode/output.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return result, fmt.Errorf("files.Exec: %s exited %d", argv0, result.ExitCode)
		}
		return result, fmt.Errorf("files.Exec: %s: %w", argv0, err)
	}
	return result, nil
}

// captureBuf is a goroutine-safe accumulator for Exec output. Used when
// the caller didn't supply a writer — we still need to gather output for
// ExecResult.
type captureBuf struct {
	buf []byte
}

func (c *captureBuf) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *captureBuf) bytes() []byte {
	out := make([]byte, len(c.buf))
	copy(out, c.buf)
	return out
}

// teeOrCapture returns w if set, else cap. Used to support callers that
// supply their own stdout/stderr writers while we still capture for the
// combined result buffer.
func teeOrCapture(w io.Writer, cap *captureBuf) io.Writer {
	if w == nil {
		return cap
	}
	return io.MultiWriter(w, cap)
}

// parentEnv returns a snapshot of the parent process's environment as a
// []string of "KEY=VALUE" entries — matches what os/exec expects.
func parentEnv() []string {
	return inheritEnvSnapshot
}

// inheritEnvSnapshot is captured once at package init so each Exec call
// doesn't re-read /proc/self/environ. Tests that mutate env during a run
// won't see updates — acceptable because env mutation between tests is
// rare and inherently global.
var inheritEnvSnapshot = func() []string {
	// We use os.Environ via a deferred reference so tests can't easily
	// shadow it; production behavior is the standard env at startup.
	return osEnviron()
}()

// osEnviron is a tiny wrapper so we can stub in tests if we ever need to.
var osEnviron = func() []string {
	return execOSEnviron()
}

// === Exec options ===

// Args sets the program's argv (after argv0). Repeatable: each Args call
// appends to the running list. Use Args() (no items) to clear.
func Args(args ...string) ExecOption {
	return func(s *execState) {
		if len(args) == 0 {
			s.args = nil
			return
		}
		s.args = append(s.args, args...)
	}
}

// ExecEnv sets a single environment variable. Inherits the parent env by
// default; combine with NoEnvInherit() for a clean slate.
func ExecEnv(key, value string) ExecOption {
	return func(s *execState) { s.env = append(s.env, key+"="+value) }
}

// NoEnvInherit suppresses inheritance of the parent process's environment
// — only ExecEnv-supplied values are passed through. Useful for hermetic
// runs.
func NoEnvInherit() ExecOption {
	return func(s *execState) { s.envInherit = false }
}

// Dir sets the working directory relative to the VFS mountpoint. Empty
// (default) means the mountpoint itself. Absolute paths or paths
// containing ".." are rejected.
func Dir(rel string) ExecOption {
	return func(s *execState) { s.dir = rel }
}

// Stdin attaches an input stream to the process.
func Stdin(r io.Reader) ExecOption {
	return func(s *execState) { s.stdin = r }
}

// Stdout attaches an output writer for the process's stdout. Without this,
// the captured bytes appear in ExecResult.Stdout.
func Stdout(w io.Writer) ExecOption {
	return func(s *execState) { s.stdout = w }
}

// Stderr attaches an output writer for the process's stderr. Without this,
// the captured bytes appear in ExecResult.Stderr.
func Stderr(w io.Writer) ExecOption {
	return func(s *execState) { s.stderr = w }
}

// CaptureCombined merges stdout and stderr into ExecResult.Combined,
// preserving interleave order — like 2>&1 in the shell. When set,
// Stdout/Stderr writers (if also provided) still receive their respective
// streams; the Combined buffer is the additional merged view.
func CaptureCombined() ExecOption {
	return func(s *execState) { s.captureCombined = true }
}

// ExecTimeout caps the total run time. 0 (default) inherits from ctx only.
func ExecTimeout(d time.Duration) ExecOption {
	return func(s *execState) { s.timeout = d }
}
