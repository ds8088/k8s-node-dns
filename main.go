package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(corev1.AddToScheme(scheme))
}

func main() {
	var (
		zone = flag.String("zone", "", "DNS zone FQDN (example: lb.pootis.network). Required.")
		bind = flag.String("bind", "", "DNS listen address (default: all addresses).")
		port = flag.Int("port", 53, "DNS listen port.")

		ttl   = flag.Uint("ttl", 30, "TTL for A/AAAA records, in seconds.")
		soaNS = flag.String("soa-ns", "", "SOA nameservers, semicolon-separated, with optional glue records (separated by commas) and delimited by a colon. "+
			"Example: ns1.lb.pootis.network:1.1.1.1,2001:db8::1;ns2.pootis.network. The first entry is the SOA primary NS. Required.")
		soaEmail       = flag.String("soa-email", "", "SOA admin email (example: admin@pootis.network). Required.")
		soaRefresh     = flag.Uint("soa-refresh", 3600, "SOA refresh interval, in seconds.")
		soaRetry       = flag.Uint("soa-retry", 900, "SOA retry interval, in seconds.")
		soaExpire      = flag.Uint("soa-expire", 86400, "SOA expire interval, in seconds.")
		soaTTL         = flag.Uint("soa-ttl", 30, "SOA TTL, in seconds.")
		soaNegativeTTL = flag.Uint("soa-neg-ttl", 30, "SOA negative TTL, in seconds.")

		areasAnnotation = flag.String("areas-annotation", "k8s.pootis.network/node-areas", "Node annotation key for area membership.")
		ipsAnnotation   = flag.String("ips-annotation", "k8s.pootis.network/node-ips", "Node annotation key for IPs.")

		leaderElect = flag.Bool("leader-elect", false, "Enable leader election.")
	)

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log

	if *zone == "" || *soaNS == "" || *soaEmail == "" {
		fmt.Fprintln(os.Stderr, "--zone, --soa-ns, and --soa-email are required")
		flag.Usage()
		os.Exit(1)
	}

	if !strings.Contains(*soaEmail, "@") {
		fmt.Fprintln(os.Stderr, "--soa-email must contain a \"@\" symbol")
		os.Exit(1)
	}

	if *port <= 0 || *port > 65535 {
		fmt.Fprintln(os.Stderr, "--port must be between 1 and 65535")
		os.Exit(1)
	}

	zoneFQDN := strings.ToLower(toFQDN(*zone))
	soaEmailFQDN := strings.ToLower(toEmail(*soaEmail))

	nameservers, err := parseNameservers(*soaNS, zoneFQDN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--soa-ns: %v\n", err)
		os.Exit(1)
	}

	store := NewStore()

	dnsCfg := DNSConfig{
		Zone:        zoneFQDN,
		BindAddr:    net.JoinHostPort(*bind, strconv.Itoa(*port)),
		TTL:         toUint32Saturate(ttl),
		Nameservers: nameservers,
		SOA: SOAConfig{
			Email:       soaEmailFQDN,
			TTL:         toUint32Saturate(soaTTL),
			Refresh:     toUint32Saturate(soaRefresh),
			Retry:       toUint32Saturate(soaRetry),
			Expire:      toUint32Saturate(soaExpire),
			NegativeTTL: toUint32Saturate(soaNegativeTTL),
		},
	}

	ctx := ctrl.SetupSignalHandler()

	go func() {
		if dnsErr := StartDNS(ctx, dnsCfg, store); dnsErr != nil {
			log.Error(dnsErr, "DNS server failed")
			os.Exit(1)
		}
	}()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           scheme,
		LeaderElection:   *leaderElect,
		LeaderElectionID: "k8s-node-dns-leader",
		Metrics: metricsserver.Options{
			BindAddress: "0", // "0" disables the metrics server
		},
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	nodeReconciler := NewNodeReconciler(mgr.GetClient(), store, *areasAnnotation, *ipsAnnotation)
	if err := nodeReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to setup node controller")
		os.Exit(1)
	}

	log.Info("starting controller", "zone", zoneFQDN)

	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// parseNameservers parses the --soa-ns flag value.
//
// Nameservers should be formatted as such:
//   - one or more semicolon-separated entries of the form "fqdn:glue1,glue2";
//   - each entry consists of a nameserver FQDN and zero, one, or more glue records;
//   - nameserver FQDN is delimited from its glue with a colon;
//   - glue records are delimited by a colon.
//
// zone must be a normalised FQDN (lowercase and with a trailing dot).
func parseNameservers(s, zone string) ([]NSConfig, error) {
	nameservers := []NSConfig{}

	for part := range strings.SplitSeq(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		nsFQDN, glueStr, ok := strings.Cut(part, ":")
		nsFQDN = strings.ToLower(toFQDN(strings.TrimSpace(nsFQDN)))

		glueAddrs := []netip.Addr{}
		if ok {
			for glue := range strings.SplitSeq(glueStr, ",") {
				glue = strings.TrimSpace(glue)
				if glue == "" {
					continue
				}

				addr, err := netip.ParseAddr(glue)
				if err != nil {
					return nil, fmt.Errorf("invalid glue IP %v for nameserver %v: %w", glue, nsFQDN, err)
				}

				glueAddrs = append(glueAddrs, addr)
			}
		}

		nameservers = append(nameservers, NSConfig{FQDN: nsFQDN, Glue: glueAddrs})
	}

	if len(nameservers) == 0 {
		return nil, errors.New("at least one nameserver is expected")
	}

	// All in-zone NS should have glue records.
	for _, ns := range nameservers {
		if dns.IsSubDomain(zone, ns.FQDN) && len(ns.Glue) == 0 {
			return nil, fmt.Errorf("nameserver %v is in-zone but has no glue records", ns.FQDN)
		}
	}

	return nameservers, nil
}

func toFQDN(s string) string {
	if !strings.HasSuffix(s, ".") {
		return s + "."
	}

	return s
}

func toEmail(s string) string {
	before, after, found := strings.Cut(s, "@")
	if !found {
		return toFQDN(s)
	}

	return toFQDN(strings.ReplaceAll(before, ".", "\\.") + "." + after)
}

func toUint32Saturate(ptr *uint) uint32 {
	if *ptr > math.MaxUint32 {
		return math.MaxUint32
	}

	return uint32(*ptr)
}
