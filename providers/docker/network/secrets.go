package network

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Secret is a value the Stack will materialize as a file on the
// host (tmpfs-backed where possible) and bind-mount into containers
// that reference it via UseSecret. The bytes are zeroed in memory
// after Down to limit the secret-residence window.
type Secret struct {
	Name  string
	Value []byte
}

// secretsDir returns the per-stack directory where secrets are
// materialized. Honors XDG_RUNTIME_DIR when set (tmpfs on systemd
// systems); falls back to /tmp on Linux without XDG; falls back to
// os.TempDir on other OSes.
func (s *Stack) secretsDir() string {
	if s.cachedSecretsDir != "" {
		return s.cachedSecretsDir
	}
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		switch runtime.GOOS {
		case "linux":
			base = "/tmp"
		default:
			base = os.TempDir()
		}
	}
	dir := filepath.Join(base, "mkfst-network", "stack-"+s.id)
	s.cachedSecretsDir = dir
	return dir
}

// materializeSecrets writes each Secret to a per-stack directory
// and records the host path so buildContainerConfig can mount it.
func (s *Stack) materializeSecrets(_ ignoredCtx) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.secrets) == 0 {
		return nil
	}
	dir := s.secretsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir secrets dir: %w", err)
	}
	if s.secretPaths == nil {
		s.secretPaths = map[string]string{}
	}
	for name, sec := range s.secrets {
		// Use a name-derived filename so paths are predictable
		// across re-Up cycles. Files are 0400 and only readable by
		// the calling user; the container mounts read-only.
		path := filepath.Join(dir, "secret-"+name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o400)
		if err != nil {
			return fmt.Errorf("create secret %q: %w", name, err)
		}
		if _, err := f.Write(sec.Value); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return fmt.Errorf("write secret %q: %w", name, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close secret %q: %w", name, err)
		}
		s.secretPaths[name] = path
	}
	return nil
}

// secretHostPath looks up the materialized host path for a secret.
// Caller must hold s.mu (read or write).
func (s *Stack) secretHostPath(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.secretPaths[name]
	return p, ok
}

// cleanupSecrets removes the per-stack secrets directory and zeroes
// the in-memory value bytes (best-effort — the GC may still hold
// references).
func (s *Stack) cleanupSecrets() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.secrets) == 0 {
		return nil
	}
	for name, sec := range s.secrets {
		// Unlock the page (best-effort) before zeroing so the
		// kernel doesn't keep them resident any longer than
		// necessary.
		_ = unlockSecretPages(sec.Value)
		for i := range sec.Value {
			sec.Value[i] = 0
		}
		_ = name
	}
	dir := s.cachedSecretsDir
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rm secrets dir %s: %w", dir, err)
	}
	s.secretPaths = nil
	s.cachedSecretsDir = ""
	return nil
}

// ignoredCtx is a placeholder — secret materialization is local-FS
// only, no context needed. Matches the call signature used in
// stack.go for symmetry with future-proofing.
type ignoredCtx interface{}
