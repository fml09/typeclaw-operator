package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
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

func reconcilerFor(t *testing.T, objs ...client.Object) (*TypeClawInstanceReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&typeclawv1alpha1.TypeClawInstance{}).
		WithObjects(objs...).
		Build()
	return &TypeClawInstanceReconciler{Client: c, Scheme: c.Scheme()}, c
}

func instance(name string, mutate func(*typeclawv1alpha1.TypeClawInstance)) *typeclawv1alpha1.TypeClawInstance {
	in := &typeclawv1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agents"},
	}
	in.GenerateName = ""
	if mutate != nil {
		mutate(in)
	}
	return in
}

func reconcile(t *testing.T, r *TypeClawInstanceReconciler, key types.NamespacedName) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	return res
}

func condition(status *typeclawv1alpha1.TypeClawInstanceStatus, condType string) *metav1.Condition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			return &status.Conditions[i]
		}
	}
	return nil
}

func TestReconcileCreatesWorkloadAndReportsResourcesReady(t *testing.T) {
	in := instance("kakao-agent", nil)
	r, c := reconcilerFor(t, in)
	key := types.NamespacedName{Namespace: in.Namespace, Name: in.Name}

	reconcile(t, r, key)

	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), key, &sts); err != nil {
		t.Fatalf("StatefulSet not created: %v", err)
	}
	if *sts.Spec.Replicas != 1 {
		t.Errorf("workload must be single-active, replicas=%d", *sts.Spec.Replicas)
	}
	var svc corev1.Service
	if err := c.Get(context.Background(), key, &svc); err != nil {
		t.Fatalf("Service not created for default exposure: %v", err)
	}

	var got typeclawv1alpha1.TypeClawInstance
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("instance read-back: %v", err)
	}
	rc := condition(&got.Status, ConditionResourcesReady)
	if rc == nil || rc.Status != metav1.ConditionTrue {
		t.Fatalf("ResourcesReady must be True after apply, got %+v", rc)
	}
	if rc.ObservedGeneration != got.Generation {
		t.Errorf("condition must observe current generation %d, got %d", got.Generation, rc.ObservedGeneration)
	}
	if owner := metav1.GetControllerOf(&sts); owner == nil || owner.Name != in.Name {
		t.Errorf("StatefulSet must be controller-owned by the Instance, got %+v", owner)
	}
}

func TestReconcileIsIdempotentOnConditions(t *testing.T) {
	in := instance("kakao-agent", nil)
	r, c := reconcilerFor(t, in)
	key := types.NamespacedName{Namespace: in.Namespace, Name: in.Name}

	reconcile(t, r, key)
	reconcile(t, r, key)

	var got typeclawv1alpha1.TypeClawInstance
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("instance read-back: %v", err)
	}
	if len(got.Status.Conditions) != 2 {
		t.Fatalf("repeated reconciliation must not duplicate conditions, got %+v", got.Status.Conditions)
	}
}

func TestReconcileAppliesSpecDriftToExistingWorkload(t *testing.T) {
	in := instance("drift", nil)
	r, c := reconcilerFor(t, in)
	key := types.NamespacedName{Namespace: in.Namespace, Name: in.Name}
	ctx := context.Background()

	reconcile(t, r, key)

	// Administrator suspends a running Instance: the existing StatefulSet
	// must scale to zero on the next reconcile, not only fresh creates.
	var got typeclawv1alpha1.TypeClawInstance
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatalf("instance read-back: %v", err)
	}
	got.Spec.Suspend = true
	if err := c.Update(ctx, &got); err != nil {
		t.Fatalf("suspending instance: %v", err)
	}
	reconcile(t, r, key)

	var sts appsv1.StatefulSet
	if err := c.Get(ctx, key, &sts); err != nil {
		t.Fatalf("StatefulSet read-back: %v", err)
	}
	if *sts.Spec.Replicas != 0 {
		t.Fatalf("spec change must reach the existing workload, replicas=%d", *sts.Spec.Replicas)
	}
}

func TestSuspendScalesToZeroAndKeepsStorage(t *testing.T) {
	in := instance("paused", func(in *typeclawv1alpha1.TypeClawInstance) { in.Spec.Suspend = true })
	r, c := reconcilerFor(t, in)
	key := types.NamespacedName{Namespace: in.Namespace, Name: in.Name}

	reconcile(t, r, key)

	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), key, &sts); err != nil {
		t.Fatalf("StatefulSet missing while suspended: %v", err)
	}
	if *sts.Spec.Replicas != 0 {
		t.Errorf("suspend must scale to zero, got %d", *sts.Spec.Replicas)
	}
	if len(sts.Spec.VolumeClaimTemplates) == 0 {
		t.Errorf("suspend must keep Agent Folder claim templates")
	}
	var got typeclawv1alpha1.TypeClawInstance
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("instance read-back: %v", err)
	}
	rc := condition(&got.Status, ConditionRuntimeReady)
	if rc == nil || rc.Status != metav1.ConditionFalse || rc.Reason != reasonInstanceSuspended {
		t.Fatalf("RuntimeReady must report Suspended=False while scaled to zero, got %+v", rc)
	}
}

func TestExplicitExposureFalseRemovesService(t *testing.T) {
	exposed := false
	in := instance("quiet", func(in *typeclawv1alpha1.TypeClawInstance) { in.Spec.ExposeTUI = &exposed })
	r, c := reconcilerFor(t, in)
	key := types.NamespacedName{Namespace: in.Namespace, Name: in.Name}

	// Pre-create the Service as if an earlier generation exposed it.
	if err := c.Create(context.Background(), func() client.Object {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Namespace}}
		return svc
	}()); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	reconcile(t, r, key)

	err := c.Get(context.Background(), key, &corev1.Service{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("explicit exposeTUI=false must remove the Service, got err=%v", err)
	}
}

func TestRuntimeReadyTracksStatefulSetReadiness(t *testing.T) {
	in := instance("kakao-agent", nil)
	r, c := reconcilerFor(t, in)
	key := types.NamespacedName{Namespace: in.Namespace, Name: in.Name}

	reconcile(t, r, key)

	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), key, &sts); err != nil {
		t.Fatalf("StatefulSet read-back: %v", err)
	}
	sts.Status.ReadyReplicas = 1
	if err := c.Status().Update(context.Background(), &sts); err != nil {
		t.Fatalf("simulating ready workload: %v", err)
	}

	reconcile(t, r, key)

	var got typeclawv1alpha1.TypeClawInstance
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("instance read-back: %v", err)
	}
	rc := condition(&got.Status, ConditionRuntimeReady)
	if rc == nil || rc.Status != metav1.ConditionTrue || rc.Reason != reasonRuntimeAvailable {
		t.Fatalf("RuntimeReady must follow StatefulSet readiness, got %+v", rc)
	}
}
