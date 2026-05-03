package network

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// === probe validation ===

func TestProbe_TCPValidation(t *testing.T) {
	p := TCPProbe(0)
	if err := p.validate(); err == nil {
		t.Fatal("expected error for invalid port")
	}
	p2 := TCPProbe(80)
	if err := p2.validate(); err != nil {
		t.Fatalf("expected valid TCP probe, got %v", err)
	}
}

func TestProbe_HTTPValidation(t *testing.T) {
	if err := HTTPProbe(80, "").validate(); err == nil {
		t.Fatal("HTTP probe with empty path should fail")
	}
	if err := HTTPProbe(80, "/health").validate(); err != nil {
		t.Fatalf("HTTP probe valid case errored: %v", err)
	}
}

func TestProbe_UDPValidation(t *testing.T) {
	if err := UDPProbe(53, nil).validate(); err == nil {
		t.Fatal("UDP probe with no send bytes should fail")
	}
	if err := UDPProbe(53, []byte("ping")).validate(); err != nil {
		t.Fatalf("UDP probe valid case errored: %v", err)
	}
}

func TestProbe_ExecValidation(t *testing.T) {
	if err := ExecProbe().validate(); err == nil {
		t.Fatal("Exec probe with no cmd should fail")
	}
	if err := ExecProbe("true").validate(); err != nil {
		t.Fatalf("Exec probe valid case errored: %v", err)
	}
}

// === egress compilation + matching ===

func TestEgress_DefaultAllow(t *testing.T) {
	c, err := compileEgress(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.AllowsName("anywhere.com") || !c.AllowsIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("default-allow should pass everything")
	}
}

func TestEgress_AllowSpecific(t *testing.T) {
	c, err := compileEgress(&EgressPolicy{
		AllowDomains: []string{"valkey.example.com", "*.db.internal"},
		AllowCIDRs:   []string{"10.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c.AllowsName("valkey.example.com") {
		t.Error("exact domain should match")
	}
	if !c.AllowsName("primary.db.internal") {
		t.Error("single-label wildcard should match")
	}
	if c.AllowsName("evil.example.com") {
		t.Error("non-listed domain should not match")
	}
	if !c.AllowsIP(net.ParseIP("10.1.2.3")) {
		t.Error("CIDR should match")
	}
	if c.AllowsIP(net.ParseIP("8.8.8.8")) {
		t.Error("non-CIDR IP should be denied")
	}
}

func TestEgress_DenyOverridesAllow(t *testing.T) {
	c, err := compileEgress(&EgressPolicy{
		AllowCIDRs: []string{"10.0.0.0/8"},
		DenyCIDRs:  []string{"10.0.0.0/16"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.AllowsIP(net.ParseIP("10.0.5.5")) {
		t.Error("deny should win over allow")
	}
	if !c.AllowsIP(net.ParseIP("10.1.0.1")) {
		t.Error("non-denied region of allow should still be allowed")
	}
}

// === rule set ===

func TestRuleSet_DefaultAllow(t *testing.T) {
	rs := &ruleSet{}
	ok, _ := rs.allowed(net.ParseIP("8.8.8.8"))
	if !ok {
		t.Fatal("empty ruleset should default-allow")
	}
}

func TestRuleSet_AllowDeny(t *testing.T) {
	a, _ := parseCIDR("10.0.0.0/8")
	d, _ := parseCIDR("10.0.0.0/16")
	rs := &ruleSet{
		allows: []*net.IPNet{a},
		denies: []*net.IPNet{d},
	}
	if ok, _ := rs.allowed(net.ParseIP("10.0.5.5")); ok {
		t.Error("denied subnet should be denied even with allow rule")
	}
	if ok, _ := rs.allowed(net.ParseIP("10.1.0.0")); !ok {
		t.Error("allowed subnet outside deny should pass")
	}
	if ok, _ := rs.allowed(net.ParseIP("8.8.8.8")); ok {
		t.Error("non-listed source should fail when allow rules exist")
	}
}

// === stack validation ===

func TestStack_ValidateRequiresImage(t *testing.T) {
	s := newStack(nil, "id", "name")
	s.MustAddService("web") // no Image
	err := s.validate()
	if err == nil || !strings.Contains(err.Error(), "image is required") {
		t.Fatalf("expected image-required error, got %v", err)
	}
}

func TestStack_ValidateUnknownDep(t *testing.T) {
	s := newStack(nil, "id", "name")
	s.MustAddService("web", Image("alpine"), DependsOn("ghost"))
	err := s.validate()
	if err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("expected unknown-service error, got %v", err)
	}
}

func TestStack_ValidateCycle(t *testing.T) {
	s := newStack(nil, "id", "name")
	s.MustAddService("a", Image("alpine"), DependsOn("b"))
	s.MustAddService("b", Image("alpine"), DependsOn("a"))
	err := s.validate()
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestStack_ValidateUnknownSecret(t *testing.T) {
	s := newStack(nil, "id", "name")
	s.MustAddService("web", Image("alpine"), UseSecret("missing", "/run/x"))
	err := s.validate()
	if err == nil || !strings.Contains(err.Error(), "unknown secret") {
		t.Fatalf("expected unknown-secret error, got %v", err)
	}
}

func TestStack_TopologicalOrder(t *testing.T) {
	s := newStack(nil, "id", "name")
	s.MustAddService("c", Image("alpine"), DependsOn("a", "b"))
	s.MustAddService("a", Image("alpine"))
	s.MustAddService("b", Image("alpine"), DependsOn("a"))
	if err := s.validate(); err != nil {
		t.Fatal(err)
	}
	order, err := s.topologicalOrder()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 {
		t.Fatalf("got %d", len(order))
	}
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	if pos["a"] >= pos["b"] || pos["a"] >= pos["c"] || pos["b"] >= pos["c"] {
		t.Fatalf("bad order %v", order)
	}
}

// === monitor ===

func TestMonitor_Drops(t *testing.T) {
	m := newMonitor("id", "name", 2)
	defer m.stop()
	for i := 0; i < 10; i++ {
		m.emit(Event{Kind: EventConnectionAccepted})
	}
	// Allow serializer to settle.
	time.Sleep(20 * time.Millisecond)
	if m.Dropped() == 0 {
		t.Error("expected some drops with tiny buffer + many emits, got 0")
	}
}

func TestMonitor_Sequence(t *testing.T) {
	m := newMonitor("id", "name", 16)
	defer m.stop()
	for i := 0; i < 5; i++ {
		m.emit(Event{Kind: EventConnectionAccepted})
	}
	time.Sleep(20 * time.Millisecond)
	prev := uint64(0)
	for i := 0; i < 5; i++ {
		select {
		case e := <-m.Events():
			if e.Seq != prev+1 {
				t.Fatalf("expected seq %d, got %d", prev+1, e.Seq)
			}
			prev = e.Seq
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for event")
		}
	}
}

// === domain pattern matching ===

func TestDomainPattern_Exact(t *testing.T) {
	p, _ := compileDomainPattern("example.com")
	if !matchDomain("example.com", p) {
		t.Error("exact match should pass")
	}
	if matchDomain("foo.example.com", p) {
		t.Error("subdomain should not pass exact pattern")
	}
}

func TestDomainPattern_DeepWildcard(t *testing.T) {
	p, _ := compileDomainPattern("**.example.com")
	if !matchDomain("a.b.c.example.com", p) {
		t.Error("deep wildcard should match nested subdomain")
	}
	if !matchDomain("example.com", p) {
		t.Error("deep wildcard should match base")
	}
	if matchDomain("evil.com", p) {
		t.Error("deep wildcard should not match unrelated domain")
	}
}

// === retry helper ===

func TestRetry_RetriesUntilSuccess(t *testing.T) {
	attempts := 0
	err := retry(context.Background(), RetryOpts{Base: time.Microsecond, MaxAttempts: 5}, func(ctx context.Context) error {
		attempts++
		if attempts < 3 {
			return &transientErr{}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts: got %d want 3", attempts)
	}
}

// === probe state ===

func TestProbeState_TransitionToHealthy(t *testing.T) {
	r := newReplicaProbeState("svc", 0, "ctr")
	if r.snapshot().Healthy {
		t.Fatal("should start unhealthy")
	}
	r.recordResult(true, "", 1, 3)
	if !r.snapshot().Healthy {
		t.Fatal("should be healthy after one success with successThreshold=1")
	}
}

func TestProbeState_FailureTransitionsToUnhealthy(t *testing.T) {
	r := newReplicaProbeState("svc", 0, "ctr")
	r.markHealthy()
	r.recordResult(false, "boom", 1, 2)
	if !r.snapshot().Healthy {
		t.Fatal("one failure should not flip healthy when threshold=2")
	}
	r.recordResult(false, "boom", 1, 2)
	if r.snapshot().Healthy {
		t.Fatal("two consecutive failures should flip unhealthy")
	}
}

// === local helpers ===

type transientErr struct{}

func (e *transientErr) Error() string { return "transient" }
