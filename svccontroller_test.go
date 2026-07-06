package main

import (
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const testServiceAnnotation = "k8s.pootis.network/dns"

// managedService returns a Service that opts into DNS management.
func managedService(namespace, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			Annotations: map[string]string{testServiceAnnotation: "true"},
		},
	}
}

// endpointSlice builds an EndpointSlice owned by the Service.
func endpointSlice(namespace, sliceName, svcName string, endpoints ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      sliceName,
			Labels:    map[string]string{discoveryv1.LabelServiceName: svcName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
	}
}

// newFakeServiceReconciler creates a serviceReconciler with a fake k8s client.
func newFakeServiceReconciler(objs ...runtime.Object) (*ServiceReconciler, *Store) {
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(discoveryv1.AddToScheme(s))

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(objs...).
		Build()

	store := NewStore()

	// Seed a couple of nodes.
	store.Update("node1", nil, []netip.Addr{netip.MustParseAddr("10.30.0.1")}, true)
	store.Update("node2", nil, []netip.Addr{netip.MustParseAddr("10.30.0.2")}, true)

	return NewServiceReconciler(cl, store, testServiceAnnotation), store
}

func reconcileService(t *testing.T, r *ServiceReconciler, namespace, name string) {
	t.Helper()

	_, err := r.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	})
	if err != nil {
		t.Fatalf("failed to reconcile: %v", err)
	}
}

func TestServiceReconcileReadyEndpoint(t *testing.T) {
	t.Parallel()

	r, store := newFakeServiceReconciler(
		managedService("default", "livekit"),
		endpointSlice("default", "livekit-1", "livekit", discoveryv1.Endpoint{
			NodeName:   new("node1"),
			Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
		}),
	)

	reconcileService(t, r, "default", "livekit")

	ips, ok := store.GetServiceIPs("default", "livekit")
	if !ok {
		t.Fatal("expected service to be present in store")
	}

	if len(ips) != 1 || ips[0] != netip.MustParseAddr("10.30.0.1") {
		t.Errorf("expected to get the IP of node1, got %v", ips)
	}
}

func TestServiceReconcileAggregatesSlices(t *testing.T) {
	t.Parallel()

	r, store := newFakeServiceReconciler(
		managedService("default", "git"),
		endpointSlice("default", "git-1", "git", discoveryv1.Endpoint{
			NodeName:   new("node1"),
			Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
		}),
		endpointSlice("default", "git-2", "git", discoveryv1.Endpoint{
			NodeName:   new("node2"),
			Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
		}),
	)

	reconcileService(t, r, "default", "git")

	ips, ok := store.GetServiceIPs("default", "git")
	if !ok {
		t.Fatal("expected service to be present in store")
	}

	if len(ips) != 2 {
		t.Errorf("expected IPs from both nodes, got %v", ips)
	}
}

func TestServiceReconcileTerminatingFallback(t *testing.T) {
	t.Parallel()

	// This creates a service with an EndpointSlice that only has a single endpoint
	// in the serving-terminating state.
	r, store := newFakeServiceReconciler(
		managedService("default", "git"),
		endpointSlice("default", "git-1", "git", discoveryv1.Endpoint{
			NodeName: new("node1"),
			Conditions: discoveryv1.EndpointConditions{
				Ready:       new(false),
				Serving:     new(true),
				Terminating: new(true),
			},
		}),
	)

	reconcileService(t, r, "default", "git")

	ips, ok := store.GetServiceIPs("default", "git")
	if !ok {
		t.Fatal("expected service to be present in store")
	}

	if len(ips) != 1 || ips[0] != netip.MustParseAddr("10.30.0.1") {
		t.Errorf("expected to get the node of the serving-terminating endpoint, got %v", ips)
	}
}

func TestServiceReconcileReadyPreferredOverTerminating(t *testing.T) {
	t.Parallel()

	// A ready endpoint on node2 should be selected instead of serving-terminating on node1.
	r, store := newFakeServiceReconciler(
		managedService("default", "internalvpn"),
		endpointSlice("default", "internalvpn-0", "internalvpn",
			discoveryv1.Endpoint{
				NodeName: new("node1"),
				Conditions: discoveryv1.EndpointConditions{
					Ready:       new(false),
					Serving:     new(true),
					Terminating: new(true),
				},
			},
			discoveryv1.Endpoint{
				NodeName:   new("node2"),
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
			},
		),
	)

	reconcileService(t, r, "default", "internalvpn")

	ips, _ := store.GetServiceIPs("default", "internalvpn")
	if len(ips) != 1 || ips[0] != netip.MustParseAddr("10.30.0.2") {
		t.Errorf("expected to get the IP of node2, got %v", ips)
	}
}

func TestServiceReconcileSkipsEndpointWithoutNode(t *testing.T) {
	t.Parallel()

	r, store := newFakeServiceReconciler(
		managedService("default", "git"),
		endpointSlice("default", "git-1", "git", discoveryv1.Endpoint{
			NodeName:   nil,
			Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
		}),
	)

	reconcileService(t, r, "default", "git")

	ips, ok := store.GetServiceIPs("default", "git")
	if !ok {
		t.Fatal("expected service to be present in store (with no usable endpoints)")
	}

	if len(ips) != 0 {
		t.Errorf("expected no IPs for an endpoint without a nodeName, got %v", ips)
	}
}

func TestServiceReconcileUnannotatedRemoved(t *testing.T) {
	t.Parallel()

	svc := managedService("default", "stuff123")
	delete(svc.Annotations, testServiceAnnotation)

	r, store := newFakeServiceReconciler(
		svc,
		endpointSlice("default", "stuff123-1", "stuff123", discoveryv1.Endpoint{
			NodeName:   new("node1"),
			Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
		}),
	)

	// Pre-populate the store, so we can be sure the reconcile actively removes it.
	store.UpdateService(types.NamespacedName{Namespace: "default", Name: "stuff123"}, []string{"node1"})

	reconcileService(t, r, "default", "stuff123")

	if _, ok := store.GetServiceIPs("default", "stuff123"); ok {
		t.Error("expected unannotated service to be removed from store")
	}
}

func TestServiceReconcileDeletedRemoved(t *testing.T) {
	t.Parallel()

	// No Service object exists in the fake client.
	r, store := newFakeServiceReconciler()
	store.UpdateService(types.NamespacedName{Namespace: "default", Name: "empty"}, []string{"node1"})

	reconcileService(t, r, "default", "empty")

	if _, ok := store.GetServiceIPs("default", "empty"); ok {
		t.Error("expected deleted service to be removed from store")
	}
}

func TestMapEndpointSliceToService(t *testing.T) {
	t.Parallel()

	slice := endpointSlice("default", "git-1", "git")
	reqs := mapEndpointSliceToService(t.Context(), slice)

	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %v", len(reqs))
	}

	want := types.NamespacedName{Namespace: "default", Name: "git"}
	if reqs[0].NamespacedName != want {
		t.Errorf("unexpected request: got %v, want %v", reqs[0].NamespacedName, want)
	}

	// A slice without the service-name label must map to nothing.
	orphan := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc123"}}
	if reqs := mapEndpointSliceToService(t.Context(), orphan); len(reqs) != 0 {
		t.Errorf("expected no requests for a slice without the service-name label, got %v", reqs)
	}
}
