package vfs

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrFUSEUnavailable is returned by Mount on platforms where no FUSE backend
// is compiled in, or when the runtime FUSE driver is missing (no /dev/fuse,
// macFUSE/WinFsp not installed, etc.).
var ErrFUSEUnavailable = errors.New("vfs: FUSE backend unavailable on this platform")

// ErrAlreadyMounted is returned by Mount when called on a Tree that already
// holds an active FUSE mount. Each Tree may be mounted at one location at
// a time; mount a fresh tree (or unmount first) for additional mountpoints.
var ErrAlreadyMounted = errors.New("vfs: tree is already mounted")

// MountOpts configures a FUSE mount of a Tree.
type MountOpts struct {
	// Mountpoint is the absolute host directory the VFS will appear at. The
	// directory must already exist (FUSE does not create it) and should be
	// empty — its contents are masked while the mount is active.
	Mountpoint string
	// Name appears in /etc/mtab, df, mount listings. Defaults to "mkfst-vfs".
	Name string
	// AllowOther grants non-owner UIDs access to the mount. Requires
	// `user_allow_other` in /etc/fuse.conf on Linux. Default false (only the
	// mounting UID can read/write).
	AllowOther bool
	// Debug enables verbose FUSE protocol logging from the backend. Cheap
	// to leave on for development; turn off in production.
	Debug bool
	// ReadOnly mounts the FUSE filesystem read-only at the kernel level —
	// host processes cannot create/modify entries. The Tree's API methods
	// (Write, Mkdir, Remove) still work; this only constrains the
	// kernel-side view. Useful when sharing a mount with untrusted
	// processes.
	ReadOnly bool
}

// Mount is the handle returned by Tree.Mount. Callers Wait for the mount to
// finish (either externally unmounted or via cancellation) and Unmount to
// trigger a graceful teardown.
type Mount struct {
	mountpoint string

	// driver is the platform-specific implementation. It encapsulates the
	// actual FUSE server lifecycle (go-fuse on Linux, cgofuse on Darwin/
	// Windows) so the public API stays uniform.
	driver mountDriver

	// done is closed when Wait would unblock — i.e. the FUSE server has
	// exited (clean unmount or external).
	done chan struct{}

	once sync.Once
	err  error

	// owner backlinks to the Tree for unmount-time cleanup of mount state.
	owner *Tree
}

// mountDriver is the platform-internal contract every FUSE backend
// implements. The public Mount type delegates to it.
type mountDriver interface {
	// unmount issues a kernel-level unmount and is safe to call multiple
	// times. Returns the final error (nil for clean unmount).
	unmount() error
	// lazyUnmount forces the kernel to detach the mount even if a process
	// is currently holding a reference (the kernel finalizes when the
	// last ref drops). The escape hatch for the "test exec'd a process,
	// process exited, kernel still holds CWD ref for a few ms" race that
	// would otherwise EBUSY a normal unmount and pin the serving
	// goroutine on Wait(). Best-effort — never errors.
	lazyUnmount()
	// wait blocks until the FUSE server loop returns, then returns the
	// loop's terminal error.
	wait() error
}

// Mountpoint returns the absolute host path the Tree is mounted at.
func (m *Mount) Mountpoint() string { return m.mountpoint }

// unmountSettleTimeout is how long Unmount waits for the FUSE goroutine
// to exit cleanly before falling back to a lazy unmount. The kernel
// usually finalizes within milliseconds; 5s is plenty of headroom.
const unmountSettleTimeout = 5 * time.Second

// Unmount triggers a graceful unmount and blocks until the FUSE serving
// goroutine has fully exited. The wait matters: without it, callers
// (and Go's testing.TempDir cleanup, and os.RemoveAll) can race the
// kernel's mount-table finalization and either fail with EBUSY or hang
// trying to remove the mountpoint dir.
//
// Robustness: if the normal unmount-syscall path doesn't release within
// unmountSettleTimeout — typically because some host process briefly
// held a CWD ref into the mount and the kernel returned EBUSY — we
// escalate to a lazy unmount. The kernel detaches the mount immediately
// from the namespace and finalizes when the last ref drops; the serving
// goroutine then sees EOF on /dev/fuse and exits, closing m.done.
//
// Idempotent — the first call performs the work; later calls return the
// same recorded error without re-issuing the unmount syscall.
func (m *Mount) Unmount() error {
	m.once.Do(func() {
		err := m.driver.unmount()
		m.err = err

		select {
		case <-m.done:
		case <-time.After(unmountSettleTimeout):
			// Normal path didn't release in time. Force the lazy
			// detach; goroutine exit follows once the kernel is done.
			m.driver.lazyUnmount()
			<-m.done
		}

		// Detach from the owning Tree so a fresh Mount call can succeed.
		m.owner.mu.Lock()
		if m.owner.mount == m {
			m.owner.mount = nil
		}
		m.owner.mu.Unlock()
	})
	return m.err
}

// Wait blocks until the FUSE server loop returns (clean unmount or external
// teardown). After Wait returns, the mount handle is no longer usable.
func (m *Mount) Wait() error {
	<-m.done
	return m.err
}

// Mount registers the Tree as a FUSE filesystem at opts.Mountpoint and
// returns a handle for lifecycle control. The mount runs in a background
// goroutine; callers Wait or Unmount on the returned handle.
//
// ctx, when cancelled, triggers a graceful Unmount — handy for tying the
// mount lifecycle to a process-wide context.
//
// Returns ErrAlreadyMounted if the Tree already has an active mount, or
// ErrFUSEUnavailable on platforms without a FUSE backend (or when the
// runtime driver is missing).
func (t *Tree) Mount(ctx context.Context, opts MountOpts) (*Mount, error) {
	t.mu.Lock()
	if t.mount != nil {
		t.mu.Unlock()
		return nil, ErrAlreadyMounted
	}
	t.mu.Unlock()

	if opts.Name == "" {
		opts.Name = "mkfst-vfs"
	}

	driver, done, err := newMountDriver(t, opts)
	if err != nil {
		return nil, err
	}

	m := &Mount{
		mountpoint: opts.Mountpoint,
		driver:     driver,
		done:       done,
		owner:      t,
	}

	t.mu.Lock()
	t.mount = m
	t.mu.Unlock()

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = m.Unmount()
			case <-done:
			}
		}()
	}

	return m, nil
}

// CurrentMount returns the active mount handle for the Tree, or nil if not
// mounted.
func (t *Tree) CurrentMount() *Mount {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.mount
}
