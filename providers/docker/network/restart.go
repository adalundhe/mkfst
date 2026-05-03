package network

import (
	"context"
	"fmt"
	mrand "math/rand"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
)

// === restart watcher ===
//
// One restart watcher goroutine per (service, replica). Fires when:
//
//   - The container exits (ContainerWait NotRunning).
//   - The liveness probe trips the replica unhealthy (handled by
//     the probe scheduler emitting a marker; the watcher checks
//     replicaProbeState on the same tick and acts).
//
// Behavior:
//
//   - RestartNever: log + emit ServiceRestarted skipped event,
//     stop watching this replica.
//   - RestartOnFailure: restart only on non-zero exit; honor
//     MaxAttempts.
//   - RestartAlways: restart unconditionally; honor MaxAttempts.
//   - RestartUnlessStopped: restart unconditionally except when
//     the user explicitly stopped the service.
//
// Backoff between restart attempts uses the policy's Backoff
// function (defaults to full-jitter exponential, capped at 30s).

func (s *Stack) startRestartWatchers() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name, svc := range s.services {
		if svc.role != "" && svc.role != RoleService {
			continue // init/sidecar containers don't restart
		}
		if svc.restart.Kind == RestartNever {
			continue
		}
		insts := s.containers[name]
		probes := s.probes[name]
		for i := range insts {
			i := i
			svc := svc
			s.bg.Go(func() error {
				s.watchReplica(svc, insts[i], probes[i])
				return nil
			})
		}
	}
}

// watchReplica observes one replica's container exit and applies
// the restart policy. Exits when stopCh closes or MaxAttempts is
// exhausted.
func (s *Stack) watchReplica(svc *Service, inst containerInstance, rps *replicaProbeState) {
	attempts := 0
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		// Wait for the container to exit. ContainerWait honors ctx.
		waitCtx, cancel := context.WithCancel(context.Background())
		statusCh, errCh := s.engine.cli.ContainerWait(waitCtx, inst.id, dockercontainer.WaitConditionNotRunning)

		var exitCode int64
		var exitErr error
		select {
		case <-s.stopCh:
			cancel()
			return
		case status := <-statusCh:
			exitCode = status.StatusCode
			if status.Error != nil {
				exitErr = fmt.Errorf("container error: %s", status.Error.Message)
			}
		case err := <-errCh:
			exitErr = err
		}
		cancel()

		// Mark unhealthy.
		reason := fmt.Sprintf("exit %d", exitCode)
		if exitErr != nil {
			reason = exitErr.Error()
		}
		rps.markUnhealthy(reason)

		// Apply policy.
		shouldRestart := false
		switch svc.restart.Kind {
		case RestartAlways:
			shouldRestart = true
		case RestartOnFailure:
			shouldRestart = exitCode != 0 || exitErr != nil
		case RestartUnlessStopped:
			shouldRestart = true // user-stop would close stopCh, caught above
		}
		if !shouldRestart {
			return
		}
		if svc.restart.MaxAttempts > 0 && attempts >= svc.restart.MaxAttempts {
			return
		}
		attempts++

		backoff := defaultRestartBackoff
		if svc.restart.Backoff != nil {
			backoff = svc.restart.Backoff
		}
		delay := backoff(attempts)
		select {
		case <-s.stopCh:
			return
		case <-time.After(delay):
		}

		// Restart the container.
		if err := s.engine.cli.ContainerStart(context.Background(), inst.id, dockercontainer.StartOptions{}); err != nil {
			// If the container is gone (removed externally), give
			// up — we can't restart what doesn't exist.
			if isNotFoundError(err) {
				return
			}
			// Otherwise, log via monitor and try again on the next
			// loop iteration (statusCh will fire immediately).
			if s.monitor != nil {
				s.monitor.emit(Event{
					Kind:        EventServiceRestarted,
					At:          time.Now(),
					Service:     svc.name,
					Replica:     inst.replica,
					ContainerID: inst.id,
					Error:       err.Error(),
				})
			}
			continue
		}

		if s.monitor != nil {
			s.monitor.emit(Event{
				Kind:        EventServiceRestarted,
				At:          time.Now(),
				Service:     svc.name,
				Replica:     inst.replica,
				ContainerID: inst.id,
			})
		}

		// Re-run the readiness probe (if any) so dependent services
		// can resume routing once the replica is healthy again.
		if svc.probe != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			_ = s.engine.probeSched.runUntilSuccess(ctx, s, svc, inst, rps)
			cancel()
		} else {
			rps.markHealthy()
		}
	}
}

// defaultRestartBackoff is the same shape as providers/tasks's
// default — exponential with full jitter, capped at 30s.
func defaultRestartBackoff(attempt int) time.Duration {
	const base = time.Second
	const cap = 30 * time.Second
	if attempt < 1 {
		attempt = 1
	}
	exp := base << (attempt - 1)
	if exp > cap || exp < 0 {
		exp = cap
	}
	return time.Duration(mrand.Int63n(int64(exp)))
}
