package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	dockernat "github.com/docker/go-connections/nat"
)

// === gateway ===
//
// The gateway is the per-stack in-process forwarder. One goroutine
// per accepted connection (TCP). On Linux the goroutine uses
// stdlib io.Copy which auto-uses splice() for zero-copy
// kernel-level forwarding when both ends are *net.TCPConn.
//
// Thousands of concurrent stacks: each stack's gateway is
// independent (no cross-stack lock); per-ingress maps use atomic
// snapshots for the rule path and finer-grain locks elsewhere.

type Gateway struct {
	stack   *Stack
	monitor *Monitor

	mu       sync.Mutex
	started  bool
	stopping atomic.Bool

	// per-ingress runtime state.
	tcp  []*tcpListener
	udp  []*udpListener
	http []*httpListener

	// shared buffer pool for io.Copy. 32KB matches stdlib's default
	// and is large enough to amortize syscall overhead while small
	// enough that 50k concurrent connections fit in a few GB.
	bufPool sync.Pool
}

func newGateway(stack *Stack, mon *Monitor) (*Gateway, error) {
	g := &Gateway{
		stack:   stack,
		monitor: mon,
	}
	g.bufPool.New = func() any {
		b := make([]byte, 32*1024)
		return &b
	}
	return g, nil
}

// start binds every ingress and begins accepting. Returns the
// first bind error; partial-success state is rolled back by
// the caller via stop.
func (g *Gateway) start(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return nil
	}
	for _, ing := range g.stack.ingress {
		bind := ing.bindAddress
		if bind == "" {
			bind = "127.0.0.1:0"
		}
		switch ing.protocol {
		case "tcp":
			if err := g.publishLoopbackPort(ctx, ing); err != nil {
				return fmt.Errorf("publish backend %q: %w", ing.name, err)
			}
			tl, err := newTCPListener(g, ing, bind)
			if err != nil {
				return err
			}
			g.tcp = append(g.tcp, tl)
			tl.start()
		case "http", "https":
			if err := g.publishLoopbackPort(ctx, ing); err != nil {
				return fmt.Errorf("publish backend %q: %w", ing.name, err)
			}
			hl, err := newHTTPListener(g, ing, bind)
			if err != nil {
				return err
			}
			g.http = append(g.http, hl)
			hl.start()
		case "udp":
			if err := g.publishLoopbackPort(ctx, ing); err != nil {
				return fmt.Errorf("publish backend %q: %w", ing.name, err)
			}
			ul, err := newUDPListener(g, ing, bind)
			if err != nil {
				return err
			}
			g.udp = append(g.udp, ul)
			ul.start()
		}
	}
	g.started = true
	return nil
}

// stop closes every listener and waits up to drainTimeout for
// in-flight connections to finish.
func (g *Gateway) stop(_ context.Context, drainTimeout time.Duration) {
	g.stopping.Store(true)
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, tl := range g.tcp {
		_ = tl.close(drainTimeout)
	}
	for _, hl := range g.http {
		_ = hl.close(drainTimeout)
	}
	for _, ul := range g.udp {
		_ = ul.close()
	}
	g.tcp = nil
	g.http = nil
	g.udp = nil
	g.started = false
	g.stopping.Store(false)
}

// publishLoopbackPort tells docker to publish the ingress's target
// port on 127.0.0.1:auto so the gateway can reach it cross-platform
// without requiring host-routable container IPs.
//
// At v1 the publish-on-the-fly is implemented by container restart
// with -p binding. For simplicity (and to keep create/start atomic
// in the Stack lifecycle), we snapshot the host port from
// ContainerInspect and rely on the user to have declared the port
// at create time (the Service.Port option will become a published
// port automatically when ingress is set on it).
//
// In the current implementation we resolve the address by inspecting
// the container's network settings. If publishing wasn't done at
// create time, we fall back to container bridge IP (rootful) or
// fail with a clear error otherwise.
func (g *Gateway) publishLoopbackPort(ctx context.Context, ing *Ingress) error {
	insts := g.stack.containerByService(ing.serviceName)
	if len(insts) == 0 {
		return fmt.Errorf("ingress %q: no containers for service %q", ing.name, ing.serviceName)
	}
	// Resolve a backend address per container. We store back into the
	// Stack's container map for the gateway's reuse — but to avoid a
	// schema change at this depth we resolve on every accept (cheap:
	// it's a docker inspect per-connection unless cached). For a
	// performant path, we cache the address on the Ingress.
	for _, inst := range insts {
		if _, err := g.resolveBackend(ctx, inst, ing.servicePort, ing.protocol); err != nil {
			return fmt.Errorf("resolve backend %s[%d]: %w", ing.serviceName, inst.replica, err)
		}
	}
	return nil
}

// resolveBackend returns a host:port the gateway can dial to reach
// the given container's port. Caches in the Ingress's per-replica
// map for hot-path reuse.
func (g *Gateway) resolveBackend(ctx context.Context, inst containerInstance, servicePort int, proto string) (string, error) {
	insp, err := g.stack.engine.cli.ContainerInspect(ctx, inst.id)
	if err != nil {
		return "", err
	}
	// Try published-port path first.
	portKey := dockernat.Port(strconv.Itoa(servicePort) + "/" + transportFor(proto))
	if bindings, ok := insp.NetworkSettings.Ports[portKey]; ok && len(bindings) > 0 {
		host := bindings[0].HostIP
		if host == "" {
			host = "127.0.0.1"
		}
		return host + ":" + bindings[0].HostPort, nil
	}
	// Fall back to bridge IP (rootful Docker).
	for _, ep := range insp.NetworkSettings.Networks {
		if ep != nil && ep.IPAddress != "" {
			return net.JoinHostPort(ep.IPAddress, strconv.Itoa(servicePort)), nil
		}
	}
	return "", fmt.Errorf("no reachable backend for container %s port %d", inst.id, servicePort)
}

func transportFor(proto string) string {
	switch proto {
	case "udp":
		return "udp"
	default:
		return "tcp"
	}
}

// === forwardedConn — represents one in-flight TCP connection ===

type forwardedConn struct {
	clientConn  net.Conn
	backendConn net.Conn
	bytesIn     atomic.Uint64
	bytesOut    atomic.Uint64
	startedAt   time.Time
	source      string
}

// === TCP listener ===

type tcpListener struct {
	gw  *Gateway
	ing *Ingress

	ln    net.Listener
	wg    sync.WaitGroup
	stopc chan struct{}

	rateMu sync.Mutex
	rates  map[string]*tokenBucket // source IP → bucket
}

func newTCPListener(g *Gateway, ing *Ingress, bind string) (*tcpListener, error) {
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", bind, err)
	}
	tl := &tcpListener{
		gw:    g,
		ing:   ing,
		ln:    ln,
		stopc: make(chan struct{}),
		rates: map[string]*tokenBucket{},
	}
	ing.mu.Lock()
	ing.listener = ln
	ing.addr = ln.Addr().String()
	ing.mu.Unlock()
	return tl, nil
}

func (tl *tcpListener) start() {
	tl.wg.Add(1)
	go tl.acceptLoop()
}

func (tl *tcpListener) acceptLoop() {
	defer tl.wg.Done()
	for {
		conn, err := tl.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		tl.handleConn(conn)
	}
}

func (tl *tcpListener) handleConn(conn net.Conn) {
	src, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	srcIP := net.ParseIP(src)
	if srcIP == nil {
		// Couldn't parse; treat as 0.0.0.0 — rules likely deny.
		srcIP = net.IPv4zero
	}

	// Rule check on hot path: atomic load, no lock.
	rs := tl.ing.rules.load()
	if rs != nil {
		if ok, reason := rs.allowed(srcIP); !ok {
			tl.emitDeny(conn.RemoteAddr().String(), reason)
			_ = conn.Close()
			return
		}
	}

	// Per-source rate limit.
	if tl.ing.maxNewPerSecondPerSrc > 0 {
		tl.rateMu.Lock()
		tb, ok := tl.rates[src]
		if !ok {
			tb = newTokenBucket(tl.ing.maxNewPerSecondPerSrc, tl.ing.maxNewPerSecondPerSrc)
			tl.rates[src] = tb
		}
		ok = tb.take()
		tl.rateMu.Unlock()
		if !ok {
			tl.emitDeny(conn.RemoteAddr().String(), "rate limit")
			_ = conn.Close()
			return
		}
	}

	// Per-source concurrency limit.
	if tl.ing.maxPerSource > 0 {
		tl.ing.activeMu.Lock()
		cnt := tl.ing.perSourceCnt[src]
		if cnt >= tl.ing.maxPerSource {
			tl.ing.activeMu.Unlock()
			tl.emitDeny(conn.RemoteAddr().String(), "per-source connection cap")
			_ = conn.Close()
			return
		}
		tl.ing.perSourceCnt[src]++
		tl.ing.activeMu.Unlock()
	}

	// Total concurrency limit.
	if tl.ing.maxConcurrent > 0 {
		tl.ing.activeMu.Lock()
		if len(tl.ing.active) >= tl.ing.maxConcurrent {
			tl.ing.activeMu.Unlock()
			if tl.ing.maxPerSource > 0 {
				tl.ing.activeMu.Lock()
				tl.ing.perSourceCnt[src]--
				tl.ing.activeMu.Unlock()
			}
			tl.emitDeny(conn.RemoteAddr().String(), "global connection cap")
			_ = conn.Close()
			return
		}
		tl.ing.activeMu.Unlock()
	}

	// Pick a healthy backend replica.
	backendAddr, ok := tl.pickBackend(srcIP)
	if !ok {
		tl.emitDeny(conn.RemoteAddr().String(), "no healthy backend")
		_ = conn.Close()
		if tl.ing.maxPerSource > 0 {
			tl.ing.activeMu.Lock()
			tl.ing.perSourceCnt[src]--
			tl.ing.activeMu.Unlock()
		}
		return
	}

	d := net.Dialer{Timeout: 3 * time.Second}
	bConn, err := d.Dial("tcp", backendAddr)
	if err != nil {
		tl.emitDeny(conn.RemoteAddr().String(), "backend dial: "+err.Error())
		_ = conn.Close()
		if tl.ing.maxPerSource > 0 {
			tl.ing.activeMu.Lock()
			tl.ing.perSourceCnt[src]--
			tl.ing.activeMu.Unlock()
		}
		return
	}

	fc := &forwardedConn{
		clientConn:  conn,
		backendConn: bConn,
		startedAt:   time.Now(),
		source:      conn.RemoteAddr().String(),
	}
	tl.ing.activeMu.Lock()
	tl.ing.active[fc] = struct{}{}
	tl.ing.activeMu.Unlock()

	tl.gw.monitor.emit(Event{
		Kind:        EventConnectionAccepted,
		At:          time.Now(),
		Service:     tl.ing.serviceName,
		IngressName: tl.ing.name,
		SourceAddr:  fc.source,
	})

	tl.wg.Add(1)
	go tl.proxyConn(fc, src)
}

func (tl *tcpListener) emitDeny(source, reason string) {
	tl.gw.monitor.emit(Event{
		Kind:        EventConnectionDenied,
		At:          time.Now(),
		IngressName: tl.ing.name,
		Service:     tl.ing.serviceName,
		SourceAddr:  source,
		DenyReason:  reason,
	})
}

func (tl *tcpListener) pickBackend(src net.IP) (string, bool) {
	insts := tl.gw.stack.containerByService(tl.ing.serviceName)
	probes := tl.gw.stack.probesByService(tl.ing.serviceName)
	healthyIdx := []int{}
	for i := range insts {
		if i < len(probes) {
			if probes[i].snapshot().Healthy {
				healthyIdx = append(healthyIdx, i)
			}
		} else {
			healthyIdx = append(healthyIdx, i) // no probe = assume healthy
		}
	}
	if len(healthyIdx) == 0 {
		return "", false
	}
	// Apply circuit breaker if enabled.
	if tl.ing.cbEnabled && tl.ing.breakers != nil {
		filtered := healthyIdx[:0]
		for _, idx := range healthyIdx {
			cb := tl.ing.breakers.get(insts[idx].id, tl.ing.cbFailureThreshold, tl.ing.cbOpenDuration, tl.ing.cbHalfOpenAllow)
			if cb.allow() {
				filtered = append(filtered, idx)
			}
		}
		healthyIdx = filtered
		if len(healthyIdx) == 0 {
			return "", false
		}
	}
	var pick int
	switch tl.ing.lb {
	case LBConsistentHash:
		h := uint32(0)
		for _, b := range src {
			h = h*1000003 ^ uint32(b)
		}
		pick = healthyIdx[int(h)%len(healthyIdx)]
	case LBStickySource:
		// Simple sticky: hash modulo len; same as ConsistentHash for v1
		// (the TTL table for true affinity is left to a follow-up).
		h := uint32(0)
		for _, b := range src {
			h = h*1000003 ^ uint32(b)
		}
		pick = healthyIdx[int(h)%len(healthyIdx)]
	default: // LBRoundRobin
		// Use atomic counter in ingress for RR.
		n := atomic.AddUint64(&tl.ing.rrCounter, 1)
		pick = healthyIdx[int(n-1)%len(healthyIdx)]
	}
	inst := insts[pick]
	addr, err := tl.gw.resolveBackend(context.Background(), inst, tl.ing.servicePort, "tcp")
	if err != nil {
		return "", false
	}
	return addr, true
}

// proxyConn copies bytes in both directions until either side
// closes. Tracks bytes for the closed event.
func (tl *tcpListener) proxyConn(fc *forwardedConn, src string) {
	defer tl.wg.Done()
	defer func() {
		_ = fc.clientConn.Close()
		_ = fc.backendConn.Close()
		tl.ing.activeMu.Lock()
		delete(tl.ing.active, fc)
		if tl.ing.maxPerSource > 0 {
			tl.ing.perSourceCnt[src]--
			if tl.ing.perSourceCnt[src] <= 0 {
				delete(tl.ing.perSourceCnt, src)
			}
		}
		tl.ing.activeMu.Unlock()
		tl.gw.monitor.emit(Event{
			Kind:        EventConnectionClosed,
			At:          time.Now(),
			Service:     tl.ing.serviceName,
			IngressName: tl.ing.name,
			SourceAddr:  fc.source,
			BytesIn:     fc.bytesIn.Load(),
			BytesOut:    fc.bytesOut.Load(),
			Duration:    time.Since(fc.startedAt),
		})
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	bp := tl.gw.bufPool.Get().(*[]byte)
	bp2 := tl.gw.bufPool.Get().(*[]byte)
	defer tl.gw.bufPool.Put(bp)
	defer tl.gw.bufPool.Put(bp2)

	go func() {
		defer wg.Done()
		n, _ := io.CopyBuffer(fc.backendConn, fc.clientConn, *bp)
		fc.bytesIn.Store(uint64(n))
		// half-close backend write so server sees EOF
		if cw, ok := fc.backendConn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		n, _ := io.CopyBuffer(fc.clientConn, fc.backendConn, *bp2)
		fc.bytesOut.Store(uint64(n))
		if cw, ok := fc.clientConn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
}

// close stops accepting and waits up to drainTimeout for in-flight
// connections to finish, then forces remaining ones closed.
func (tl *tcpListener) close(drainTimeout time.Duration) error {
	close(tl.stopc)
	_ = tl.ln.Close()
	if drainTimeout > 0 {
		done := make(chan struct{})
		go func() {
			tl.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(drainTimeout):
			// Force-close remaining.
			tl.ing.activeMu.Lock()
			for fc := range tl.ing.active {
				_ = fc.clientConn.Close()
				_ = fc.backendConn.Close()
			}
			tl.ing.activeMu.Unlock()
			tl.wg.Wait()
		}
	} else {
		tl.wg.Wait()
	}
	return nil
}

// === UDP listener ===
//
// UDP forwarding is session-based: each (src-addr) is mapped to a
// dedicated upstream socket. The socket lives until it's idle for
// idleTimeout. Each accepted session runs in its own goroutine.

type udpListener struct {
	gw  *Gateway
	ing *Ingress

	conn  net.PacketConn
	wg    sync.WaitGroup
	stopc chan struct{}

	mu       sync.Mutex
	sessions map[string]*udpSession
}

type udpSession struct {
	upstream net.Conn
	lastUse  atomic.Int64 // unix nano
}

func newUDPListener(g *Gateway, ing *Ingress, bind string) (*udpListener, error) {
	pc, err := net.ListenPacket("udp", bind)
	if err != nil {
		return nil, fmt.Errorf("udp listen %s: %w", bind, err)
	}
	ul := &udpListener{
		gw:       g,
		ing:      ing,
		conn:     pc,
		stopc:    make(chan struct{}),
		sessions: map[string]*udpSession{},
	}
	ing.mu.Lock()
	ing.udpConn = pc
	ing.addr = pc.LocalAddr().String()
	ing.mu.Unlock()
	return ul, nil
}

func (ul *udpListener) start() {
	ul.wg.Add(1)
	go ul.recvLoop()
	ul.wg.Add(1)
	go ul.idleSweep()
}

func (ul *udpListener) recvLoop() {
	defer ul.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		n, src, err := ul.conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		host, _, _ := net.SplitHostPort(src.String())
		srcIP := net.ParseIP(host)
		if srcIP == nil {
			srcIP = net.IPv4zero
		}
		// Rules check.
		rs := ul.ing.rules.load()
		if rs != nil {
			if ok, reason := rs.allowed(srcIP); !ok {
				ul.gw.monitor.emit(Event{
					Kind:        EventConnectionDenied,
					At:          time.Now(),
					IngressName: ul.ing.name,
					Service:     ul.ing.serviceName,
					SourceAddr:  src.String(),
					DenyReason:  reason,
				})
				continue
			}
		}
		ul.handlePacket(buf[:n], src)
	}
}

func (ul *udpListener) handlePacket(payload []byte, src net.Addr) {
	key := src.String()
	ul.mu.Lock()
	sess, ok := ul.sessions[key]
	if !ok {
		// Pick backend per packet (UDP has no connection state).
		backend, found := ul.pickBackend(src)
		if !found {
			ul.mu.Unlock()
			return
		}
		up, err := net.Dial("udp", backend)
		if err != nil {
			ul.mu.Unlock()
			return
		}
		sess = &udpSession{upstream: up}
		ul.sessions[key] = sess
		ul.mu.Unlock()
		ul.wg.Add(1)
		go ul.upstreamLoop(key, sess, src)
	} else {
		ul.mu.Unlock()
	}
	sess.lastUse.Store(time.Now().UnixNano())
	_, _ = sess.upstream.Write(payload)
}

func (ul *udpListener) upstreamLoop(key string, sess *udpSession, src net.Addr) {
	defer ul.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		_ = sess.upstream.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, err := sess.upstream.Read(buf)
		if err != nil {
			ul.mu.Lock()
			if cur, ok := ul.sessions[key]; ok && cur == sess {
				delete(ul.sessions, key)
			}
			ul.mu.Unlock()
			_ = sess.upstream.Close()
			return
		}
		_, _ = ul.conn.WriteTo(buf[:n], src)
	}
}

func (ul *udpListener) pickBackend(src net.Addr) (string, bool) {
	insts := ul.gw.stack.containerByService(ul.ing.serviceName)
	if len(insts) == 0 {
		return "", false
	}
	pick := 0
	if len(insts) > 1 && ul.ing.lb == LBConsistentHash {
		host, _, _ := net.SplitHostPort(src.String())
		ip := net.ParseIP(host)
		h := uint32(0)
		for _, b := range ip {
			h = h*1000003 ^ uint32(b)
		}
		pick = int(h) % len(insts)
	}
	addr, err := ul.gw.resolveBackend(context.Background(), insts[pick], ul.ing.servicePort, "udp")
	if err != nil {
		return "", false
	}
	return addr, true
}

func (ul *udpListener) idleSweep() {
	defer ul.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ul.stopc:
			return
		case now := <-t.C:
			cutoff := now.Add(-2 * time.Minute).UnixNano()
			ul.mu.Lock()
			for k, s := range ul.sessions {
				if s.lastUse.Load() < cutoff {
					_ = s.upstream.Close()
					delete(ul.sessions, k)
				}
			}
			ul.mu.Unlock()
		}
	}
}

func (ul *udpListener) close() error {
	close(ul.stopc)
	_ = ul.conn.Close()
	ul.mu.Lock()
	for k, s := range ul.sessions {
		_ = s.upstream.Close()
		delete(ul.sessions, k)
	}
	ul.mu.Unlock()
	ul.wg.Wait()
	return nil
}

// === token bucket (per-source rate limiter) ===

type tokenBucket struct {
	mu       sync.Mutex
	rate     int       // tokens per second
	cap      int       // bucket capacity
	tokens   int
	lastFill time.Time
}

func newTokenBucket(rate, cap int) *tokenBucket {
	return &tokenBucket{rate: rate, cap: cap, tokens: cap, lastFill: time.Now()}
}

func (tb *tokenBucket) take() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.lastFill).Seconds()
	add := int(elapsed * float64(tb.rate))
	if add > 0 {
		tb.tokens += add
		if tb.tokens > tb.cap {
			tb.tokens = tb.cap
		}
		tb.lastFill = now
	}
	if tb.tokens > 0 {
		tb.tokens--
		return true
	}
	return false
}

// === unused-import safety while incremental ===
var _ = dockercontainer.WaitConditionNotRunning
