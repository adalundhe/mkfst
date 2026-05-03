package network

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// replicaProbeState is per-replica probe runtime state. Used by:
//   - the probe scheduler (writes results)
//   - the load balancer (reads health for routing)
//   - the gateway / monitor (reads probe events for emission)
//
// Hot-path reads (load balancer per-connection) use atomic loads
// to avoid lock contention at scale.
type replicaProbeState struct {
	serviceName string
	replica     int
	containerID string

	// snapshot is an atomic.Pointer to the latest immutable
	// snapshot. Reads are lock-free; writes publish a new pointer.
	snapPtr atomic.Pointer[ProbeStatus]

	// signal mu+cond used by waitHealthy. ProbeScheduler signals
	// when a replica transitions to healthy.
	mu        sync.Mutex
	cond      *sync.Cond
	healthyCh chan struct{} // closed once on first healthy

	// consecutiveFailures / consecutiveSuccesses are mutated only
	// by the scheduler (single writer per replica).
	consecutiveFailures  int
	consecutiveSuccesses int
}

// ProbeStatus is the immutable read-only snapshot.
type ProbeStatus struct {
	ServiceName string
	Replica     int
	ContainerID string
	Healthy     bool
	LastProbeAt time.Time
	LastError   string
	Attempts    uint64
	Successes   uint64
	Failures    uint64
}

func newReplicaProbeState(serviceName string, replica int, containerID string) *replicaProbeState {
	r := &replicaProbeState{
		serviceName: serviceName,
		replica:     replica,
		containerID: containerID,
		healthyCh:   make(chan struct{}),
	}
	r.cond = sync.NewCond(&r.mu)
	r.snapPtr.Store(&ProbeStatus{
		ServiceName: serviceName,
		Replica:     replica,
		ContainerID: containerID,
		Healthy:     false,
	})
	return r
}

// snapshot returns the latest immutable status. Lock-free.
func (r *replicaProbeState) snapshot() ProbeStatus {
	return *r.snapPtr.Load()
}

// recordResult is called by the scheduler after each probe attempt.
// successThreshold / failureThreshold determine the transition.
func (r *replicaProbeState) recordResult(ok bool, err string, successThreshold, failureThreshold int) {
	prev := r.snapPtr.Load()
	next := *prev
	next.Attempts++
	next.LastProbeAt = time.Now()
	if ok {
		next.Successes++
		next.LastError = ""
		r.consecutiveFailures = 0
		r.consecutiveSuccesses++
		if !next.Healthy && r.consecutiveSuccesses >= successThreshold {
			next.Healthy = true
		}
	} else {
		next.Failures++
		next.LastError = err
		r.consecutiveSuccesses = 0
		r.consecutiveFailures++
		if next.Healthy && r.consecutiveFailures >= failureThreshold {
			next.Healthy = false
		}
	}
	r.snapPtr.Store(&next)

	// Wake any waiters on first healthy.
	if next.Healthy {
		r.mu.Lock()
		select {
		case <-r.healthyCh:
			// already closed
		default:
			close(r.healthyCh)
		}
		r.cond.Broadcast()
		r.mu.Unlock()
	}
}

// waitHealthy blocks until snapshot.Healthy is true. Returns
// ctx.Err() on cancellation.
func (r *replicaProbeState) waitHealthy(ctx context.Context) error {
	if r.snapshot().Healthy {
		return nil
	}
	select {
	case <-r.healthyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// markHealthy is used by Stack when no probe is configured —
// service is treated as healthy as soon as the container is started.
func (r *replicaProbeState) markHealthy() {
	prev := r.snapPtr.Load()
	if prev.Healthy {
		return
	}
	next := *prev
	next.Healthy = true
	next.Attempts++
	next.Successes++
	next.LastProbeAt = time.Now()
	r.snapPtr.Store(&next)
	r.mu.Lock()
	select {
	case <-r.healthyCh:
	default:
		close(r.healthyCh)
	}
	r.cond.Broadcast()
	r.mu.Unlock()
}

// markUnhealthy is used by the restart watcher when a container
// exits unexpectedly.
func (r *replicaProbeState) markUnhealthy(reason string) {
	prev := r.snapPtr.Load()
	next := *prev
	next.Healthy = false
	next.LastError = reason
	next.LastProbeAt = time.Now()
	r.snapPtr.Store(&next)
	// Reset healthyCh so a future markHealthy can fire it again.
	r.mu.Lock()
	select {
	case <-r.healthyCh:
		r.healthyCh = make(chan struct{})
	default:
		// not yet closed; leave as-is
	}
	r.consecutiveFailures = 0
	r.consecutiveSuccesses = 0
	r.mu.Unlock()
}
