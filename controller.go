package main

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// labelRegexp validates a single DNS label, according to RFC 1123,
// but it also allows underscores (as per k8s constraints).
// The DNS label is assumed to be in lower case.
var labelRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,61}[a-z0-9])?$`)

// NodeReconciler watches corev1.Node objects and updates the Store
// according to the node annotations.
type NodeReconciler struct {
	client.Client

	store           *Store
	areasAnnotation string
	ipsAnnotation   string
}

// NewNodeReconciler creates an instance of NodeReconciler.
func NewNodeReconciler(cl client.Client, store *Store, areasAnnotation, ipsAnnotation string) *NodeReconciler {
	return &NodeReconciler{
		Client:          cl,
		store:           store,
		areasAnnotation: areasAnnotation,
		ipsAnnotation:   ipsAnnotation,
	}
}

// SetupWithManager sets up the NodeReconciler controller with the Manager.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&corev1.Node{}).Named("node-dns").Complete(r)
}

// Reconcile performs the Kubernetes reconciliation loop.
func (r *NodeReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	node := corev1.Node{}

	// Fetch the node and check if it exists.
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		if apierrors.IsNotFound(err) {
			r.store.Remove(req.Name)
			log.V(1).Info("removed node from store because apiserver says that it has been deleted", "node", req.Name)
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, fmt.Errorf("getting node from apiserver: %w", err)
	}

	// If graceful deletion of a node has been requested, treat that node as deleted.
	if node.DeletionTimestamp != nil {
		r.store.Remove(node.Name)
		log.V(1).Info("removed node from store because it has a non-null deletionTimestamp", "node", node.Name)
		return reconcile.Result{}, nil
	}

	// Parse the data for this node.
	ready := isNodeReady(&node)
	areas := parseAreas(log.WithName("parseAreas"), node.Annotations[r.areasAnnotation])
	ips := parseIPs(log.WithName("parseIPs"), node.Annotations[r.ipsAnnotation])

	// Update the store.
	r.store.Update(node.Name, areas, ips, ready)
	log.V(1).Info("reconciled node", "node", node.Name, "ready", ready, "areas", areas, "ips", ips)
	return reconcile.Result{}, nil
}

// parseAreas parses comma-separated area names from a Kubernetes annotation.
//
// It returns a slice of valid area names.
func parseAreas(log logr.Logger, areas string) []string {
	res := []string{}
	for area := range strings.SplitSeq(areas, ",") {
		area = strings.ToLower(strings.TrimSpace(area))
		if area == "" {
			continue
		}

		if !labelRegexp.MatchString(area) {
			log.Info("skipping invalid area name (must be a valid DNS label)", "area", area)
			continue
		}

		res = append(res, area)
	}

	return res
}

// parseIPs parses comma-separated IP addresses from a Kubernetes annotation.
//
// It returns a slice of IP addresses.
//
// IPv4-mapped IPv6 addresses are normalized to plain IPv4.
func parseIPs(log logr.Logger, ips string) []netip.Addr {
	res := []netip.Addr{}
	for ipStr := range strings.SplitSeq(ips, ",") {
		ipStr = strings.TrimSpace(ipStr)
		if ipStr == "" {
			continue
		}

		ip, err := netip.ParseAddr(ipStr)
		if err != nil {
			log.Error(err, "skipping invalid node IP", "ip", ipStr)
			continue
		}
		// Normalize IPv6-mapped addrs to IPv4.
		ip = ip.Unmap()

		res = append(res, ip)
	}

	return res
}

// isNodeReady checks if the node's Ready condition is True.
func isNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}

	return false
}
