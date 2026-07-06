package main

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codeberg.org/miekg/dns"
	"codeberg.org/miekg/dns/dnsutil"
	"codeberg.org/miekg/dns/rdata"
	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	ctrl "sigs.k8s.io/controller-runtime"
)

// NSConfig represents a nameserver FQDN and all of its associated glue records,
// if the nameserver is located in-zone.
type NSConfig struct {
	FQDN string // with trailing dot
	Glue []netip.Addr
}

// SOAConfig represents the configuration for zone's SOA record.
type SOAConfig struct {
	Email       string // email in DNS dot-format
	TTL         uint32 // TTL of SOA RR
	Refresh     uint32
	Retry       uint32
	Expire      uint32
	NegativeTTL uint32 // negative-cache TTL
}

// DNSConfig represents the DNS server configuration.
type DNSConfig struct {
	Zone     string // FQDN with trailing dot
	BindAddr string // address to listen on

	TTL         uint32     // default TTL for node records
	Nameservers []NSConfig // first entry is the SOA primary NS

	SOA SOAConfig
}

type dnsHandler struct {
	cfg   DNSConfig
	store *Store
	log   logr.Logger

	// ready reports if the store is authoritative and ready.
	//
	// If it returns false, the handler must answer with SERVFAIL
	// so that resolvers do not negatively cache a bad response code.
	//
	// A nil ready is treated as always-ready (it is used by tests).
	ready func() bool

	inZoneNS map[string]NSConfig // In-zone nameservers, keyed by their lowercased FQDN
}

// newDNSHandler creates an instance of dnsHandler.
//
// ready may be nil, in which case the handler is always considered ready.
func newDNSHandler(cfg DNSConfig, store *Store, log logr.Logger, ready func() bool) *dnsHandler {
	dh := &dnsHandler{cfg: cfg, store: store, log: log, ready: ready, inZoneNS: map[string]NSConfig{}}

	for _, ns := range cfg.Nameservers {
		lower := strings.ToLower(ns.FQDN)
		if dnsutil.IsBelow(cfg.Zone, lower) {
			dh.inZoneNS[lower] = ns
		}
	}

	return dh
}

// ServeDNS processes incoming DNS requests and writes a response.
func (h *dnsHandler) ServeDNS(_ context.Context, w dns.ResponseWriter, r *dns.Msg) {
	resp := h.buildReply(r, w.LocalAddr().Network() == "udp")

	if err := resp.Pack(); err != nil {
		h.log.Error(err, "failed to pack DNS reply")
		return
	}

	if _, err := io.Copy(w, resp); err != nil {
		h.log.Error(err, "failed to send DNS reply")
	}
}

// buildReply builds the response message for the request r. If udp is true, the
// response is truncated to fit the advertised UDP buffer size.
func (h *dnsHandler) buildReply(r *dns.Msg, udp bool) *dns.Msg {
	resp := new(dns.Msg)
	dnsutil.SetReply(resp, r)

	if err := h.processDNSMessage(r, resp); err != nil {
		h.log.Error(err, "failed to process DNS message")
		resp.Rcode = dns.RcodeServerFailure
	}

	if udp {
		maxSize := dns.MinMsgSize // 512 bytes
		if resp.UDPSize > dns.MinMsgSize {
			maxSize = int(resp.UDPSize)
		}

		truncateResponse(resp, maxSize)
	}

	return resp
}

// truncateResponse truncates a DNS message so it fits in maxSize bytes,
// setting the TC bit if any answer records had to be dropped.
func truncateResponse(m *dns.Msg, maxSize int) {
	if m.Len() <= maxSize {
		return
	}

	// Drop glue records first.
	m.Extra = nil

	// Drop answer records until the message fits, flagging truncation.
	for m.Len() > maxSize && len(m.Answer) > 0 {
		m.Answer = m.Answer[:len(m.Answer)-1]
		m.Truncated = true
	}
}

func (h *dnsHandler) processDNSMessage(req, resp *dns.Msg) error {
	// Echo EDNS0 back to the client.
	if req.UDPSize > 0 {
		resp.UDPSize = 4096
	}

	// If the readiness callback is present and it returns false, reply with SERVFAIL.
	if h.ready != nil && !h.ready() {
		resp.Rcode = dns.RcodeServerFailure
		return nil
	}

	// A DNS query must have exactly one question.
	if len(req.Question) != 1 {
		resp.Rcode = dns.RcodeFormatError
		return nil
	}

	q := req.Question[0]

	// We only serve classes IN and ANY.
	if qclass := q.Header().Class; qclass != dns.ClassINET && qclass != dns.ClassANY {
		resp.Rcode = dns.RcodeRefused
		return nil
	}

	qtype := dns.RRToType(q)
	name := q.Header().Name                      // original case, echoed in answer records
	qname := strings.ToLower(dnsutil.Fqdn(name)) // normalized for lookups

	// Refuse queries outside our zone.
	if !dnsutil.IsBelow(h.cfg.Zone, qname) {
		resp.Rcode = dns.RcodeRefused
		return nil
	}

	// Claim authority over the zone.
	resp.Authoritative = true

	// Handle zone apex.
	if qname == h.cfg.Zone {
		h.handleApex(resp, qtype)
		return nil
	}

	// Handle queries for in-zone nameservers.
	// These queries take priority compared to area queries.
	if ns, ok := h.inZoneNS[qname]; ok {
		h.handleInZoneNameserver(resp, name, qtype, ns)
		return nil
	}

	// Strip the zone suffix and dispatch on the number of remaining labels:
	//   - one label: "<area>" - this is an area record;
	//   - two labels: "<service>.<ns>" - this is a Service record.
	sub := strings.TrimSuffix(qname, "."+h.cfg.Zone)
	labels := strings.Split(sub, ".")
	ips := []netip.Addr{}
	known := false

	switch len(labels) {
	case 1:
		if labels[0] == "" {
			break
		}

		ips, known = h.store.GetAreaIPs(labels[0])

	case 2:
		// Record is "<service>.<namespace>.<zone>".
		ips, known = h.store.GetServiceIPs(labels[1], labels[0])
	}

	if !known {
		// Unknown name; send NXDOMAIN with SOA in authority.
		resp.Rcode = dns.RcodeNameError
		resp.Ns = []dns.RR{h.soaRR()}
		return nil
	}

	// Name is known (but it may resolve to no IPs).
	h.writeAddrAnswers(resp, name, qtype, ips)
	return nil
}

// writeAddrAnswers builds the answer section for a known name that resolves to
// a list of IPs, according to the query type.
//
// If no records match, it returns a NODATA response with the SOA in the authority section.
func (h *dnsHandler) writeAddrAnswers(resp *dns.Msg, name string, qtype uint16, ips []netip.Addr) {
	switch qtype {
	case dns.TypeA:
		for _, ip := range ips {
			if ip.Is4() {
				resp.Answer = append(resp.Answer, &dns.A{
					Hdr: dns.Header{Name: name, Class: dns.ClassINET, TTL: h.cfg.TTL},
					A:   rdata.A{Addr: ip},
				})
			}
		}

	case dns.TypeAAAA:
		for _, ip := range ips {
			if ip.Is6() {
				resp.Answer = append(resp.Answer, &dns.AAAA{
					Hdr:  dns.Header{Name: name, Class: dns.ClassINET, TTL: h.cfg.TTL},
					AAAA: rdata.AAAA{Addr: ip},
				})
			}
		}

	case dns.TypeANY:
		// Minimal response according to RFC 8482.
		resp.Answer = append(resp.Answer, &dns.HINFO{
			Hdr:   dns.Header{Name: name, Class: dns.ClassINET, TTL: h.cfg.TTL},
			HINFO: rdata.HINFO{Cpu: "RFC8482"},
		})
	}

	// NODATA: the name exists but no records match the query type.
	// SOA goes in the authority section.
	if len(resp.Answer) == 0 {
		resp.Ns = []dns.RR{h.soaRR()}
	}
}

// handleApex processes requests for the zone apex.
func (h *dnsHandler) handleApex(m *dns.Msg, qtype uint16) {
	switch qtype {
	case dns.TypeSOA:
		m.Answer = append(m.Answer, h.soaRR())
		m.Ns = append(m.Ns, h.nsRRs()...)
		h.appendGlue(m)

	case dns.TypeANY:
		m.Answer = append(m.Answer, h.soaRR())
		m.Answer = append(m.Answer, h.nsRRs()...)
		h.appendGlue(m)

	case dns.TypeNS:
		m.Answer = append(m.Answer, h.nsRRs()...)
		h.appendGlue(m)

	default:
		// NODATA at apex; SOA goes in the authority section.
		m.Ns = []dns.RR{h.soaRR()}
	}
}

// handleInZoneNameserver handles queries for an in-zone nameserver.
//
// It constructs the nameserver address from the glue records.
func (h *dnsHandler) handleInZoneNameserver(m *dns.Msg, name string, qtype uint16, ns NSConfig) {
	switch qtype {
	case dns.TypeA:
		for _, glue := range ns.Glue {
			if glue.Is4() {
				m.Answer = append(m.Answer, &dns.A{
					Hdr: dns.Header{Name: name, Class: dns.ClassINET, TTL: h.cfg.TTL},
					A:   rdata.A{Addr: glue},
				})
			}
		}

	case dns.TypeAAAA:
		for _, glue := range ns.Glue {
			if glue.Is6() {
				m.Answer = append(m.Answer, &dns.AAAA{
					Hdr:  dns.Header{Name: name, Class: dns.ClassINET, TTL: h.cfg.TTL},
					AAAA: rdata.AAAA{Addr: glue},
				})
			}
		}

	case dns.TypeANY:
		// Minimal response according to RFC 8482.
		m.Answer = append(m.Answer, &dns.HINFO{
			Hdr:   dns.Header{Name: name, Class: dns.ClassINET, TTL: h.cfg.TTL},
			HINFO: rdata.HINFO{Cpu: "RFC8482"},
		})
	}

	// NODATA: NS is known but no records match the query type.
	// SOA goes in the authority section.
	if len(m.Answer) == 0 {
		m.Ns = []dns.RR{h.soaRR()}
	}
}

// soaRR returns the SOA record.
//
// The primary NS is taken from the first configured nameserver
// (there is always at least one nameserver).
func (h *dnsHandler) soaRR() *dns.SOA {
	return &dns.SOA{
		Hdr: dns.Header{Name: h.cfg.Zone, Class: dns.ClassINET, TTL: h.cfg.SOA.TTL},
		SOA: rdata.SOA{
			Ns:      h.cfg.Nameservers[0].FQDN,
			Mbox:    h.cfg.SOA.Email,
			Serial:  h.store.Serial(),
			Refresh: h.cfg.SOA.Refresh,
			Retry:   h.cfg.SOA.Retry,
			Expire:  h.cfg.SOA.Expire,
			Minttl:  h.cfg.SOA.NegativeTTL,
		},
	}
}

// nsRRs returns NS records for all configured nameservers.
func (h *dnsHandler) nsRRs() []dns.RR {
	rrs := make([]dns.RR, 0, len(h.cfg.Nameservers))
	for _, ns := range h.cfg.Nameservers {
		rrs = append(rrs, &dns.NS{
			Hdr: dns.Header{Name: h.cfg.Zone, Class: dns.ClassINET, TTL: h.cfg.TTL},
			NS:  rdata.NS{Ns: dnsutil.Fqdn(ns.FQDN)},
		})
	}

	return rrs
}

// appendGlue appends glue records to the DNS message for in-zone nameservers.
func (h *dnsHandler) appendGlue(m *dns.Msg) {
	for _, ns := range h.cfg.Nameservers {
		fqdn := dnsutil.Fqdn(ns.FQDN)
		if !dnsutil.IsBelow(h.cfg.Zone, strings.ToLower(fqdn)) {
			continue
		}

		for _, glue := range ns.Glue {
			if glue.Is4() {
				m.Extra = append(m.Extra, &dns.A{
					Hdr: dns.Header{Name: fqdn, Class: dns.ClassINET, TTL: h.cfg.TTL},
					A:   rdata.A{Addr: glue},
				})
			} else {
				m.Extra = append(m.Extra, &dns.AAAA{
					Hdr:  dns.Header{Name: fqdn, Class: dns.ClassINET, TTL: h.cfg.TTL},
					AAAA: rdata.AAAA{Addr: glue},
				})
			}
		}
	}
}

// StartDNS starts TCP and UDP DNS servers and blocks until ctx is cancelled.
// It returns an error if either server fails.
func StartDNS(ctx context.Context, cfg DNSConfig, store *Store, ready func() bool) error {
	tm := 5 * time.Second
	h := newDNSHandler(cfg, store, ctrl.Log.WithName("dns"), ready)
	udp := &dns.Server{Addr: cfg.BindAddr, Net: "udp", Handler: h, ReadTimeout: tm}
	tcp := &dns.Server{Addr: cfg.BindAddr, Net: "tcp", Handler: h, ReadTimeout: tm}

	eg, egctx := errgroup.WithContext(ctx)

	for _, srv := range []*dns.Server{tcp, udp} {
		startOnce := sync.Once{}
		startedCh := make(chan struct{})
		closeStarted := func() { startOnce.Do(func() { close(startedCh) }) }

		// started reports whether the server actually began serving.
		started := atomic.Bool{}
		srv.NotifyStartedFunc = func(context.Context) {
			started.Store(true)
			closeStarted()
		}

		eg.Go(func() error {
			err := srv.ListenAndServe()
			closeStarted() // Unblock the shutdown goroutine if the server never started.
			if err != nil {
				return fmt.Errorf("serving DNS server (%v): %w", srv.Net, err)
			}

			return nil
		})

		eg.Go(func() error {
			<-egctx.Done()
			<-startedCh // Also wait until the server has started or ListenAndServe has returned.

			if !started.Load() {
				return nil
			}

			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()

			srv.Shutdown(sctx)

			return nil
		})
	}

	return eg.Wait()
}
