/*
Copyright 2026 fml09.

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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/resources"
)

// NetworkPolicyReconciler reconciles a TypeClawInstance's traffic boundary:
// the NetworkPolicy is re-applied on every reconcile and never deleted, even
// when the Instance itself is removed. Deleting the Instance deletes the
// policy through owner references; a suspended or gone runtime keeps its
// boundary so no other workload can claim its identity meanwhile.
type NetworkPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile applies the declared Network Authority for one TypeClaw Instance
// as an exact NetworkPolicy render. No status is written here: Network
// Authority is externally enforced state, not an Instance milestone.
func (r *NetworkPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var instance typeclawv1alpha1.TypeClawInstance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// SelfConfig observation gives the relay sidecar a legitimate need to
	// reach the Kubernetes API (ADR 0005). Discover the API server's real
	// endpoints so the policy admits exactly those destinations instead of
	// widening PublicWeb.
	var apiServerIPs []string
	// The restart relay reaches the Kubernetes API whenever it runs — not
	// only for SelfConfig observation — so gate on either consumer.
	relayEnabled := instance.Spec.RestartRelay == nil || *instance.Spec.RestartRelay
	if relayEnabled || instance.Spec.SelfConfig != nil {
		var eps corev1.Endpoints
		if err := r.Get(ctx, client.ObjectKey{Name: "kubernetes", Namespace: "default"}, &eps); err == nil {
			for _, subset := range eps.Subsets {
				for _, addr := range subset.Addresses {
					apiServerIPs = append(apiServerIPs, addr.IP)
				}
			}
		}
		// Egress policies match pre-DNAT destinations, so the Service's
		// cluster IP — what DNS hands the relay — needs its own /32.
		var svc corev1.Service
		if err := r.Get(ctx, client.ObjectKey{Name: "kubernetes", Namespace: "default"}, &svc); err == nil && svc.Spec.ClusterIP != "" {
			apiServerIPs = append(apiServerIPs, svc.Spec.ClusterIP)
		}
	}

	policy := resources.NetworkPolicy(&instance, apiServerIPs...)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		// Re-derive the full desired spec so a stale live object cannot keep
		// old ingress CIDRs or an egress universe from a previous spec.
		desired := resources.NetworkPolicy(&instance, apiServerIPs...)
		policy.Spec = desired.Spec
		return netOwn(r, &instance, policy)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply NetworkPolicy: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the NetworkPolicy controller with the Manager.
func (r *NetworkPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&typeclawv1alpha1.TypeClawInstance{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Named("networkpolicy").
		Complete(r)
}

// netOwn mirrors the shared ownership helper with this lane's prefix so both
// controllers can merge into one package without symbol collisions.
func netOwn(r *NetworkPolicyReconciler, instance *typeclawv1alpha1.TypeClawInstance, obj client.Object) error {
	if err := controllerutil.SetControllerReference(instance, obj, r.Scheme); err != nil {
		return err
	}
	obj.SetLabels(resources.Labels(instance))
	return nil
}
