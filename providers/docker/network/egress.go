package network

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"
)

// EgressPolicy controls what external endpoints a service may
// reach. Enforcement happens at two layers:
//
//   - DNS layer: the per-stack DNS resolver answers queries only
//     when the queried name is allowed by the policy. Disallowed
//     names are returned as NXDOMAIN.
//   - Connection layer: the gateway-aware NAT path (where present)
//     drops connections to disallowed CIDRs. On platforms where
//     mkfst doesn't intermediate egress traffic (rootful Docker
//     with default bridge), the DNS layer is the primary control.
//
// Default-deny when at least one Allow rule is set; default-allow
// otherwise. This matches the principle of "if you specify any
// allow rules, you mean to be restrictive."
type EgressPolicy struct {
	// AllowCIDRs is a list of CIDRs the service may reach.
	AllowCIDRs []string
	// DenyCIDRs is a list of CIDRs explicitly denied. Evaluated
	// after Allow — a deny wins over an allow when they overlap.
	DenyCIDRs []string
	// AllowDomains is a list of domain patterns (DNS names) the
	// service may resolve. Patterns:
	//   - "example.com"      → exact match
	//   - "*.example.com"    → any subdomain (one or more labels)
	//   - "**.example.com"   → any descendant (multi-label deep)
	AllowDomains []string
	// DenyDomains uses the same pattern syntax as AllowDomains.
	DenyDomains []string
}

// AllowAll is a permissive policy used as the implicit default for
// services with no Egress option. Exposed so callers can be
// explicit.
func AllowAll() *EgressPolicy { return &EgressPolicy{} }

// AllowOnly is a convenience constructor for "only these
// CIDRs/domains, deny everything else."
func AllowOnly(cidrs []string, domains []string) *EgressPolicy {
	return &EgressPolicy{
		AllowCIDRs:   cidrs,
		AllowDomains: domains,
	}
}

// === compiled form ===

// compiledEgress is the runtime form: validated CIDRs and
// pre-parsed domain patterns. Held under atomic.Pointer so the DNS
// resolver and connection enforcer can read it without a lock.
type compiledEgress struct {
	allowNets    []*net.IPNet
	denyNets     []*net.IPNet
	allowDomains []domainPattern
	denyDomains  []domainPattern
	defaultAllow bool // true when no Allow rules exist (default-allow)
}

type domainPattern struct {
	raw   string
	parts []string // labels in reverse order: ["com", "example", "*"]
	deep  bool     // ** prefix: matches any descendant
}

func compileEgress(p *EgressPolicy) (*compiledEgress, error) {
	if p == nil {
		return &compiledEgress{defaultAllow: true}, nil
	}
	c := &compiledEgress{
		defaultAllow: len(p.AllowCIDRs) == 0 && len(p.AllowDomains) == 0,
	}
	for _, cidr := range p.AllowCIDRs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("%w: bad allow CIDR %q: %v", ErrInvalidConfig, cidr, err)
		}
		c.allowNets = append(c.allowNets, n)
	}
	for _, cidr := range p.DenyCIDRs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("%w: bad deny CIDR %q: %v", ErrInvalidConfig, cidr, err)
		}
		c.denyNets = append(c.denyNets, n)
	}
	for _, d := range p.AllowDomains {
		dp, err := compileDomainPattern(d)
		if err != nil {
			return nil, fmt.Errorf("%w: bad allow domain %q: %v", ErrInvalidConfig, d, err)
		}
		c.allowDomains = append(c.allowDomains, dp)
	}
	for _, d := range p.DenyDomains {
		dp, err := compileDomainPattern(d)
		if err != nil {
			return nil, fmt.Errorf("%w: bad deny domain %q: %v", ErrInvalidConfig, d, err)
		}
		c.denyDomains = append(c.denyDomains, dp)
	}
	return c, nil
}

func compileDomainPattern(s string) (domainPattern, error) {
	if s == "" {
		return domainPattern{}, fmt.Errorf("empty domain pattern")
	}
	dp := domainPattern{raw: s}
	if strings.HasPrefix(s, "**.") {
		dp.deep = true
		s = s[3:]
	}
	// Split on "." into labels and reverse so the TLD comes first.
	labels := strings.Split(s, ".")
	for i, j := 0, len(labels)-1; i < j; i, j = i+1, j-1 {
		labels[i], labels[j] = labels[j], labels[i]
	}
	dp.parts = labels
	return dp, nil
}

// matchDomain reports whether name (full DNS name, no trailing dot)
// matches the pattern. Case-insensitive.
func matchDomain(name string, dp domainPattern) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	nameLabels := strings.Split(name, ".")
	// Reverse for back-to-front match.
	for i, j := 0, len(nameLabels)-1; i < j; i, j = i+1, j-1 {
		nameLabels[i], nameLabels[j] = nameLabels[j], nameLabels[i]
	}
	if dp.deep {
		// Pattern parts must form a prefix of nameLabels.
		if len(nameLabels) < len(dp.parts) {
			return false
		}
		for i, p := range dp.parts {
			if !labelMatch(nameLabels[i], p) {
				return false
			}
		}
		return true
	}
	if len(nameLabels) != len(dp.parts) {
		// Allow "*" as a single-label wildcard at the end (left-most
		// in original orientation).
		if len(dp.parts) == 0 || dp.parts[len(dp.parts)-1] != "*" {
			return false
		}
		// "*" can match exactly one extra label.
		if len(nameLabels) != len(dp.parts) {
			// Already handled by the != above; this branch unreachable
			// in practice but kept defensive.
		}
	}
	for i, p := range dp.parts {
		if i >= len(nameLabels) {
			return false
		}
		if !labelMatch(nameLabels[i], p) {
			return false
		}
	}
	return true
}

func labelMatch(label, pattern string) bool {
	if pattern == "*" {
		return label != ""
	}
	return strings.EqualFold(label, pattern)
}

// AllowsName reports whether the egress policy allows resolving the
// given DNS name.
func (c *compiledEgress) AllowsName(name string) bool {
	for _, dp := range c.denyDomains {
		if matchDomain(name, dp) {
			return false
		}
	}
	if c.defaultAllow {
		return true
	}
	for _, dp := range c.allowDomains {
		if matchDomain(name, dp) {
			return true
		}
	}
	return false
}

// AllowsIP reports whether the egress policy allows connecting to
// the given IP.
func (c *compiledEgress) AllowsIP(ip net.IP) bool {
	for _, n := range c.denyNets {
		if n.Contains(ip) {
			return false
		}
	}
	if c.defaultAllow {
		return true
	}
	for _, n := range c.allowNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// === atomic snapshot holder ===

// egressHolder wraps an atomic.Pointer so per-service egress
// policies can be hot-swapped without locking. Used by the DNS
// resolver and (where present) the connection enforcer.
type egressHolder struct {
	p atomic.Pointer[compiledEgress]
}

func (h *egressHolder) load() *compiledEgress { return h.p.Load() }

func (h *egressHolder) store(c *compiledEgress) { h.p.Store(c) }

// allowAllHolder returns a holder pre-populated with default-allow.
func allowAllHolder() *egressHolder {
	h := &egressHolder{}
	h.store(&compiledEgress{defaultAllow: true})
	return h
}
