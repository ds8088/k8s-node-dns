package main

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestParseAreas(t *testing.T) {
	t.Parallel()
	log := logr.Discard()

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty string", "", []string{}},
		{"single valid", "home", []string{"home"}},
		{"multiple valid", "home,external", []string{"home", "external"}},
		{"dot not allowed", "cluster,test.cluster,external", []string{"cluster", "external"}},
		{"whitespace trimmed", " home , external ", []string{"home", "external"}},
		{"uppercased", "Home,EXTERNAL", []string{"home", "external"}},
		{"invalid label skipped", "invalid!name,valid", []string{"valid"}},
		{"leading hyphen skipped", "-bad,good", []string{"good"}},
		{"trailing hyphen skipped", "bad-,good", []string{"good"}},
		{"underscore allowed", "my_home", []string{"my_home"}},
		{"hyphen allowed", "my-home", []string{"my-home"}},
		{"single char", "a", []string{"a"}},
		{"63 chars (max)", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		{"64 chars (over max)", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{}},
		{"only commas", ",,,", []string{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := parseAreas(log, test.input); !slices.Equal(got, test.want) {
				t.Errorf("areas parsed incorrectly: got %v, expected %v", got, test.want)
			}
		})
	}
}

func TestParseIPs(t *testing.T) {
	t.Parallel()
	log := logr.Discard()

	tests := []struct {
		name  string
		input string
		want  []netip.Addr
	}{
		{"empty string", "", []netip.Addr{}},
		{"single IPv4", "1.2.3.4", []netip.Addr{netip.MustParseAddr("1.2.3.4")}},
		{"single IPv6", "2001:db8::1", []netip.Addr{netip.MustParseAddr("2001:db8::1")}},
		{"IPv4-mapped IPv6 normalized", "::ffff:1.2.3.4", []netip.Addr{netip.MustParseAddr("1.2.3.4")}},
		{"multiple mixed", "1.2.3.4,2001:db8::1", []netip.Addr{netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("2001:db8::1")}},
		{"3 addresses", "1.2.3.4,2001:db8::1,1.2.3.5", []netip.Addr{
			netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("1.2.3.5"),
		}},
		{"invalid skipped", "123,1.2.3.4", []netip.Addr{netip.MustParseAddr("1.2.3.4")}},
		{"whitespace trimmed", " 1.2.3.4 ", []netip.Addr{netip.MustParseAddr("1.2.3.4")}},
		{"only commas", ",,,", []netip.Addr{}},
		{"loopback IPv4", "127.0.0.1", []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		{"loopback IPv6", "::1", []netip.Addr{netip.MustParseAddr("::1")}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := parseIPs(log, test.input); !slices.Equal(got, test.want) {
				t.Errorf("IPs parsed incorrectly: got %v, expected %v", got, test.want)
			}
		})
	}
}

func TestIsNodeReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		node *corev1.Node
		want bool
	}{
		{"no conditions", &corev1.Node{}, false},
		{"ready", &corev1.Node{
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				},
			},
		}, true},
		{"not ready", &corev1.Node{
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
				},
			},
		}, false},
		{"unrelated statuses", &corev1.Node{
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse},
					{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
				},
			},
		}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := isNodeReady(test.node); got != test.want {
				t.Errorf("unexpected node status: got %v, expected %v", got, test.want)
			}
		})
	}
}

func newFakeReconciler(nodes ...*corev1.Node) (*NodeReconciler, *Store) {
	runtimeObjs := make([]runtime.Object, 0, len(nodes))
	for _, node := range nodes {
		runtimeObjs = append(runtimeObjs, node)
	}

	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(runtimeObjs...).
		Build()

	store := NewStore()
	r := NewNodeReconciler(cl, store, "areas", "ips")

	return r, store
}

func TestReconcilerNodeDelete(t *testing.T) {
	t.Parallel()

	reconciler, store := newFakeReconciler()

	// Pre-populate the store.
	store.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.2.3.4")}, true)

	_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "node1"}})
	if err != nil {
		t.Fatalf("failed to reconcile: %v", err)
	}

	_, ok := store.GetAreaIPs("home")
	if ok {
		t.Error("expected area \"home\" to be removed after node removal")
	}
}

func TestReconcilerNodeUpdate(t *testing.T) {
	t.Parallel()

	reconciler, store := newFakeReconciler(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node2",
			Annotations: map[string]string{
				"areas": "home, external",
				"ips":   "1.2.3.4,2001:db8::1",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	})

	_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "node2"}})
	if err != nil {
		t.Fatalf("failed to reconcile: %v", err)
	}

	for _, area := range []string{"home", "external"} {
		ips, ok := store.GetAreaIPs(area)
		if !ok {
			t.Errorf("expected presence of area \"%v\" after reconciliation", area)
		}

		if len(ips) != 2 {
			t.Errorf("expected 2 IPs in area \"%v\", got %v IPs", area, len(ips))
		}
	}
}

func TestReconcilerNodeUpdateNotReady(t *testing.T) {
	t.Parallel()

	reconciler, store := newFakeReconciler(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node3",
			Annotations: map[string]string{
				"areas": "home",
				"ips":   "2001:db8::1",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	})

	_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "node3"}})
	if err != nil {
		t.Fatalf("failed to reconcile: %v", err)
	}

	ips, ok := store.GetAreaIPs("home")
	if !ok {
		t.Errorf("expected presence of area \"home\" after reconciliation")
	}

	if len(ips) != 0 {
		t.Errorf("expected 0 IPs for not-ready node in area \"home\", got %v IPs", ips)
	}
}

func TestReconcilerDeletionTimestamp(t *testing.T) {
	t.Parallel()

	now := metav1.NewTime(time.Now())

	reconciler, store := newFakeReconciler(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node1",
			DeletionTimestamp: &now,
			// Finalizer is needed to keep the node alive in the fake tracker.
			Finalizers: []string{"k8s.pootis.network/finalizer"},
			Annotations: map[string]string{
				"areas": "home",
				"ips":   "1.2.3.4,not-an-ip",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	})

	store.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.2.3.4")}, true)

	_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "node1"}})
	if err != nil {
		t.Fatalf("failed to reconcile: %v", err)
	}

	_, ok := store.GetAreaIPs("home")
	if ok {
		t.Errorf("expected area \"home\" to be removed after reconciliation due to DeletionTimestamp")
	}
}

func TestReconcilerNoAnnotations(t *testing.T) {
	t.Parallel()

	reconciler, store := newFakeReconciler(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node4"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	})

	_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "node4"}})
	if err != nil {
		t.Fatalf("failed to reconcile: %v", err)
	}

	_, ok := store.GetAreaIPs("stuff")
	if ok {
		t.Error("expected no areas for a node with no annotations")
	}
}

func TestReconcilerIdempotent(t *testing.T) {
	t.Parallel()

	reconciler, store := newFakeReconciler(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				"areas": "home",
				"ips":   "1.2.3.4",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	})

	for range 3 {
		_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "node1"}})
		if err != nil {
			t.Fatalf("failed to reconcile: %v", err)
		}
	}

	ips, ok := store.GetAreaIPs("home")
	if !ok {
		t.Errorf("expected presence of area \"home\" after reconciliation")
	}

	if len(ips) != 1 || ips[0] != netip.MustParseAddr("1.2.3.4") {
		t.Errorf("expected a single IP 1.2.3.4 in area \"home\" after reconciliation, got IPs: %v", ips)
	}
}
