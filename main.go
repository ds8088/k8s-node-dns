package main

import (
	"flag"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

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

		ttl            = flag.Uint("ttl", 30, "TTL for A/AAAA records, in seconds.")
		soaNS          = flag.String("soa-ns", "", "SOA primary NS FQDN. Required.")
		soaEmail       = flag.String("soa-email", "", "SOA admin email (example: admin@pootis.network). Required.")
		soaRefresh     = flag.Uint("soa-refresh", 3600, "SOA refresh interval, in seconds.")
		soaRetry       = flag.Uint("soa-retry", 900, "SOA retry interval, in seconds.")
		soaExpire      = flag.Uint("soa-expire", 86400, "SOA expire interval, in seconds.")
		soaTTL         = flag.Uint("soa-ttl", 30, "SOA TTL, in seconds.")
		soaNegativeTTL = flag.Uint("soa-neg-ttl", 30, "SOA negative TTL, in seconds.")
		glueStr        = flag.String("glue", "", "Glue IPs for in-zone NS, comma-separated.")

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
	soaNSFQDN := strings.ToLower(toFQDN(*soaNS))
	soaEmailFQDN := strings.ToLower(toEmail(*soaEmail))
	var glueAddrs []netip.Addr

	for glue := range strings.SplitSeq(*glueStr, ",") {
		glue = strings.TrimSpace(glue)
		if glue == "" {
			continue
		}

		addr, err := netip.ParseAddr(glue)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid glue IP: %v\n", err)
			os.Exit(1)
		}

		glueAddrs = append(glueAddrs, addr)
	}

	store := NewStore()

	dnsCfg := DNSConfig{
		Zone:     zoneFQDN,
		BindAddr: net.JoinHostPort(*bind, strconv.Itoa(*port)),
		TTL:      toUint32Saturate(ttl),
		Glue:     glueAddrs,
		SOA: SOAConfig{
			NS:          soaNSFQDN,
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
		if err := StartDNS(ctx, dnsCfg, store); err != nil {
			log.Error(err, "DNS server failed")
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
