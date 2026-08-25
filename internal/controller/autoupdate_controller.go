package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/resources"
	"github.com/fml09/typeclaw-operator/internal/update"
)

const (
	// ConditionAutoUpdateReady reports that rollout management is healthy:
	// the Managed Runtime tracks its track's latest release, or an in-flight
	// rollout is progressing within policy.
	ConditionAutoUpdateReady = "AutoUpdateReady"

	reasonUpdTrackingCurrent     = "TrackingCurrent"
	reasonUpdRolloutInProgress   = "RolloutInProgress"
	reasonUpdRolloutConfirmed    = "RolloutConfirmed"
	reasonUpdAwaitingFreshBackup = "AwaitingFreshBackup"
	reasonUpdRolledBack          = "RolledBack"
	reasonUpdRegistryError       = "RegistryError"

	// RuntimeContainerName names the Managed Runtime container inside the
	// rendered StatefulSet podspec (resources.StatefulSet contract).
	RuntimeContainerName = "runtime"

	// autoUpdatePollInterval is the steady-state registry poll cadence;
	// every reconcile ends with this requeue so new releases surface without
	// dedicated watches.
	autoUpdatePollInterval = 30 * time.Minute

	defaultMaxBackupAgeHours       = int32(24)
	defaultConfirmationTimeoutMins = int32(15)
)

// AutoUpdateReconciler drives registry-tag polling and managed rollout
// promotion for one TypeClaw Instance. It patches the StatefulSet runtime
// container image directly; spec.runtime stays untouched so GitOps-authored
// specs remain authoritative about intent while status.update tracks what
// actually runs (ADR 0004).
type AutoUpdateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Registry polls the container registry for available versions; nil uses
	// the default anonymous GHCR client.
	Registry *update.RegistryClient
}

// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=typeclawinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=typeclawinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete

// Reconcile advances the rollout state machine:
//
//	disabled → Idle cleanup
//	newer candidate than current → backup freshness gate → promote (patch
//	STS image) → Updating → ready → Confirming (deadline set once) → past
//	deadline still ready → Ready (CurrentVersion promoted, fields cleared)
//	past deadline not ready → rollback to spec-derived image → RolledBack
//
// Limitation: a crash-after-ready regression inside the confirmation window
// is not detected. Readiness is sampled at reconcile boundaries, so a Pod
// that flaps ready→crashed→ready can still confirm; probe-failure watching
// is deferred.
func (r *AutoUpdateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var instance typeclawv1alpha1.TypeClawInstance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	base := instance.DeepCopy()
	status := &instance.Status
	generation := instance.Generation

	finish := func() (ctrl.Result, error) {
		if err := r.Status().Patch(ctx, &instance, client.MergeFrom(base)); err != nil {
			// Status conflicts under rapid requeues are expected; the next
			// poll re-reads and re-derives.
			log.V(1).Info("status patch skipped", "error", err)
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: autoUpdatePollInterval}, nil
	}

	au := instance.Spec.AutoUpdate
	if au == nil || !au.Enabled {
		setIdle(status)
		return finish()
	}

	u := ensureUpdate(status)

	readyReplicas, err := r.readyReplicas(ctx, req.NamespacedName)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("read workload readiness: %w", err)
	}

	// Active rollouts are driven purely by observed workload state until
	// they confirm or roll back; the poll result must not preempt them.
	if u.Phase == update.PhaseUpdating || u.Phase == update.PhaseConfirming {
		if err := r.advanceRollout(ctx, status, au, generation, req.NamespacedName, instance.Spec, readyReplicas, time.Now()); err != nil {
			return ctrl.Result{}, err
		}
		return finish()
	}

	tags, err := r.registry().ListVersions(ctx)
	if err != nil {
		setCondition(status, generation, ConditionAutoUpdateReady,
			false, reasonUpdRegistryError, reasonUpdRegistryError,
			fmt.Sprintf("registry poll failed: %v", err))
		return finish()
	}
	candidate, ok := update.PickVersion(tags, trackOrDefault(au.Track))
	if !ok {
		setCondition(status, generation, ConditionAutoUpdateReady,
			false, reasonUpdRegistryError, reasonUpdRegistryError,
			fmt.Sprintf("no qualifying release tag on track %q", trackOrDefault(au.Track)))
		return finish()
	}

	current := u.CurrentVersion
	if current == "" {
		current = versionTag(resources.ResolveRuntimeImage(instance.Spec))
	}
	// Mid-flight toward exactly this version: nothing new to do. The
	// RolledBack sub-case matters because a rollback keeps TargetVersion as
	// the record of the failed attempt — without it every poll would
	// re-promote that release and loop promote→rollback forever.
	if candidate == u.TargetVersion {
		if u.Phase == update.PhaseRolledBack {
			setCondition(status, generation, ConditionAutoUpdateReady,
				false, reasonUpdRolledBack, reasonUpdRolledBack, u.Message)
			return finish()
		}
		setCondition(status, generation, ConditionAutoUpdateReady,
			true, reasonUpdTrackingCurrent, reasonUpdTrackingCurrent,
			fmt.Sprintf("managed runtime tracks %s", current))
		return finish()
	}

	if !update.Newer(candidate, current) {
		setCondition(status, generation, ConditionAutoUpdateReady,
			true, reasonUpdTrackingCurrent, reasonUpdTrackingCurrent,
			fmt.Sprintf("managed runtime tracks %s", current))
		return finish()
	}

	if r.backupGateBlocked(status, au, time.Now()) {
		u.Phase = update.PhaseAwaitingBackup
		u.Message = fmt.Sprintf("rollout of %s deferred: no snapshot within MaxBackupAgeHours", candidate)
		setCondition(status, generation, ConditionAutoUpdateReady,
			false, reasonUpdAwaitingFreshBackup, reasonUpdAwaitingFreshBackup, u.Message)
		return finish()
	}

	imageRef := fmt.Sprintf("%s:%s", resources.DefaultRuntimeRepository, candidate)
	if err := r.patchRuntimeImage(ctx, req.NamespacedName, imageRef); err != nil {
		return ctrl.Result{}, fmt.Errorf("promote runtime image: %w", err)
	}
	u.Phase = update.PhaseUpdating
	u.TargetVersion = candidate
	u.PromotedImage = imageRef
	u.ConfirmationDeadline = nil
	u.Message = "promoted"
	setCondition(status, generation, ConditionAutoUpdateReady,
		false, reasonUpdRolloutInProgress, reasonUpdRolloutInProgress,
		fmt.Sprintf("promoted %s; awaiting readiness before confirmation window opens", candidate))
	return finish()
}

func (r *AutoUpdateReconciler) registry() *update.RegistryClient {
	if r.Registry != nil {
		return r.Registry
	}
	return &update.RegistryClient{}
}

// readyReplicas reads the workload StatefulSet's observed readiness; a
// missing StatefulSet simply means the rollout cannot be confirmed yet.
func (r *AutoUpdateReconciler) readyReplicas(ctx context.Context, key client.ObjectKey) (int32, error) {
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, key, &sts); err != nil {
		return 0, err
	}
	return sts.Status.ReadyReplicas, nil
}

// setIdle clears rollout bookkeeping when auto-update is disabled. The last
// confirmed CurrentVersion survives so opting out does not erase history.
func setIdle(status *typeclawv1alpha1.TypeClawInstanceStatus) {
	u := ensureUpdate(status)
	u.Phase = update.PhaseIdle
	u.TargetVersion = ""
	u.PromotedImage = ""
	u.ConfirmationDeadline = nil
	u.Message = ""
	removeCondition(status, ConditionAutoUpdateReady)
}

// advanceRollout moves an active rollout through Updating → Confirming →
// Ready, or rolls back once the confirmation window expires without a ready
// workload.
func (r *AutoUpdateReconciler) advanceRollout(
	ctx context.Context,
	status *typeclawv1alpha1.TypeClawInstanceStatus,
	au *typeclawv1alpha1.AutoUpdateSpec,
	generation int64,
	key client.ObjectKey,
	spec typeclawv1alpha1.TypeClawInstanceSpec,
	readyReplicas int32,
	now time.Time,
) error {
	u := status.Update
	timeout := time.Duration(au.ConfirmationTimeoutMinutes) * time.Minute
	if au.ConfirmationTimeoutMinutes <= 0 {
		timeout = time.Duration(defaultConfirmationTimeoutMins) * time.Minute
	}

	if readyReplicas > 0 {
		if u.Phase == update.PhaseUpdating {
			u.Phase = update.PhaseConfirming
			// Set exactly once: the window starts at first observed
			// readiness, never resets on subsequent reconciles.
			if u.ConfirmationDeadline == nil {
				deadline := metav1.NewTime(now.Add(timeout))
				u.ConfirmationDeadline = &deadline
			}
			u.Message = fmt.Sprintf("%s reported ready; confirming", u.TargetVersion)
			setCondition(status, generation, ConditionAutoUpdateReady,
				false, reasonUpdRolloutInProgress, reasonUpdRolloutInProgress, u.Message)
		}
		if u.Phase == update.PhaseConfirming &&
			u.ConfirmationDeadline != nil && !now.Before(u.ConfirmationDeadline.Time) {
			// The window elapsed with a ready workload: promote for real.
			u.Phase = update.PhaseReady
			u.CurrentVersion = u.TargetVersion
			u.TargetVersion = ""
			u.PromotedImage = ""
			u.ConfirmationDeadline = nil
			u.Message = ""
			setCondition(status, generation, ConditionAutoUpdateReady,
				true, reasonUpdRolloutConfirmed, reasonUpdRolloutConfirmed,
				fmt.Sprintf("confirmed %s as current version", u.CurrentVersion))
		}
		return nil
	}

	if u.ConfirmationDeadline == nil || !now.After(u.ConfirmationDeadline.Time) {
		// Still inside the window (or Updating before any readiness was ever
		// seen): let the rollout breathe until the next poll.
		return nil
	}

	fallbackImage := resources.ResolveRuntimeImage(spec)
	if err := r.patchRuntimeImage(ctx, key, fallbackImage); err != nil {
		return fmt.Errorf("roll back runtime image: %w", err)
	}
	u.Phase = update.PhaseRolledBack
	// TargetVersion stays: it records the failed attempt so the steady-state
	// check does not re-promote the same release on every poll.
	u.Message = fmt.Sprintf("rollout of %s rolled back to %s after confirmation timeout", u.TargetVersion, versionTag(fallbackImage))
	u.PromotedImage = ""
	u.ConfirmationDeadline = nil
	setCondition(status, generation, ConditionAutoUpdateReady,
		false, reasonUpdRolledBack, reasonUpdRolledBack, u.Message)
	return nil
}

// backupGateBlocked reports whether RequireFreshBackup defers the rollout:
// either no snapshot has ever completed or the newest one exceeds
// MaxBackupAgeHours.
func (r *AutoUpdateReconciler) backupGateBlocked(
	status *typeclawv1alpha1.TypeClawInstanceStatus,
	au *typeclawv1alpha1.AutoUpdateSpec,
	now time.Time,
) bool {
	if !au.RequireFreshBackup {
		return false
	}
	maxAge := time.Duration(au.MaxBackupAgeHours) * time.Hour
	if au.MaxBackupAgeHours <= 0 {
		maxAge = time.Duration(defaultMaxBackupAgeHours) * time.Hour
	}
	backup := status.Backup
	if backup == nil || backup.LastSnapshotTime == nil {
		return true
	}
	return now.Sub(backup.LastSnapshotTime.Time) > maxAge
}

// patchRuntimeImage swaps the Managed Runtime container image on the live
// StatefulSet via a merge patch, leaving every other field untouched.
func (r *AutoUpdateReconciler) patchRuntimeImage(ctx context.Context, key client.ObjectKey, image string) error {
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, key, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("workload StatefulSet %s does not exist yet", key)
		}
		return err
	}
	base := sts.DeepCopy()
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == RuntimeContainerName {
			sts.Spec.Template.Spec.Containers[i].Image = image
			return r.Patch(ctx, &sts, client.MergeFrom(base))
		}
	}
	return fmt.Errorf("StatefulSet %s renders no %q container", key, RuntimeContainerName)
}

func ensureUpdate(status *typeclawv1alpha1.TypeClawInstanceStatus) *typeclawv1alpha1.UpdateStatus {
	if status.Update == nil {
		status.Update = &typeclawv1alpha1.UpdateStatus{}
	}
	return status.Update
}

func removeCondition(status *typeclawv1alpha1.TypeClawInstanceStatus, conditionType string) {
	for i := range status.Conditions {
		if status.Conditions[i].Type == conditionType {
			status.Conditions = append(status.Conditions[:i], status.Conditions[i+1:]...)
			return
		}
	}
}

func trackOrDefault(track string) string {
	if track == "" {
		return "latest"
	}
	return track
}

// versionTag extracts the release tag from an image reference; images
// without a tag resolve to "" so any real candidate counts as newer.
func versionTag(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return image
}

// SetupWithManager sets up the controller with the Manager. Owning the
// StatefulSet re-triggers the rollout check whenever workload readiness
// changes, not only on the 30-minute poll cadence.
func (r *AutoUpdateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&typeclawv1alpha1.TypeClawInstance{}).
		Owns(&appsv1.StatefulSet{}).
		Complete(r)
}
