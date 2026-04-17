/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

// GovernedResourceReconciler reconciles a GovernedResource object
type GovernedResourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=governance.aip.io,resources=governedresources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=governance.aip.io,resources=governedresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=governance.aip.io,resources=governedresources/finalizers,verbs=update
// +kubebuilder:rbac:groups=governance.aip.io,resources=agentrequests,verbs=get;list;watch

func (r *GovernedResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gr governancev1alpha1.GovernedResource
	if err := r.Get(ctx, req.NamespacedName, &gr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1. Check for active AgentRequests referencing this GovernedResource
	activeRequests, err := r.hasActiveRequests(ctx, gr.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 2. Handle Deletion
	if !gr.DeletionTimestamp.IsZero() {
		if !activeRequests {
			// No active requests, safe to remove finalizer
			if controllerutil.ContainsFinalizer(&gr, governancev1alpha1.GovernedResourceFinalizer) {
				controllerutil.RemoveFinalizer(&gr, governancev1alpha1.GovernedResourceFinalizer)
				if err := r.Update(ctx, &gr); err != nil {
					return ctrl.Result{}, err
				}
				logger.Info("Removed finalizer from deleted GovernedResource", "name", gr.Name)
			}
			return ctrl.Result{}, nil
		}
		// Active requests exist; block deletion. Watch events from AgentRequest
		// changes (via .Watches() in SetupWithManager) will drive re-reconciliation
		// when requests complete, so no explicit requeue is needed.
		logger.Info("GovernedResource deletion blocked by active AgentRequests", "name", gr.Name)
		return ctrl.Result{}, nil
	}

	// 3. Proactively ensure the finalizer is present on every GovernedResource.
	// Adding it eagerly avoids a TOCTOU race where a delete arrives after an
	// AgentRequest is admitted but before the first active-requests reconcile.
	if !controllerutil.ContainsFinalizer(&gr, governancev1alpha1.GovernedResourceFinalizer) {
		controllerutil.AddFinalizer(&gr, governancev1alpha1.GovernedResourceFinalizer)
		if err := r.Update(ctx, &gr); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Added finalizer to GovernedResource", "name", gr.Name)
	}

	return ctrl.Result{}, nil
}

func (r *GovernedResourceReconciler) hasActiveRequests(ctx context.Context, grName string) (bool, error) {
	var list governancev1alpha1.AgentRequestList
	if err := r.List(ctx, &list); err != nil {
		return false, fmt.Errorf("failed to list AgentRequests: %w", err)
	}

	for _, ar := range list.Items {
		if ar.Spec.GovernedResourceRef != nil && ar.Spec.GovernedResourceRef.Name == grName {
			// Check if non-terminal
			if ar.Status.Phase != governancev1alpha1.PhaseCompleted &&
				ar.Status.Phase != governancev1alpha1.PhaseFailed &&
				ar.Status.Phase != governancev1alpha1.PhaseDenied {
				return true, nil
			}
		}
	}
	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GovernedResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&governancev1alpha1.GovernedResource{}).
		Watches(
			&governancev1alpha1.AgentRequest{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				ar, ok := o.(*governancev1alpha1.AgentRequest)
				if !ok || ar.Spec.GovernedResourceRef == nil {
					return nil
				}
				return []reconcile.Request{
					{NamespacedName: client.ObjectKey{Name: ar.Spec.GovernedResourceRef.Name}},
				}
			}),
		).
		Complete(r)
}
