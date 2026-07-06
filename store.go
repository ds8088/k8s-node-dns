package main

import (
	"net/netip"
	"slices"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

type storeNode struct {
	areas []string
	ips   []netip.Addr
	ready bool
}

// storeService holds the state of a single DNS-managed Service.
type storeService struct {
	nodes []string
}

// Store handles a map of all known Kubernetes nodes and DNS-managed Services,
// along with their most recent state.
type Store struct {
	nodes    map[string]*storeNode
	services map[types.NamespacedName]*storeService
	serial   uint32
	mu       sync.RWMutex
}

// NewStore creates an instance of Store.
func NewStore() *Store {
	return &Store{
		nodes:    map[string]*storeNode{},
		services: map[types.NamespacedName]*storeService{},
	}
}

func (s *Store) Update(name string, areas []string, ips []netip.Addr, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existingNode, ok := s.nodes[name]
	if ok {
		// Compare the node's stored state against the current stage.
		// Exit if they are equal, to prevent bumping the DNS serial.
		if slices.Equal(existingNode.areas, areas) &&
			slices.Equal(existingNode.ips, ips) &&
			existingNode.ready == ready {
			return
		}
	}

	s.nodes[name] = &storeNode{areas: areas, ips: ips, ready: ready}
	s.increaseSerial()
}

func (s *Store) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.nodes[name]; ok {
		delete(s.nodes, name)
		s.increaseSerial()
	}
}

// GetAreaIPs returns the deduplicated IPs of all ready nodes in area,
// and also whether the area has at least one known node.
func (s *Store) GetAreaIPs(area string) (ips []netip.Addr, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := map[netip.Addr]struct{}{}

	// Iterate over all areas; this is OK for small number of nodes and areas.
	for _, node := range s.nodes {
		for _, a := range node.areas {
			if a != area {
				continue
			}

			ok = true
			if node.ready {
				for _, ip := range node.ips {
					if _, isSeen := seen[ip]; !isSeen {
						seen[ip] = struct{}{}
						ips = append(ips, ip)
					}
				}
			}

			break
		}
	}

	return
}

// UpdateService updates the store with the set of node names
// each of which hosts endpoints for a Service.
//
// Nodes must be sorted and deduplicated by the caller so that
// semantically-equal updates compare equal and do not bump the serial.
func (s *Store) UpdateService(key types.NamespacedName, nodes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.services[key]; ok && slices.Equal(existing.nodes, nodes) {
		return
	}

	s.services[key] = &storeService{nodes: nodes}
	s.increaseSerial()
}

// RemoveService removes a Service from the store.
func (s *Store) RemoveService(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.services[key]; ok {
		delete(s.services, key)
		s.increaseSerial()
	}
}

// GetServiceIPs returns the deduplicated IPs of all ready nodes
// that host an advertised endpoint of the Service,
// and also the result whether the Service is known by the store.
func (s *Store) GetServiceIPs(namespace, name string) (ips []netip.Addr, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	svc, ok := s.services[types.NamespacedName{Namespace: namespace, Name: name}]
	if !ok {
		return nil, false
	}

	seen := map[netip.Addr]struct{}{}
	for _, nodeName := range svc.nodes {
		node, exists := s.nodes[nodeName]
		if !exists || !node.ready {
			continue // Either an unknown or non-ready node; skip it.
		}

		for _, ip := range node.ips {
			if _, isSeen := seen[ip]; !isSeen {
				seen[ip] = struct{}{}
				ips = append(ips, ip)
			}
		}
	}

	return ips, true
}

// Serial returns the current SOA serial.
func (s *Store) Serial() uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.serial
}

// increaseSerial advances the SOA serial.
// It uses the current time as a serial, but it also keeps the monotonic property
// even if the time goes back, or for sub-second updates.
//
// This function is not thread-safe; callers should implement thread-safety themselves.
func (s *Store) increaseSerial() {
	// This may wrap but wrapping is OK in this case.
	now := uint32(time.Now().Unix()) //nolint:gosec
	old := s.serial

	if old >= now {
		s.serial = old + 1
	} else {
		s.serial = now
	}
}
