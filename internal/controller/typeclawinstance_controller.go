package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/resources"
)

const (
	// ConditionResourcesReady reports that every desired workload resource is
	// accepted by the API server.
	ConditionResourcesReady = "ResourcesReady"
	// ConditionRuntimeReady reports that the managed runtime Pod crossed the
	// upstream /health/ready boundary (or is intentionally suspended).
	ConditionRuntimeReady = "RuntimeReady"

	reasonResourcesApplied  = "ResourcesApplied"
	reasonResourceError     = "ResourceError"
	reasonRuntimeAvailable  = "RuntimeAvailable"
	reasonRuntimeNotReady   = "RuntimeNotReady"
	reasonInstanceSuspended = "Suspended"
)

// TypeClawInstanceReconciler reconciles a TypeClawInstance object.
type TypeClawInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=typeclawinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=typeclawinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=typeclawinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the cluster toward the declared Instance policy: one
// single-active workload owning one Agent Folder.
func (r *TypeClawInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var instance typeclawv1alpha1.TypeClawInstance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	resourcesReady := func() error {
		sts, err := resources.StatefulSet(&instance)
		if err != nil {
			return fmt.Errorf("render StatefulSet: %w", err)
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
			// Re-derive the full desired spec: CreateOrUpdate only applies
			// what the mutator changes, so a stale live object would
			// otherwise keep old replicas/image/storage forever.
			desired, err := resources.StatefulSet(&instance)
			if err != nil {
				return err
			}
			// Selector, serviceName, and volumeClaimTemplates are immutable;
			// keep the API server's copies so updates never fight admission.
			desired.Spec.Selector = sts.Spec.Selector
			desired.Spec.ServiceName = sts.Spec.ServiceName
			desired.Spec.VolumeClaimTemplates = sts.Spec.VolumeClaimTemplates
			sts.Spec = desired.Spec
			return r.own(&instance, sts)
		}); err != nil {
			return fmt.Errorf("apply StatefulSet: %w", err)
		}

		exposed := instance.Spec.ExposeTUI == nil || *instance.Spec.ExposeTUI
		if exposed {
			svc := resources.Service(&instance)
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
				desired := resources.Service(&instance)
				svc.Spec = desired.Spec
				return r.own(&instance, svc)
			}); err != nil {
				return fmt.Errorf("apply Service: %w", err)
			}
		} else if err := r.deleteService(ctx, &instance); err != nil {
			return err
		}

		relayEnabled := instance.Spec.RestartRelay == nil || *instance.Spec.RestartRelay
		if relayEnabled {
			for _, obj := range []client.Object{
				resources.RelayServiceAccount(&instance),
				resources.RelayRole(&instance),
				resources.RelayRoleBinding(&instance),
			} {
				if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
					return r.own(&instance, obj)
				}); err != nil {
					return fmt.Errorf("apply relay RBAC %T: %w", obj, err)
				}
			}
		} else if err := r.deleteRelayRBAC(ctx, &instance); err != nil {
			return err
		}
		return nil
	}()

	var runtimeReady bool
	if resourcesReady == nil {
		var sts appsv1.StatefulSet
		err := r.Get(ctx, req.NamespacedName, &sts)
		switch {
		case apierrors.IsNotFound(err):
			runtimeReady = false
		case err != nil:
			return ctrl.Result{}, err
		default:
			runtimeReady = !instance.Spec.Suspend && sts.Status.ReadyReplicas > 0
		}
	}

	base := instance.DeepCopy()
	status := &instance.Status
	status.ObservedGeneration = instance.Generation
	setCondition(status, instance.Generation, ConditionResourcesReady,
		resourcesReady == nil,
		reasonResourcesApplied, reasonResourceError,
		func() string {
			if resourcesReady == nil {
				return "desired workload resources are applied"
			}
			return resourcesReady.Error()
		}())
	switch {
	case instance.Spec.Suspend:
		setCondition(status, instance.Generation, ConditionRuntimeReady,
			false, reasonInstanceSuspended, reasonInstanceSuspended,
			"workload scaled to zero by request; Agent Folder retained")
	case runtimeReady:
		setCondition(status, instance.Generation, ConditionRuntimeReady,
			true, reasonRuntimeAvailable, reasonRuntimeNotReady,
			"managed runtime crossed the readiness boundary")
	default:
		setCondition(status, instance.Generation, ConditionRuntimeReady,
			false, reasonRuntimeNotReady, reasonRuntimeNotReady,
			"managed runtime has not reported ready replicas")
	}

	if err := r.Status().Patch(ctx, &instance, client.MergeFrom(base)); err != nil {
		// Status conflicts with our own resource writes are expected under
		// rapid requeues; a fresh reconcile re-reads and re-applies.
		log.V(1).Info("status patch skipped", "error", err)
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

func (r *TypeClawInstanceReconciler) deleteService(ctx context.Context, instance *typeclawv1alpha1.TypeClawInstance) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	err := r.Delete(ctx, svc)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove TUI Service: %w", err)
	}
	return nil
}

// deleteRelayRBAC removes the relay identity trio when the sidecar is
// disabled, keeping a clean cutover in both directions.
func (r *TypeClawInstanceReconciler) deleteRelayRBAC(ctx context.Context, instance *typeclawv1alpha1.TypeClawInstance) error {
	name := instance.Name + "-relay"
	for _, obj := range []client.Object{
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}},
	} {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("remove relay RBAC %T: %w", obj, err)
		}
	}
	return nil
}

func (r *TypeClawInstanceReconciler) own(instance *typeclawv1alpha1.TypeClawInstance, obj client.Object) error {
	if err := controllerutil.SetControllerReference(instance, obj, r.Scheme); err != nil {
		return err
	}
	obj.SetLabels(resources.Labels(instance))
	return nil
}

func setCondition(
	status *typeclawv1alpha1.TypeClawInstanceStatus,
	generation int64,
	conditionType string,
	positive bool,
	trueReason, falseReason, message string,
) {
	reason := falseReason
	condStatus := metav1.ConditionFalse
	if positive {
		reason = trueReason
		condStatus = metav1.ConditionTrue
	}
	now := metav1.Now()
	cond := metav1.Condition{
		Type:               conditionType,
		Status:             condStatus,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	}
	for i := range status.Conditions {
		existing := &status.Conditions[i]
		if existing.Type != conditionType {
			continue
		}
		// Update in place: a changed condition must replace the old entry,
		// and LastTransitionTime advances only on real status transitions
		// so watchers see genuine state changes.
		if existing.Status == cond.Status {
			cond.LastTransitionTime = existing.LastTransitionTime
			if !cond.LastTransitionTime.IsZero() && existing.Reason == reason && existing.Message == message {
				existing.ObservedGeneration = generation
				return
			}
		}
		status.Conditions[i] = cond
		return
	}
	status.Conditions = append(status.Conditions, cond)
}

// SetupWithManager sets up the controller with the Manager.
func (r *TypeClawInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&typeclawv1alpha1.TypeClawInstance{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Named("typeclawinstance").
		Complete(r)
}
