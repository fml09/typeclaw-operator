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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/desktop"
	"github.com/fml09/typeclaw-operator/internal/resources"
)

// desktopRequeue re-reads the desktop while it is enabled. KubeVirt and CDI
// objects are deliberately not watched — their CRDs may be absent, and a
// controller that watched a missing kind would fail to start — so the observed
// VM state, the CDI phases, and the console URL arrive on this poll instead.
const desktopRequeue = 30 * time.Second

// PersonalDesktopReconciler owns one Personal Desktop per TypeClawInstance:
// the KubeVirt VirtualMachine and its disks, the Desktop Gateway in front of
// it, the console publication, and the computer-use Platform Extension the
// Managed Runtime mounts.
//
// It is the only controller here that cleans up through a finalizer. A desktop
// may live in a namespace of its own — KubeVirt relabels a VM's namespace to
// pod-security enforce=privileged — and an owner reference cannot cross
// namespaces: Kubernetes treats such a reference as an owner that does not
// exist and garbage-collects the object almost immediately. So cross-namespace
// objects carry no controller reference and are removed by name on the
// finalizer path instead.
type PersonalDesktopReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// OperatorImage is the image the Desktop Gateway runs unless the Instance
	// overrides it: the gateway binary ships inside the operator image so the
	// manager and the gateway upgrade together.
	OperatorImage string
}

// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines;virtualmachineinstances,verbs=get;list;watch;create;update;patch;delete
// The manager cannot grant the Desktop Gateway's Role rules it does not hold
// itself; the API server's privilege-escalation guard would reject the Role.
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachineinstances/vnc;virtualmachineinstances/vnc/screenshot,verbs=get
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachines/start;virtualmachines/stop,verbs=update
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the declared Personal Desktop toward the cluster and
// reports what it observed on the Instance's PersonalDesktopReady condition.
func (r *PersonalDesktopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var instance typeclawv1alpha1.TypeClawInstance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !instance.DeletionTimestamp.IsZero() {
		// Only a desktop that was provisioned holds the finalizer, and only
		// that desktop has objects garbage collection cannot reach. The
		// Instance is going away, so its declared retention policy is the last
		// word on the root disk.
		if !controllerutil.ContainsFinalizer(&instance, desktop.Finalizer) {
			return ctrl.Result{}, nil
		}
		// Teardown can wait on a disk whose PVC is still finalizing, so the
		// phase is published before the objects go: otherwise the Instance
		// shows its last Ready observation for the whole removal window. A
		// status write that fails must not strand the Instance behind this
		// finalizer, so it is reported and the removal continues.
		if err := r.publishDesktop(ctx, &instance, deletingDesktop(&instance)); err != nil {
			log.Info("personal desktop deleting status not published", "error", err)
		}
		if err := r.desktopCleanup(ctx, &instance, desktopRetainsDisk(&instance)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.dropDesktopFinalizer(ctx, &instance)
	}

	outcome, err := r.convergeDesktop(ctx, &instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.publishDesktop(ctx, &instance, outcome); err != nil {
		// A conflict with our own resource writes is expected under rapid
		// requeues and is absorbed because a fresh reconcile re-reads and
		// re-applies. Anything else — a missing status permission, a
		// rejecting webhook — is returned instead, so the workqueue backs off
		// and the failure surfaces rather than re-rendering every desktop
		// object forever at a log level nobody reads.
		if !apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
		log.V(1).Info("personal desktop status patch conflicted, retrying", "error", err)
		return ctrl.Result{Requeue: true}, nil
	}
	if desktop.Enabled(&instance) {
		return ctrl.Result{RequeueAfter: desktopRequeue}, nil
	}
	return ctrl.Result{}, nil
}

// desktopOutcome is one reconcile's observation, folded into the Instance
// status and the PersonalDesktopReady condition in a single patch.
type desktopOutcome struct {
	status  typeclawv1alpha1.PersonalDesktopStatus
	ready   bool
	reason  string
	message string
}

// convergeDesktop applies the declared desktop, or dismantles it, and reports
// what it observed. Cluster errors become a Degraded observation rather than a
// returned error: the enabled path already requeues every desktopRequeue, and
// an Instance whose desktop cannot be applied must still show why.
func (r *PersonalDesktopReconciler) convergeDesktop(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
) (desktopOutcome, error) {
	names := desktop.Names(instance)

	if !desktop.Enabled(instance) {
		// Disabling is not deletion. The VM, the Gateway, the console and the
		// extension all go, but the root DataVolume and the token Secret stay:
		// re-enabling then resumes the same disk with credentials the guest
		// already wrote to its own filesystem at first boot. Rotating either
		// would strand the guest agent behind a bearer nobody holds.
		if err := r.desktopCleanup(ctx, instance, true); err != nil {
			return desktopOutcome{}, err
		}
		if err := r.dropDesktopFinalizer(ctx, instance); err != nil {
			return desktopOutcome{}, err
		}
		return desktopOutcome{
			status: typeclawv1alpha1.PersonalDesktopStatus{
				Phase: typeclawv1alpha1.PersonalDesktopPhaseDisabled,
			},
			reason:  typeclawv1alpha1.PersonalDesktopReasonDisabled,
			message: "personal desktop is disabled; the root DataVolume and the token Secret are retained",
		}, nil
	}

	if err := desktop.Validate(instance); err != nil {
		return desktopOutcome{
			status: typeclawv1alpha1.PersonalDesktopStatus{
				Phase:       typeclawv1alpha1.PersonalDesktopPhaseDegraded,
				DesktopName: names.Desktop,
				Namespace:   names.Namespace,
				Message:     err.Error(),
			},
			reason:  typeclawv1alpha1.PersonalDesktopReasonError,
			message: err.Error(),
		}, nil
	}

	if !desktop.RuntimeSupportsExtensions(instance) {
		// Nothing is provisioned and nothing existing is removed: the gate is
		// about the runtime that would drive the desktop, and an administrator
		// mid-upgrade must not lose a disk by pinning an old version for an
		// afternoon.
		message := desktop.RuntimeTooOldMessage(instance)
		return desktopOutcome{
			status: typeclawv1alpha1.PersonalDesktopStatus{
				Phase:       typeclawv1alpha1.PersonalDesktopPhasePending,
				DesktopName: names.Desktop,
				Namespace:   names.Namespace,
				Message:     message,
			},
			reason:  typeclawv1alpha1.PersonalDesktopReasonRuntimeTooOld,
			message: message,
		}, nil
	}

	if err := r.ensureDesktopFinalizer(ctx, instance); err != nil {
		return desktopOutcome{}, err
	}

	// The extension ConfigMap and the token Secrets come first and do not
	// depend on the virtualization stack. The runtime StatefulSet projects
	// both as soon as the desktop is enabled, and a Pod whose ConfigMap volume
	// or secretKeyRef is missing never starts — so they must exist even on a
	// cluster where nothing else here can be created.
	tokens, err := r.applyDesktopCredentials(ctx, instance)
	if err != nil {
		return r.degradedDesktop(names, err), nil
	}

	if !r.kubeVirtAvailable() {
		const message = "cluster has no KubeVirt VirtualMachine or CDI DataVolume API; install KubeVirt and CDI"
		return desktopOutcome{
			status: typeclawv1alpha1.PersonalDesktopStatus{
				Phase:       typeclawv1alpha1.PersonalDesktopPhasePending,
				DesktopName: names.Desktop,
				Namespace:   names.Namespace,
				Message:     message,
			},
			reason:  typeclawv1alpha1.PersonalDesktopReasonKubeVirtUnavailable,
			message: message,
		}, nil
	}

	outcome, err := r.applyDesktop(ctx, instance, tokens)
	if err != nil {
		return r.degradedDesktop(names, err), nil
	}
	return outcome, nil
}

// degradedDesktop reports a desktop that could not be applied. The error text
// reaches the condition verbatim so an administrator sees the API server's own
// complaint (a missing namespace, a rejected quantity) instead of a summary.
func (r *PersonalDesktopReconciler) degradedDesktop(names desktop.NameSet, err error) desktopOutcome {
	return desktopOutcome{
		status: typeclawv1alpha1.PersonalDesktopStatus{
			Phase:       typeclawv1alpha1.PersonalDesktopPhaseDegraded,
			DesktopName: names.Desktop,
			Namespace:   names.Namespace,
			Message:     err.Error(),
		},
		reason:  typeclawv1alpha1.PersonalDesktopReasonError,
		message: err.Error(),
	}
}

// deletingDesktop reports a desktop that is being removed with its Instance.
// The condition reason stays inside the set the contract fixes; the message
// carries the retention decision, which is what an administrator watching a
// deletion actually needs to know.
func deletingDesktop(instance *typeclawv1alpha1.TypeClawInstance) desktopOutcome {
	names := desktop.Names(instance)
	message := "personal desktop is being removed with the Instance; the root DataVolume is retained"
	if !desktopRetainsDisk(instance) {
		message = "personal desktop is being removed with the Instance; the root DataVolume is deleted"
	}
	return desktopOutcome{
		status: typeclawv1alpha1.PersonalDesktopStatus{
			Phase:       typeclawv1alpha1.PersonalDesktopPhaseDeleting,
			DesktopName: names.Desktop,
			Namespace:   names.Namespace,
			Message:     message,
		},
		reason:  typeclawv1alpha1.PersonalDesktopReasonDisabled,
		message: message,
	}
}

// applyDesktopCredentials writes the token Secret, its Instance-namespace
// mirror, and the extension ConfigMap, and returns the token set the rest of
// the render needs in plaintext.
func (r *PersonalDesktopReconciler) applyDesktopCredentials(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
) (desktop.TokenSet, error) {
	names := desktop.Names(instance)

	extension := desktop.ExtensionConfigMap(instance)
	if err := r.applyDesktopObject(ctx, instance, extension, func() error {
		extension.Data = desktop.ExtensionConfigMap(instance).Data
		return nil
	}); err != nil {
		return desktop.TokenSet{}, fmt.Errorf("apply extension ConfigMap: %w", err)
	}

	var tokens desktop.TokenSet
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.Tokens, Namespace: names.Namespace}}
	if err := r.applyRetainedDesktopObject(ctx, instance, secret, func() error {
		// CreateOrUpdate hands the mutator the live object, so the tokens
		// already in the cluster are what NewTokenSet preserves; only an
		// absent key is generated.
		set, err := desktop.NewTokenSet(instance, secret)
		if err != nil {
			return err
		}
		tokens = set
		desired := desktop.TokensSecret(instance, set)
		secret.Type = desired.Type
		secret.Data = desired.Data
		return nil
	}); err != nil {
		return desktop.TokenSet{}, fmt.Errorf("apply token Secret: %w", err)
	}

	if desktop.CrossNamespace(instance) {
		// A Pod can only project a Secret from its own namespace, so the
		// plugin bearer — and only that key — is mirrored beside the Instance.
		mirror := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.Tokens, Namespace: names.InstanceNamespace}}
		if err := r.applyDesktopObject(ctx, instance, mirror, func() error {
			desired := desktop.MirroredAgentTokenSecret(instance, tokens.Agent)
			mirror.Type = desired.Type
			mirror.Data = desired.Data
			return nil
		}); err != nil {
			return desktop.TokenSet{}, fmt.Errorf("apply mirrored token Secret: %w", err)
		}
	}
	return tokens, nil
}

// applyDesktop renders and applies everything that depends on KubeVirt or CDI,
// then reports the observed desktop state.
func (r *PersonalDesktopReconciler) applyDesktop(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
	tokens desktop.TokenSet,
) (desktopOutcome, error) {
	names := desktop.Names(instance)
	status := typeclawv1alpha1.PersonalDesktopStatus{
		DesktopName: names.Desktop,
		Namespace:   names.Namespace,
	}

	goldenPhase, err := r.ensureDataVolume(ctx, instance, desktop.GoldenDataVolume(instance), names.GoldenVolume)
	if err != nil {
		return desktopOutcome{}, fmt.Errorf("golden DataVolume: %w", err)
	}
	status.GoldenImagePhase = goldenPhase

	rootPhase, err := r.ensureDataVolume(ctx, instance, desktop.RootDataVolume(instance), names.RootVolume)
	if err != nil {
		return desktopOutcome{}, fmt.Errorf("root DataVolume: %w", err)
	}
	status.RootVolumePhase = rootPhase

	if err := r.applyGuestPayload(ctx, instance, tokens); err != nil {
		return desktopOutcome{}, err
	}

	vm := desktop.NewObject(desktop.VirtualMachineGVK, names.Desktop, names.Namespace)
	if err := r.applyDesktopObject(ctx, instance, vm, func() error {
		desktop.SetSpec(vm, desktop.Spec(desktop.VM(instance)))
		return nil
	}); err != nil {
		return desktopOutcome{}, fmt.Errorf("apply VirtualMachine: %w", err)
	}
	status.VMPrintableStatus = desktop.VMPrintableStatus(vm)

	agentService := desktop.AgentService(instance)
	if err := r.applyDesktopObject(ctx, instance, agentService, func() error {
		desired := desktop.AgentService(instance)
		desired.Spec.ClusterIP = agentService.Spec.ClusterIP
		agentService.Spec = desired.Spec
		return nil
	}); err != nil {
		return desktopOutcome{}, fmt.Errorf("apply agent Service: %w", err)
	}

	consoleURL, err := r.applyConsoleIngress(ctx, instance)
	if err != nil {
		return desktopOutcome{}, err
	}
	status.ConsoleURL = consoleURL

	gatewayReady, err := r.applyGateway(ctx, instance, consoleURL)
	if err != nil {
		return desktopOutcome{}, err
	}
	status.GatewayReady = gatewayReady

	if err := r.applyGatewayNetworkPolicy(ctx, instance, agentService.Spec.ClusterIP); err != nil {
		return desktopOutcome{}, err
	}

	// Ready means an agent can actually drive the desktop: the Gateway is
	// serving and the root disk finished cloning. A VM that is powered off is
	// still Ready — the owner and the agent can start it through the Gateway.
	if gatewayReady && rootPhase == "Succeeded" {
		status.Phase = typeclawv1alpha1.PersonalDesktopPhaseReady
		return desktopOutcome{
			status:  status,
			ready:   true,
			reason:  typeclawv1alpha1.PersonalDesktopReasonReady,
			message: "Desktop Gateway is serving and the root volume is populated",
		}, nil
	}
	status.Phase = typeclawv1alpha1.PersonalDesktopPhaseProvisioning
	return desktopOutcome{
		status:  status,
		reason:  typeclawv1alpha1.PersonalDesktopReasonProvisioning,
		message: fmt.Sprintf("root volume phase %q, gateway ready %t", rootPhase, gatewayReady),
	}, nil
}

// applyGuestPayload writes the first-boot material for the selected guest OS
// and removes the payload the other OS would have used, so switching os= never
// leaves a stale answer file or cloud-config holding an old token.
func (r *PersonalDesktopReconciler) applyGuestPayload(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
	tokens desktop.TokenSet,
) error {
	names := desktop.Names(instance)

	if desktop.OS(instance.Spec.PersonalDesktop) == desktop.OSWindows {
		sysprep, err := desktop.SysprepSecret(instance, tokens.Guest, tokens.WindowsPassword)
		if err != nil {
			return fmt.Errorf("render sysprep Secret: %w", err)
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.Sysprep, Namespace: names.Namespace}}
		if err := r.applyDesktopObject(ctx, instance, secret, func() error {
			secret.Type = sysprep.Type
			secret.Data = sysprep.Data
			return nil
		}); err != nil {
			return fmt.Errorf("apply sysprep Secret: %w", err)
		}
		return r.deleteDesktopObject(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: names.CloudInit, Namespace: names.Namespace},
		})
	}

	cloudInit := desktop.CloudInitSecret(instance, tokens.Guest)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.CloudInit, Namespace: names.Namespace}}
	if err := r.applyDesktopObject(ctx, instance, secret, func() error {
		secret.Type = cloudInit.Type
		secret.Data = cloudInit.Data
		return nil
	}); err != nil {
		return fmt.Errorf("apply cloud-init Secret: %w", err)
	}
	return r.deleteDesktopObject(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: names.Sysprep, Namespace: names.Namespace},
	})
}

// applyConsoleIngress publishes or unpublishes the Desktop Console and returns
// the address the Tailscale operator reported, empty until the device exists.
func (r *PersonalDesktopReconciler) applyConsoleIngress(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
) (string, error) {
	names := desktop.Names(instance)
	desired := desktop.ConsoleIngress(instance)
	if desired == nil {
		// No Ingress is rendered either when the console is unpublished or
		// when tailscaled inside the Gateway Pod publishes it. Only the first
		// of those has no address, so the URL still comes from ConsoleURL.
		if err := r.deleteDesktopObject(ctx, &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: names.ConsoleIngress, Namespace: names.Namespace},
		}); err != nil {
			return "", err
		}
		return desktop.ConsoleURL(instance, nil), nil
	}

	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: names.ConsoleIngress, Namespace: names.Namespace}}
	if err := r.applyDesktopObject(ctx, instance, ingress, func() error {
		fresh := desktop.ConsoleIngress(instance)
		ingress.Annotations = fresh.Annotations
		ingress.Spec = fresh.Spec
		return nil
	}); err != nil {
		return "", fmt.Errorf("apply console Ingress: %w", err)
	}
	return desktop.ConsoleURL(instance, ingress), nil
}

// applyGateway applies the Desktop Gateway's identity, workload and Service,
// and reports whether it is serving.
func (r *PersonalDesktopReconciler) applyGateway(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
	consoleURL string,
) (bool, error) {
	names := desktop.Names(instance)

	serviceAccount := desktop.GatewayServiceAccount(instance)
	if err := r.applyDesktopObject(ctx, instance, serviceAccount, func() error {
		// A ServiceAccount carries no mutable spec of ours.
		return nil
	}); err != nil {
		return false, fmt.Errorf("apply gateway ServiceAccount: %w", err)
	}

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: names.Gateway, Namespace: names.Namespace}}
	if err := r.applyDesktopObject(ctx, instance, role, func() error {
		role.Rules = desktop.GatewayRole(instance).Rules
		return nil
	}); err != nil {
		return false, fmt.Errorf("apply gateway Role: %w", err)
	}

	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: names.Gateway, Namespace: names.Namespace}}
	if err := r.applyDesktopObject(ctx, instance, binding, func() error {
		desired := desktop.GatewayRoleBinding(instance)
		binding.RoleRef = desired.RoleRef
		binding.Subjects = desired.Subjects
		return nil
	}); err != nil {
		return false, fmt.Errorf("apply gateway RoleBinding: %w", err)
	}

	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: names.Gateway, Namespace: names.Namespace}}
	if err := r.applyDesktopObject(ctx, instance, service, func() error {
		desired := desktop.GatewayService(instance)
		// The cluster IP is allocated once and immutable; re-deriving the
		// whole spec would otherwise offer an empty value on every update.
		desired.Spec.ClusterIP = service.Spec.ClusterIP
		service.Spec = desired.Spec
		return nil
	}); err != nil {
		return false, fmt.Errorf("apply gateway Service: %w", err)
	}

	// The Serve config must exist before the Deployment that mounts it, or the
	// Pod sits in ContainerCreating waiting for a ConfigMap nobody wrote.
	if serve := desktop.GatewayServeConfig(instance); serve != nil {
		configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.ServeConfig, Namespace: names.Namespace}}
		if err := r.applyDesktopObject(ctx, instance, configMap, func() error {
			configMap.Data = desktop.GatewayServeConfig(instance).Data
			return nil
		}); err != nil {
			return false, fmt.Errorf("apply gateway Serve config: %w", err)
		}
	} else if err := r.deleteDesktopObject(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: names.ServeConfig, Namespace: names.Namespace},
	}); err != nil {
		return false, fmt.Errorf("remove gateway Serve config: %w", err)
	}

	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: names.Gateway, Namespace: names.Namespace}}
	if err := r.applyDesktopObject(ctx, instance, deployment, func() error {
		desired := desktop.GatewayDeployment(instance, r.gatewayImage(), consoleURL)
		// The selector is immutable; keep the API server's copy so an update
		// never fights admission.
		if deployment.Spec.Selector != nil {
			desired.Spec.Selector = deployment.Spec.Selector
		}
		deployment.Spec = desired.Spec
		return nil
	}); err != nil {
		return false, fmt.Errorf("apply gateway Deployment: %w", err)
	}

	return deployment.Status.ReadyReplicas > 0, nil
}

// applyGatewayNetworkPolicy draws the Gateway's traffic boundary. The API
// server peers are discovered because KubeVirt's VNC, screenshot and power
// paths are Kubernetes subresources, and egress matches pre-DNAT
// destinations, so a name the Gateway resolves needs its address admitted.
func (r *PersonalDesktopReconciler) applyGatewayNetworkPolicy(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
	agentServiceIP string,
) error {
	names := desktop.Names(instance)
	apiServerIPs := discoverAPIServerIPs(ctx, r.Client)

	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.Gateway, Namespace: names.Namespace}}
	if err := r.applyDesktopObject(ctx, instance, policy, func() error {
		desired := desktop.GatewayNetworkPolicy(instance, resources.Labels(instance), apiServerIPs, agentServiceIP)
		policy.Spec = desired.Spec
		return nil
	}); err != nil {
		return fmt.Errorf("apply gateway NetworkPolicy: %w", err)
	}
	return nil
}

// ensureDataVolume creates a disk that does not exist yet and otherwise leaves
// it alone, returning the CDI phase either way. A populated DataVolume is
// never rewritten: CDI ignores spec changes once an import or clone finished,
// so re-applying would only make the object disagree with the disk it
// describes. desired is nil when the desktop adopts a disk it does not own.
func (r *PersonalDesktopReconciler) ensureDataVolume(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
	desired *unstructured.Unstructured,
	name string,
) (string, error) {
	if name == "" {
		return "", nil
	}
	namespace := desktop.Names(instance).Namespace
	live := desktop.NewObject(desktop.DataVolumeGVK, name, namespace)
	err := r.Get(ctx, client.ObjectKeyFromObject(live), live)
	switch {
	case err == nil:
		return desktop.DataVolumePhase(live), nil
	case !apierrors.IsNotFound(err):
		return "", err
	case desired == nil:
		// An adopted or administrator-managed disk the operator must never
		// create; report no phase until it appears.
		return "", nil
	}
	// Labelled for the finalizer path, never owned: garbage collection deletes
	// every dependent of a deleted owner, and these two disks are exactly the
	// ones nothing but desktopCleanup may remove — the root disk because
	// onInstanceDeletion decides its fate, the golden image because every
	// other desktop in the namespace clones from it.
	labelDesktopObject(instance, desired)
	if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", err
	}
	return desktop.DataVolumePhase(desired), nil
}

// desktopCleanup removes the desktop objects. retainDisk keeps the root
// DataVolume and the token Secret, which is what makes disabling reversible;
// only an Instance deletion under onInstanceDeletion=Delete clears them.
func (r *PersonalDesktopReconciler) desktopCleanup(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
	retainDisk bool,
) error {
	names := desktop.Names(instance)
	at := func(namespace, name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: namespace}
	}

	objects := []client.Object{
		&networkingv1.Ingress{ObjectMeta: at(names.Namespace, names.ConsoleIngress)},
		&networkingv1.NetworkPolicy{ObjectMeta: at(names.Namespace, names.Gateway)},
		&appsv1.Deployment{ObjectMeta: at(names.Namespace, names.Gateway)},
		&corev1.Service{ObjectMeta: at(names.Namespace, names.Gateway)},
		&rbacv1.RoleBinding{ObjectMeta: at(names.Namespace, names.Gateway)},
		&rbacv1.Role{ObjectMeta: at(names.Namespace, names.Gateway)},
		&corev1.ServiceAccount{ObjectMeta: at(names.Namespace, names.Gateway)},
		&corev1.Service{ObjectMeta: at(names.Namespace, names.AgentService)},
		&corev1.Secret{ObjectMeta: at(names.Namespace, names.CloudInit)},
		&corev1.Secret{ObjectMeta: at(names.Namespace, names.Sysprep)},
		&corev1.ConfigMap{ObjectMeta: at(names.InstanceNamespace, names.Extension)},
		&corev1.ConfigMap{ObjectMeta: at(names.Namespace, names.ServeConfig)},
	}
	if desktop.CrossNamespace(instance) {
		objects = append(objects, &corev1.Secret{ObjectMeta: at(names.InstanceNamespace, names.Tokens)})
	}
	if r.kubeVirtAvailable() {
		// Deleting an unstructured object of a kind the cluster does not serve
		// fails with "no matches for kind", so a cluster that never had
		// KubeVirt would otherwise block the finalizer forever.
		objects = append(objects, desktop.NewObject(desktop.VirtualMachineGVK, names.Desktop, names.Namespace))
	}
	if !retainDisk {
		objects = append(objects, &corev1.Secret{ObjectMeta: at(names.Namespace, names.Tokens)})
		// An adopted disk was never ours to delete; RootDataVolume renders nil
		// for exactly that case.
		if r.kubeVirtAvailable() && desktop.RootDataVolume(instance) != nil {
			objects = append(objects, desktop.NewObject(desktop.DataVolumeGVK, names.RootVolume, names.Namespace))
		}
	}

	for _, obj := range objects {
		if err := r.deleteDesktopObject(ctx, obj); err != nil {
			return err
		}
	}
	return nil
}

// desktopRetainsDisk reports whether an Instance deletion keeps the root disk.
// Anything but an explicit Delete retains it.
func desktopRetainsDisk(instance *typeclawv1alpha1.TypeClawInstance) bool {
	spec := instance.Spec.PersonalDesktop
	return spec == nil || spec.RootVolume.OnInstanceDeletion != "Delete"
}

func (r *PersonalDesktopReconciler) deleteDesktopObject(ctx context.Context, obj client.Object) error {
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

// applyDesktopObject re-derives the full desired state on every reconcile:
// CreateOrUpdate only persists what the mutator changed, so a partial merge
// would let a field from a previous Instance generation survive forever.
//
// It is for objects that die with the Instance. Anything desktopCleanup
// deliberately spares must go through applyRetainedDesktopObject instead.
func (r *PersonalDesktopReconciler) applyDesktopObject(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
	obj client.Object,
	mutate func() error,
) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if err := mutate(); err != nil {
			return err
		}
		return r.ownDesktopObject(instance, obj)
	})
	return err
}

// applyRetainedDesktopObject applies an object that must outlive the Instance
// it belongs to. It is applyDesktopObject without the controller reference:
// garbage collection removes every dependent of a deleted owner regardless of
// what any cleanup path decided, so owning the root disk or the token Secret
// would silently turn onInstanceDeletion=Retain — the default — into Delete.
func (r *PersonalDesktopReconciler) applyRetainedDesktopObject(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
	obj client.Object,
	mutate func() error,
) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if err := mutate(); err != nil {
			return err
		}
		labelDesktopObject(instance, obj)
		return nil
	})
	return err
}

// ownDesktopObject labels a desktop object for label-selected cleanup and adds
// a controller reference only inside the Instance namespace. An owner
// reference naming an object in another namespace is not merely ignored:
// Kubernetes resolves it in the dependent's own namespace, finds nothing, and
// garbage-collects the dependent — which would delete the desktop moments
// after creating it.
//
// The namespace test alone does not decide ownership: it says whether a
// reference would work, not whether the object may be collected at all. Only
// the callers of this function make that second claim.
func (r *PersonalDesktopReconciler) ownDesktopObject(
	instance *typeclawv1alpha1.TypeClawInstance,
	obj client.Object,
) error {
	labelDesktopObject(instance, obj)
	if obj.GetNamespace() != instance.Namespace {
		return nil
	}
	return controllerutil.SetControllerReference(instance, obj, r.Scheme)
}

// labelDesktopObject stamps the labels that let the finalizer path find every
// object of one desktop, including those no owner reference reaches.
func labelDesktopObject(instance *typeclawv1alpha1.TypeClawInstance, obj client.Object) {
	obj.SetLabels(desktop.Labels(instance))
}

// kubeVirtAvailable reports whether this cluster serves the virtualization
// kinds the desktop needs. The RESTMapper answers from discovery, so a cluster
// without KubeVirt or CDI is reported rather than retried against an API that
// will never answer.
func (r *PersonalDesktopReconciler) kubeVirtAvailable() bool {
	mapper := r.RESTMapper()
	if mapper == nil {
		return false
	}
	for _, gvk := range desktop.RequiredGVKs() {
		if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			return false
		}
	}
	return true
}

// gatewayImage resolves the image the Desktop Gateway runs when the Instance
// does not override it.
func (r *PersonalDesktopReconciler) gatewayImage() string {
	if r.OperatorImage != "" {
		return r.OperatorImage
	}
	return resources.DefaultOperatorImage
}

func (r *PersonalDesktopReconciler) ensureDesktopFinalizer(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
) error {
	if controllerutil.ContainsFinalizer(instance, desktop.Finalizer) {
		return nil
	}
	controllerutil.AddFinalizer(instance, desktop.Finalizer)
	return r.Update(ctx, instance)
}

func (r *PersonalDesktopReconciler) dropDesktopFinalizer(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
) error {
	if !controllerutil.ContainsFinalizer(instance, desktop.Finalizer) {
		return nil
	}
	controllerutil.RemoveFinalizer(instance, desktop.Finalizer)
	return r.Update(ctx, instance)
}

// publishDesktop folds one reconcile's observation into the Instance status.
func (r *PersonalDesktopReconciler) publishDesktop(
	ctx context.Context,
	instance *typeclawv1alpha1.TypeClawInstance,
	outcome desktopOutcome,
) error {
	base := instance.DeepCopy()
	status := outcome.status
	instance.Status.PersonalDesktop = &status
	setCondition(&instance.Status, instance.Generation,
		typeclawv1alpha1.ConditionPersonalDesktopReady,
		outcome.ready,
		typeclawv1alpha1.PersonalDesktopReasonReady, outcome.reason,
		outcome.message)
	return r.Status().Patch(ctx, instance, client.MergeFrom(base))
}

// SetupWithManager registers the Personal Desktop controller. Only
// same-namespace kinds are watched: an Owns() on a KubeVirt kind would make
// the manager fail to start on every cluster without KubeVirt installed, and
// owner references never reach a desktop in another namespace anyway.
func (r *PersonalDesktopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&typeclawv1alpha1.TypeClawInstance{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Named("typeclawinstance-personaldesktop").
		Complete(r)
}
