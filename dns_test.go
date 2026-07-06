package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"codeberg.org/miekg/dns"
	"codeberg.org/miekg/dns/dnsutil"
	"github.com/go-logr/logr"
)

// newTestHandler returns a dnsHandler wired to the given store.
//
// A single in-zone NS "ns.<zone>" with no glue is used as default.
func newTestHandler(zone string, store *Store, nameservers ...NSConfig) *dnsHandler {
	if len(nameservers) == 0 {
		nameservers = []NSConfig{{FQDN: "ns." + zone}}
	}

	return newDNSHandler(DNSConfig{
		Zone:        zone,
		BindAddr:    ":53",
		TTL:         30,
		Nameservers: nameservers,
		SOA: SOAConfig{
			Email:       "admin." + zone,
			Refresh:     3600,
			Retry:       900,
			Expire:      86400,
			NegativeTTL: 30,
		},
	}, store, logr.Discard(), nil)
}

// queryRoundtrip sends a single-question DNS query to the handler and waits for a response.
func queryRoundtrip(t *testing.T, h *dnsHandler, qtype uint16, name string) *dns.Msg {
	t.Helper()

	req := dnsutil.SetQuestion(new(dns.Msg), name, qtype)
	resp := new(dns.Msg)
	dnsutil.SetReply(resp, req)

	if err := h.processDNSMessage(req, resp); err != nil {
		t.Fatalf("processing DNS message: %v", err)
	}

	return resp
}

func TestDNSEdns0(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore())
	req := new(dns.Msg)
	req.UDPSize = 4096
	resp := new(dns.Msg)
	dnsutil.SetReply(resp, req)

	if err := h.processDNSMessage(req, resp); err != nil {
		t.Fatalf("processing DNS message: %v", err)
	}

	if resp.UDPSize == 0 {
		t.Error("expected EDNS0 record")
	}
}

func TestDNSInvalidQuestions(t *testing.T) {
	t.Parallel()

	questions := [][]dns.RR{nil, {
		&dns.A{Hdr: dns.Header{Name: "example.com.", Class: dns.ClassINET}},
		&dns.AAAA{Hdr: dns.Header{Name: "example.com.", Class: dns.ClassINET}},
	}}

	for _, q := range questions {
		t.Run(fmt.Sprintf("%v questions", len(q)), func(t *testing.T) {
			t.Parallel()

			h := newTestHandler("example.com.", NewStore())
			req := new(dns.Msg)
			req.Question = q
			resp := new(dns.Msg)
			dnsutil.SetReply(resp, req)

			if err := h.processDNSMessage(req, resp); err != nil {
				t.Fatalf("processing DNS message: %v", err)
			}

			if resp.Rcode != dns.RcodeFormatError {
				t.Errorf("unexpected DNS code: got %v, expected FORMERR", dns.RcodeToString[resp.Rcode])
			}
		})
	}
}

func TestDNSInvalidClass(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore())
	req := new(dns.Msg)
	req.Question = []dns.RR{&dns.A{Hdr: dns.Header{Name: "example.com.", Class: dns.ClassCHAOS}}}
	resp := new(dns.Msg)
	dnsutil.SetReply(resp, req)

	if err := h.processDNSMessage(req, resp); err != nil {
		t.Fatalf("processing DNS message: %v", err)
	}

	if resp.Rcode != dns.RcodeRefused {
		t.Errorf("unexpected DNS code: got %v, expected REFUSED", dns.RcodeToString[resp.Rcode])
	}
}

func TestDNSOutsideZone(t *testing.T) {
	t.Parallel()

	h := newTestHandler("test.example.com.", NewStore())
	resp := queryRoundtrip(t, h, dns.TypeA, "nottest.example.com.")

	if resp.Rcode != dns.RcodeRefused {
		t.Errorf("unexpected DNS code: got %v, expected REFUSED", dns.RcodeToString[resp.Rcode])
	}
}

func TestDNSTooDeepSubdomain(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore())
	resp := queryRoundtrip(t, h, dns.TypeA, "deep.node1.example.com.")

	if resp.Rcode != dns.RcodeNameError {
		t.Errorf("unexpected DNS code: got %v, expected NXDOMAIN", dns.RcodeToString[resp.Rcode])
	}
}

func TestDNSIsAuthoritative(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore())
	resp := queryRoundtrip(t, h, dns.TypeSOA, "example.com.")

	if !resp.Authoritative {
		t.Error("expected Authoritative flag to be set for in-zone query")
	}
}

func TestDNSHasSOAInAuthority(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore())
	resp := queryRoundtrip(t, h, dns.TypeA, "node2.example.com.")

	for _, rr := range resp.Ns {
		if _, ok := rr.(*dns.SOA); ok {
			return
		}
	}

	t.Error("expected SOA record in the Authority section")
}

func TestDNSApexSOA(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore())
	resp := queryRoundtrip(t, h, dns.TypeSOA, "example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	for _, rr := range resp.Answer {
		if _, ok := rr.(*dns.SOA); ok {
			return
		}
	}

	t.Error("expected SOA record in the Answer section for SOA query at apex")
}

func TestDNSApexNS(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore())
	resp := queryRoundtrip(t, h, dns.TypeNS, "example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	for _, rr := range resp.Answer {
		if _, ok := rr.(*dns.NS); ok {
			return
		}
	}

	t.Error("expected NS record in the Answer section for NS query at apex")
}

func TestDNSApexANY(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore())
	resp := queryRoundtrip(t, h, dns.TypeANY, "example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	// ANY at apex should return both SOA and NS.
	var soaFound, nsFound bool
	for _, rr := range resp.Answer {
		switch rr.(type) {
		case *dns.SOA:
			soaFound = true
		case *dns.NS:
			nsFound = true
		}
	}

	if !soaFound {
		t.Error("expected SOA record in the Answer section for ANY query at apex")
	}

	if !nsFound {
		t.Error("expected NS record in the Answer section for ANY query at apex")
	}
}

func TestDNSGlueInZone(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore(), NSConfig{
		FQDN: "ns.example.com.",
		Glue: []netip.Addr{netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("2001:db8::1")},
	})
	resp := queryRoundtrip(t, h, dns.TypeNS, "example.com.")

	var ipv4Found, ipv6Found bool
	for _, rr := range resp.Extra {
		if a, ok := rr.(*dns.A); ok {
			if a.Addr.String() == "1.2.3.4" {
				ipv4Found = true
			}
		}

		if a, ok := rr.(*dns.AAAA); ok {
			if a.Addr.String() == "2001:db8::1" {
				ipv6Found = true
			}
		}
	}

	if !ipv4Found || !ipv6Found {
		t.Error("expected glue A and AAAA records in the Extra section for in-zone NS")
	}
}

func TestDNSGlueOutOfZone(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore(), NSConfig{
		FQDN: "ns.example.org.",
		Glue: []netip.Addr{netip.MustParseAddr("1.2.3.4")},
	})
	resp := queryRoundtrip(t, h, dns.TypeNS, "example.com.")

	if len(resp.Extra) != 0 {
		t.Errorf("expected no glue for out-of-zone NS, got %v glue records", resp.Extra)
	}
}

func TestDNSAreaUnknownNode(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore())
	resp := queryRoundtrip(t, h, dns.TypeA, "node1.example.com.")

	if resp.Rcode != dns.RcodeNameError {
		t.Errorf("unexpected DNS code: got %v, expected NXDOMAIN", dns.RcodeToString[resp.Rcode])
	}
}

func TestDNSAreaNotReadyNode(t *testing.T) {
	t.Parallel()

	store := NewStore()
	// Node is present but marked as not ready.
	store.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.2.3.4")}, false)

	h := newTestHandler("example.com.", store)
	resp := queryRoundtrip(t, h, dns.TypeA, "home.example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	if len(resp.Answer) != 0 {
		t.Error("expected empty answer section for known area with a non-ready node")
	}
}

func TestDNSAreaReturnsIPs(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Update("node1", []string{"home"}, []netip.Addr{
		netip.MustParseAddr("1.2.3.4"),
		netip.MustParseAddr("2001:db8::1"),
	}, true)

	for _, test := range []struct {
		title string
		tp    uint16
	}{
		{"IPv4", dns.TypeA},
		{"IPv6", dns.TypeAAAA},
	} {
		t.Run(test.title, func(t *testing.T) {
			t.Parallel()

			h := newTestHandler("example.com.", store)
			resp := queryRoundtrip(t, h, test.tp, "home.example.com.")

			if resp.Rcode != dns.RcodeSuccess {
				t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
			}

			if len(resp.Answer) != 1 {
				t.Errorf("expected 1 answer, got %v", len(resp.Answer))
			}

			for _, rr := range resp.Answer {
				switch test.tp {
				case dns.TypeA:
					a, ok := rr.(*dns.A)
					if !ok {
						t.Errorf("unexpected non-A record in answer section: %T", rr)
					}

					if a.Addr.String() != "1.2.3.4" {
						t.Errorf("unexpected IP address: %v", a.Addr.String())
					}

				case dns.TypeAAAA:
					aaaa, ok := rr.(*dns.AAAA)
					if !ok {
						t.Errorf("unexpected non-AAAA record in answer section: %T", rr)
					}

					if aaaa.Addr.String() != "2001:db8::1" {
						t.Errorf("unexpected IP address: %v", aaaa.Addr.String())
					}
				}
			}
		})
	}
}

func TestDNSServiceReturnsIPs(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Update("node1", nil, []netip.Addr{netip.MustParseAddr("10.30.0.2")}, true)
	store.UpdateService(svcKey("default", "git"), []string{"node1"})

	h := newTestHandler("example.com.", store)
	resp := queryRoundtrip(t, h, dns.TypeA, "git.default.example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	if len(resp.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %v", len(resp.Answer))
	}

	a, ok := resp.Answer[0].(*dns.A)
	if !ok || a.Addr.String() != "10.30.0.2" {
		t.Errorf("unexpected answer: %v", resp.Answer[0])
	}
}

func TestDNSServiceUnknown(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore())
	resp := queryRoundtrip(t, h, dns.TypeA, "git.default.example.com.")

	if resp.Rcode != dns.RcodeNameError {
		t.Errorf("unexpected DNS code: got %v, expected NXDOMAIN", dns.RcodeToString[resp.Rcode])
	}
}

func TestDNSServiceKnownButEmpty(t *testing.T) {
	t.Parallel()

	store := NewStore()
	// Service is known but its hosting node is not ready, so we expect NODATA.
	store.Update("node1", nil, []netip.Addr{netip.MustParseAddr("10.30.0.1")}, false)
	store.UpdateService(svcKey("default", "git"), []string{"node1"})

	h := newTestHandler("example.com.", store)
	resp := queryRoundtrip(t, h, dns.TypeA, "git.default.example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	if len(resp.Answer) != 0 {
		t.Errorf("expected empty answer for a known service with no ready node, got %v", resp.Answer)
	}

	for _, rr := range resp.Ns {
		if _, ok := rr.(*dns.SOA); ok {
			return
		}
	}

	t.Error("expected SOA in authority section for NODATA response")
}

func TestDNSNotReadyServfail(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("10.30.0.1")}, true)

	// Create a handler that is perpetually non-ready.
	h := newDNSHandler(DNSConfig{
		Zone:        "example.com.",
		TTL:         30,
		Nameservers: []NSConfig{{FQDN: "ns.example.com."}},
		SOA:         SOAConfig{Email: "admin.example.com.", NegativeTTL: 30},
	}, store, logr.Discard(), func() bool { return false })

	resp := queryRoundtrip(t, h, dns.TypeA, "home.example.com.")
	if resp.Rcode != dns.RcodeServerFailure {
		t.Errorf("unexpected DNS code: got %v, expected SERVFAIL", dns.RcodeToString[resp.Rcode])
	}
}

func TestDNSRFC8482(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.2.3.4")}, true)

	h := newTestHandler("example.com.", store)
	resp := queryRoundtrip(t, h, dns.TypeANY, "home.example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	for _, rr := range resp.Answer {
		if hinfo, ok := rr.(*dns.HINFO); ok {
			if hinfo.Cpu != "RFC8482" {
				t.Errorf("invalid HINFO CPU field: got %v, want %v", hinfo.Cpu, "RFC8482")
			}

			return
		}
	}

	t.Error("expected HINFO record (RFC 8482)")
}

func TestDNSAreaMultipleNodes(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, true)
	store.Update("node2", []string{"home"}, []netip.Addr{netip.MustParseAddr("2.2.2.2")}, true)
	store.Update("node3", []string{"home"}, []netip.Addr{netip.MustParseAddr("3.3.3.3")}, false) // not ready

	h := newTestHandler("example.com.", store)
	resp := queryRoundtrip(t, h, dns.TypeA, "home.example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	if len(resp.Answer) != 2 {
		t.Errorf("expected 2 A records with IPs of ready nodes, got %v", len(resp.Answer))
	}
}

func TestDNSQueryCaseInsensitive(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, true)

	h := newTestHandler("example.com.", store)
	resp := queryRoundtrip(t, h, dns.TypeA, "HOME.exAmple.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}
}

// populateLargeArea adds multiple nodes, each with an unique IPv4 address,
// to the area "home" in the store.
//
// This is mostly needed for DNS truncation tests.
func populateLargeArea(store *Store, n int) {
	for i := range n {
		ip := netip.AddrFrom4([4]byte{1, 2, byte(i >> 8), byte(i)})
		store.Update(fmt.Sprintf("node%v", i+1), []string{"home"}, []netip.Addr{ip}, true)
	}
}

func TestDNSUDPTruncation(t *testing.T) {
	t.Parallel()

	store := NewStore()
	populateLargeArea(store, 50)

	h := newTestHandler("example.com.", store)
	req := dnsutil.SetQuestion(new(dns.Msg), "home.example.com.", dns.TypeA)

	resp := h.buildReply(req, true)

	if !resp.Truncated {
		t.Error("expected TC bit to be set for oversized UDP response")
	}

	if err := resp.Pack(); err != nil {
		t.Fatalf("packing truncated response: %v", err)
	}

	if len(resp.Data) > dns.MinMsgSize {
		t.Errorf("response is too large after truncation (%v bytes): want at most %v bytes", len(resp.Data), dns.MinMsgSize)
	}
}

func TestDNSTCPNoTruncation(t *testing.T) {
	t.Parallel()

	store := NewStore()
	populateLargeArea(store, 50)

	h := newTestHandler("example.com.", store)
	req := dnsutil.SetQuestion(new(dns.Msg), "home.example.com.", dns.TypeA)

	resp := h.buildReply(req, false)

	if resp.Truncated {
		t.Error("TC bit must not be set for TCP responses")
	}

	if len(resp.Answer) != 50 {
		t.Errorf("expected all 50 answers in TCP response, got %v", len(resp.Answer))
	}
}

// newStartDNSConfig returns a minimal valid DNSConfig for tests.
func newStartDNSConfig(addr string) DNSConfig {
	return DNSConfig{
		Zone:        "example.com.",
		BindAddr:    addr,
		TTL:         5,
		Nameservers: []NSConfig{{FQDN: "ns.example.com."}},
		SOA: SOAConfig{
			Email:       "admin.example.com.",
			TTL:         5,
			Refresh:     60,
			Retry:       30,
			Expire:      600,
			NegativeTTL: 5,
		},
	}
}

// TestStartDNS verifies that StartDNS starts both a UDP and TCP server,
// serves queries, and shuts down cleanly when its context is cancelled.
func TestStartDNS(t *testing.T) {
	t.Parallel()

	// Getting a free port in this way is pretty funky but I think it's OK for tests.
	lc := net.ListenConfig{}
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}

	tcpAddr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("getting TCP address: %v", err)
	}

	err = l.Close()
	if err != nil {
		t.Fatalf("closing temporary TCP listener: %v", err)
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(tcpAddr.Port))

	store := NewStore()
	store.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.2.3.4")}, true)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- StartDNS(ctx, newStartDNSConfig(addr), store, nil)
	}()

	// Poll UDP until the server is ready.
	newReq := func() *dns.Msg {
		return dnsutil.SetQuestion(new(dns.Msg), "home.example.com.", dns.TypeA)
	}

	udpClient := dns.NewClient()
	udpClient.ReadTimeout = 500 * time.Millisecond

	var (
		udpResp *dns.Msg
		lastErr error
	)

	for range 40 {
		ectx, ecancel := context.WithTimeout(ctx, 500*time.Millisecond)
		udpResp, _, lastErr = udpClient.Exchange(ectx, newReq(), "udp", addr)
		ecancel()
		if lastErr == nil {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	if lastErr != nil {
		t.Fatalf("UDP server did not become ready: %v", lastErr)
	}

	// Verify the UDP response.
	if udpResp.Rcode != dns.RcodeSuccess {
		t.Errorf("UDP: unexpected rcode: %v", dns.RcodeToString[udpResp.Rcode])
	}

	if len(udpResp.Answer) != 1 {
		t.Errorf("UDP: expected 1 A record, got %v", len(udpResp.Answer))
	}

	// Verify that the TCP server is also up.
	tcpClient := dns.NewClient()
	tcpClient.ReadTimeout = 2 * time.Second

	tctx, tcancel := context.WithTimeout(ctx, 2*time.Second)
	defer tcancel()

	tcpResp, _, err := tcpClient.Exchange(tctx, newReq(), "tcp", addr)
	if err != nil {
		t.Fatalf("TCP query failed: %v", err)
	}

	if tcpResp.Rcode != dns.RcodeSuccess {
		t.Errorf("TCP: unexpected rcode: %v", dns.RcodeToString[tcpResp.Rcode])
	}

	// Cancel the context and verify StartDNS shuts down correctly.
	cancel()

	if err := <-errCh; err != nil {
		t.Errorf("StartDNS returned unexpected error on clean shutdown: %v", err)
	}
}

// TestStartDNSBindError verifies that StartDNS returns a non-nil error when
// the requested address is already in use.
func TestStartDNSBindError(t *testing.T) {
	t.Parallel()

	// Hold a TCP listener open so that StartDNS cannot bind the same port.
	lc := net.ListenConfig{}
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding test listener: %v", err)
	}

	t.Cleanup(func() {
		closeErr := l.Close()
		if closeErr != nil {
			t.Logf("closing TCP listener: %v", closeErr)
		}
	})

	addr := l.Addr().String()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if err := StartDNS(ctx, newStartDNSConfig(addr), NewStore(), nil); err == nil {
		t.Error("expected StartDNS to return an error when the address is already in use")
	}
}

func TestDNSMultipleNameservers(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore(),
		NSConfig{FQDN: "ns1.example.com.", Glue: []netip.Addr{netip.MustParseAddr("1.1.1.1")}},
		NSConfig{FQDN: "ns2.pootis.network."},
	)
	resp := queryRoundtrip(t, h, dns.TypeNS, "example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	ns1Found, ns2Found := false, false
	for _, rr := range resp.Answer {
		if ns, ok := rr.(*dns.NS); ok {
			switch ns.Ns {
			case "ns1.example.com.":
				ns1Found = true
			case "ns2.pootis.network.":
				ns2Found = true
			}
		}
	}

	if !ns1Found {
		t.Errorf("expected NS1 (ns1.example.com) in DNS response")
	}

	if !ns2Found {
		t.Errorf("expected NS2 (ns2.pootis.network) in DNS response")
	}

	// Check if glue is present for ns1, since it is in-zone.
	for _, rr := range resp.Extra {
		if a, ok := rr.(*dns.A); ok && a.Addr.String() == "1.1.1.1" {
			return
		}
	}

	t.Error("expected glue A record for in-zone ns1")
}

func TestDNSInZoneNS(t *testing.T) {
	t.Parallel()

	h := newTestHandler("example.com.", NewStore(), NSConfig{
		FQDN: "ns.example.com.",
		Glue: []netip.Addr{netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("2001:db8::1")},
	})

	for _, test := range []struct {
		title      string
		qtype      uint16
		expectedIP string
	}{
		{"A", dns.TypeA, "1.2.3.4"},
		{"AAAA", dns.TypeAAAA, "2001:db8::1"},
	} {
		t.Run(test.title, func(t *testing.T) {
			t.Parallel()

			resp := queryRoundtrip(t, h, test.qtype, "ns.example.com.")

			if resp.Rcode != dns.RcodeSuccess {
				t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
			}

			if len(resp.Answer) != 1 {
				t.Fatalf("expected 1 answer, got %v answers", len(resp.Answer))
			}

			switch test.qtype {
			case dns.TypeA:
				a, ok := resp.Answer[0].(*dns.A)
				if !ok {
					t.Errorf("expected A record in answer section, got %T", resp.Answer[0])
				}

				if a.Addr.String() != test.expectedIP {
					t.Errorf("unexpected IP: got %v, want %v", a.Addr.String(), test.expectedIP)
				}

			case dns.TypeAAAA:
				aaaa, ok := resp.Answer[0].(*dns.AAAA)
				if !ok {
					t.Errorf("expected AAA record in answer section, got %T", resp.Answer[0])
				}

				if aaaa.Addr.String() != test.expectedIP {
					t.Errorf("unexpected IP: got %v, want %v", aaaa.Addr.String(), test.expectedIP)
				}
			}
		})
	}
}

func TestDNSInZoneNSNoGlue(t *testing.T) {
	t.Parallel()

	// NS is in-zone but has no glue records configured; NODATA should be expected.
	h := newTestHandler("example.com.", NewStore(), NSConfig{
		FQDN: "ns.example.com.",
	})
	resp := queryRoundtrip(t, h, dns.TypeA, "ns.example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	if len(resp.Answer) != 0 {
		t.Errorf("expected empty answer for NODATA response, got %v records", len(resp.Answer))
	}

	for _, rr := range resp.Ns {
		if _, ok := rr.(*dns.SOA); ok {
			return
		}
	}

	t.Error("expected SOA in authority section for NODATA response")
}

// TestDNSInZoneNSPriority checks that if a nameserver and an area are defined with the same name,
// the nameserver takes priority.
func TestDNSInZoneNSPriority(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Update("node1", []string{"ns"}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, true)

	h := newTestHandler("example.com.", store, NSConfig{
		FQDN: "ns.example.com.",
		Glue: []netip.Addr{netip.MustParseAddr("1.2.3.4")},
	})
	resp := queryRoundtrip(t, h, dns.TypeA, "ns.example.com.")

	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("unexpected DNS code: got %v, expected NOERROR", dns.RcodeToString[resp.Rcode])
	}

	if len(resp.Answer) != 1 {
		t.Fatalf("expected a single A record, got %v records", len(resp.Answer))
	}

	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Errorf("expected A record in answer section, got %T", resp.Answer[0])
	}

	if a.Addr.String() != "1.2.3.4" {
		t.Errorf("expected NS address 1.2.3.4 but got %v", a.Addr.String())
	}
}
