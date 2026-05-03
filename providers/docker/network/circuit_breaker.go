package network

import (
	"sync"
	"sync/atomic"
	"time"
)

// circuitBreaker implements the standard three-state breaker over
// a per-replica failure rate. Used by the gateway's load balancer
// to short-circuit traffic away from a misbehaving backend until
// it recovers.
//
// States:
//
//   Closed:    requests flow normally, failures counted via EWMA
//   Open:      requests denied immediately for OpenDuration
//   HalfOpen:  one probe request allowed; success → Closed,
//              failure → Open again with reset timer
//
// The breaker is per-replica; the gateway swaps the snapshot
// pointer atomically when state transitions, so reads on the
// hot path are lock-free.
type circuitBreaker struct {
	state atomic.Int32

	// failure rate (EWMA, 0..1) and last update timestamp.
	mu          sync.Mutex
	failureRate float64
	lastUpdate  time.Time
	openedAt    time.Time

	// configuration (immutable after construction).
	failureThreshold float64       // EWMA above which we trip
	openDuration     time.Duration // how long to stay open before half-open
	halfOpenAllow    int           // probes allowed in half-open
	halfOpenInFlight atomic.Int32
	halfOpenSuccess  int // configured: successes needed to close
}

const (
	cbStateClosed   = 0
	cbStateOpen     = 1
	cbStateHalfOpen = 2
)

// newCircuitBreaker returns a breaker with sensible defaults if
// failureThreshold/openDuration/halfOpenAllow are zero/negative.
func newCircuitBreaker(failureThreshold float64, openDuration time.Duration, halfOpenAllow int) *circuitBreaker {
	if failureThreshold <= 0 || failureThreshold >= 1 {
		failureThreshold = 0.5
	}
	if openDuration <= 0 {
		openDuration = 5 * time.Second
	}
	if halfOpenAllow <= 0 {
		halfOpenAllow = 1
	}
	cb := &circuitBreaker{
		failureThreshold: failureThreshold,
		openDuration:     openDuration,
		halfOpenAllow:    halfOpenAllow,
		halfOpenSuccess:  1,
	}
	cb.state.Store(cbStateClosed)
	cb.lastUpdate = time.Now()
	return cb
}

// allow reports whether a new request may proceed. Caller must
// invoke recordSuccess / recordFailure on completion exactly once
// when allow returned true.
func (cb *circuitBreaker) allow() bool {
	switch cb.state.Load() {
	case cbStateClosed:
		return true
	case cbStateOpen:
		// Has the open window elapsed?
		cb.mu.Lock()
		opened := cb.openedAt
		cb.mu.Unlock()
		if time.Since(opened) >= cb.openDuration {
			// Transition to half-open.
			if cb.state.CompareAndSwap(cbStateOpen, cbStateHalfOpen) {
				cb.halfOpenInFlight.Store(0)
			}
			return cb.allowHalfOpen()
		}
		return false
	case cbStateHalfOpen:
		return cb.allowHalfOpen()
	}
	return false
}

func (cb *circuitBreaker) allowHalfOpen() bool {
	for {
		cur := cb.halfOpenInFlight.Load()
		if int(cur) >= cb.halfOpenAllow {
			return false
		}
		if cb.halfOpenInFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// recordSuccess updates EWMA and may close the breaker if we were
// in half-open with enough successes.
func (cb *circuitBreaker) recordSuccess() {
	cb.update(false)
	if cb.state.Load() == cbStateHalfOpen {
		// Single success closes (halfOpenSuccess defaults to 1).
		if cb.state.CompareAndSwap(cbStateHalfOpen, cbStateClosed) {
			cb.mu.Lock()
			cb.failureRate = 0
			cb.mu.Unlock()
		}
		cb.halfOpenInFlight.Add(-1)
	}
}

// recordFailure bumps EWMA and may trip the breaker.
func (cb *circuitBreaker) recordFailure() {
	cb.update(true)
	switch cb.state.Load() {
	case cbStateClosed:
		cb.mu.Lock()
		rate := cb.failureRate
		cb.mu.Unlock()
		if rate >= cb.failureThreshold {
			if cb.state.CompareAndSwap(cbStateClosed, cbStateOpen) {
				cb.mu.Lock()
				cb.openedAt = time.Now()
				cb.mu.Unlock()
			}
		}
	case cbStateHalfOpen:
		// Half-open failure → straight back to open.
		if cb.state.CompareAndSwap(cbStateHalfOpen, cbStateOpen) {
			cb.mu.Lock()
			cb.openedAt = time.Now()
			cb.mu.Unlock()
		}
		cb.halfOpenInFlight.Add(-1)
	}
}

// update applies a time-weighted EWMA to the failure rate.
// The decay factor is per-second (alpha=0.3 per second by
// default). Failure events count as 1; successes as 0.
func (cb *circuitBreaker) update(failed bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := time.Now()
	dt := now.Sub(cb.lastUpdate).Seconds()
	if dt < 0 {
		dt = 0
	}
	// alpha per second; smaller = slower decay.
	const alphaPerSec = 0.3
	decay := alphaPerSec * dt
	if decay > 1 {
		decay = 1
	}
	x := 0.0
	if failed {
		x = 1.0
	}
	cb.failureRate = cb.failureRate*(1-decay) + x*decay
	cb.lastUpdate = now
}

// state returns the current state for observability.
func (cb *circuitBreaker) currentState() int32 { return cb.state.Load() }

// === replica breaker registry on Ingress ===
//
// One breaker per (ingress, replica). Lazily created.
type breakerRegistry struct {
	mu       sync.Mutex
	breakers map[string]*circuitBreaker
}

func newBreakerRegistry() *breakerRegistry {
	return &breakerRegistry{breakers: map[string]*circuitBreaker{}}
}

func (r *breakerRegistry) get(key string, ft float64, od time.Duration, ha int) *circuitBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	cb, ok := r.breakers[key]
	if !ok {
		cb = newCircuitBreaker(ft, od, ha)
		r.breakers[key] = cb
	}
	return cb
}
