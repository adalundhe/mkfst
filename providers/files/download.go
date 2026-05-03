package files

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrChecksumMismatch is returned when a download's computed digest
// doesn't match a Verify() expectation.
var ErrChecksumMismatch = errors.New("files: checksum mismatch")

// DownloadOption mutates a single download's request just before submission.
type DownloadOption func(*downloadState)

type downloadState struct {
	header        http.Header
	method        string
	body          io.Reader
	mode          fs.FileMode
	timeout       time.Duration
	retries       int
	verifyAlgo    string // "sha256" — extensible
	verifyExpect  string // hex-encoded expected digest
	autoDecompress bool
	maxRedirects  int
	onProgress    func(DownloadProgress)
}

// DownloadProgress is delivered to OnProgress callbacks (and embedded in
// Event when DownloadAll's stream emits progress events).
type DownloadProgress struct {
	URL     string
	Dst     string
	Current int64
	Total   int64 // 0 if Content-Length is missing or chunked
}

// Download fetches url and writes the body into the VFS at dst. Parents
// are created as needed. Returns when the download completes (or fails).
//
// Download is synchronous; for parallel batches use DownloadAll. For a
// progress stream on a single download, pass OnProgress(...).
func (s *Service) Download(ctx context.Context, url, dst string, opts ...DownloadOption) error {
	state := s.newDownloadState()
	for _, opt := range opts {
		opt(state)
	}

	if state.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, state.timeout)
		defer cancel()
	}

	var lastErr error
	for attempt := 0; attempt <= state.retries; attempt++ {
		if attempt > 0 {
			// Exponential backoff capped at 5s — common case is "second
			// attempt succeeds"; we don't want to wait forever on flaky
			// servers in a small retry budget.
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := s.downloadOnce(ctx, url, dst, state)
		if err == nil {
			return nil
		}
		// Don't retry on ctx cancellation, validation, or write errors —
		// they won't get better with another attempt.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, ErrChecksumMismatch) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("files.Download %s: %w", url, lastErr)
}

// downloadOnce performs a single attempt of the download — used by the
// retry loop in Download. Verification (Verify) runs after the body is
// fully read but before the file is persisted to the tree, so a corrupt
// download never lands in the VFS.
func (s *Service) downloadOnce(ctx context.Context, url, dst string, state *downloadState) error {
	req, err := http.NewRequestWithContext(ctx, state.method, url, state.body)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	for k, vs := range state.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	client := s.http
	if state.maxRedirects > 0 {
		// Clone the client for this request so per-call redirect policy
		// doesn't bleed into the shared client.
		clone := *client
		max := state.maxRedirects
		clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= max {
				return fmt.Errorf("stopped after %d redirects", max)
			}
			return nil
		}
		client = &clone
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	body := io.Reader(resp.Body)
	if state.autoDecompress && strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(body)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		body = gz
	}

	// Buffer the response into memory before handing to the tree. This
	// trades RAM for atomicity — the tree only sees a complete file, never
	// a half-written one. For very large downloads we might want a streaming
	// path that writes directly through the tree's chunked write API; not
	// needed for the common build-context / package-fetch use case.
	buf, err := readAllWithProgress(body, state, url, dst, resp.ContentLength)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if state.verifyExpect != "" {
		if err := verify(state.verifyAlgo, state.verifyExpect, buf); err != nil {
			return err
		}
	}

	mode := state.mode
	if mode == 0 {
		mode = 0o644
	}
	if err := s.tree.Write(dst, buf, mode); err != nil {
		return fmt.Errorf("vfs write: %w", err)
	}
	return nil
}

// readAllWithProgress reads from r into a buffer, optionally calling state.onProgress
// after each chunk. We read in 64 KiB chunks to keep the progress cadence
// useful (~64 KiB/MB worth of callbacks rather than once at end).
func readAllWithProgress(r io.Reader, state *downloadState, url, dst string, contentLength int64) ([]byte, error) {
	const chunk = 64 << 10
	var buf []byte
	tmp := make([]byte, chunk)
	var total int64

	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			total += int64(n)
			if state.onProgress != nil {
				state.onProgress(DownloadProgress{
					URL:     url,
					Dst:     dst,
					Current: total,
					Total:   contentLength,
				})
			}
		}
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// verify computes the digest of buf using algo and compares with expected.
// Currently supports "sha256"; extensible — add more cases as needed.
func verify(algo, expectedHex string, buf []byte) error {
	var got string
	switch strings.ToLower(algo) {
	case "sha256":
		sum := sha256.Sum256(buf)
		got = hex.EncodeToString(sum[:])
	default:
		return fmt.Errorf("files: unsupported checksum algorithm %q", algo)
	}
	if !strings.EqualFold(got, expectedHex) {
		return fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, got, expectedHex)
	}
	return nil
}

// newDownloadState seeds defaults from the Service's configuration.
func (s *Service) newDownloadState() *downloadState {
	return &downloadState{
		header:  http.Header{},
		method:  http.MethodGet,
		timeout: s.defaultTimeout,
		retries: s.defaultRetries,
	}
}

// === Download options ===

// Header sets a single request header.
func Header(name, value string) DownloadOption {
	return func(s *downloadState) { s.header.Set(name, value) }
}

// AddHeader appends a header value (vs Header which sets/overwrites).
func AddHeader(name, value string) DownloadOption {
	return func(s *downloadState) { s.header.Add(name, value) }
}

// Bearer sets an Authorization: Bearer <token> header — ergonomic shortcut
// for the most common API auth style.
func Bearer(token string) DownloadOption {
	return func(s *downloadState) { s.header.Set("Authorization", "Bearer "+token) }
}

// BasicAuth sets HTTP Basic Auth via the Authorization header.
func BasicAuth(user, pass string) DownloadOption {
	return func(s *downloadState) {
		req := &http.Request{Header: s.header}
		req.SetBasicAuth(user, pass)
	}
}

// Method overrides the HTTP method (default GET). Use for POST/PUT
// downloads of generated artifacts.
func Method(method string) DownloadOption {
	return func(s *downloadState) { s.method = method }
}

// Body attaches a request body — typically paired with Method("POST").
func Body(r io.Reader) DownloadOption {
	return func(s *downloadState) { s.body = r }
}

// Mode sets the file mode applied to the VFS entry. Default 0o644.
func Mode(mode fs.FileMode) DownloadOption {
	return func(s *downloadState) { s.mode = mode }
}

// Timeout overrides the per-download timeout. 0 disables.
func Timeout(d time.Duration) DownloadOption {
	return func(s *downloadState) { s.timeout = d }
}

// RetryUpTo overrides the per-download retry count. 0 = no retries.
func RetryUpTo(n int) DownloadOption {
	return func(s *downloadState) {
		if n >= 0 {
			s.retries = n
		}
	}
}

// VerifySHA256 asserts the SHA-256 of the downloaded body matches expected
// (lowercase or uppercase hex). On mismatch the file is NOT written and
// Download returns ErrChecksumMismatch.
func VerifySHA256(expectedHex string) DownloadOption {
	return func(s *downloadState) {
		s.verifyAlgo = "sha256"
		s.verifyExpect = expectedHex
	}
}

// AutoDecompress transparently inflates gzip-encoded responses. The VFS
// receives the uncompressed bytes.
func AutoDecompress() DownloadOption {
	return func(s *downloadState) { s.autoDecompress = true }
}

// MaxRedirects caps how many HTTP redirects to follow. 0 (default) uses
// Go's net/http default (10).
func MaxRedirects(n int) DownloadOption {
	return func(s *downloadState) { s.maxRedirects = n }
}

// OnProgress registers a callback for read-progress updates. The callback
// is invoked on the goroutine that's reading the body — keep it cheap.
func OnProgress(fn func(DownloadProgress)) DownloadOption {
	return func(s *downloadState) { s.onProgress = fn }
}

// === DownloadAll: parallel jobs ===

// Job is a single download entry in a DownloadAll batch.
type Job struct {
	URL     string
	Dst     string
	Options []DownloadOption
}

// JobResult reports the outcome of a single Job in a DownloadAll batch.
type JobResult struct {
	Job Job
	Err error
}

// DownloadAll runs the jobs in parallel up to ConcurrencyLimit, returns
// per-job results in input order. Cancellation of ctx aborts in-flight
// jobs and reports them as ctx.Err() in the result slice.
func (s *Service) DownloadAll(ctx context.Context, jobs []Job) []JobResult {
	if len(jobs) == 0 {
		return nil
	}
	limit := s.concurrencyLimit
	if limit > len(jobs) {
		limit = len(jobs)
	}

	results := make([]JobResult, len(jobs))
	for i := range results {
		results[i].Job = jobs[i]
	}

	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[idx].Err = ctx.Err()
				return
			}
			results[idx].Err = s.Download(ctx, jobs[idx].URL, jobs[idx].Dst, jobs[idx].Options...)
		}(i)
	}
	wg.Wait()
	return results
}
