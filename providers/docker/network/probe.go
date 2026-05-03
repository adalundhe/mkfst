package network

import (
	"errors"
	"fmt"
	"time"
)

// === probe modes ===

// ProbeMode picks readiness vs liveness semantics. Per the design,
// each service has at most one probe, used in one of these modes.
type ProbeMode int

const (
	// ProbeReadiness: Stack.Up blocks until the probe passes.
	// Once passed, probing stops — the service is considered ready
	// and dependents may start.
	ProbeReadiness ProbeMode = iota + 1
	// ProbeLiveness: Stack.Up still waits for the first success
	// (so dependents can start), but probing continues
	// indefinitely. On consecutive failures (FailureThreshold),
	// the container is restarted per the service's RestartPolicy.
	ProbeLiveness
)

// === probe protocols ===

// ProbeKind enumerates the supported probe transports.
type ProbeKind int

const (
	ProbeTCP ProbeKind = iota + 1
	ProbeHTTP
	ProbeUDP
	ProbeGRPC
	ProbeExec
)

// Probe is the unified probe specification. Construct with one of
// the typed helpers (TCPProbe, HTTPProbe, UDPProbe, GRPCProbe,
// ExecProbe) and customize via the With* options.
type Probe struct {
	Kind ProbeKind

	// Common port (for non-Exec probes). Refers to the
	// container's internal port — the probe runner resolves this
	// to a reachable address per the probe location strategy.
	Port int

	// HTTP-specific.
	HTTPPath        string
	HTTPMethod      string // GET if empty
	HTTPHeaders     map[string]string
	HTTPExpectCode  int    // 200 if 0 (matches 200..399 if 0)
	HTTPExpectBody  string // optional substring

	// UDP-specific.
	UDPSend         []byte
	UDPExpectReply  bool
	UDPExpectBytes  []byte // optional exact match on reply

	// gRPC-specific.
	GRPCService string // empty = ""

	// Exec-specific. Cmd runs inside the target container via
	// docker exec. Exit code 0 = success.
	ExecCmd []string

	// Location decides where the probe runs from.
	Location ProbeLocation

	// Timing.
	InitialDelay     time.Duration // wait this long after start before first probe
	Interval         time.Duration // gap between probes (default 1s)
	Timeout          time.Duration // per-probe timeout (default 1s)
	FailureThreshold int           // consecutive failures before unhealthy (default 3)
	SuccessThreshold int           // consecutive successes before healthy (default 1; only meaningful for liveness recovery)

	// Jitter is added to Interval to avoid synchronized polling at
	// scale. Default Interval/4. 0 disables.
	Jitter time.Duration
}

// ProbeLocation picks the path the probe traverses.
type ProbeLocation int

const (
	// ProbeLocationAuto picks: ProbeLocationExec when the probe is
	// ProbeExec, otherwise ProbeLocationLoopback. This is the
	// default and works on every platform.
	ProbeLocationAuto ProbeLocation = iota
	// ProbeLocationLoopback resolves the target container's
	// loopback-published address (which is the same path the
	// gateway reaches it through). Best for cross-platform
	// uniformity. Only available when the service's port has been
	// loopback-published (Stack does this automatically when an
	// Ingress targets the port, or always when ProbeLoopbackForce
	// is set on the stack).
	ProbeLocationLoopback
	// ProbeLocationContainerIP connects directly to the
	// container's bridge IP. Reflects what other containers in
	// the same stack see. Only works on Linux rootful Docker
	// (host can route to bridge IPs); on Mac/Windows/rootless
	// this falls back to Loopback.
	ProbeLocationContainerIP
	// ProbeLocationExec runs ExecCmd inside the container. Closest
	// to "is the app actually answering its own loopback?" but
	// requires the image to have a working shell (or ExecCmd to
	// be a single statically-linked binary that's already in the
	// image). The only valid location for ProbeExec; selectable
	// for other kinds when the image carries a probe tool.
	ProbeLocationExec
)

// === typed constructors ===

// TCPProbe makes a TCP-connect probe against the given container
// port. Pass-through reachability: the probe succeeds if a TCP
// SYN/ACK handshake completes.
func TCPProbe(port int) *Probe {
	return &Probe{Kind: ProbeTCP, Port: port}
}

// HTTPProbe makes an HTTP probe. Default method GET, default
// expected status 200..399.
func HTTPProbe(port int, path string) *Probe {
	return &Probe{Kind: ProbeHTTP, Port: port, HTTPPath: path}
}

// UDPProbe makes a UDP probe. By default it just sends `send` and
// considers any reply a success; pass WithUDPExpect to require an
// exact-byte reply.
func UDPProbe(port int, send []byte) *Probe {
	return &Probe{
		Kind: ProbeUDP, Port: port, UDPSend: send, UDPExpectReply: true,
	}
}

// GRPCProbe makes a gRPC health-check probe (grpc.health.v1).
// service is the optional service name to check; empty = check
// overall server health.
func GRPCProbe(port int, service string) *Probe {
	return &Probe{Kind: ProbeGRPC, Port: port, GRPCService: service}
}

// ExecProbe makes an exec probe — runs cmd inside the container
// via docker exec; exit code 0 = success.
func ExecProbe(cmd ...string) *Probe {
	return &Probe{
		Kind:     ProbeExec,
		ExecCmd:  append([]string(nil), cmd...),
		Location: ProbeLocationExec,
	}
}

// === fluent customization ===

// WithMethod sets HTTP method.
func (p *Probe) WithMethod(method string) *Probe {
	p.HTTPMethod = method
	return p
}

// WithHeader sets one HTTP header.
func (p *Probe) WithHeader(k, v string) *Probe {
	if p.HTTPHeaders == nil {
		p.HTTPHeaders = map[string]string{}
	}
	p.HTTPHeaders[k] = v
	return p
}

// WithExpectStatus sets the expected HTTP status code (single).
// Default 0 = "any 2xx/3xx".
func (p *Probe) WithExpectStatus(code int) *Probe {
	p.HTTPExpectCode = code
	return p
}

// WithExpectBody requires the response body to contain substring s.
func (p *Probe) WithExpectBody(s string) *Probe {
	p.HTTPExpectBody = s
	return p
}

// WithUDPExpect requires the UDP reply to equal exactly want.
func (p *Probe) WithUDPExpect(want []byte) *Probe {
	p.UDPExpectReply = true
	p.UDPExpectBytes = append([]byte(nil), want...)
	return p
}

// WithLocation sets the probe location strategy. See ProbeLocation
// constants.
func (p *Probe) WithLocation(loc ProbeLocation) *Probe {
	p.Location = loc
	return p
}

// WithInitialDelay sets the wait between container start and first
// probe.
func (p *Probe) WithInitialDelay(d time.Duration) *Probe {
	p.InitialDelay = d
	return p
}

// WithInterval sets the gap between probes.
func (p *Probe) WithInterval(d time.Duration) *Probe {
	p.Interval = d
	return p
}

// WithTimeout sets the per-probe timeout.
func (p *Probe) WithTimeout(d time.Duration) *Probe {
	p.Timeout = d
	return p
}

// WithFailureThreshold sets consecutive-failures-to-unhealthy.
func (p *Probe) WithFailureThreshold(n int) *Probe {
	p.FailureThreshold = n
	return p
}

// WithSuccessThreshold sets consecutive-successes-to-healthy. Only
// affects liveness recovery (a service marked unhealthy must pass
// SuccessThreshold consecutive probes to be marked healthy again).
func (p *Probe) WithSuccessThreshold(n int) *Probe {
	p.SuccessThreshold = n
	return p
}

// WithJitter sets the jitter added to Interval.
func (p *Probe) WithJitter(d time.Duration) *Probe {
	p.Jitter = d
	return p
}

// === defaults & validation ===

func (p *Probe) withDefaults() *Probe {
	out := *p
	if out.Interval <= 0 {
		out.Interval = time.Second
	}
	if out.Timeout <= 0 {
		out.Timeout = time.Second
	}
	if out.FailureThreshold <= 0 {
		out.FailureThreshold = 3
	}
	if out.SuccessThreshold <= 0 {
		out.SuccessThreshold = 1
	}
	if out.Jitter <= 0 {
		out.Jitter = out.Interval / 4
	}
	if out.HTTPMethod == "" && out.Kind == ProbeHTTP {
		out.HTTPMethod = "GET"
	}
	return &out
}

func (p *Probe) validate() error {
	switch p.Kind {
	case ProbeTCP:
		if p.Port <= 0 || p.Port > 65535 {
			return fmt.Errorf("%w: TCP probe needs valid port", ErrInvalidConfig)
		}
	case ProbeHTTP:
		if p.Port <= 0 || p.Port > 65535 {
			return fmt.Errorf("%w: HTTP probe needs valid port", ErrInvalidConfig)
		}
		if p.HTTPPath == "" {
			return fmt.Errorf("%w: HTTP probe needs path", ErrInvalidConfig)
		}
	case ProbeUDP:
		if p.Port <= 0 || p.Port > 65535 {
			return fmt.Errorf("%w: UDP probe needs valid port", ErrInvalidConfig)
		}
		if len(p.UDPSend) == 0 {
			return fmt.Errorf("%w: UDP probe needs send bytes", ErrInvalidConfig)
		}
	case ProbeGRPC:
		if p.Port <= 0 || p.Port > 65535 {
			return fmt.Errorf("%w: gRPC probe needs valid port", ErrInvalidConfig)
		}
	case ProbeExec:
		if len(p.ExecCmd) == 0 {
			return fmt.Errorf("%w: exec probe needs cmd", ErrInvalidConfig)
		}
		if p.Location != ProbeLocationExec && p.Location != ProbeLocationAuto {
			return fmt.Errorf("%w: exec probe must use ProbeLocationExec", ErrInvalidConfig)
		}
	default:
		return errors.New("probe: unknown kind")
	}
	if p.Interval < 0 || p.Timeout < 0 || p.FailureThreshold < 0 || p.SuccessThreshold < 0 {
		return fmt.Errorf("%w: negative timing/threshold values not allowed", ErrInvalidConfig)
	}
	return nil
}
