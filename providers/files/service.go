// Package files is mkfst's file-operations provider — a VFS-aware companion
// to providers/docker. It downloads files into a *vfs.Tree, copies/moves/
// hashes/stats VFS paths, and executes host processes whose working
// directory is the tree's FUSE mountpoint (so the process reads VFS files
// at real paths without buffering through us).
//
// Like providers/docker, every per-call setting is a functional option:
//
//	svc := files.NewService(tree)
//	err := svc.Download(ctx, "https://example.com/foo.tar.gz", "/incoming/foo.tar.gz",
//	    files.Header("Authorization", "Bearer "+token),
//	    files.Mode(0o600),
//	    files.RetryUpTo(3),
//	)
//
// Downloads land in the in-memory tree (or the FUSE mount, if active) the
// same way `tree.Write` does — chunked, dedup-aware, compressible under
// pressure. Exec gives host processes a real path to read from without
// materializing anything to disk.
package files

import (
	"net/http"
	"sync"
	"time"

	"mkfst/providers/vfs"
)

// Service is the files provider — bound to a single *vfs.Tree at
// construction. Construct one and reuse; safe for concurrent use.
type Service struct {
	tree *vfs.Tree
	http *http.Client

	defaultTimeout    time.Duration
	defaultRetries    int
	concurrencyLimit  int

	mu sync.Mutex
}

// ServiceOption mutates Service construction. Applied in NewService.
type ServiceOption func(*Service)

// NewService returns a Service bound to tree. Provide ServiceOptions to
// customize the HTTP client, default timeouts/retries, or download
// concurrency limit.
func NewService(tree *vfs.Tree, opts ...ServiceOption) *Service {
	s := &Service{
		tree:             tree,
		http:             defaultHTTPClient(),
		defaultTimeout:   60 * time.Second,
		defaultRetries:   0,
		concurrencyLimit: 8,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Tree returns the underlying VFS the service operates on. Exposed for
// callers that need direct tree access alongside provider operations.
func (s *Service) Tree() *vfs.Tree { return s.tree }

// === Service options ===

// HTTPClient overrides the default HTTP client used for downloads. Useful
// for instrumented transports, proxy configuration, or pre-authenticated
// clients (e.g. one with a registered cookie jar or transport-level auth).
func HTTPClient(c *http.Client) ServiceOption {
	return func(s *Service) {
		if c != nil {
			s.http = c
		}
	}
}

// DefaultDownloadTimeout sets the per-download timeout used when no
// override is provided in DownloadOptions. 0 disables the timeout.
func DefaultDownloadTimeout(d time.Duration) ServiceOption {
	return func(s *Service) { s.defaultTimeout = d }
}

// DefaultDownloadRetries sets the per-download retry count used when no
// override is provided. Default 0 (no retries).
func DefaultDownloadRetries(n int) ServiceOption {
	return func(s *Service) {
		if n >= 0 {
			s.defaultRetries = n
		}
	}
}

// ConcurrencyLimit caps how many downloads DownloadAll runs in parallel.
// Default 8. Lower for memory-constrained hosts; higher for fast networks
// fetching many small files.
func ConcurrencyLimit(n int) ServiceOption {
	return func(s *Service) {
		if n > 0 {
			s.concurrencyLimit = n
		}
	}
}

// defaultHTTPClient returns a sensible HTTP client for downloads — a
// reasonable connection timeout but no overall request timeout (per-
// request timeouts are applied via context).
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: http.DefaultTransport,
	}
}
