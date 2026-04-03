package main

import (
	"net/netip"
	"slices"
	"sync"
	"time"
)

type storeNode struct {
	areas []string
	ips   []netip.Addr
	ready bool
}

// Store handles a map of all known Kubernetes nodes and their most recent state.
type Store struct {
	nodes  map[string]*storeNode
	serial uint32
	mu     sync.RWMutex
}

// NewStore creates an instance of Store.
func NewStore() *Store {
	return &Store{
		nodes: map[string]*storeNode{},
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
