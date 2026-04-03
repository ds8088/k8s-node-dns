package main

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/miekg/dns"
	"golang.org/x/sync/errgroup"
	ctrl "sigs.k8s.io/controller-runtime"
)

// SOAConfig represents the configuration for zone's SOA record.
type SOAConfig struct {
	NS          string // primary NS FQDN
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

	TTL  uint32 // default TTL for node records
	Glue []netip.Addr

	SOA SOAConfig
}

type dnsHandler struct {
	cfg   DNSConfig
	store *Store
	log   logr.Logger
}

// ServeDNS processes incoming DNS requests and writes a response.
func (h *dnsHandler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetReply(r)

	if err := h.processDNSMessage(r, resp); err != nil {
		h.log.Error(err, "failed to process DNS message")
		resp.SetRcode(r, dns.RcodeServerFailure)
	}

	// Truncate oversized UDP responses and set the TC bit.
	if w.LocalAddr().Network() == "udp" {
		maxSize := uint16(dns.MinMsgSize) // 512 bytes
		if opt := resp.IsEdns0(); opt != nil {
			maxSize = opt.UDPSize()
		}

		resp.Truncate(int(maxSize))
	}

	if err := w.WriteMsg(resp); err != nil {
		h.log.Error(err, "failed to send DNS reply")
	}
}

func (h *dnsHandler) processDNSMessage(req, resp *dns.Msg) error {
	// Echo EDNS0 back to the client.
	if opt := req.IsEdns0(); opt != nil {
		resp.SetEdns0(4096, false)
	}

	// A DNS query must have exactly one question.
	if len(req.Question) != 1 {
		resp.SetRcode(req, dns.RcodeFormatError)
		return nil
	}

	q := req.Question[0]

	// We only serve classes IN and ANY.
	if q.Qclass != dns.ClassINET && q.Qclass != dns.ClassANY {
		resp.SetRcode(req, dns.RcodeRefused)
		return nil
	}

	// Refuse queries outside our zone.
	qname := strings.ToLower(dns.Fqdn(q.Name))
	if !dns.IsSubDomain(h.cfg.Zone, qname) {
		resp.SetRcode(req, dns.RcodeRefused)
		return nil
	}

	// Claim authority over the zone.
	resp.Authoritative = true

	// Handle zone apex.
	if qname == h.cfg.Zone {
		h.handleApex(resp, q)
		return nil
	}

	// Try to strip zone suffix; should return exactly one label (area name).
	areaName := strings.TrimSuffix(qname, "."+h.cfg.Zone)
	if areaName == "" || strings.Contains(areaName, ".") {
		// Invalid area; send NXDOMAIN with SOA in authority.
		resp.SetRcode(req, dns.RcodeNameError)
		resp.Ns = []dns.RR{h.soaRR()}
		return nil
	}

	// Fetch IPs for this area.
	ips, ok := h.store.GetAreaIPs(areaName)
	if !ok {
		// Unknown area; send NXDOMAIN with SOA in authority.
		resp.SetRcode(req, dns.RcodeNameError)
		resp.Ns = []dns.RR{h.soaRR()}
		return nil
	}

	// Area is known (but it may be empty).
	// Iterate over IPs and build the answer section.
	switch q.Qtype {
	case dns.TypeA:
		for _, ip := range ips {
			if ip.Is4() {
				resp.Answer = append(resp.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: h.cfg.TTL},
					A:   ip.AsSlice(),
				})
			}
		}

	case dns.TypeAAAA:
		for _, ip := range ips {
			if ip.Is6() {
				resp.Answer = append(resp.Answer, &dns.AAAA{
					Hdr:  dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: h.cfg.TTL},
					AAAA: ip.AsSlice(),
				})
			}
		}

	case dns.TypeANY:
		// Minimal response according to RFC 8482.
		resp.Answer = append(resp.Answer, &dns.HINFO{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeHINFO, Class: dns.ClassINET, Ttl: h.cfg.TTL},
			Cpu: "RFC8482",
		})
	}

	// NODATA: the name exists but no records match the query type.
	// SOA goes in the authority section.
	if len(resp.Answer) == 0 {
		resp.Ns = []dns.RR{h.soaRR()}
	}

	return nil
}

// handleApex processes requests for the zone apex.
func (h *dnsHandler) handleApex(m *dns.Msg, q dns.Question) {
	switch q.Qtype {
	case dns.TypeSOA:
		m.Answer = append(m.Answer, h.soaRR())
		m.Ns = append(m.Ns, h.nsRR())
		h.appendGlue(m)

	case dns.TypeANY:
		m.Answer = append(m.Answer, h.soaRR(), h.nsRR())
		h.appendGlue(m)

	case dns.TypeNS:
		m.Answer = append(m.Answer, h.nsRR())
		h.appendGlue(m)

	default:
		// NODATA at apex; SOA goes in the authority section.
		m.Ns = []dns.RR{h.soaRR()}
	}
}

// soaRR returns the SOA record.
func (h *dnsHandler) soaRR() *dns.SOA {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: h.cfg.Zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: h.cfg.SOA.TTL},
		Ns:      h.cfg.SOA.NS,
		Mbox:    h.cfg.SOA.Email,
		Serial:  h.store.Serial(),
		Refresh: h.cfg.SOA.Refresh,
		Retry:   h.cfg.SOA.Retry,
		Expire:  h.cfg.SOA.Expire,
		Minttl:  h.cfg.SOA.NegativeTTL,
	}
}

// nsRR returns the NS record for SOA's primary NS.
func (h *dnsHandler) nsRR() *dns.NS {
	return &dns.NS{
		Hdr: dns.RR_Header{Name: h.cfg.Zone, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: h.cfg.TTL},
		Ns:  dns.Fqdn(h.cfg.SOA.NS),
	}
}

// appendGlue appends glue records to the DNS message if SOA NS is located inside the dnsHandler's zone.
func (h *dnsHandler) appendGlue(m *dns.Msg) {
	// Get the FQDN of SOA NS. If it's in our zone, proceed.
	ns := dns.Fqdn(h.cfg.SOA.NS)
	if !dns.IsSubDomain(h.cfg.Zone, strings.ToLower(ns)) {
		return
	}

	for _, glue := range h.cfg.Glue {
		if glue.Is4() {
			m.Extra = append(m.Extra, &dns.A{
				Hdr: dns.RR_Header{Name: ns, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: h.cfg.TTL},
				A:   glue.AsSlice(),
			})
		} else {
			// IPv6
			m.Extra = append(m.Extra, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: ns, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: h.cfg.TTL},
				AAAA: glue.AsSlice(),
			})
		}
	}
}

// StartDNS starts TCP and UDP DNS servers and blocks until ctx is cancelled.
// It returns an error if either server fails.
func StartDNS(ctx context.Context, cfg DNSConfig, store *Store) error {
	tm := 5 * time.Second
	h := &dnsHandler{cfg: cfg, store: store, log: ctrl.Log.WithName("dns")}
	udp := &dns.Server{Addr: cfg.BindAddr, Net: "udp", Handler: h, ReadTimeout: tm, WriteTimeout: tm}
	tcp := &dns.Server{Addr: cfg.BindAddr, Net: "tcp", Handler: h, ReadTimeout: tm, WriteTimeout: tm}

	eg, egctx := errgroup.WithContext(ctx)

	for _, srv := range []*dns.Server{tcp, udp} {
		startOnce := sync.Once{}
		startedCh := make(chan struct{})
		closeStarted := func() { startOnce.Do(func() { close(startedCh) }) }
		srv.NotifyStartedFunc = closeStarted

		eg.Go(func() error {
			err := srv.ListenAndServe()
			closeStarted() // Send the cancellation after the server stops.
			if err != nil {
				return fmt.Errorf("serving DNS server (%v): %w", srv.Net, err)
			}

			return nil
		})

		eg.Go(func() error {
			<-egctx.Done()
			<-startedCh // Also wait until the server has started or ListenAndServe has returned.

			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()

			if err := srv.ShutdownContext(sctx); err != nil {
				h.log.Error(err, "failed to shut down DNS server", "network", srv.Net)
			}

			return nil
		})
	}

	return eg.Wait()
}
