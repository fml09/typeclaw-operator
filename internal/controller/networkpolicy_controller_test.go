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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/resources"
)

// netSchemeFor is this lane's local scheme helper; the lane prefix avoids a
// symbol collision with the other controllers' helpers in this package.
func netSchemeFor(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	if err := typeclawv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("typeclaw scheme: %v", err)
	}
	return s
}

func netReconcilerFor(t *testing.T, objs ...client.Object) (*NetworkPolicyReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(netSchemeFor(t)).
		WithStatusSubresource(&typeclawv1alpha1.TypeClawInstance{}).
		WithObjects(objs...).
		Build()
	return &NetworkPolicyReconciler{Client: c, Scheme: c.Scheme()}, c
}

func netReconcile(t *testing.T, r *NetworkPolicyReconciler, key types.NamespacedName) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	return res
}

func getNetPolicy(t *testing.T, c client.Client, key types.NamespacedName) *networkingv1.NetworkPolicy {
	t.Helper()
	var policy networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), key, &policy); err != nil {
		t.Fatalf("Get NetworkPolicy %s: %v", key, err)
	}
	return &policy
}

func TestNetworkPolicyReconcileCreatesAndNeverDeletes(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Network.IngressCIDRs = []string{"203.0.113.0/24"}
	})
	r, c := netReconcilerFor(t, in)
	key := types.NamespacedName{Name: in.Name, Namespace: in.Namespace}
	netReconcile(t, r, key)

	policy := getNetPolicy(t, c, key)
	if len(policy.Spec.Ingress) != 2 {
		t.Fatalf("ingress rules = %d, want same-namespace + CIDR", len(policy.Spec.Ingress))
	}

	// Instance gone: the boundary stays. Deletion flows through owner
	// references only.
	if err := c.Delete(context.Background(), in); err != nil {
		t.Fatalf("Delete instance: %v", err)
	}
	netReconcile(t, r, key)

	policy = getNetPolicy(t, c, key)
	if owner := metav1.GetControllerOf(policy); owner == nil || owner.Name != in.Name {
		t.Fatalf("controller owner = %+v, want instance", owner)
	}
}

func TestNetworkPolicyReconcileAppliesSpecDrift(t *testing.T) {
	in := instance("kakao-agent", nil)
	r, c := netReconcilerFor(t, in)
	key := types.NamespacedName{Name: in.Name, Namespace: in.Namespace}
	netReconcile(t, r, key)

	// Drift the spec toward Unrestricted and re-reconcile: the live policy
	// must converge to the new egress universe.
	in.Spec.Network.Egress = resources.EgressUnrestricted
	if err := c.Update(context.Background(), in); err != nil {
		t.Fatalf("Update instance: %v", err)
	}
	netReconcile(t, r, key)

	got := getNetPolicy(t, c, key)
	if len(got.Spec.Egress) != 1 || got.Spec.Egress[0].To[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Fatalf("egress did not converge to Unrestricted: %+v", got.Spec.Egress)
	}
}

func TestNetworkPolicyReconcileIsIdempotentOnSecondPass(t *testing.T) {
	in := instance("kakao-agent", nil)
	r, c := netReconcilerFor(t, in)
	key := types.NamespacedName{Name: in.Name, Namespace: in.Namespace}
	netReconcile(t, r, key)

	first := getNetPolicy(t, c, key)

	res := netReconcile(t, r, key)
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("second reconcile requested requeue: %+v", res)
	}

	second := getNetPolicy(t, c, key)
	if second.ResourceVersion != first.ResourceVersion {
		t.Fatalf("second reconcile rewrote identical policy: rv %s -> %s",
			first.ResourceVersion, second.ResourceVersion)
	}
}
