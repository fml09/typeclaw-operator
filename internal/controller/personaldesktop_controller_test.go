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
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/desktop"
	"github.com/fml09/typeclaw-operator/internal/resources"
)

// pdScheme registers the KubeVirt and CDI kinds as unstructured so the fake
// client can store them; the operator never links kubevirt.io types because
// those CRDs are optional in a cluster.
func pdScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	if err := typeclawv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("typeclaw scheme: %v", err)
	}
	s.AddKnownTypeWithName(desktop.VirtualMachineGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(desktop.DataVolumeGVK, &unstructured.Unstructured{})
	return s
}

// pdMapper reports a cluster that serves the virtualization CRDs. An empty
// mapper is the "KubeVirt is not installed" case the controller must survive.
func pdMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(desktop.VirtualMachineGVK, meta.RESTScopeNamespace)
	mapper.Add(desktop.DataVolumeGVK, meta.RESTScopeNamespace)
	return mapper
}

func pdInstance(mutate func(*typeclawv1alpha1.TypeClawInstance)) *typeclawv1alpha1.TypeClawInstance {
	in := &typeclawv1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "kakao-agent", Namespace: "agents", UID: "instance-uid"},
		Spec: typeclawv1alpha1.TypeClawInstanceSpec{
			Runtime: typeclawv1alpha1.RuntimeSpec{Version: typeclawv1alpha1.PersonalDesktopMinimumRuntimeVersion},
			PersonalDesktop: &typeclawv1alpha1.PersonalDesktopSpec{
				Enabled: true,
				Owner:   typeclawv1alpha1.PersonalDesktopOwnerSpec{Subject: "alice@example.com"},
				Image:   typeclawv1alpha1.PersonalDesktopImageSpec{GoldenDataVolume: "ubuntu-golden"},
			},
		},
	}
	if mutate != nil {
		mutate(in)
	}
	return in
}

func pdReconcilerFor(t *testing.T, mapper meta.RESTMapper, objs ...client.Object) (*PersonalDesktopReconciler, client.Client) {
	t.Helper()
	builder := fake.NewClientBuilder().
		WithScheme(pdScheme(t)).
		WithStatusSubresource(&typeclawv1alpha1.TypeClawInstance{}).
		WithObjects(objs...)
	if mapper != nil {
		builder = builder.WithRESTMapper(mapper)
	}
	c := builder.Build()
	return &PersonalDesktopReconciler{
		Client:        c,
		Scheme:        c.Scheme(),
		OperatorImage: "ghcr.io/fml09/typeclaw-operator:test",
	}, c
}

func pdReconcile(t *testing.T, r *PersonalDesktopReconciler, key types.NamespacedName) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	return result
}

func pdKey(in *typeclawv1alpha1.TypeClawInstance) types.NamespacedName {
	return types.NamespacedName{Name: in.Name, Namespace: in.Namespace}
}

func pdGet(t *testing.T, c client.Client, obj client.Object, namespace, name string) {
	t.Helper()
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		t.Fatalf("Get %T %s/%s: %v", obj, namespace, name, err)
	}
}

func pdAbsent(t *testing.T, c client.Client, obj client.Object, namespace, name string) {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, obj)
	if err == nil {
		t.Fatalf("%T %s/%s still exists", obj, namespace, name)
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Get %T %s/%s: %v", obj, namespace, name, err)
	}
}

func pdInstanceAfter(t *testing.T, c client.Client, key types.NamespacedName) *typeclawv1alpha1.TypeClawInstance {
	t.Helper()
	var in typeclawv1alpha1.TypeClawInstance
	if err := c.Get(context.Background(), key, &in); err != nil {
		t.Fatalf("Get instance: %v", err)
	}
	return &in
}

func pdCondition(t *testing.T, in *typeclawv1alpha1.TypeClawInstance) metav1.Condition {
	t.Helper()
	for _, cond := range in.Status.Conditions {
		if cond.Type == typeclawv1alpha1.ConditionPersonalDesktopReady {
			return cond
		}
	}
	t.Fatalf("PersonalDesktopReady condition is missing from %+v", in.Status.Conditions)
	return metav1.Condition{}
}

func pdVM(t *testing.T, c client.Client, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	vm := desktop.NewObject(desktop.VirtualMachineGVK, name, namespace)
	pdGet(t, c, vm, namespace, name)
	return vm
}

func TestPersonalDesktopEnabledRendersTheWholeDesktop(t *testing.T) {
	in := pdInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Image.Import = &typeclawv1alpha1.PersonalDesktopImageImportSpec{
			URL: "https://example.test/ubuntu.img", Checksum: "sha256:beef",
		}
		in.Spec.PersonalDesktop.Access = &typeclawv1alpha1.PersonalDesktopAccessSpec{
			Tailscale: &typeclawv1alpha1.PersonalDesktopTailscaleAccessSpec{Hostname: "kakao-desktop"},
		}
	})
	r, c := pdReconcilerFor(t, pdMapper(), in)
	result := pdReconcile(t, r, pdKey(in))

	if result.RequeueAfter != desktopRequeue {
		t.Fatalf("requeue = %v, want the enabled cadence %v", result.RequeueAfter, desktopRequeue)
	}

	pdGet(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-tokens")
	pdGet(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-cloudinit")
	pdGet(t, c, &corev1.ConfigMap{}, "agents", "kakao-agent-desktop-extension")
	pdGet(t, c, &corev1.Service{}, "agents", "kakao-agent-desktop-agent")
	pdGet(t, c, &corev1.Service{}, "agents", "kakao-agent-desktop-gateway")
	pdGet(t, c, &corev1.ServiceAccount{}, "agents", "kakao-agent-desktop-gateway")
	pdGet(t, c, &rbacv1.Role{}, "agents", "kakao-agent-desktop-gateway")
	pdGet(t, c, &rbacv1.RoleBinding{}, "agents", "kakao-agent-desktop-gateway")
	pdGet(t, c, &appsv1.Deployment{}, "agents", "kakao-agent-desktop-gateway")
	pdGet(t, c, &networkingv1.NetworkPolicy{}, "agents", "kakao-agent-desktop-gateway")
	pdGet(t, c, &networkingv1.Ingress{}, "agents", "kakao-agent-desktop-console")
	pdGet(t, c, desktop.NewObject(desktop.DataVolumeGVK, "ubuntu-golden", "agents"), "agents", "ubuntu-golden")
	pdGet(t, c, desktop.NewObject(desktop.DataVolumeGVK, "kakao-agent-desktop-root", "agents"),
		"agents", "kakao-agent-desktop-root")

	vm := pdVM(t, c, "agents", "kakao-agent-desktop")
	if got, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy"); got != "Manual" {
		t.Fatalf("VM runStrategy = %q", got)
	}
	// Inside the Instance namespace the desktop is garbage-collectable, so it
	// carries a controller reference like every other owned object.
	if owner := metav1.GetControllerOf(vm); owner == nil || owner.Name != "kakao-agent" {
		t.Fatalf("same-namespace VM owner = %+v", owner)
	}

	after := pdInstanceAfter(t, c, pdKey(in))
	if !controllerutilContains(after.Finalizers, desktop.Finalizer) {
		t.Fatalf("finalizer missing: %v", after.Finalizers)
	}
	if after.Status.PersonalDesktop == nil ||
		after.Status.PersonalDesktop.Phase != typeclawv1alpha1.PersonalDesktopPhaseProvisioning {
		t.Fatalf("status = %+v, want Provisioning before the gateway is ready", after.Status.PersonalDesktop)
	}
	if cond := pdCondition(t, after); cond.Status != metav1.ConditionFalse ||
		cond.Reason != typeclawv1alpha1.PersonalDesktopReasonProvisioning {
		t.Fatalf("condition = %+v", cond)
	}
}

func controllerutilContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestPersonalDesktopReportsReadyWhenGatewayServesAndDiskIsPopulated(t *testing.T) {
	in := pdInstance(nil)
	r, c := pdReconcilerFor(t, pdMapper(), in)
	pdReconcile(t, r, pdKey(in))

	var deployment appsv1.Deployment
	pdGet(t, c, &deployment, "agents", "kakao-agent-desktop-gateway")
	deployment.Status.ReadyReplicas = 1
	if err := c.Status().Update(context.Background(), &deployment); err != nil {
		t.Fatalf("Update gateway Deployment status: %v", err)
	}

	root := desktop.NewObject(desktop.DataVolumeGVK, "kakao-agent-desktop-root", "agents")
	pdGet(t, c, root, "agents", "kakao-agent-desktop-root")
	if err := unstructured.SetNestedField(root.Object, "Succeeded", "status", "phase"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}
	if err := c.Update(context.Background(), root); err != nil {
		t.Fatalf("Update root DataVolume: %v", err)
	}

	pdReconcile(t, r, pdKey(in))

	after := pdInstanceAfter(t, c, pdKey(in))
	if after.Status.PersonalDesktop.Phase != typeclawv1alpha1.PersonalDesktopPhaseReady {
		t.Fatalf("phase = %q, want Ready", after.Status.PersonalDesktop.Phase)
	}
	if !after.Status.PersonalDesktop.GatewayReady ||
		after.Status.PersonalDesktop.RootVolumePhase != "Succeeded" {
		t.Fatalf("observed status = %+v", after.Status.PersonalDesktop)
	}
	if cond := pdCondition(t, after); cond.Status != metav1.ConditionTrue ||
		cond.Reason != typeclawv1alpha1.PersonalDesktopReasonReady {
		t.Fatalf("condition = %+v", cond)
	}
}

func TestPersonalDesktopDisableKeepsTheDiskAndTokens(t *testing.T) {
	in := pdInstance(nil)
	r, c := pdReconcilerFor(t, pdMapper(), in)
	pdReconcile(t, r, pdKey(in))

	var tokens corev1.Secret
	pdGet(t, c, &tokens, "agents", "kakao-agent-desktop-tokens")
	agentToken := string(tokens.Data[desktop.TokenKeyAgent])
	guestToken := string(tokens.Data[desktop.TokenKeyGuest])
	if agentToken == "" || guestToken == "" {
		t.Fatalf("tokens were not generated: %v", tokens.Data)
	}

	live := pdInstanceAfter(t, c, pdKey(in))
	live.Spec.PersonalDesktop.Enabled = false
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatalf("Update instance: %v", err)
	}
	pdReconcile(t, r, pdKey(in))

	// Everything the desktop runs on is gone.
	pdAbsent(t, c, desktop.NewObject(desktop.VirtualMachineGVK, "kakao-agent-desktop", "agents"),
		"agents", "kakao-agent-desktop")
	pdAbsent(t, c, &appsv1.Deployment{}, "agents", "kakao-agent-desktop-gateway")
	pdAbsent(t, c, &corev1.Service{}, "agents", "kakao-agent-desktop-gateway")
	pdAbsent(t, c, &corev1.Service{}, "agents", "kakao-agent-desktop-agent")
	pdAbsent(t, c, &networkingv1.NetworkPolicy{}, "agents", "kakao-agent-desktop-gateway")
	pdAbsent(t, c, &corev1.ConfigMap{}, "agents", "kakao-agent-desktop-extension")
	pdAbsent(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-cloudinit")

	// The owner's disk and the credentials the guest already wrote to it are
	// not: that asymmetry is what makes disabling reversible.
	pdGet(t, c, desktop.NewObject(desktop.DataVolumeGVK, "kakao-agent-desktop-root", "agents"),
		"agents", "kakao-agent-desktop-root")
	pdGet(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-tokens")

	after := pdInstanceAfter(t, c, pdKey(in))
	if controllerutilContains(after.Finalizers, desktop.Finalizer) {
		t.Fatalf("a disabled desktop must not hold the Instance: %v", after.Finalizers)
	}
	if after.Status.PersonalDesktop.Phase != typeclawv1alpha1.PersonalDesktopPhaseDisabled {
		t.Fatalf("phase = %q, want Disabled", after.Status.PersonalDesktop.Phase)
	}
	if cond := pdCondition(t, after); cond.Reason != typeclawv1alpha1.PersonalDesktopReasonDisabled {
		t.Fatalf("condition = %+v", cond)
	}

	// Re-enabling resumes the same disk with the same credentials.
	live = pdInstanceAfter(t, c, pdKey(in))
	live.Spec.PersonalDesktop.Enabled = true
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatalf("Update instance: %v", err)
	}
	pdReconcile(t, r, pdKey(in))

	var resumed corev1.Secret
	pdGet(t, c, &resumed, "agents", "kakao-agent-desktop-tokens")
	if string(resumed.Data[desktop.TokenKeyAgent]) != agentToken ||
		string(resumed.Data[desktop.TokenKeyGuest]) != guestToken {
		t.Fatalf("tokens rotated across a disable/enable cycle")
	}
	pdVM(t, c, "agents", "kakao-agent-desktop")
}

func TestPersonalDesktopCrossNamespaceUsesTheFinalizerNotOwnerReferences(t *testing.T) {
	in := pdInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
	})
	r, c := pdReconcilerFor(t, pdMapper(), in)
	pdReconcile(t, r, pdKey(in))

	// An owner reference resolves in the dependent's own namespace, so one
	// pointing at another namespace would make the garbage collector delete
	// the desktop immediately.
	for _, tc := range []struct {
		obj  client.Object
		name string
	}{
		{desktop.NewObject(desktop.VirtualMachineGVK, "kakao-agent-desktop", "typeclaw-desktops"), "kakao-agent-desktop"},
		{&appsv1.Deployment{}, "kakao-agent-desktop-gateway"},
		{&corev1.Secret{}, "kakao-agent-desktop-tokens"},
	} {
		pdGet(t, c, tc.obj, "typeclaw-desktops", tc.name)
		if owner := metav1.GetControllerOf(tc.obj); owner != nil {
			t.Fatalf("%s carries a cross-namespace owner reference %+v", tc.name, owner)
		}
		if tc.obj.GetLabels()[desktop.LabelInstanceUID] != "instance-uid" {
			t.Fatalf("%s is not selectable for cleanup: %v", tc.name, tc.obj.GetLabels())
		}
	}

	// The plugin bearer is mirrored beside the Instance because a Pod can
	// only project a Secret from its own namespace.
	var mirror corev1.Secret
	pdGet(t, c, &mirror, "agents", "kakao-agent-desktop-tokens")
	if len(mirror.Data) != 1 || len(mirror.Data[desktop.TokenKeyAgent]) == 0 {
		t.Fatalf("mirror data = %v, want the agent bearer alone", mirror.Data)
	}

	live := pdInstanceAfter(t, c, pdKey(in))
	if err := c.Delete(context.Background(), live); err != nil {
		t.Fatalf("Delete instance: %v", err)
	}
	pdReconcile(t, r, pdKey(in))

	pdAbsent(t, c, desktop.NewObject(desktop.VirtualMachineGVK, "kakao-agent-desktop", "typeclaw-desktops"),
		"typeclaw-desktops", "kakao-agent-desktop")
	pdAbsent(t, c, &appsv1.Deployment{}, "typeclaw-desktops", "kakao-agent-desktop-gateway")
	pdAbsent(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-tokens")
	pdAbsent(t, c, &corev1.ConfigMap{}, "agents", "kakao-agent-desktop-extension")
	// Retain is the default, so the owner's disk and its credentials survive.
	pdGet(t, c, desktop.NewObject(desktop.DataVolumeGVK, "kakao-agent-desktop-root", "typeclaw-desktops"),
		"typeclaw-desktops", "kakao-agent-desktop-root")
	pdGet(t, c, &corev1.Secret{}, "typeclaw-desktops", "kakao-agent-desktop-tokens")

	var gone typeclawv1alpha1.TypeClawInstance
	err := c.Get(context.Background(), pdKey(in), &gone)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the finalizer was not released: %v", err)
	}
}

func TestPersonalDesktopDeletePolicyRemovesTheDisk(t *testing.T) {
	in := pdInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
		in.Spec.PersonalDesktop.RootVolume.OnInstanceDeletion = "Delete"
		in.Spec.PersonalDesktop.Image.Import = &typeclawv1alpha1.PersonalDesktopImageImportSpec{
			URL: "https://example.test/ubuntu.img", Checksum: "sha256:beef",
		}
	})
	r, c := pdReconcilerFor(t, pdMapper(), in)
	pdReconcile(t, r, pdKey(in))

	live := pdInstanceAfter(t, c, pdKey(in))
	if err := c.Delete(context.Background(), live); err != nil {
		t.Fatalf("Delete instance: %v", err)
	}
	pdReconcile(t, r, pdKey(in))

	pdAbsent(t, c, desktop.NewObject(desktop.DataVolumeGVK, "kakao-agent-desktop-root", "typeclaw-desktops"),
		"typeclaw-desktops", "kakao-agent-desktop-root")
	pdAbsent(t, c, &corev1.Secret{}, "typeclaw-desktops", "kakao-agent-desktop-tokens")
	// The golden image is shared by every desktop cloned from it and is never
	// the operator's to delete.
	pdGet(t, c, desktop.NewObject(desktop.DataVolumeGVK, "ubuntu-golden", "typeclaw-desktops"),
		"typeclaw-desktops", "ubuntu-golden")
}

func TestPersonalDesktopKeepsTheDisksAndTokensUnownedInTheInstanceNamespace(t *testing.T) {
	in := pdInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Image.Import = &typeclawv1alpha1.PersonalDesktopImageImportSpec{
			URL: "https://example.test/ubuntu.img", Checksum: "sha256:beef",
		}
	})
	r, c := pdReconcilerFor(t, pdMapper(), in)
	pdReconcile(t, r, pdKey(in))

	// Garbage collection removes every dependent of a deleted owner before any
	// cleanup path is consulted. A controller reference here would make
	// onInstanceDeletion=Retain — the default — destroy the owner's root disk,
	// and would take the shared golden image down with the first Instance in
	// the namespace that is deleted.
	for _, tc := range []struct {
		obj  client.Object
		name string
	}{
		{desktop.NewObject(desktop.DataVolumeGVK, "kakao-agent-desktop-root", "agents"), "kakao-agent-desktop-root"},
		{desktop.NewObject(desktop.DataVolumeGVK, "ubuntu-golden", "agents"), "ubuntu-golden"},
		{&corev1.Secret{}, "kakao-agent-desktop-tokens"},
	} {
		pdGet(t, c, tc.obj, "agents", tc.name)
		if owner := metav1.GetControllerOf(tc.obj); owner != nil {
			t.Fatalf("%s is garbage collected with the Instance: %+v", tc.name, owner)
		}
		if tc.obj.GetLabels()[desktop.LabelInstanceUID] != "instance-uid" {
			t.Fatalf("%s is not selectable for cleanup: %v", tc.name, tc.obj.GetLabels())
		}
	}

	// Everything the desktop merely runs on stays owned: those objects are
	// rebuilt from the spec, so garbage collection is the cheaper cleanup.
	var deployment appsv1.Deployment
	pdGet(t, c, &deployment, "agents", "kakao-agent-desktop-gateway")
	if owner := metav1.GetControllerOf(&deployment); owner == nil || owner.Name != "kakao-agent" {
		t.Fatalf("gateway Deployment owner = %+v", owner)
	}
}

func TestPersonalDesktopDeletePolicyRemovesTheDiskInTheInstanceNamespace(t *testing.T) {
	in := pdInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.RootVolume.OnInstanceDeletion = "Delete"
		in.Spec.PersonalDesktop.Image.Import = &typeclawv1alpha1.PersonalDesktopImageImportSpec{
			URL: "https://example.test/ubuntu.img", Checksum: "sha256:beef",
		}
	})
	r, c := pdReconcilerFor(t, pdMapper(), in)
	pdReconcile(t, r, pdKey(in))

	live := pdInstanceAfter(t, c, pdKey(in))
	if err := c.Delete(context.Background(), live); err != nil {
		t.Fatalf("Delete instance: %v", err)
	}
	pdReconcile(t, r, pdKey(in))

	// Nothing is owned here any more, so the finalizer path — not garbage
	// collection — is what has to honour the declared policy.
	pdAbsent(t, c, desktop.NewObject(desktop.DataVolumeGVK, "kakao-agent-desktop-root", "agents"),
		"agents", "kakao-agent-desktop-root")
	pdAbsent(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-tokens")
	pdGet(t, c, desktop.NewObject(desktop.DataVolumeGVK, "ubuntu-golden", "agents"), "agents", "ubuntu-golden")
}

func TestPersonalDesktopPublishesTheDeletingPhaseWhileTearingDown(t *testing.T) {
	in := pdInstance(nil)
	r, c := pdReconcilerFor(t, pdMapper(), in)
	pdReconcile(t, r, pdKey(in))

	// A second finalizer keeps the Instance readable after our own is
	// released, which is the only way to observe the teardown window that a
	// slow PVC would otherwise hold open in a real cluster.
	live := pdInstanceAfter(t, c, pdKey(in))
	live.Finalizers = append(live.Finalizers, "test.typeclaw.fml09.io/hold")
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatalf("Update instance: %v", err)
	}
	live = pdInstanceAfter(t, c, pdKey(in))
	if err := c.Delete(context.Background(), live); err != nil {
		t.Fatalf("Delete instance: %v", err)
	}
	pdReconcile(t, r, pdKey(in))

	after := pdInstanceAfter(t, c, pdKey(in))
	if after.Status.PersonalDesktop == nil ||
		after.Status.PersonalDesktop.Phase != typeclawv1alpha1.PersonalDesktopPhaseDeleting {
		t.Fatalf("status = %+v, want the Deleting phase", after.Status.PersonalDesktop)
	}
	if controllerutilContains(after.Finalizers, desktop.Finalizer) {
		t.Fatalf("the desktop finalizer was not released: %v", after.Finalizers)
	}
}

func pdEnvNamed(container corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, env := range container.Env {
		if env.Name == name {
			return env, true
		}
	}
	return corev1.EnvVar{}, false
}

// TestPersonalDesktopProjectedObjectsExistWheneverTheRuntimeMountsThem joins
// the two halves of the feature. resources.StatefulSet decides what the
// runtime Pod references and this controller decides what exists; a Pod whose
// secretKeyRef or ConfigMap volume names a missing object never starts, so a
// desktop the controller refuses to provision must not be projected either.
func TestPersonalDesktopProjectedObjectsExistWheneverTheRuntimeMountsThem(t *testing.T) {
	cases := map[string]func(*typeclawv1alpha1.TypeClawInstance){
		"valid desktop": func(in *typeclawv1alpha1.TypeClawInstance) {},
		"access declared without a hostname": func(in *typeclawv1alpha1.TypeClawInstance) {
			in.Spec.PersonalDesktop.Access = &typeclawv1alpha1.PersonalDesktopAccessSpec{}
		},
		"windows desktop with an image import": func(in *typeclawv1alpha1.TypeClawInstance) {
			in.Spec.PersonalDesktop.OS = desktop.OSWindows
			in.Spec.PersonalDesktop.Image.Import = &typeclawv1alpha1.PersonalDesktopImageImportSpec{
				URL: "https://example.test/windows.img", Checksum: "sha256:beef",
			}
		},
		"runtime predates platform extensions": func(in *typeclawv1alpha1.TypeClawInstance) {
			in.Spec.Runtime = typeclawv1alpha1.RuntimeSpec{Version: "0.48.9"}
		},
	}
	projectedAtLeastOnce := false
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := pdInstance(mutate)
			r, c := pdReconcilerFor(t, pdMapper(), in)
			pdReconcile(t, r, pdKey(in))

			sts, err := resources.StatefulSet(pdInstanceAfter(t, c, pdKey(in)))
			if err != nil {
				t.Fatalf("StatefulSet() error: %v", err)
			}
			container := sts.Spec.Template.Spec.Containers[0]
			if token, found := pdEnvNamed(container, "PERSONAL_DESKTOP_AGENT_TOKEN"); found {
				projectedAtLeastOnce = true
				pdGet(t, c, &corev1.Secret{}, "agents", token.ValueFrom.SecretKeyRef.Name)
			}
			for _, volume := range sts.Spec.Template.Spec.Volumes {
				if volume.Name != desktop.ExtensionVolumeName {
					continue
				}
				projectedAtLeastOnce = true
				pdGet(t, c, &corev1.ConfigMap{}, "agents", volume.ConfigMap.Name)
			}
		})
	}
	if !projectedAtLeastOnce {
		t.Fatalf("no case projected the desktop, so the pairing was never exercised")
	}
}

func TestPersonalDesktopAdoptsAnExistingRootDataVolume(t *testing.T) {
	adopted := desktop.NewObject(desktop.DataVolumeGVK, "poc-desktop-root", "agents")
	desktop.SetSpec(adopted, map[string]any{"source": map[string]any{"blank": map[string]any{}}})

	in := pdInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.RootVolume.ExistingDataVolume = "poc-desktop-root"
	})
	r, c := pdReconcilerFor(t, pdMapper(), in, adopted)
	pdReconcile(t, r, pdKey(in))

	// The adopted disk is neither re-rendered nor relabelled: it was not
	// created here and the migration of a PoC desktop must be non-destructive.
	live := desktop.NewObject(desktop.DataVolumeGVK, "poc-desktop-root", "agents")
	pdGet(t, c, live, "agents", "poc-desktop-root")
	if _, found, _ := unstructured.NestedMap(live.Object, "spec", "source", "blank"); !found {
		t.Fatalf("the adopted DataVolume was rewritten: %v", live.Object)
	}
	pdAbsent(t, c, desktop.NewObject(desktop.DataVolumeGVK, "kakao-agent-desktop-root", "agents"),
		"agents", "kakao-agent-desktop-root")

	vm := pdVM(t, c, "agents", "kakao-agent-desktop")
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	root := volumes[0].(map[string]any)
	if root["dataVolume"].(map[string]any)["name"] != "poc-desktop-root" {
		t.Fatalf("the VM does not boot from the adopted disk: %v", root)
	}
}

func TestPersonalDesktopLeavesAPopulatedRootDataVolumeAlone(t *testing.T) {
	existing := desktop.NewObject(desktop.DataVolumeGVK, "kakao-agent-desktop-root", "agents")
	desktop.SetSpec(existing, map[string]any{"source": map[string]any{"blank": map[string]any{}}})
	if err := unstructured.SetNestedField(existing.Object, "Succeeded", "status", "phase"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}

	in := pdInstance(nil)
	r, c := pdReconcilerFor(t, pdMapper(), in, existing)
	pdReconcile(t, r, pdKey(in))

	live := desktop.NewObject(desktop.DataVolumeGVK, "kakao-agent-desktop-root", "agents")
	pdGet(t, c, live, "agents", "kakao-agent-desktop-root")
	// CDI ignores spec changes after a clone finished; re-applying would only
	// make the object disagree with the disk it describes.
	if _, found, _ := unstructured.NestedMap(live.Object, "spec", "source", "blank"); !found {
		t.Fatalf("a populated disk was rewritten: %v", live.Object)
	}
	after := pdInstanceAfter(t, c, pdKey(in))
	if after.Status.PersonalDesktop.RootVolumePhase != "Succeeded" {
		t.Fatalf("observed root phase = %q", after.Status.PersonalDesktop.RootVolumePhase)
	}
}

func TestPersonalDesktopRuntimeVersionGateProvisionsNothing(t *testing.T) {
	in := pdInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Runtime = typeclawv1alpha1.RuntimeSpec{Version: "0.48.9"}
	})
	r, c := pdReconcilerFor(t, pdMapper(), in)
	pdReconcile(t, r, pdKey(in))

	pdAbsent(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-tokens")
	pdAbsent(t, c, &corev1.ConfigMap{}, "agents", "kakao-agent-desktop-extension")
	pdAbsent(t, c, desktop.NewObject(desktop.VirtualMachineGVK, "kakao-agent-desktop", "agents"),
		"agents", "kakao-agent-desktop")

	after := pdInstanceAfter(t, c, pdKey(in))
	if cond := pdCondition(t, after); cond.Reason != typeclawv1alpha1.PersonalDesktopReasonRuntimeTooOld {
		t.Fatalf("condition = %+v", cond)
	}
	if after.Status.PersonalDesktop.Phase != typeclawv1alpha1.PersonalDesktopPhasePending {
		t.Fatalf("phase = %q, want Pending", after.Status.PersonalDesktop.Phase)
	}
}

func TestPersonalDesktopWithoutKubeVirtStaysPendingAndKeepsTheRuntimeMountable(t *testing.T) {
	in := pdInstance(nil)
	// No RESTMapper entries: the cluster has neither KubeVirt nor CDI.
	r, c := pdReconcilerFor(t, nil, in)
	result := pdReconcile(t, r, pdKey(in))

	if result.RequeueAfter != desktopRequeue {
		t.Fatalf("requeue = %v, want a bounded retry rather than a hot loop", result.RequeueAfter)
	}
	pdAbsent(t, c, desktop.NewObject(desktop.VirtualMachineGVK, "kakao-agent-desktop", "agents"),
		"agents", "kakao-agent-desktop")
	pdAbsent(t, c, &appsv1.Deployment{}, "agents", "kakao-agent-desktop-gateway")
	// The runtime StatefulSet projects these two the moment the desktop is
	// enabled, and a Pod whose ConfigMap or secretKeyRef is missing never
	// starts — so they exist even on a cluster that cannot host a desktop.
	pdGet(t, c, &corev1.ConfigMap{}, "agents", "kakao-agent-desktop-extension")
	pdGet(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-tokens")

	after := pdInstanceAfter(t, c, pdKey(in))
	if cond := pdCondition(t, after); cond.Reason != typeclawv1alpha1.PersonalDesktopReasonKubeVirtUnavailable {
		t.Fatalf("condition = %+v", cond)
	}
}

func TestPersonalDesktopInvalidSpecIsReported(t *testing.T) {
	in := pdInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Owner.Subject = ""
	})
	r, c := pdReconcilerFor(t, pdMapper(), in)
	pdReconcile(t, r, pdKey(in))

	after := pdInstanceAfter(t, c, pdKey(in))
	cond := pdCondition(t, after)
	if cond.Reason != typeclawv1alpha1.PersonalDesktopReasonError {
		t.Fatalf("condition = %+v", cond)
	}
	if after.Status.PersonalDesktop.Phase != typeclawv1alpha1.PersonalDesktopPhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", after.Status.PersonalDesktop.Phase)
	}
	pdAbsent(t, c, desktop.NewObject(desktop.VirtualMachineGVK, "kakao-agent-desktop", "agents"),
		"agents", "kakao-agent-desktop")
}

func TestPersonalDesktopStatusFailuresOnlyAbsorbConflicts(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "typeclaw.fml09.io", Resource: "typeclawinstances"},
		"kakao-agent",
		errors.New("status patch is not granted"),
	)
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: "typeclaw.fml09.io", Resource: "typeclawinstances"},
		"kakao-agent",
		errors.New("the object has been modified"),
	)

	for name, tc := range map[string]struct {
		patchErr error
		wantErr  bool
	}{
		// A conflict is our own doing under rapid requeues, and a fresh
		// reconcile re-reads and re-applies.
		"conflict is retried quietly": {patchErr: conflict, wantErr: false},
		// Anything else would otherwise loop invisibly: no error means no
		// rate limiting, so the controller would re-render every desktop
		// object forever while the Instance shows no condition at all.
		"forbidden reaches the workqueue": {patchErr: forbidden, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			in := pdInstance(nil)
			c := fake.NewClientBuilder().
				WithScheme(pdScheme(t)).
				WithStatusSubresource(&typeclawv1alpha1.TypeClawInstance{}).
				WithObjects(in).
				WithRESTMapper(pdMapper()).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(
						_ context.Context,
						_ client.Client,
						_ string,
						_ client.Object,
						_ client.Patch,
						_ ...client.SubResourcePatchOption,
					) error {
						return tc.patchErr
					},
				}).
				Build()
			r := &PersonalDesktopReconciler{Client: c, Scheme: c.Scheme(), OperatorImage: "ghcr.io/fml09/typeclaw-operator:test"}

			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: pdKey(in)})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Reconcile() error = nil, want the status failure surfaced; result %+v", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reconcile() error: %v", err)
			}
			if !result.Requeue {
				t.Fatalf("result = %+v, want a retry after a status conflict", result)
			}
		})
	}
}

func TestPersonalDesktopSwitchingOSReplacesTheGuestPayload(t *testing.T) {
	in := pdInstance(nil)
	r, c := pdReconcilerFor(t, pdMapper(), in)
	pdReconcile(t, r, pdKey(in))
	pdGet(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-cloudinit")

	live := pdInstanceAfter(t, c, pdKey(in))
	live.Spec.PersonalDesktop.OS = desktop.OSWindows
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatalf("Update instance: %v", err)
	}
	pdReconcile(t, r, pdKey(in))

	// A stale cloud-config would keep an obsolete token readable in the
	// desktop namespace forever.
	pdAbsent(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-cloudinit")
	pdGet(t, c, &corev1.Secret{}, "agents", "kakao-agent-desktop-sysprep")
}

func TestPersonalDesktopAbsentBlockIsANoOp(t *testing.T) {
	in := &typeclawv1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "kakao-agent", Namespace: "agents", UID: "instance-uid"},
	}
	r, c := pdReconcilerFor(t, pdMapper(), in)
	result := pdReconcile(t, r, pdKey(in))

	if result.RequeueAfter != 0 {
		t.Fatalf("an Instance with no desktop must not be polled: %+v", result)
	}
	after := pdInstanceAfter(t, c, pdKey(in))
	if cond := pdCondition(t, after); cond.Reason != typeclawv1alpha1.PersonalDesktopReasonDisabled {
		t.Fatalf("condition = %+v", cond)
	}
}
