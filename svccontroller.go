package main

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ServiceReconciler watches corev1.Service objects (and their EndpointSlices)
// and updates the Store, so that a DNS record with the form
// "<service>.<namespace>.<zone>" resolves to the external IPs
// of the nodes currently hosting the viable endpoints.
type ServiceReconciler struct {
	client.Client

	store             *Store
	serviceAnnotation string
}

// NewServiceReconciler creates an instance of ServiceReconciler.
func NewServiceReconciler(cl client.Client, store *Store, serviceAnnotation string) *ServiceReconciler {
	return &ServiceReconciler{
		Client:            cl,
		store:             store,
		serviceAnnotation: serviceAnnotation,
	}
}

// SetupWithManager sets up the ServiceReconciler controller with the Manager.
//
// The reconciler does two things:
//   - reconciles Services;
//   - and also watches EndpointSlices, extracting Services from them.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(mapEndpointSliceToService)).
		Named("service-dns").
		Complete(r)
}

// mapEndpointSliceToService accepts an EndpointSlice event
// and creates a reconcile request for the Service that owns the EndpointSlice.
func mapEndpointSliceToService(_ context.Context, obj client.Object) []reconcile.Request {
	svcName, ok := obj.GetLabels()[discoveryv1.LabelServiceName]
	if !ok || svcName == "" {
		return nil
	}

	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: svcName},
	}}
}

// Reconcile performs the Kubernetes reconciliation loop.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	key := req.NamespacedName

	// Fetch the service and check if it exists.
	svc := corev1.Service{}
	if err := r.Get(ctx, key, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			r.store.RemoveService(key)
			log.V(1).Info("removed service from store because apiserver says that it has been deleted", "service", key)
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, fmt.Errorf("getting service from apiserver: %w", err)
	}

	// We only manage Services that have expliticly opted in and are currently viable
	// (they have the annotation and are not being deleted).
	if svc.DeletionTimestamp != nil || svc.Annotations[r.serviceAnnotation] != "true" {
		r.store.RemoveService(key)
		log.V(1).Info("removed service from store because it is not (or no longer) managed", "service", key)
		return reconcile.Result{}, nil
	}

	// Aggregate across all of EndpointSlices, since the service may have more than one.
	slices := discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, &slices,
		client.InNamespace(key.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: key.Name},
	); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing EndpointSlices for service %v: %w", key, err)
	}

	// Calculate which nodes are ready and which are in the serving-terminating mode.
	readyNodes, termNodes := map[string]struct{}{}, map[string]struct{}{}
	for i := range slices.Items {
		for _, ep := range slices.Items[i].Endpoints {
			if ep.NodeName == nil || *ep.NodeName == "" {
				log.V(1).Info("skipping endpoint with empty nodeName", "service", key)
				continue
			}

			// isReady calculation looks like a typo, but per the EndpointSlice API,
			// a nil Ready condition should be interpreted as ready.
			isReady := ep.Conditions.Ready == nil || *ep.Conditions.Ready
			serving := ep.Conditions.Serving != nil && *ep.Conditions.Serving
			terminating := ep.Conditions.Terminating != nil && *ep.Conditions.Terminating
			servingTerminating := serving && terminating

			switch {
			case isReady:
				readyNodes[*ep.NodeName] = struct{}{}
			case servingTerminating:
				termNodes[*ep.NodeName] = struct{}{}
			}
		}
	}

	// We infer the kube-proxy semantics here:
	//   - prefer ready endpoints;
	//   - if there are none, fall back to serving-terminating endpoints;
	//   - if there are still no endpoints, bail.
	effectiveNodes := readyNodes
	if len(effectiveNodes) == 0 {
		effectiveNodes = termNodes
	}

	nodes := make([]string, 0, len(effectiveNodes))
	for name := range effectiveNodes {
		nodes = append(nodes, name)
	}
	sort.Strings(nodes)

	if len(nodes) == 0 {
		log.V(1).Info("service has no viable endpoints", "service", key)
	}

	// Update the store.
	r.store.UpdateService(key, nodes)
	log.V(1).Info("reconciled service", "service", key, "nodes", nodes)
	return reconcile.Result{}, nil
}
