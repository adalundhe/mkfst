package network

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
)

// === per-stack spool directory helpers ===

// spoolDirFor returns the per-one-shot spool directory under the
// stack's spool root. The directory is created lazily by the
// capSpoolWriter on first overflow write, so unused one-shots
// don't leave breadcrumbs.
func (s *Stack) spoolDirFor(oneShotName string) string {
	root := s.cachedSecretsDir
	// We co-locate spool with the secrets dir's parent (per-stack
	// scratch root) to keep all per-stack on-disk state in one
	// place. If secrets weren't materialized, derive from the
	// stack ID directly.
	if root == "" {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = os.TempDir()
		}
		root = filepath.Join(base, "mkfst-network", "stack-"+s.id)
	}
	return filepath.Join(filepath.Dir(root), "spool-"+s.id, oneShotName)
}

// spoolHardCap returns the per-one-shot disk byte cap for spooled
// output. 0 = unbounded (default for v1; operators can lock down
// via Stack.SetSpoolHardCap).
func (s *Stack) spoolHardCap() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.spoolCapBytes
}

// SetSpoolHardCap caps total spool bytes per one-shot stream
// (stdout or stderr). 0 = unbounded. Past the cap, the spool
// writer returns ErrSpoolFull and the container's output is
// truncated mid-write.
func (s *Stack) SetSpoolHardCap(bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spoolCapBytes = bytes
}

// SweepSpool removes every spool directory under this stack's
// scratch root. Call periodically (or after Down) to reclaim
// disk. Returns the number of directories removed.
func (s *Stack) SweepSpool() (int, error) {
	root := s.cachedSecretsDir
	if root == "" {
		return 0, nil
	}
	parent := filepath.Dir(root)
	matches, err := filepath.Glob(filepath.Join(parent, "spool-"+s.id, "*"))
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, m := range matches {
		if rmErr := os.RemoveAll(m); rmErr != nil {
			continue
		}
		removed++
	}
	return removed, nil
}

// === capture-then-spool writer ===
//
// capSpoolWriter is an io.Writer that buffers up to capBytes in
// memory; subsequent writes are streamed to a spool file on disk.
// Both the captured-bytes view and the spool file are available
// after the underlying source closes.
//
// Use case: container stdout / stderr capture for one-shots. The
// in-memory portion is what gets returned to the caller (and serves
// as the workflow node's output bytes), capped to bound the
// process's RSS. The spool file holds everything past the cap so
// operators can retrieve full output for debugging.

type capSpoolWriter struct {
	cap        int64
	written    atomic.Int64 // total bytes written
	buf        []byte       // in-memory portion (≤ cap)
	spoolPath  string
	spoolFile  *os.File
	truncated  bool
	hardCap    int64 // refuse writes past this on the spool too; 0 = unbounded
	hardErr    error
}

// newCapSpoolWriter constructs a writer with the given in-memory
// cap. Spooling is engaged on the first write past `cap`. spoolDir
// is created on demand. hardCap (when > 0) bounds the total bytes
// written across memory + spool — past that we refuse writes
// (Write returns ErrSpoolFull).
func newCapSpoolWriter(cap int64, spoolDir, name string, hardCap int64) *capSpoolWriter {
	if cap <= 0 {
		cap = 10 << 20 // 10 MiB default
	}
	return &capSpoolWriter{
		cap:       cap,
		buf:       make([]byte, 0, 64<<10), // 64 KiB initial alloc
		spoolPath: filepath.Join(spoolDir, name),
		hardCap:   hardCap,
	}
}

// ErrSpoolFull is returned by Write when hardCap is exceeded.
var ErrSpoolFull = errors.New("spool: hard cap exceeded")

func (w *capSpoolWriter) Write(p []byte) (int, error) {
	if w.hardErr != nil {
		return 0, w.hardErr
	}
	prev := w.written.Load()
	want := prev + int64(len(p))
	if w.hardCap > 0 && want > w.hardCap {
		w.hardErr = ErrSpoolFull
		return 0, w.hardErr
	}
	// Fill memory first, up to cap.
	if int64(len(w.buf)) < w.cap {
		room := w.cap - int64(len(w.buf))
		take := int64(len(p))
		if take > room {
			take = room
		}
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		w.written.Add(take)
		if len(p) == 0 {
			return int(take), nil
		}
		w.truncated = true
	}
	// Past cap → spool.
	if err := w.openSpool(); err != nil {
		return 0, err
	}
	n, err := w.spoolFile.Write(p)
	w.written.Add(int64(n))
	return n, err
}

func (w *capSpoolWriter) openSpool() error {
	if w.spoolFile != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(w.spoolPath), 0o700); err != nil {
		return err
	}
	f, err := os.Create(w.spoolPath)
	if err != nil {
		return err
	}
	w.spoolFile = f
	return nil
}

// Close finalizes the spool file. Idempotent.
func (w *capSpoolWriter) Close() error {
	if w.spoolFile != nil {
		err := w.spoolFile.Close()
		w.spoolFile = nil
		return err
	}
	return nil
}

// Bytes returns a copy of the in-memory portion.
func (w *capSpoolWriter) Bytes() []byte {
	out := make([]byte, len(w.buf))
	copy(out, w.buf)
	return out
}

// Truncated reports whether output exceeded the in-memory cap.
func (w *capSpoolWriter) Truncated() bool { return w.truncated }

// SpoolPath returns the on-disk path of the overflow file, or ""
// if nothing was spooled.
func (w *capSpoolWriter) SpoolPath() string {
	if !w.truncated {
		return ""
	}
	return w.spoolPath
}

// === bounded stdin reader ===
//
// limitedStdin wraps an io.Reader with an absolute byte cap. Past
// the cap, Read returns 0 + ErrSpoolFull so the caller can decide
// to surface a clear error rather than hang.

type limitedStdin struct {
	src io.Reader
	cap int64
	n   int64
}

func newLimitedStdin(src io.Reader, cap int64) *limitedStdin {
	return &limitedStdin{src: src, cap: cap}
}

func (l *limitedStdin) Read(p []byte) (int, error) {
	if l.n >= l.cap {
		return 0, ErrSpoolFull
	}
	rem := l.cap - l.n
	if int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := l.src.Read(p)
	l.n += int64(n)
	return n, err
}
