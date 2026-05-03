package network

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// === HTTP/HTTPS gateway ===
//
// httpListener provides L7 reverse-proxy behavior on top of the
// stack's gateway. Built atop net/http/httputil.ReverseProxy with a
// custom Director that picks the backend per-request via the same
// load balancer + circuit breaker the TCP path uses.
//
// TLS termination uses tls.Config built from the ingress's
// tlsConfig. mTLS is enabled by setting WithMutualTLS.

type httpListener struct {
	gw  *Gateway
	ing *Ingress

	server *http.Server
	ln     net.Listener

	wg     sync.WaitGroup
	stopc  chan struct{}
	closed atomic.Bool

	rateMu sync.Mutex
	rates  map[string]*tokenBucket // source IP → bucket
}

func newHTTPListener(g *Gateway, ing *Ingress, bind string) (*httpListener, error) {
	hl := &httpListener{
		gw:    g,
		ing:   ing,
		stopc: make(chan struct{}),
		rates: map[string]*tokenBucket{},
	}

	// Director picks the backend for each request.
	director := func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = "" // resolved per-request via Transport.DialContext
		req.Host = req.URL.Host
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return hl.dialBackend(ctx, req2sourceIP(ctx))
		},
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
	}

	proxy := &httputil.ReverseProxy{
		Director:  director,
		Transport: transport,
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			rw.WriteHeader(http.StatusBadGateway)
			g.monitor.emit(Event{
				Kind:        EventConnectionDenied,
				At:          time.Now(),
				IngressName: ing.name,
				Service:     ing.serviceName,
				SourceAddr:  r.RemoteAddr,
				DenyReason:  "backend error: " + err.Error(),
			})
		},
	}

	// Wrap with rule + rate-limit + counting middleware.
	handler := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		srcIP := net.ParseIP(host)
		if srcIP == nil {
			srcIP = net.IPv4zero
		}
		// Rules.
		rs := ing.rules.load()
		if rs != nil {
			if ok, reason := rs.allowed(srcIP); !ok {
				g.monitor.emit(Event{
					Kind:        EventConnectionDenied,
					At:          time.Now(),
					IngressName: ing.name,
					Service:     ing.serviceName,
					SourceAddr:  r.RemoteAddr,
					DenyReason:  reason,
				})
				http.Error(rw, "forbidden", http.StatusForbidden)
				return
			}
		}
		// Rate limit.
		if ing.maxNewPerSecondPerSrc > 0 {
			hl.rateMu.Lock()
			tb, ok := hl.rates[host]
			if !ok {
				tb = newTokenBucket(ing.maxNewPerSecondPerSrc, ing.maxNewPerSecondPerSrc)
				hl.rates[host] = tb
			}
			ok = tb.take()
			hl.rateMu.Unlock()
			if !ok {
				g.monitor.emit(Event{
					Kind:        EventConnectionDenied,
					At:          time.Now(),
					IngressName: ing.name,
					Service:     ing.serviceName,
					SourceAddr:  r.RemoteAddr,
					DenyReason:  "rate limit",
				})
				http.Error(rw, "too many requests", http.StatusTooManyRequests)
				return
			}
		}
		// Stash source IP into the request context so transport.DialContext
		// can use it for sticky/consistent-hash backend selection.
		ctx := context.WithValue(r.Context(), sourceIPKey{}, host)
		r = r.WithContext(ctx)

		startedAt := time.Now()
		g.monitor.emit(Event{
			Kind:        EventConnectionAccepted,
			At:          startedAt,
			IngressName: ing.name,
			Service:     ing.serviceName,
			SourceAddr:  r.RemoteAddr,
		})
		bw := &byteCountingResponseWriter{ResponseWriter: rw}
		proxy.ServeHTTP(bw, r)
		g.monitor.emit(Event{
			Kind:        EventConnectionClosed,
			At:          time.Now(),
			IngressName: ing.name,
			Service:     ing.serviceName,
			SourceAddr:  r.RemoteAddr,
			BytesOut:    bw.bytes.Load(),
			Duration:    time.Since(startedAt),
		})
	})

	hl.server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Bind.
	var ln net.Listener
	var err error
	switch ing.protocol {
	case "https":
		tlsCfg, terr := buildTLSConfig(ing.tls)
		if terr != nil {
			return nil, fmt.Errorf("tls config: %w", terr)
		}
		ln, err = tls.Listen("tcp", bind, tlsCfg)
	default:
		ln, err = net.Listen("tcp", bind)
	}
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", bind, err)
	}
	hl.ln = ln
	ing.mu.Lock()
	ing.listener = ln
	ing.addr = ln.Addr().String()
	ing.mu.Unlock()
	return hl, nil
}

func (hl *httpListener) start() {
	hl.wg.Add(1)
	go func() {
		defer hl.wg.Done()
		if err := hl.server.Serve(hl.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			hl.gw.monitor.emit(Event{
				Kind:        EventConnectionDenied,
				At:          time.Now(),
				IngressName: hl.ing.name,
				Service:     hl.ing.serviceName,
				DenyReason:  "server error: " + err.Error(),
			})
		}
	}()
}

func (hl *httpListener) close(drainTimeout time.Duration) error {
	if !hl.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(hl.stopc)
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	_ = hl.server.Shutdown(ctx)
	hl.wg.Wait()
	return nil
}

// dialBackend picks a healthy replica and dials it. Used by the
// reverse proxy's Transport.
func (hl *httpListener) dialBackend(ctx context.Context, srcIP string) (net.Conn, error) {
	addr, ok := hl.pickBackendForHTTP(srcIP)
	if !ok {
		return nil, errors.New("no healthy backend")
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

func (hl *httpListener) pickBackendForHTTP(srcIP string) (string, bool) {
	insts := hl.gw.stack.containerByService(hl.ing.serviceName)
	probes := hl.gw.stack.probesByService(hl.ing.serviceName)
	healthyIdx := []int{}
	for i := range insts {
		if i < len(probes) {
			if probes[i].snapshot().Healthy {
				healthyIdx = append(healthyIdx, i)
			}
		} else {
			healthyIdx = append(healthyIdx, i)
		}
	}
	if len(healthyIdx) == 0 {
		return "", false
	}
	// Apply circuit breaker: filter out replicas whose breaker is open.
	if hl.ing.cbEnabled && hl.ing.breakers != nil {
		filtered := healthyIdx[:0]
		for _, idx := range healthyIdx {
			cb := hl.ing.breakers.get(insts[idx].id, hl.ing.cbFailureThreshold, hl.ing.cbOpenDuration, hl.ing.cbHalfOpenAllow)
			if cb.allow() {
				filtered = append(filtered, idx)
				cb.recordSuccess() // optimistic; we don't track HTTP success here per-request
			}
		}
		healthyIdx = filtered
		if len(healthyIdx) == 0 {
			return "", false
		}
	}
	var pick int
	switch hl.ing.lb {
	case LBConsistentHash, LBStickySource:
		ip := net.ParseIP(srcIP)
		if ip == nil {
			ip = net.IPv4zero
		}
		h := uint32(0)
		for _, b := range ip {
			h = h*1000003 ^ uint32(b)
		}
		pick = healthyIdx[int(h)%len(healthyIdx)]
	default:
		n := atomic.AddUint64(&hl.ing.rrCounter, 1)
		pick = healthyIdx[int(n-1)%len(healthyIdx)]
	}
	addr, err := hl.gw.resolveBackend(context.Background(), insts[pick], hl.ing.servicePort, "tcp")
	if err != nil {
		return "", false
	}
	return addr, true
}

// === TLS config ===

func buildTLSConfig(tc *tlsConfig) (*tls.Config, error) {
	if tc == nil {
		return nil, errors.New("https ingress requires WithTLS option")
	}
	if tc.certFile == "" || tc.keyFile == "" {
		// Autocert path is intentionally deferred — Let's Encrypt
		// requires a publicly-reachable :80, which doesn't fit the
		// local-development default. Users wanting autocert can
		// supply a pre-generated cert via certFile/keyFile.
		return nil, errors.New("https ingress requires certFile + keyFile (autocert deferred to follow-up)")
	}
	cert, err := tls.LoadX509KeyPair(tc.certFile, tc.keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS cert/key: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if tc.requireClientCert && tc.clientCAFile != "" {
		caBytes, err := os.ReadFile(tc.clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("could not parse client CA pem")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// === helpers ===

type sourceIPKey struct{}

func req2sourceIP(ctx context.Context) string {
	if v, ok := ctx.Value(sourceIPKey{}).(string); ok {
		return v
	}
	return ""
}

type byteCountingResponseWriter struct {
	http.ResponseWriter
	bytes atomic.Uint64
}

func (w *byteCountingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes.Add(uint64(n))
	return n, err
}

// keep io imported for byte counter symmetry
var _ = io.Discard

// must reference url to silence unused-import in environments that
// might not have url accessed at compile time.
var _ = url.URL{}
