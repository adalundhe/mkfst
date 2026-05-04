package network

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// === image digest pinning ===
//
// Tag-based image references are mutable — `nginx:alpine` today is
// not necessarily the same image as `nginx:alpine` tomorrow. For
// security-sensitive workloads the operator pins images by digest:
//
//   stack.PinImage("nginx:alpine", "sha256:abc...")
//
// Subsequent `RunOneShot` / Service definitions that reference
// `nginx:alpine` get rewritten to `nginx@sha256:abc...` before the
// docker daemon ever sees them.
//
// When `RequireDigestPinning(true)` is set, ANY image reference
// without a digest (either via tag or directly) is rejected at
// validation time — operators cannot accidentally introduce
// unpinned images.

// pinTable holds tag → digest mappings for one Stack.
type pinTable struct {
	mu       sync.RWMutex
	pins     map[string]string // tag → digest like "sha256:abc..."
	required bool
}

func newPinTable() *pinTable {
	return &pinTable{pins: map[string]string{}}
}

// PinImage records a digest pin for an image reference. The
// reference must be a tag form ("nginx:alpine"); an existing digest
// reference returns an error (already pinned).
//
// digest must start with "sha256:" (the only algorithm docker
// currently supports for content addressing).
func (s *Stack) PinImage(ref, digest string) error {
	if ref == "" {
		return fmt.Errorf("PinImage: %w: empty ref", ErrInvalidConfig)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) < len("sha256:")+8 {
		return fmt.Errorf("PinImage: %w: digest must be sha256:...", ErrInvalidConfig)
	}
	if strings.Contains(ref, "@") {
		return fmt.Errorf("PinImage: %w: ref %q already digest-pinned", ErrInvalidConfig, ref)
	}
	s.imagePins.mu.Lock()
	defer s.imagePins.mu.Unlock()
	s.imagePins.pins[ref] = digest
	return nil
}

// RequireDigestPinning toggles the rejection of unpinned image
// references for every container the stack creates (services AND
// one-shots). When true, an image string without an explicit
// `@sha256:...` portion that isn't covered by a `PinImage` mapping
// is rejected at validation time.
func (s *Stack) RequireDigestPinning(required bool) {
	s.imagePins.mu.Lock()
	defer s.imagePins.mu.Unlock()
	s.imagePins.required = required
}

// resolveImage returns the digest-pinned form of ref. When ref
// already includes a digest it is returned unchanged; when a pin
// is registered the digest is appended; otherwise (ref unpinned)
// the original ref is returned, unless required-pinning mode is
// on, in which case an error is returned.
func (s *Stack) resolveImage(ref string) (string, error) {
	if strings.Contains(ref, "@sha256:") {
		return ref, nil
	}
	s.imagePins.mu.RLock()
	digest, ok := s.imagePins.pins[ref]
	required := s.imagePins.required
	s.imagePins.mu.RUnlock()
	if ok {
		// Strip the tag and append the digest. Docker accepts
		// "name@sha256:..." with no tag.
		base := stripTag(ref)
		return base + "@" + digest, nil
	}
	if required {
		return "", fmt.Errorf("image %q is not digest-pinned and stack.RequireDigestPinning(true): refusing to use mutable tag", ref)
	}
	return ref, nil
}

// stripTag returns the image reference without its `:tag` suffix.
// Handles registry refs with port numbers carefully so we don't
// remove the port instead of the tag.
func stripTag(ref string) string {
	// Skip past any registry portion (everything before the first
	// '/'). Then look for the last ':' in the remainder.
	slash := strings.IndexByte(ref, '/')
	prefix := ""
	rest := ref
	if slash >= 0 {
		prefix = ref[:slash+1]
		rest = ref[slash+1:]
	}
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		return prefix + rest[:i]
	}
	return ref
}

// === error sentinel ===

// ErrUnpinnedImage is returned when RequireDigestPinning is set
// and an unpinned image reference is encountered.
var ErrUnpinnedImage = errors.New("network: image reference is not digest-pinned")
