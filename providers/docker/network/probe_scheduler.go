package network

import (
	"bytes"
	"container/heap"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
)

// === probe scheduler ===
//
// One scheduler per Engine. Maintains a min-heap of (next-fire-time,
// job) and dispatches due jobs into a bounded worker pool. This
// scales to many thousands of services without per-service
// goroutines or per-service tickers.
//
// All probe execution happens in the worker goroutines; no probe
// blocks the scheduler tick. Workers handle TCP/HTTP/UDP/gRPC/Exec.

type probeScheduler struct {
	engine  *Engine
	workers int

	mu   sync.Mutex
	pq   probeHeap // due-time ordered jobs
	wake   chan struct{}
	jobs   chan *probeJob // to workers
	stopCh chan struct{}
	done   chan struct{}
}

type probeJob struct {
	stack    *Stack
	service  *Service
	instance containerInstance
	rps      *replicaProbeState
	probe    *Probe // already withDefaults applied
	dueAt    time.Time
	index    int // heap.Interface
	stopOnce sync.Once
	stopped  bool
}

// === heap impl ===
type probeHeap []*probeJob

func (h probeHeap) Len() int            { return len(h) }
func (h probeHeap) Less(i, j int) bool  { return h[i].dueAt.Before(h[j].dueAt) }
func (h probeHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *probeHeap) Push(x interface{}) {
	job := x.(*probeJob)
	job.index = len(*h)
	*h = append(*h, job)
}
func (h *probeHeap) Pop() interface{} {
	old := *h
	n := len(old)
	job := old[n-1]
	old[n-1] = nil
	job.index = -1
	*h = old[:n-1]
	return job
}

// === scheduler API ===

func newProbeScheduler(engine *Engine, workers int) *probeScheduler {
	return &probeScheduler{
		engine:  engine,
		workers: workers,
		wake:    make(chan struct{}, 1),
		jobs:    make(chan *probeJob, workers*2),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (s *probeScheduler) run() {
	defer close(s.done)
	// Spawn worker pool.
	var wg sync.WaitGroup
	for i := 0; i < s.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.workerLoop()
		}()
	}
	// Scheduler loop: pop due jobs and feed into the workers channel.
	for {
		select {
		case <-s.stopCh:
			close(s.jobs)
			wg.Wait()
			return
		default:
		}

		s.mu.Lock()
		var nextDelay time.Duration
		if s.pq.Len() == 0 {
			nextDelay = 100 * time.Millisecond // idle
		} else {
			top := s.pq[0]
			now := time.Now()
			if !top.dueAt.After(now) {
				heap.Pop(&s.pq)
				s.mu.Unlock()
				if !top.stopped {
					select {
					case s.jobs <- top:
					case <-s.stopCh:
						return
					}
				}
				continue
			}
			nextDelay = top.dueAt.Sub(now)
		}
		s.mu.Unlock()
		if nextDelay > 25*time.Millisecond {
			nextDelay = 25 * time.Millisecond // resolution cap
		}
		select {
		case <-time.After(nextDelay):
		case <-s.wake:
		case <-s.stopCh:
			return
		}
	}
}

// stop signals the scheduler and worker pool to drain and exit.
func (s *probeScheduler) stop() {
	select {
	case <-s.stopCh:
		// already closed
	default:
		close(s.stopCh)
	}
	<-s.done
}

// schedule (re)schedules a job for the given fire time.
func (s *probeScheduler) schedule(job *probeJob, dueAt time.Time) {
	s.mu.Lock()
	job.dueAt = dueAt
	heap.Push(&s.pq, job)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// === worker ===

func (s *probeScheduler) workerLoop() {
	for job := range s.jobs {
		if job.stopped {
			continue
		}
		s.executeOnce(job)
		// Re-schedule if the probe is in liveness mode and the
		// service is still running.
		if job.service.probeMode == ProbeLiveness && !job.stopped {
			next := time.Now().Add(jitter(job.probe.Interval, job.probe.Jitter))
			s.schedule(job, next)
		}
	}
}

// executeOnce runs the probe once and records the result.
func (s *probeScheduler) executeOnce(job *probeJob) {
	ctx, cancel := context.WithTimeout(context.Background(), job.probe.Timeout)
	defer cancel()

	ok, err := s.runOne(ctx, job)
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	job.rps.recordResult(ok, errMsg, job.probe.SuccessThreshold, job.probe.FailureThreshold)

	// Emit monitor event on transition to unhealthy.
	if !ok && job.stack.monitor != nil {
		job.stack.monitor.emit(Event{
			Kind:        EventProbeFailed,
			Service:     job.service.name,
			Replica:     job.instance.replica,
			ContainerID: job.instance.id,
			Error:       errMsg,
			At:          time.Now(),
		})
	}
}

// runOne dispatches by probe kind.
func (s *probeScheduler) runOne(ctx context.Context, job *probeJob) (bool, error) {
	switch job.probe.Kind {
	case ProbeTCP:
		return s.runTCP(ctx, job)
	case ProbeHTTP:
		return s.runHTTP(ctx, job)
	case ProbeUDP:
		return s.runUDP(ctx, job)
	case ProbeGRPC:
		return s.runGRPC(ctx, job)
	case ProbeExec:
		return s.runExec(ctx, job)
	}
	return false, fmt.Errorf("unknown probe kind: %d", job.probe.Kind)
}

// === probe implementations ===

func (s *probeScheduler) resolveAddr(ctx context.Context, job *probeJob) (string, error) {
	loc := job.probe.Location
	if loc == ProbeLocationAuto {
		if job.probe.Kind == ProbeExec {
			loc = ProbeLocationExec
		} else {
			loc = ProbeLocationLoopback
		}
	}
	switch loc {
	case ProbeLocationLoopback:
		// Look up the loopback-published port. The Stack publishes
		// a service's primary port to 127.0.0.1:auto when an
		// ingress targets that port; for probes we also publish on
		// demand. The instance's recorded hostPort is "ip:port".
		if job.instance.hostPort != "" {
			return job.instance.hostPort, nil
		}
		// Fall back to docker inspect → primary port mapping.
		insp, err := job.stack.engine.cli.ContainerInspect(ctx, job.instance.id)
		if err != nil {
			return "", err
		}
		key := strconv.Itoa(job.probe.Port) + "/tcp"
		if job.probe.Kind == ProbeUDP {
			key = strconv.Itoa(job.probe.Port) + "/udp"
		}
		bindings := insp.NetworkSettings.Ports
		for portKey, binds := range bindings {
			if string(portKey) == key && len(binds) > 0 {
				host := binds[0].HostIP
				if host == "" {
					host = "127.0.0.1"
				}
				return host + ":" + binds[0].HostPort, nil
			}
		}
		// Last-resort: try container IP directly (works rootful).
		ipFallback, err := s.containerIP(ctx, job)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(ipFallback, strconv.Itoa(job.probe.Port)), nil

	case ProbeLocationContainerIP:
		ip, err := s.containerIP(ctx, job)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(ip, strconv.Itoa(job.probe.Port)), nil

	case ProbeLocationExec:
		// Exec probes don't use an address; the worker uses ExecCmd
		// directly. Return empty so callers detect "no address path."
		return "", nil
	}
	return "", fmt.Errorf("unsupported probe location %d", loc)
}

func (s *probeScheduler) containerIP(ctx context.Context, job *probeJob) (string, error) {
	insp, err := job.stack.engine.cli.ContainerInspect(ctx, job.instance.id)
	if err != nil {
		return "", err
	}
	if insp.NetworkSettings == nil {
		return "", fmt.Errorf("no NetworkSettings on container %s", job.instance.id)
	}
	for _, ep := range insp.NetworkSettings.Networks {
		if ep != nil && ep.IPAddress != "" {
			return ep.IPAddress, nil
		}
	}
	return "", fmt.Errorf("container %s has no IP", job.instance.id)
}

func (s *probeScheduler) runTCP(ctx context.Context, job *probeJob) (bool, error) {
	addr, err := s.resolveAddr(ctx, job)
	if err != nil {
		return false, err
	}
	d := net.Dialer{Timeout: job.probe.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, err
	}
	_ = conn.Close()
	return true, nil
}

func (s *probeScheduler) runHTTP(ctx context.Context, job *probeJob) (bool, error) {
	addr, err := s.resolveAddr(ctx, job)
	if err != nil {
		return false, err
	}
	url := "http://" + addr + job.probe.HTTPPath
	req, err := http.NewRequestWithContext(ctx, job.probe.HTTPMethod, url, nil)
	if err != nil {
		return false, err
	}
	for k, v := range job.probe.HTTPHeaders {
		req.Header.Set(k, v)
	}
	cli := &http.Client{Timeout: job.probe.Timeout}
	resp, err := cli.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if job.probe.HTTPExpectCode != 0 {
		if resp.StatusCode != job.probe.HTTPExpectCode {
			return false, fmt.Errorf("status %d ≠ %d", resp.StatusCode, job.probe.HTTPExpectCode)
		}
	} else {
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			return false, fmt.Errorf("status %d", resp.StatusCode)
		}
	}
	if job.probe.HTTPExpectBody != "" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if !bytes.Contains(body, []byte(job.probe.HTTPExpectBody)) {
			return false, fmt.Errorf("body missing %q", job.probe.HTTPExpectBody)
		}
	}
	return true, nil
}

func (s *probeScheduler) runUDP(ctx context.Context, job *probeJob) (bool, error) {
	addr, err := s.resolveAddr(ctx, job)
	if err != nil {
		return false, err
	}
	d := net.Dialer{Timeout: job.probe.Timeout}
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(job.probe.Timeout))
	}
	if _, err := conn.Write(job.probe.UDPSend); err != nil {
		return false, err
	}
	if !job.probe.UDPExpectReply {
		return true, nil
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return false, err
	}
	if len(job.probe.UDPExpectBytes) > 0 {
		if !bytes.Equal(buf[:n], job.probe.UDPExpectBytes) {
			return false, fmt.Errorf("UDP reply mismatch")
		}
	}
	return true, nil
}

func (s *probeScheduler) runGRPC(ctx context.Context, job *probeJob) (bool, error) {
	// Minimal gRPC health check via raw HTTP/2 would pull in
	// google.golang.org/grpc — instead, treat as TCP-connect for
	// now and document that full gRPC health-protocol is a future
	// enhancement. Returning success on TCP connect is correct in
	// the common "is the listener up" sense.
	return s.runTCP(ctx, job)
}

func (s *probeScheduler) runExec(ctx context.Context, job *probeJob) (bool, error) {
	cli := job.stack.engine.cli
	exec, err := cli.ContainerExecCreate(ctx, job.instance.id, dockercontainer.ExecOptions{
		Cmd:          job.probe.ExecCmd,
		AttachStderr: false,
		AttachStdout: false,
	})
	if err != nil {
		return false, err
	}
	if err := cli.ContainerExecStart(ctx, exec.ID, dockercontainer.ExecStartOptions{}); err != nil {
		return false, err
	}
	// Poll for completion (bounded by ctx).
	deadline := time.Now().Add(job.probe.Timeout)
	for time.Now().Before(deadline) {
		insp, err := cli.ContainerExecInspect(ctx, exec.ID)
		if err != nil {
			return false, err
		}
		if !insp.Running {
			if insp.ExitCode != 0 {
				return false, fmt.Errorf("exec exit %d", insp.ExitCode)
			}
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return false, fmt.Errorf("exec timed out")
}

// === public-from-stack helpers ===

// runUntilSuccess synchronously probes until success or ctx
// expires. Used at startup gating before dependents start.
func (s *probeScheduler) runUntilSuccess(ctx context.Context, stack *Stack, svc *Service, inst containerInstance, rps *replicaProbeState) error {
	probe := svc.probe.withDefaults()
	if probe.InitialDelay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(probe.InitialDelay):
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		job := &probeJob{
			stack:    stack,
			service:  svc,
			instance: inst,
			rps:      rps,
			probe:    probe,
		}
		s.executeOnce(job)
		if rps.snapshot().Healthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter(probe.Interval, probe.Jitter)):
		}
	}
}

// runLiveness drives continuous liveness probing for one replica.
// Exits when stopCh closes.
func (s *probeScheduler) runLiveness(stack *Stack, svc *Service, inst containerInstance, rps *replicaProbeState, stopCh <-chan struct{}) {
	probe := svc.probe.withDefaults()
	job := &probeJob{
		stack:    stack,
		service:  svc,
		instance: inst,
		rps:      rps,
		probe:    probe,
	}
	// Initial delay (if not already paid by readiness gate).
	if probe.InitialDelay > 0 && rps.snapshot().Attempts == 0 {
		select {
		case <-stopCh:
			job.stopped = true
			return
		case <-time.After(probe.InitialDelay):
		}
	}
	// Schedule the first liveness tick. The scheduler will re-add
	// the job after each execution while in liveness mode.
	s.schedule(job, time.Now().Add(jitter(probe.Interval, probe.Jitter)))
	<-stopCh
	job.stopOnce.Do(func() { job.stopped = true })
}

// === helpers ===

func jitter(base, j time.Duration) time.Duration {
	if j <= 0 {
		return base
	}
	d := rand.Int63n(int64(2 * j))
	return base + time.Duration(d) - j
}
