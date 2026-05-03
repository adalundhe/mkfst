package network

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// === ingress declaration ===

// Ingress is a declared host-side entrypoint into the stack. Created
// via Stack.Ingress(name, service, port, opts...). The actual
// listener is started by Stack.Up; the assigned address (host:port)
// is read via Address() once the gateway is up.
type Ingress struct {
	name        string
	serviceName string
	servicePort int
	protocol    string

	// bindAddress, when non-empty, forces the listener to a
	// specific host:port (e.g., "127.0.0.1:8080"). Empty defaults
	// to "127.0.0.1:0" — kernel-assigned ephemeral port.
	bindAddress string

	rules ruleHolder

	// runtime state populated by gateway.start.
	mu       sync.RWMutex
	listener net.Listener // TCP listener (if proto=tcp)
	udpConn  net.PacketConn // UDP listener (if proto=udp)
	addr     string        // resolved host:port
	closed   atomic.Bool

	// connection limits (zero = unlimited).
	maxConcurrent           int
	maxPerSource            int
	maxNewPerSecondPerSrc   int

	// load balancing strategy.
	lb LBStrategy

	// connection tracking for limits + drain.
	activeMu      sync.Mutex
	active        map[*forwardedConn]struct{}
	perSourceCnt  map[string]int

	// for HTTP/HTTPS ingress (future).
	tls *tlsConfig

	// rrCounter is the round-robin pick counter (atomic).
	rrCounter uint64

	// breakers tracks per-replica circuit breakers for this
	// ingress (lazy-allocated). Reads from pickBackend.
	breakers *breakerRegistry

	// Circuit-breaker tunables.
	cbFailureThreshold float64       // EWMA above which we trip
	cbOpenDuration     time.Duration // how long to stay open
	cbHalfOpenAllow    int           // probes allowed in half-open
	cbEnabled          bool

	// optErrs accumulates errors from option closures (CIDR
	// parsing, etc.) that can't return errors directly. Surfaced
	// by Stack.Ingress.
	optErrs []error
}

// EnableCircuitBreaker enables per-replica circuit breaking. failureRate
// is the EWMA of failures above which a replica is opened (default 0.5).
// openDuration is the cool-down before half-open (default 5s).
// halfOpenAllow is the number of probe requests permitted in half-open
// (default 1).
func EnableCircuitBreaker(failureRate float64, openDuration time.Duration, halfOpenAllow int) IngressOption {
	return func(i *Ingress) {
		i.cbEnabled = true
		i.cbFailureThreshold = failureRate
		i.cbOpenDuration = openDuration
		i.cbHalfOpenAllow = halfOpenAllow
	}
}

// IngressOption customizes an ingress.
type IngressOption func(*Ingress)

// Protocol sets the ingress protocol ("tcp", "udp", "http", "https").
// Default "tcp".
func Protocol(proto string) IngressOption {
	return func(i *Ingress) { i.protocol = proto }
}

// BindAddress overrides the default ephemeral binding. Use this for
// stable known addresses (e.g., "127.0.0.1:8080"). Returns an error
// at Up time if the bind fails (port already in use).
func BindAddress(addr string) IngressOption {
	return func(i *Ingress) { i.bindAddress = addr }
}

// MaxConcurrent caps total concurrent connections through this
// ingress. New connections beyond the cap are accepted then closed
// with a denial event.
func MaxConcurrent(n int) IngressOption {
	return func(i *Ingress) { i.maxConcurrent = n }
}

// MaxPerSource caps concurrent connections from a single source IP.
// Defends against single-source connection floods.
func MaxPerSource(n int) IngressOption {
	return func(i *Ingress) { i.maxPerSource = n }
}

// MaxNewPerSecondPerSource caps the new-connection rate per source IP.
// Token-bucket limiter; sustained excess connections are denied.
func MaxNewPerSecondPerSource(n int) IngressOption {
	return func(i *Ingress) { i.maxNewPerSecondPerSrc = n }
}

// LoadBalancer sets the replica selection strategy.
func LoadBalancer(strategy LBStrategy) IngressOption {
	return func(i *Ingress) { i.lb = strategy }
}

// AllowSource adds a source-IP allow rule. If any allow rule is set,
// default-deny applies. Invalid CIDRs are deferred to Stack.Ingress
// which returns the error.
func AllowSource(cidr string) IngressOption {
	return func(i *Ingress) {
		n, err := parseCIDR(cidr)
		if err != nil {
			i.optErrs = append(i.optErrs, fmt.Errorf("AllowSource %q: %w", cidr, err))
			return
		}
		i.rules.update(func(rs *ruleSet) {
			rs.allows = append(rs.allows, n)
		})
	}
}

// DenySource adds a source-IP deny rule. Evaluated after Allow.
// Invalid CIDRs are deferred to Stack.Ingress.
func DenySource(cidr string) IngressOption {
	return func(i *Ingress) {
		n, err := parseCIDR(cidr)
		if err != nil {
			i.optErrs = append(i.optErrs, fmt.Errorf("DenySource %q: %w", cidr, err))
			return
		}
		i.rules.update(func(rs *ruleSet) {
			rs.denies = append(rs.denies, n)
		})
	}
}

// WithTLS terminates TLS at the gateway. certFile / keyFile are
// PEM-encoded paths. Pass empty strings to use autocert (Let's
// Encrypt) — host must be reachable on :80 for the ACME challenge.
func WithTLS(certFile, keyFile string) IngressOption {
	return func(i *Ingress) {
		i.tls = &tlsConfig{certFile: certFile, keyFile: keyFile}
	}
}

// WithMutualTLS additionally requires client certs signed by
// caFile.
func WithMutualTLS(caFile string) IngressOption {
	return func(i *Ingress) {
		if i.tls == nil {
			i.tls = &tlsConfig{}
		}
		i.tls.clientCAFile = caFile
		i.tls.requireClientCert = true
	}
}

// === Stack.Ingress ===

// Ingress declares a new ingress on the stack. Call before Up. The
// returned *Ingress reports its assigned host:port address after
// the stack is Up, via Address().
func (s *Stack) Ingress(name, serviceName string, servicePort int, opts ...IngressOption) (*Ingress, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: ingress name required", ErrInvalidConfig)
	}
	if serviceName == "" {
		return nil, fmt.Errorf("%w: ingress service required", ErrInvalidConfig)
	}
	if servicePort < 1 || servicePort > 65535 {
		return nil, fmt.Errorf("%w: invalid service port %d", ErrInvalidConfig, servicePort)
	}
	ing := &Ingress{
		name:         name,
		serviceName:  serviceName,
		servicePort:  servicePort,
		protocol:     "tcp",
		active:       map[*forwardedConn]struct{}{},
		perSourceCnt: map[string]int{},
		lb:           LBRoundRobin,
		breakers:     newBreakerRegistry(),
	}
	ing.rules.store(&ruleSet{})
	for _, opt := range opts {
		opt(ing)
	}
	if len(ing.optErrs) > 0 {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, errors.Join(ing.optErrs...))
	}
	switch ing.protocol {
	case "tcp", "udp", "http", "https":
	default:
		return nil, fmt.Errorf("%w: unsupported ingress protocol %q", ErrInvalidConfig, ing.protocol)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ingress = append(s.ingress, ing)
	return ing, nil
}

// === Ingress methods ===

// Name returns the ingress name.
func (i *Ingress) Name() string { return i.name }

// Address returns the resolved host:port the gateway is listening
// on. Empty until the stack is Up.
func (i *Ingress) Address() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.addr
}

// SetRules atomically replaces the source-IP rule set. Hot path
// readers get the new snapshot on their next connection.
func (i *Ingress) SetRules(allow, deny []string) error {
	rs := &ruleSet{}
	for _, c := range allow {
		n, err := parseCIDR(c)
		if err != nil {
			return err
		}
		rs.allows = append(rs.allows, n)
	}
	for _, c := range deny {
		n, err := parseCIDR(c)
		if err != nil {
			return err
		}
		rs.denies = append(rs.denies, n)
	}
	i.rules.store(rs)
	return nil
}

// === rules ===

type ruleSet struct {
	allows []*net.IPNet
	denies []*net.IPNet
}

// allowed reports whether the source IP passes the rule set.
// Default-allow when no rules are present; default-deny when at
// least one allow rule exists; denies always win.
func (rs *ruleSet) allowed(ip net.IP) (bool, string) {
	for _, n := range rs.denies {
		if n.Contains(ip) {
			return false, "denied by " + n.String()
		}
	}
	if len(rs.allows) == 0 {
		return true, ""
	}
	for _, n := range rs.allows {
		if n.Contains(ip) {
			return true, ""
		}
	}
	return false, "no allow rule matches"
}

// ruleHolder wraps the atomic.Pointer for rule snapshots.
type ruleHolder struct {
	p atomic.Pointer[ruleSet]
}

func (h *ruleHolder) load() *ruleSet { return h.p.Load() }
func (h *ruleHolder) store(rs *ruleSet) { h.p.Store(rs) }
func (h *ruleHolder) update(fn func(*ruleSet)) {
	for {
		cur := h.load()
		next := &ruleSet{}
		if cur != nil {
			next.allows = append(next.allows, cur.allows...)
			next.denies = append(next.denies, cur.denies...)
		}
		fn(next)
		if h.p.CompareAndSwap(cur, next) {
			return
		}
	}
}

// === helpers ===

func parseCIDR(s string) (*net.IPNet, error) {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		// Allow bare IPs by appending /32 (or /128 for IPv6).
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, errors.New("invalid CIDR or IP: " + s)
		}
		if ip.To4() != nil {
			_, n, err = net.ParseCIDR(s + "/32")
		} else {
			_, n, err = net.ParseCIDR(s + "/128")
		}
		if err != nil {
			return nil, err
		}
	}
	return n, nil
}


// tlsConfig holds the parsed TLS settings.
type tlsConfig struct {
	certFile          string
	keyFile           string
	clientCAFile      string
	requireClientCert bool
}

// LBStrategy enumerates load-balancing strategies for replica
// selection.
type LBStrategy int

const (
	// LBRoundRobin cycles through healthy replicas.
	LBRoundRobin LBStrategy = iota
	// LBConsistentHash hashes the source IP to a replica — same
	// source always lands on the same replica unless the replica
	// set changes.
	LBConsistentHash
	// LBStickySource is like ConsistentHash but tracks per-source
	// replica assignments in a TTL'd table; new sources pick
	// round-robin then stick.
	LBStickySource
)
