package controller

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/resources"
)

const (
	// ConditionBackupReady reports scheduled snapshot health for a TypeClaw
	// Instance and gates restore requests.
	ConditionBackupReady = "BackupReady"

	reasonBackupsHealthy          = "BackupsHealthy"
	reasonBackupFailed            = "BackupFailed"
	reasonBackupPending           = "BackupPending"
	reasonRestoreBlockedSuspended = "RestoreBlockedSuspended"
	reasonRestoreTargetNotEmpty   = "RestoreTargetNotEmpty"

	// RestoreAnnotation carries the snapshot archive to unpack onto the
	// Agent Folder; it is processed exactly once per distinct value.
	RestoreAnnotation = "typeclaw.fml09.io/restore"

	// restoreTargetNotEmptyExit is the custom exit code the restore script
	// uses when /agent/typeclaw.json already exists (target not empty); the
	// Job surfaces it inside the JobFailed condition message.
	restoreTargetNotEmptyExit = "exit code 78"

	// backupComponentLabelKey selects snapshot Jobs spawned by the backup
	// CronJob; mirrors internal/resources componentLabelKey.
	backupComponentLabelKey = "app.kubernetes.io/component"
	backupComponentValue    = "backup"
)

// BackupController reconciles scheduled Agent Folder snapshots and guarded
// one-shot restores for one TypeClawInstance.
type BackupController struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=typeclawinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=typeclawinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile drives snapshot infrastructure for the Instance's declared
// backup policy and acts on guarded restore annotations exactly once.
func (r *BackupController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var instance typeclawv1alpha1.TypeClawInstance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	base := instance.DeepCopy()
	status := &instance.Status
	status.ObservedGeneration = instance.Generation

	if instance.Spec.Backup == nil {
		if err := r.deleteSnapshotResources(ctx, &instance); err != nil {
			return ctrl.Result{}, err
		}
		removeBackupCondition(status)
		status.Backup = nil
	} else {
		if err := r.applySnapshotResources(ctx, &instance); err != nil {
			setCondition(status, instance.Generation, ConditionBackupReady,
				false, reasonBackupPending, reasonResourceError, err.Error())
			log.Error(err, "applying snapshot resources failed")
		} else if err := r.observeSnapshots(ctx, status, instance.Generation, &instance); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.processRestore(ctx, &instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.Status().Patch(ctx, &instance, client.MergeFrom(base)); err != nil {
		// Status conflicts with our own resource writes are expected under
		// rapid requeues; a fresh reconcile re-reads and re-applies.
		log.V(1).Info("status patch skipped", "error", err)
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

// applySnapshotResources ensures the destination PVC and the pruning
// snapshot CronJob match spec.backup; suspending an Instance suspends the
// schedule with it because snapshots must not run while the Agent Folder
// could still be written by a live runtime.
func (r *BackupController) applySnapshotResources(ctx context.Context, instance *typeclawv1alpha1.TypeClawInstance) error {
	pvc := resources.SnapshotPVC(instance)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		desired := resources.SnapshotPVC(instance)
		pvc.Spec = desired.Spec
		return r.own(instance, pvc)
	}); err != nil {
		return fmt.Errorf("apply snapshot PVC: %w", err)
	}

	cronJob := resources.BackupCronJob(instance)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cronJob, func() error {
		desired := resources.BackupCronJob(instance)
		cronJob.Spec = desired.Spec
		return r.own(instance, cronJob)
	}); err != nil {
		return fmt.Errorf("apply backup CronJob: %w", err)
	}
	return nil
}

// deleteSnapshotResources removes snapshot infrastructure when backups are
// disabled; removing the destination PVC here intentionally drops snapshots
// along with the schedule that would extend them.
func (r *BackupController) deleteSnapshotResources(ctx context.Context, instance *typeclawv1alpha1.TypeClawInstance) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: resources.SnapshotPVCName(instance.Name), Namespace: instance.Namespace,
	}}
	if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove snapshot PVC: %w", err)
	}
	cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{
		Name: resources.BackupCronJobName(instance.Name), Namespace: instance.Namespace,
	}}
	if err := r.Delete(ctx, cronJob); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove backup CronJob: %w", err)
	}
	return nil
}

// observeSnapshots maps the newest snapshot Job outcome onto BackupReady and
// records the most recent succeeded archive in status.backup. Snapshot name
// equals the spawning Job name, completion time from the Job itself.
func (r *BackupController) observeSnapshots(ctx context.Context, status *typeclawv1alpha1.TypeClawInstanceStatus, generation int64, instance *typeclawv1alpha1.TypeClawInstance) error {
	var jobs batchv1.JobList
	labels := resources.Labels(instance)
	labels[backupComponentLabelKey] = backupComponentValue
	if err := r.List(ctx, &jobs,
		client.InNamespace(instance.Namespace),
		client.MatchingLabels(labels)); err != nil {
		return fmt.Errorf("list snapshot Jobs: %w", err)
	}

	var latest, latestSucceeded *batchv1.Job
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if latest == nil || backupNewer(job, latest) {
			latest = job
		}
		if backupJobConditionTrue(job, batchv1.JobComplete) &&
			(latestSucceeded == nil || backupCompletedAfter(job, latestSucceeded)) {
			latestSucceeded = job
		}
	}

	switch {
	case latest == nil:
		setCondition(status, generation, ConditionBackupReady,
			false, reasonBackupPending, reasonBackupPending,
			"waiting for the first scheduled snapshot Job")
	case backupJobConditionTrue(latest, batchv1.JobComplete):
		setCondition(status, generation, ConditionBackupReady,
			true, reasonBackupsHealthy, reasonBackupFailed,
			fmt.Sprintf("snapshot %q completed", latest.Name))
	case backupJobConditionTrue(latest, batchv1.JobFailed):
		setCondition(status, generation, ConditionBackupReady,
			false, reasonBackupFailed, reasonBackupFailed,
			fmt.Sprintf("snapshot %q failed", latest.Name))
	default:
		setCondition(status, generation, ConditionBackupReady,
			false, reasonBackupPending, reasonBackupPending,
			fmt.Sprintf("snapshot %q still running", latest.Name))
	}

	status.Backup = nil
	if latestSucceeded != nil {
		status.Backup = &typeclawv1alpha1.BackupStatus{
			LatestSnapshot:   latestSucceeded.Name,
			LastSnapshotTime: latestSucceeded.Status.CompletionTime,
		}
	}
	return nil
}

// processRestore acts on the restore annotation exactly once per value:
// it refuses restores of non-suspended Instances (a live runtime would
// corrupt an Agent Folder being overwritten), otherwise creates the restore
// Job and records the annotation regardless of the Job's own outcome —
// failures surface through the Job and the BackupReady condition afterwards.
func (r *BackupController) processRestore(ctx context.Context, instance *typeclawv1alpha1.TypeClawInstance) error {
	value := instance.Annotations[RestoreAnnotation]
	if value == "" {
		return nil
	}
	status := &instance.Status

	if value != status.RestoreLastProcessed {
		if !instance.Spec.Suspend {
			setCondition(status, instance.Generation, ConditionBackupReady,
				false, reasonRestoreBlockedSuspended, reasonRestoreBlockedSuspended,
				fmt.Sprintf("restore of %q refused: suspend the TypeClaw Instance before restoring", value))
			return nil
		}
		job := resources.RestoreJob(instance, value)
		if err := r.own(instance, job); err != nil {
			return err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create restore Job: %w", err)
		}
		status.RestoreLastProcessed = value
	}

	// A previously dispatched restore may have since failed with the
	// target-not-empty exit code; surface it on the condition without
	// re-running anything.
	var job batchv1.Job
	key := types.NamespacedName{
		Name:      resources.RestoreJobName(instance.Name, value),
		Namespace: instance.Namespace,
	}
	err := r.Get(ctx, key, &job)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if backupJobFailedWithExitCode(&job, restoreTargetNotEmptyExit) {
		setCondition(status, instance.Generation, ConditionBackupReady,
			false, reasonRestoreTargetNotEmpty, reasonRestoreTargetNotEmpty,
			fmt.Sprintf("restore of %q aborted: Agent Folder target not empty (%s)",
				value, restoreTargetNotEmptyExit))
	}
	return nil
}

func (r *BackupController) own(instance *typeclawv1alpha1.TypeClawInstance, obj client.Object) error {
	if err := controllerutil.SetControllerReference(instance, obj, r.Scheme); err != nil {
		return err
	}
	obj.SetLabels(resources.Labels(instance))
	return nil
}

// removeBackupCondition clears BackupReady entirely when backups are off;
// setCondition can only upsert, never drop.
func removeBackupCondition(status *typeclawv1alpha1.TypeClawInstanceStatus) {
	kept := make([]metav1.Condition, 0, len(status.Conditions))
	for _, c := range status.Conditions {
		if c.Type == ConditionBackupReady {
			continue
		}
		kept = append(kept, c)
	}
	status.Conditions = kept
}

func backupJobConditionTrue(job *batchv1.Job, want batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == want && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func backupJobFailedWithExitCode(job *batchv1.Job, needle string) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue &&
			strings.Contains(c.Message, needle) {
			return true
		}
	}
	return false
}

// backupNewer orders snapshot Jobs by creation time, breaking ties by name
// so selection stays deterministic within one timestamp tick.
func backupNewer(a, b *batchv1.Job) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.After(b.CreationTimestamp.Time)
	}
	return a.Name > b.Name
}

// backupCompletedAfter orders succeeded Jobs by their actual completion
// time rather than object age.
func backupCompletedAfter(a, b *batchv1.Job) bool {
	if !a.Status.CompletionTime.Equal(b.Status.CompletionTime) {
		return a.Status.CompletionTime.After(b.Status.CompletionTime.Time)
	}
	return a.Name > b.Name
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&typeclawv1alpha1.TypeClawInstance{}).
		Owns(&batchv1.CronJob{}).
		Owns(&batchv1.Job{}).
		Named("typeclawinstance-backup").
		Complete(r)
}
