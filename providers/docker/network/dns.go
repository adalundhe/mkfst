package network

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// === per-stack DNS resolver ===
//
// The resolver is a minimal in-process DNS server that does two
// things for the stack:
//
//   1. Resolves stack-internal service names to the right container
//      IPs (replicated services return all replica IPs round-robin).
//   2. Enforces per-service egress allowlists at name-resolution
//      time. A query whose service-of-origin disallows the name
//      returns NXDOMAIN.
//
// External (allowed) names are recursively resolved via the
// upstream resolver (the host's libc resolver via net.Resolver).
//
// Binding strategy:
//
//   - The resolver binds to the stack's bridge gateway IP on UDP 53
//     and TCP 53. Containers in the stack network reach the gateway
//     IP for DNS by default (Docker's embedded DNS uses the bridge
//     gateway), and we override DNS for the containers via
//     HostConfig.DNS to point them at our resolver.
//   - Binding to port 53 requires root or CAP_NET_BIND_SERVICE on
//     Linux. When the bind fails (rootless setups, Mac/Windows
//     where the gateway IP isn't bind-eligible), the resolver is
//     skipped and a warning is emitted; egress policies still
//     compile and can be queried programmatically via
//     Stack.AllowsEgress.

type dnsResolver struct {
	stack *Stack

	udp   net.PacketConn
	tcpLn net.Listener
	stopc chan struct{}
	wg    sync.WaitGroup
	bound atomic.Bool

	// upstream is the host's resolver used for non-stack names.
	upstream *net.Resolver
}

func newDNSResolver(s *Stack) *dnsResolver {
	return &dnsResolver{
		stack:    s,
		stopc:    make(chan struct{}),
		upstream: net.DefaultResolver,
	}
}

// start attempts to bind UDP+TCP on bindAddr:53. Returns nil on
// success; a wrapped error on failure (caller decides whether to
// continue without DNS).
func (r *dnsResolver) start(bindAddr string) error {
	udpAddr := net.JoinHostPort(bindAddr, "53")
	pc, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("dns udp listen %s: %w", udpAddr, err)
	}
	tcpLn, err := net.Listen("tcp", udpAddr)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("dns tcp listen %s: %w", udpAddr, err)
	}
	r.udp = pc
	r.tcpLn = tcpLn
	r.bound.Store(true)
	r.wg.Add(1)
	go r.udpLoop()
	r.wg.Add(1)
	go r.tcpLoop()
	return nil
}

func (r *dnsResolver) stop() {
	if !r.bound.CompareAndSwap(true, false) {
		return
	}
	close(r.stopc)
	if r.udp != nil {
		_ = r.udp.Close()
	}
	if r.tcpLn != nil {
		_ = r.tcpLn.Close()
	}
	r.wg.Wait()
}

func (r *dnsResolver) udpLoop() {
	defer r.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, src, err := r.udp.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		query := append([]byte(nil), buf[:n]...)
		// Track per-query goroutines in the resolver wg so stop()
		// joins them deterministically. Each query has its own
		// short timeout (3s upstream lookup), so worst-case
		// shutdown blocks ~3s on in-flight queries.
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.handleUDPQuery(query, src)
		}()
	}
}

func (r *dnsResolver) handleUDPQuery(query []byte, src net.Addr) {
	resp := r.respond(query, src.String())
	if resp == nil {
		return
	}
	_, _ = r.udp.WriteTo(resp, src)
}

func (r *dnsResolver) tcpLoop() {
	defer r.wg.Done()
	for {
		conn, err := r.tcpLn.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.handleTCPQuery(conn)
		}()
	}
}

func (r *dnsResolver) handleTCPQuery(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var lenBuf [2]byte
	if _, err := readFull(conn, lenBuf[:]); err != nil {
		return
	}
	msgLen := int(binary.BigEndian.Uint16(lenBuf[:]))
	if msgLen == 0 || msgLen > 65535 {
		return
	}
	body := make([]byte, msgLen)
	if _, err := readFull(conn, body); err != nil {
		return
	}
	resp := r.respond(body, conn.RemoteAddr().String())
	if resp == nil {
		return
	}
	out := make([]byte, 2+len(resp))
	binary.BigEndian.PutUint16(out[:2], uint16(len(resp)))
	copy(out[2:], resp)
	_, _ = conn.Write(out)
}

func readFull(c net.Conn, b []byte) (int, error) {
	read := 0
	for read < len(b) {
		n, err := c.Read(b[read:])
		if n > 0 {
			read += n
		}
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

// === DNS message encode/decode ===
//
// We implement just enough of RFC 1035 to handle A and AAAA queries.
// Anything else gets a minimal NXDOMAIN response with the question
// echoed back.

const (
	dnsTypeA    = 1
	dnsTypeAAAA = 28
	dnsClassIN  = 1
)

type dnsHeader struct {
	ID      uint16
	Flags   uint16
	QDCount uint16
	ANCount uint16
	NSCount uint16
	ARCount uint16
}

func (r *dnsResolver) respond(query []byte, srcAddr string) []byte {
	if len(query) < 12 {
		return nil
	}
	hdr := dnsHeader{
		ID:      binary.BigEndian.Uint16(query[0:2]),
		Flags:   binary.BigEndian.Uint16(query[2:4]),
		QDCount: binary.BigEndian.Uint16(query[4:6]),
	}
	if hdr.QDCount == 0 {
		return r.respondError(hdr, query, dnsRCodeFormErr)
	}
	name, qtype, qclass, end, err := decodeQuestion(query, 12)
	if err != nil {
		return r.respondError(hdr, query, dnsRCodeFormErr)
	}
	_ = end
	if qclass != dnsClassIN {
		return r.respondError(hdr, query, dnsRCodeNotImpl)
	}

	// 1. Stack-internal name?
	if ips, ok := r.resolveStackName(name); ok {
		return r.respondAnswers(hdr, query, name, qtype, ips)
	}

	// 2. Egress check.
	srcHost, _, _ := net.SplitHostPort(srcAddr)
	srcIP := net.ParseIP(srcHost)
	allowed := r.allowedFor(srcIP, name)
	if !allowed {
		return r.respondError(hdr, query, dnsRCodeNXDomain)
	}

	// 3. Recurse via upstream.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	netSel := "ip4"
	if qtype == dnsTypeAAAA {
		netSel = "ip6"
	}
	ips, lookupErr := r.upstream.LookupIP(ctx, netSel, strings.TrimSuffix(name, "."))
	if lookupErr != nil || len(ips) == 0 {
		return r.respondError(hdr, query, dnsRCodeNXDomain)
	}
	return r.respondAnswers(hdr, query, name, qtype, ips)
}

// resolveStackName checks if name matches a service in the stack
// and returns the container IPs of that service's healthy replicas.
func (r *dnsResolver) resolveStackName(name string) ([]net.IP, bool) {
	clean := strings.TrimSuffix(name, ".")
	r.stack.mu.RLock()
	insts, ok := r.stack.containers[clean]
	r.stack.mu.RUnlock()
	if !ok || len(insts) == 0 {
		return nil, false
	}
	// Use docker inspect to get IPs. Cheap-but-not-free; cache
	// could be added later.
	out := []net.IP{}
	for _, inst := range insts {
		insp, err := r.stack.engine.cli.ContainerInspect(context.Background(), inst.id)
		if err != nil {
			continue
		}
		for _, ep := range insp.NetworkSettings.Networks {
			if ep != nil && ep.IPAddress != "" {
				if ip := net.ParseIP(ep.IPAddress); ip != nil {
					out = append(out, ip)
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// allowedFor reports whether the source container is allowed to
// resolve name per its egress policy.
func (r *dnsResolver) allowedFor(srcIP net.IP, name string) bool {
	if srcIP == nil {
		return true // no source → can't enforce
	}
	r.stack.mu.RLock()
	defer r.stack.mu.RUnlock()
	for svcName, insts := range r.stack.containers {
		for _, inst := range insts {
			insp, err := r.stack.engine.cli.ContainerInspect(context.Background(), inst.id)
			if err != nil {
				continue
			}
			for _, ep := range insp.NetworkSettings.Networks {
				if ep != nil && net.ParseIP(ep.IPAddress).Equal(srcIP) {
					holder, ok := r.stack.egress[svcName]
					if !ok || holder.load() == nil {
						return true
					}
					return holder.load().AllowsName(name)
				}
			}
		}
	}
	return true
}

const (
	dnsRCodeNoErr   = 0
	dnsRCodeFormErr = 1
	dnsRCodeServErr = 2
	dnsRCodeNXDomain = 3
	dnsRCodeNotImpl = 4
)

func decodeQuestion(buf []byte, off int) (name string, qtype, qclass uint16, end int, err error) {
	var labels []string
	for {
		if off >= len(buf) {
			return "", 0, 0, 0, errors.New("decode: short")
		}
		l := int(buf[off])
		off++
		if l == 0 {
			break
		}
		if l > 63 || off+l > len(buf) {
			return "", 0, 0, 0, errors.New("decode: bad label")
		}
		labels = append(labels, string(buf[off:off+l]))
		off += l
	}
	if off+4 > len(buf) {
		return "", 0, 0, 0, errors.New("decode: short type/class")
	}
	qtype = binary.BigEndian.Uint16(buf[off : off+2])
	qclass = binary.BigEndian.Uint16(buf[off+2 : off+4])
	off += 4
	return strings.Join(labels, ".") + ".", qtype, qclass, off, nil
}

func (r *dnsResolver) respondError(hdr dnsHeader, query []byte, rcode uint16) []byte {
	out := make([]byte, len(query))
	copy(out, query)
	binary.BigEndian.PutUint16(out[0:2], hdr.ID)
	// QR=1, Opcode=0 (query), AA=1, TC=0, RD=copy, RA=1, Z=0, RCODE=rcode
	flags := uint16(0x8000) | (hdr.Flags & 0x0100) | 0x0480 | (rcode & 0xF)
	binary.BigEndian.PutUint16(out[2:4], flags)
	binary.BigEndian.PutUint16(out[6:8], 0) // AN=0
	binary.BigEndian.PutUint16(out[8:10], 0)
	binary.BigEndian.PutUint16(out[10:12], 0)
	return out
}

func (r *dnsResolver) respondAnswers(hdr dnsHeader, query []byte, name string, qtype uint16, ips []net.IP) []byte {
	out := make([]byte, 0, 512)
	// Header.
	headerLen := 12
	out = append(out, query[:headerLen]...)
	// Find end of question (=> length of question section).
	qEnd := headerLen
	for qEnd < len(query) {
		l := int(query[qEnd])
		qEnd++
		if l == 0 {
			qEnd += 4 // type+class
			break
		}
		qEnd += l
	}
	out = append(out, query[headerLen:qEnd]...)

	// Answers.
	answers := 0
	for _, ip := range ips {
		var rdata []byte
		var rtype uint16
		switch qtype {
		case dnsTypeA:
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			rdata = ip4
			rtype = dnsTypeA
		case dnsTypeAAAA:
			ip6 := ip.To16()
			if ip6 == nil || ip.To4() != nil {
				continue
			}
			rdata = ip6
			rtype = dnsTypeAAAA
		default:
			continue
		}
		// Encode the answer name as raw labels (compression pointer
		// to question would shorten, but the simple form is more
		// robust to client implementations).
		nameBytes := encodeName(name)
		ans := make([]byte, 0, len(nameBytes)+10+len(rdata))
		ans = append(ans, nameBytes...)
		t := make([]byte, 10)
		binary.BigEndian.PutUint16(t[0:2], rtype)
		binary.BigEndian.PutUint16(t[2:4], dnsClassIN)
		binary.BigEndian.PutUint32(t[4:8], 30) // TTL=30s
		binary.BigEndian.PutUint16(t[8:10], uint16(len(rdata)))
		ans = append(ans, t...)
		ans = append(ans, rdata...)
		out = append(out, ans...)
		answers++
	}
	binary.BigEndian.PutUint16(out[0:2], hdr.ID)
	flags := uint16(0x8000) | (hdr.Flags & 0x0100) | 0x0480
	binary.BigEndian.PutUint16(out[2:4], flags)
	binary.BigEndian.PutUint16(out[6:8], uint16(answers))
	binary.BigEndian.PutUint16(out[8:10], 0)
	binary.BigEndian.PutUint16(out[10:12], 0)
	return out
}

func encodeName(name string) []byte {
	out := []byte{}
	clean := strings.TrimSuffix(name, ".")
	if clean == "" {
		return []byte{0}
	}
	for _, label := range strings.Split(clean, ".") {
		if len(label) > 63 {
			label = label[:63]
		}
		out = append(out, byte(len(label)))
		out = append(out, []byte(label)...)
	}
	out = append(out, 0)
	return out
}
