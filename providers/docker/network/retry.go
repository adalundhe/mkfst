package network

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand"
	"time"
)

// RetryOpts configures bounded retry-with-backoff for docker SDK
// calls. Same shape as providers/tasks's exponential-with-full-jitter
// formula so all of mkfst behaves consistently under transient
// daemon flake.
type RetryOpts struct {
	// MaxAttempts caps total tries (initial + retries). 0 = use
	// default (4).
	MaxAttempts int
	// Base is the first retry delay. 0 = use default (100ms).
	Base time.Duration
	// Cap is the maximum retry delay. 0 = use default (5s).
	Cap time.Duration
	// IsRetryable, if non-nil, decides whether an error is worth
	// retrying. Default: nil = retry every error.
	IsRetryable func(error) bool
}

func (o RetryOpts) withDefaults() RetryOpts {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 4
	}
	if o.Base <= 0 {
		o.Base = 100 * time.Millisecond
	}
	if o.Cap <= 0 {
		o.Cap = 5 * time.Second
	}
	return o
}

// retry runs op until it returns nil or attempts are exhausted.
// Honors ctx — cancellation short-circuits the next sleep.
//
// op's signature returns the final error of the operation. If retry
// gives up because attempts are exhausted, the returned error is the
// most recent failure (annotated with the attempt count).
func retry(ctx context.Context, opts RetryOpts, op func(ctx context.Context) error) error {
	opts = opts.withDefaults()
	var lastErr error
	for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if opts.IsRetryable != nil && !opts.IsRetryable(err) {
			return err
		}
		if attempt == opts.MaxAttempts {
			break
		}
		// Full-jitter exponential backoff: sleep ∈ [0, min(cap, base*2^(attempt-1))).
		exp := opts.Base << (attempt - 1)
		if exp > opts.Cap || exp < 0 { // overflow guard
			exp = opts.Cap
		}
		sleep := time.Duration(mrand.Int63n(int64(exp)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}
	return fmt.Errorf("retry: gave up after %d attempts: %w", opts.MaxAttempts, lastErr)
}

// retryWithResult is the typed-return variant of retry. Useful when
// the operation produces a value plus an error.
func retryWithResult[T any](ctx context.Context, opts RetryOpts, op func(ctx context.Context) (T, error)) (T, error) {
	var result T
	err := retry(ctx, opts, func(ctx context.Context) error {
		var inner error
		result, inner = op(ctx)
		return inner
	})
	return result, err
}

// === retryability classifiers ===

// IsRetryableDocker reports whether a docker SDK error is the kind
// worth retrying — transient timeouts, connection resets, daemon
// busy errors. Does NOT retry "not found" / "already exists" /
// validation errors, which are deterministic.
func IsRetryableDocker(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// ctx is the *caller's* deadline expiring — propagate, don't
		// retry past it. ctx-deadline-from-ours-not-the-callers is
		// caught by the retry loop via ctx.Err().
		return false
	}
	// String-matching is regrettable, but the docker SDK does not
	// expose typed error sentinels for these conditions. We're
	// conservative: only retry on patterns known to be transient.
	msg := err.Error()
	transientHints := []string{
		"connection refused",
		"connection reset",
		"i/o timeout",
		"EOF",
		"temporarily unavailable",
		"server is busy",
		"network is unreachable",
		"no such host",      // DNS flake to daemon
		"broken pipe",
		"unexpected EOF",
		"server closed",
	}
	for _, hint := range transientHints {
		if containsFold(msg, hint) {
			return true
		}
	}
	return false
}

// containsFold is a tiny case-insensitive substring helper avoiding
// strings imports for one call. Inlined for clarity.
func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(substr) > len(s) {
		return false
	}
	// Case-insensitive ASCII match. Sufficient for docker SDK
	// errors which are always English ASCII.
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			a := s[i+j]
			b := substr[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
