package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeberg.org/miekg/dns"
	"codeberg.org/miekg/dns/dnsutil"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
)

func shouldRunIntegrationTests() bool {
	s := strings.ToLower(os.Getenv("RUN_INTEGRATION_TESTS"))
	return s == "1" || s == "true" || s == "yes"
}

// TestMain acts as a harness for a real Kubernetes API server (set up via envtest),
// used for the integration test suite.
//
// Requires kubebuilder binaries and two env variables:
//   - RUN_INTEGRATION_TESTS = 1
//   - KUBEBUILDER_ASSETS = (path to directory with k8s binaries)
//
// Use setup-envtest to populate a directory with binaries:
//
//	setup-envtest use -p path 1.34.1
func TestMain(m *testing.M) {
	exitCode := 0
	defer func() {
		os.Exit(exitCode)
	}()

	if shouldRunIntegrationTests() {
		scheme := runtime.NewScheme()
		utilruntime.Must(corev1.AddToScheme(scheme))
		utilruntime.Must(discoveryv1.AddToScheme(scheme))

		testEnv = &envtest.Environment{}
		cfg, err := testEnv.Start()
		if err != nil {
			panic(fmt.Errorf("starting envtest: %w", err))
		}

		defer func() {
			if envErr := testEnv.Stop(); envErr != nil {
				fmt.Printf("failed to clean up k8s environment: %v\n", envErr.Error())
			}
		}()

		k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			panic(fmt.Errorf("creating k8s client: %w", err))
		}
	}

	exitCode = m.Run()
}

// startTestDNSServer starts a UDP DNS server on a random loopback port,
// backed by the given store and zone, and ensures it will be cleaned up
// with t.Cleanup.
//
// It returns the server address in "host:port" form.
func startTestDNSServer(t *testing.T, store *Store, zone string) string {
	t.Helper()

	h := &dnsHandler{
		cfg: DNSConfig{
			Zone:        zone,
			TTL:         5,
			Nameservers: []NSConfig{{FQDN: "ns." + zone}},
			SOA: SOAConfig{
				Email:       "admin." + zone,
				TTL:         5,
				Refresh:     60,
				Retry:       30,
				Expire:      600,
				NegativeTTL: 5,
			},
		},
		store: store,
		log:   logr.Discard(),
	}

	lc := &net.ListenConfig{}
	listener, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("creating UDP server socket: %v", err)
	}

	udpAddr, ok := listener.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("unexpected address type from ListenPacket")
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(udpAddr.Port))

	srv := &dns.Server{
		PacketConn:  listener,
		Net:         "udp",
		Handler:     h,
		ReadTimeout: 5 * time.Second,
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		srv.Shutdown(ctx)
	})

	go func() {
		serveErr := srv.ListenAndServe()
		if serveErr != nil {
			t.Logf("running DNS server: %v", serveErr)
		}
	}()

	return addr
}

// dnsExchange sends a single DNS query to addr and returns the response.
func dnsExchange(ctx context.Context, addr, qname string, qtype uint16) (*dns.Msg, error) {
	c := dns.NewClient()
	c.ReadTimeout = 2 * time.Second

	var err error
	for range 3 {
		req := dnsutil.SetQuestion(new(dns.Msg), dnsutil.Fqdn(qname), qtype)
		req.RecursionDesired = false

		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		var resp *dns.Msg
		resp, _, err = c.Exchange(sctx, req, "udp", addr)
		cancel()
		if err == nil {
			return resp, nil
		}

		time.Sleep(20 * time.Millisecond)
	}

	return nil, fmt.Errorf("sending DNS query: %w", err)
}

// TestIntegrationNodeReconciler runs the full reconcile loop against a real k8s API server,
// verifying that Store is updated as nodes are created, updated, and deleted.
//
// It also queries a live DNS server to confirm the store state is reflected in DNS responses.
func TestIntegrationNodeReconciler(t *testing.T) {
	if !shouldRunIntegrationTests() {
		t.Skip("RUN_INTEGRATION_TESTS env var is not set, skipping integration tests")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))

	cfg := testEnv.Config
	store := NewStore()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		t.Fatalf("creating manager: %v", err)
	}

	reconciler := NewNodeReconciler(mgr.GetClient(), store, "k8s.pootis.network/areas", "k8s.pootis.network/ips")
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("setting up reconciler with manager: %v", err)
	}

	// Start the DNS server backed by the same store the reconciler updates.
	const dnsZone = "lb.pootis.network."
	dnsAddr := startTestDNSServer(t, store, dnsZone)

	// Send an error to the mgrErrCh if manager itself returns an error.
	mgrErrCh := make(chan error, 1)

	go func() {
		defer close(mgrErrCh)

		if startErr := mgr.Start(ctx); startErr != nil {
			mgrErrCh <- startErr
		}
	}()

	//
	// 1. Create a node and verify that it is present in the store and DNS.
	//

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				"k8s.pootis.network/areas": "home",
				"k8s.pootis.network/ips":   "1.2.3.4",
			},
		},
	}

	if err := k8sClient.Create(ctx, node); err != nil {
		t.Fatalf("creating node: %v", err)
	}

	// The status must be patched separately.
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
	}

	if err := k8sClient.Status().Patch(ctx, node, patch); err != nil {
		t.Fatalf("patching node status: %v", err)
	}

	if !pollCondition(t, func() bool {
		ips, ok := store.GetAreaIPs("home")
		return ok && len(ips) == 1 && ips[0] == netip.MustParseAddr("1.2.3.4")
	}) {
		ips, ok := store.GetAreaIPs("home")
		t.Errorf("expected area to contain IP 1.2.3.4 after node creation, got: ok = %v, ips = %v", ok, ips)
	}

	// A query for home.lb.pootis.network. should also return 1.2.3.4.
	if !pollCondition(t, func() bool {
		resp, err := dnsExchange(ctx, dnsAddr, "home."+dnsZone, dns.TypeA)
		if err != nil {
			return false
		}

		if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
			return false
		}

		a, ok := resp.Answer[0].(*dns.A)
		return ok && a.Addr.String() == "1.2.3.4"
	}) {
		t.Error("expected A record from DNS with value = 1.2.3.4 after node creation")
	}

	//
	// 2. Mark the node as non-ready and wait for the store and DNS to update.
	//
	patch = client.MergeFrom(node.DeepCopy())
	node.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}

	if err := k8sClient.Status().Patch(ctx, node, patch); err != nil {
		t.Fatalf("patching node status (not ready): %v", err)
	}

	if !pollCondition(t, func() bool {
		ips, ok := store.GetAreaIPs("home")
		return ok && len(ips) == 0
	}) {
		ips, ok := store.GetAreaIPs("home")
		t.Errorf("expected area to contain no IPs after node becomes unready, got: ok = %v, ips = %v", ok, ips)
	}

	// Area is still known but has no IPs (expecting NODATA)
	if !pollCondition(t, func() bool {
		resp, err := dnsExchange(ctx, dnsAddr, "home."+dnsZone, dns.TypeA)
		if err != nil {
			return false
		}

		return resp.Rcode == dns.RcodeSuccess && len(resp.Answer) == 0
	}) {
		t.Error("expected NODATA from DNS after node becomes unready")
	}

	//
	// 3. Delete the node and verify it is removed from the store and DNS.
	//
	if err := k8sClient.Delete(ctx, node); err != nil {
		t.Fatalf("deleting node: %v", err)
	}

	if !pollCondition(t, func() bool {
		_, ok := store.GetAreaIPs("home")
		return !ok
	}) {
		_, ok := store.GetAreaIPs("home")
		t.Errorf("expected area to be removed after node gets deleted, got: ok = %v", ok)
	}

	// DNS: area is no longer known; should expect NXDOMAIN.
	if !pollCondition(t, func() bool {
		resp, err := dnsExchange(ctx, dnsAddr, "home."+dnsZone, dns.TypeA)
		if err != nil {
			return false
		}

		return resp.Rcode == dns.RcodeNameError
	}) {
		t.Error("expected NXDOMAIN from DNS after node becomes deleted")
	}

	cancel()

	if err := <-mgrErrCh; err != nil {
		t.Errorf("manager exited with error: %v", err)
	}
}

// TestIntegrationServiceReconciler runs the full reconcile loop against a real k8s API server
// for the Service/EndpointSlice path.
//
// It checks the exact scenario of a single replica that moves between nodes, followed by a node failure.
func TestIntegrationServiceReconciler(t *testing.T) {
	if !shouldRunIntegrationTests() {
		t.Skip("RUN_INTEGRATION_TESTS env var is not set, skipping integration tests")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(discoveryv1.AddToScheme(scheme))

	cfg := testEnv.Config
	store := NewStore()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// The node-dns controller name is also registered by
		// TestIntegrationNodeReconciler in the same process.
		Controller: crconfig.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatalf("creating manager: %v", err)
	}

	const (
		areasAnn = "k8s.pootis.network/areas"
		ipsAnn   = "k8s.pootis.network/ips"
		dnsAnn   = "k8s.pootis.network/dns"
	)

	if err := NewNodeReconciler(mgr.GetClient(), store, areasAnn, ipsAnn).SetupWithManager(mgr); err != nil {
		t.Fatalf("setting up node reconciler: %v", err)
	}

	if err := NewServiceReconciler(mgr.GetClient(), store, dnsAnn).SetupWithManager(mgr); err != nil {
		t.Fatalf("setting up service reconciler: %v", err)
	}

	const (
		dnsZone = "lb.pootis.network."
		svcNS   = "default"
		svcName = "git"
		qname   = svcName + "." + svcNS + "." + dnsZone
	)

	dnsAddr := startTestDNSServer(t, store, dnsZone)

	mgrErrCh := make(chan error, 1)
	go func() {
		defer close(mgrErrCh)
		if startErr := mgr.Start(ctx); startErr != nil {
			mgrErrCh <- startErr
		}
	}()

	// Two ready nodes, each with a distinct external IP.
	for name, ip := range map[string]string{"snode1": "1.1.1.1", "snode2": "2.2.2.2"} {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Annotations: map[string]string{ipsAnn: ip},
			},
		}
		if err := k8sClient.Create(ctx, node); err != nil {
			t.Fatalf("creating node %v: %v", name, err)
		}

		patch := client.MergeFrom(node.DeepCopy())
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		if err := k8sClient.Status().Patch(ctx, node, patch); err != nil {
			t.Fatalf("patching node %v status: %v", name, err)
		}
	}

	// An annotated, headless Service.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   svcNS,
			Name:        svcName,
			Annotations: map[string]string{dnsAnn: "true"},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Ports:     []corev1.ServicePort{{Port: 80}},
		},
	}
	if err := k8sClient.Create(ctx, svc); err != nil {
		t.Fatalf("creating service: %v", err)
	}

	// endpointSliceOn returns a one-endpoint slice hosted on the node.
	endpointSliceOn := func(node string) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: svcNS,
				Name:      svcName + "-slice",
				Labels:    map[string]string{discoveryv1.LabelServiceName: svcName},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{"10.20.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				NodeName:   new(node),
			}},
		}
	}

	// 1. First, replica lands on snode1. The record must resolve to 1.1.1.1.
	slice := endpointSliceOn("snode1")
	if err := k8sClient.Create(ctx, slice); err != nil {
		t.Fatalf("creating endpointslice: %v", err)
	}

	if !pollCondition(t, func() bool {
		return singleAAnswer(ctx, dnsAddr, qname) == "1.1.1.1"
	}) {
		t.Error("expected git service to resolve to 1.1.1.1 while hosted on snode1")
	}

	// 2. Replica moves to snode2. The record resolves to 2.2.2.2.
	patch := client.MergeFrom(slice.DeepCopy())
	slice.Endpoints[0].NodeName = new("snode2")
	if err := k8sClient.Patch(ctx, slice, patch); err != nil {
		t.Fatalf("patching endpointslice to snode2: %v", err)
	}

	if !pollCondition(t, func() bool {
		return singleAAnswer(ctx, dnsAddr, qname) == "2.2.2.2"
	}) {
		t.Error("expected git service to resolve to 2.2.2.2 after the replica moved to snode2")
	}

	// 3. snode2 fails. Record becomes empty (NODATA).
	node2 := &corev1.Node{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "snode2"}, node2); err != nil {
		t.Fatalf("getting snode2: %v", err)
	}

	patch = client.MergeFrom(node2.DeepCopy())
	node2.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	if err := k8sClient.Status().Patch(ctx, node2, patch); err != nil {
		t.Fatalf("patching snode2 to NotReady: %v", err)
	}

	if !pollCondition(t, func() bool {
		resp, err := dnsExchange(ctx, dnsAddr, qname, dns.TypeA)
		return err == nil && resp.Rcode == dns.RcodeSuccess && len(resp.Answer) == 0
	}) {
		t.Error("expected NODATA for git service after its only node failed")
	}

	cancel()
	if err := <-mgrErrCh; err != nil {
		t.Errorf("manager exited with error: %v", err)
	}
}

// singleAAnswer queries addr for qname and returns the single A record in a successful response,
// or "" if the response is not a single-A NOERROR.
func singleAAnswer(ctx context.Context, addr, qname string) string {
	resp, err := dnsExchange(ctx, addr, qname, dns.TypeA)
	if err != nil || resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		return ""
	}

	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		return ""
	}

	return a.Addr.String()
}

// pollCondition polls a condition callback until it returns true or timeout of 15 seconds elapses.
func pollCondition(t *testing.T, condition func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}

		time.Sleep(50 * time.Millisecond)
	}

	return condition()
}
