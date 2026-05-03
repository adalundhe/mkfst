package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"mkfst/providers/vfs"
)

// HashAlgorithm enumerates supported content-hash algorithms for Hash and
// VerifySHA256. Constants below.
type HashAlgorithm string

const (
	HashSHA256 HashAlgorithm = "sha256"
)

// Copy duplicates the file at src to dst within the VFS. Parents of dst
// are created as needed. Mode is preserved from src; mtime gets a fresh
// stamp (matches POSIX cp). Errors if src is a directory.
func (s *Service) Copy(src, dst string) error {
	srcInode, err := s.tree.Stat(src)
	if err != nil {
		return fmt.Errorf("files.Copy: stat %s: %w", src, err)
	}
	if srcInode.IsDir() {
		return fmt.Errorf("files.Copy: %s: %w", src, vfs.ErrIsDir)
	}
	body, err := s.tree.Read(src)
	if err != nil {
		return fmt.Errorf("files.Copy: read %s: %w", src, err)
	}
	if err := s.tree.Write(dst, body, srcInode.Mode); err != nil {
		return fmt.Errorf("files.Copy: write %s: %w", dst, err)
	}
	return nil
}

// Move renames src to dst within the VFS. It's an atomic rename for
// in-memory entries; for cross-overlay moves (e.g. host-overlay file →
// memory location) we copy-then-remove because the underlying VFS rename
// can't move from overlay-only to memory-only in one step.
func (s *Service) Move(src, dst string) error {
	if err := s.tree.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, vfs.ErrNotExist) {
		// Try the copy/remove fallback for overlay-side sources where
		// Rename can't directly relocate.
		if cpErr := s.Copy(src, dst); cpErr == nil {
			if rmErr := s.tree.Remove(src); rmErr == nil {
				return nil
			} else {
				return fmt.Errorf("files.Move: remove src after copy %s: %w", src, rmErr)
			}
		} else {
			return fmt.Errorf("files.Move: copy fallback %s -> %s: %w (rename: %v)", src, dst, cpErr, err)
		}
	} else {
		return fmt.Errorf("files.Move: %s -> %s: %w", src, dst, err)
	}
}

// Hash computes the digest of the file at p in the VFS. Currently supports
// sha256.
func (s *Service) Hash(p string, algo HashAlgorithm) (string, error) {
	body, err := s.tree.Read(p)
	if err != nil {
		return "", fmt.Errorf("files.Hash: read %s: %w", p, err)
	}
	switch algo {
	case HashSHA256, "":
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:]), nil
	default:
		return "", fmt.Errorf("files.Hash: unsupported algorithm %q", algo)
	}
}

// Stat returns the VFS inode at p (cloned, safe to inspect freely). Thin
// wrapper around tree.Stat — exposed here so callers using the Service can
// stay within the provider's API rather than reaching for the underlying
// tree.
func (s *Service) Stat(p string) (*vfs.Inode, error) {
	return s.tree.Stat(p)
}

// Exists is a convenience predicate. Returns false (no error) for "not
// exist"; surfaces other errors (e.g. permission, corrupt) so callers can
// distinguish "absent" from "broken".
func (s *Service) Exists(p string) (bool, error) {
	_, err := s.tree.Stat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, vfs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// ReadDir lists the contents of the directory at p. Pass-through to the
// underlying tree, which honors the union (memory + host overlay).
func (s *Service) ReadDir(p string) ([]vfs.DirEntry, error) {
	return s.tree.ReadDir(p)
}

// MkdirAll creates p and any missing ancestors with the given mode. No-op
// if p already exists as a directory; errors if it exists as a non-directory.
func (s *Service) MkdirAll(p string) error {
	return s.tree.MkdirAll(p, 0o755)
}

// Remove deletes the entry at p. For files and empty directories: drops
// the entry; with overlay configured, also adds a whiteout. For non-empty
// directories: returns vfs.ErrNotEmpty (use RemoveAll to recurse).
func (s *Service) Remove(p string) error {
	return s.tree.Remove(p)
}

// RemoveAll recursively removes p and everything beneath it. Idempotent
// (returns nil when p doesn't exist).
func (s *Service) RemoveAll(p string) error {
	return s.tree.RemoveAll(p)
}
