package main

import (
	"net/netip"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func svcKey(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func TestStoreUnknownArea(t *testing.T) {
	t.Parallel()

	s := NewStore()
	_, ok := s.GetAreaIPs("home")

	if ok {
		t.Error("unknown area should not be present in store")
	}
}

func TestStoreGetReadyNode(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, true)

	ips, ok := s.GetAreaIPs("home")
	if !ok {
		t.Error("area with ready node should be present in store")
	}

	if !slices.Equal(ips, []netip.Addr{netip.MustParseAddr("1.1.1.1")}) {
		t.Errorf("got unexpected IPs from store: %v", ips)
	}
}

func TestStoreGetNotReadyNode(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, false)

	// Area is known (node exists) but IPs are empty (node not ready).
	ips, ok := s.GetAreaIPs("home")
	if !ok {
		t.Error("area with non-ready node should be present in store")
	}

	if len(ips) != 0 {
		t.Errorf("expected no IPs for non-ready node, got %v", ips)
	}
}

func TestStoreUpdate(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, true)
	// Move node1 to a different area and IPs.
	s.Update("node1", []string{"external"}, []netip.Addr{netip.MustParseAddr("2.2.2.2")}, true)

	_, ok := s.GetAreaIPs("home")
	if ok {
		t.Error("expected old area to be removed after overwrite")
	}

	ips, ok := s.GetAreaIPs("external")
	if !ok {
		t.Fatal("expected new area to be present in store")
	}

	if !slices.Equal(ips, []netip.Addr{netip.MustParseAddr("2.2.2.2")}) {
		t.Errorf("got unexpected IPs from store: %v", ips)
	}
}

func TestStoreRemove(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, true)
	s.Remove("node1")

	_, ok := s.GetAreaIPs("home")
	if ok {
		t.Error("expected old area to be removed after node deletion")
	}
}

func TestStoreRemoveNonExistent(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Remove("node5")
}

func TestStoreDeduplicatesIPs(t *testing.T) {
	t.Parallel()

	s := NewStore()
	ip := netip.MustParseAddr("1.1.1.1")
	s.Update("node1", []string{"home"}, []netip.Addr{ip}, true)
	s.Update("node2", []string{"home"}, []netip.Addr{ip}, true)

	ips, _ := s.GetAreaIPs("home")
	if len(ips) != 1 {
		t.Errorf("expected 1 deduplicated IP, got %v: %v", len(ips), ips)
	}
}

func TestStoreMixedReadiness(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Update("node1", []string{"home"}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, true)
	s.Update("node2", []string{"home"}, []netip.Addr{netip.MustParseAddr("2.2.2.2")}, false)

	ips, ok := s.GetAreaIPs("home")
	if !ok {
		t.Error("area should be present in store")
	}

	if !slices.Equal(ips, []netip.Addr{netip.MustParseAddr("1.1.1.1")}) {
		t.Errorf("got unexpected IPs from store: %v", ips)
	}
}

func TestStoreMixedIPs(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Update("node1", []string{"home"}, []netip.Addr{
		netip.MustParseAddr("1.2.3.4"),
		netip.MustParseAddr("2001:db8::1"),
	}, true)

	ips, ok := s.GetAreaIPs("home")
	if !ok {
		t.Error("area should be present in store")
	}

	if len(ips) != 2 {
		t.Fatalf("expected 2 IPs, got %v: %v", len(ips), ips)
	}

	var hasV4, hasV6 bool
	for _, ip := range ips {
		if ip.Is4() {
			hasV4 = true
		}

		if ip.Is6() {
			hasV6 = true
		}
	}

	if !hasV4 {
		t.Error("expected IPv4 address in result")
	}

	if !hasV6 {
		t.Error("expected IPv6 address in result")
	}
}

func TestStoreNodeInMultipleAreas(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Update("node1", []string{"home", "external"}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, true)

	for _, area := range []string{"home", "external"} {
		ips, ok := s.GetAreaIPs(area)
		if !ok {
			t.Errorf("area %v should be present in store", area)
		}

		if len(ips) != 1 || ips[0] != netip.MustParseAddr("1.1.1.1") {
			t.Errorf("got unexpected IPs from store for area %v: %v", area, ips)
		}
	}

	_, ok := s.GetAreaIPs("invalid")
	if ok {
		t.Error("expected invalid area to be unknown")
	}
}

func TestStoreUnknownService(t *testing.T) {
	t.Parallel()

	s := NewStore()
	if _, ok := s.GetServiceIPs("default", "git"); ok {
		t.Error("unknown service should not be present in store")
	}
}

func TestStoreServiceReadyNode(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Update("node1", nil, []netip.Addr{netip.MustParseAddr("10.30.0.1")}, true)
	s.UpdateService(svcKey("default", "git"), []string{"node1"})

	ips, ok := s.GetServiceIPs("default", "git")
	if !ok {
		t.Fatal("expected service to be present in store")
	}

	if !slices.Equal(ips, []netip.Addr{netip.MustParseAddr("10.30.0.1")}) {
		t.Errorf("got unexpected IPs from store: %v", ips)
	}
}

func TestStoreServiceNodeNotReady(t *testing.T) {
	t.Parallel()

	s := NewStore()

	// A non-ready node hosts the endpoint: service is known, but resolves to no IPs.
	s.Update("node1", nil, []netip.Addr{netip.MustParseAddr("10.30.0.1")}, false)
	s.UpdateService(svcKey("default", "git"), []string{"node1"})

	ips, ok := s.GetServiceIPs("default", "git")
	if !ok {
		t.Fatal("expected service to be present in store")
	}

	if len(ips) != 0 {
		t.Errorf("expected no IPs for service on a not-ready node, got %v", ips)
	}
}

func TestStoreServiceNodeMissing(t *testing.T) {
	t.Parallel()

	s := NewStore()

	// The service references a node that is not yet known to the store.
	s.UpdateService(svcKey("default", "git"), []string{"node7"})

	ips, ok := s.GetServiceIPs("default", "git")
	if !ok {
		t.Fatal("expected service to be present in store")
	}

	if len(ips) != 0 {
		t.Errorf("expected no IPs when the node is unknown, got %v", ips)
	}
}

func TestStoreServiceDeduplicatesIPs(t *testing.T) {
	t.Parallel()

	s := NewStore()
	ip := netip.MustParseAddr("10.30.0.5")
	s.Update("node1", nil, []netip.Addr{ip}, true)
	s.Update("node2", nil, []netip.Addr{ip}, true)
	s.UpdateService(svcKey("default", "git"), []string{"node1", "node2"})

	ips, _ := s.GetServiceIPs("default", "git")
	if len(ips) != 1 {
		t.Errorf("expected 1 deduplicated IP, got %v: %v", len(ips), ips)
	}
}

func TestStoreRemoveService(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.UpdateService(svcKey("default", "git"), []string{"node1"})
	s.RemoveService(svcKey("default", "git"))

	if _, ok := s.GetServiceIPs("default", "git"); ok {
		t.Error("expected service to be removed")
	}
}

func TestStoreServiceSerialIdempotent(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.UpdateService(svcKey("default", "git"), []string{"node1", "node2"})
	before := s.Serial()

	// Updates for the same node set must be idempotent and thus must not bump the serial.
	s.UpdateService(svcKey("default", "git"), []string{"node1", "node2"})
	if after := s.Serial(); after != before {
		t.Errorf("serial changed after a no-op service update: before %v, after %v", before, after)
	}

	// But a different node set must bump the serial.
	s.UpdateService(svcKey("default", "git"), []string{"node1"})
	if after := s.Serial(); after <= before {
		t.Errorf("serial did not increase after a changed service update: before %v, after %v", before, after)
	}
}

func TestStoreRemoveServiceSerialOnNonexistent(t *testing.T) {
	t.Parallel()

	s := NewStore()
	before := s.Serial()

	s.RemoveService(svcKey("default", "git"))
	if after := s.Serial(); after != before {
		t.Errorf("serial changed after removing a nonexistent service: before %v, after %v", before, after)
	}
}

func TestStoreSerialIncreasesOnUpdate(t *testing.T) {
	t.Parallel()

	s := NewStore()
	before := s.Serial()

	s.Update("node1", []string{"home"}, nil, true)

	after := s.Serial()
	if after <= before {
		t.Errorf("serial did not increase after Update: before: %v, after: %v", before, after)
	}
}

func TestStoreSerialIncreasesOnRemove(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Update("node1", []string{"home"}, nil, true)
	before := s.Serial()

	s.Remove("node1")

	after := s.Serial()
	if after <= before {
		t.Errorf("serial did not increase after Remove: before: %v, after: %v", before, after)
	}
}

func TestStoreSerialOnNonexistentRemove(t *testing.T) {
	t.Parallel()

	s := NewStore()
	before := s.Serial()

	s.Remove("node1")
	s.Remove("node2")

	after := s.Serial()
	if after != before {
		t.Errorf("serial changed after removal of a nonexistent node: before: %v, after: %v", before, after)
	}
}
