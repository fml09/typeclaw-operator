package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/resources"
)

// backupReconcilerFor mirrors reconcilerFor for the backup lane.
func backupReconcilerFor(t *testing.T, objs ...client.Object) (*BackupController, client.Client) {
	t.Helper()
	c := fakeBackupClient(t, objs...)
	return &BackupController{Client: c, Scheme: c.Scheme()}, c
}

func fakeBackupClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&typeclawv1alpha1.TypeClawInstance{}).
		WithObjects(objs...).
		Build()
}

func backupEnabledInstance(mutate func(*typeclawv1alpha1.TypeClawInstance)) *typeclawv1alpha1.TypeClawInstance {
	size := resource.MustParse("10Gi")
	return instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Backup = &typeclawv1alpha1.BackupSpec{
			Schedule:       "17 * * * *",
			Retention:      7,
			SnapshotVolume: typeclawv1alpha1.VolumeClaimSpec{Size: size},
		}
		if mutate != nil {
			mutate(in)
		}
	})
}

func backupReconcile(t *testing.T, r *BackupController, name string) {
	t.Helper()
	key := types.NamespacedName{Name: name, Namespace: "agents"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("BackupController.Reconcile() error: %v", err)
	}
}

func snapshotJob(name string, at time.Time) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "agents",
			CreationTimestamp: metav1.NewTime(at),
			Labels: map[string]string{
				"app.kubernetes.io/name":       "typeclaw",
				"app.kubernetes.io/instance":   "kakao-agent",
				"app.kubernetes.io/managed-by": "typeclaw-operator",
				backupComponentLabelKey:        backupComponentValue,
			},
		},
		Status: batchv1.JobStatus{
			CompletionTime: func() *metav1.Time { t := metav1.NewTime(at); return &t }(),
		},
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type:   batchv1.JobComplete,
		Status: corev1.ConditionTrue,
	})
	return job
}

func TestBackupDisabledDeletesSnapshotResourcesAndClearsStatus(t *testing.T) {
	in := instance("kakao-agent", nil)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: resources.SnapshotPVCName(in.Name), Namespace: "agents"}}
	cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{
		Name: resources.BackupCronJobName(in.Name), Namespace: "agents"}}

	r, c := backupReconcilerFor(t, in, pvc, cronJob)
	ctx := context.Background()
	// Seed stale observed state that a previously enabled backup left behind.
	stale := in.DeepCopy()
	stale.Status.Conditions = append(stale.Status.Conditions, metav1.Condition{
		Type: ConditionBackupReady, Status: metav1.ConditionTrue, Reason: reasonBackupsHealthy,
	})
	stale.Status.Backup = &typeclawv1alpha1.BackupStatus{LatestSnapshot: "old"}
	if err := c.Status().Update(ctx, stale); err != nil {
		t.Fatalf("seed status: %v", err)
	}

	backupReconcile(t, r, in.Name)

	var got typeclawv1alpha1.TypeClawInstance
	if err := c.Get(ctx, client.ObjectKeyFromObject(in), &got); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if condition(&got.Status, ConditionBackupReady) != nil {
		t.Fatal("BackupReady must be removed when backups are disabled")
	}
	if got.Status.Backup != nil {
		t.Fatalf("status.backup = %+v, want cleared", got.Status.Backup)
	}
	for _, obj := range []struct {
		name string
		get  error
	}{
		{"kakao-agent-snapshots", c.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{})},
		{"kakao-agent-backup", c.Get(ctx, client.ObjectKeyFromObject(cronJob), &batchv1.CronJob{})},
	} {
		if !apierrors.IsNotFound(obj.get) {
			t.Fatalf("%s still present after disabling backups (err=%v)", obj.name, obj.get)
		}
	}
}

func TestBackupEnabledCreatesResourcesAndReportsPending(t *testing.T) {
	in := backupEnabledInstance(nil)
	r, c := backupReconcilerFor(t, in)

	backupReconcile(t, r, in.Name)

	ctx := context.Background()
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Name: "kakao-agent-snapshots", Namespace: "agents"}, &pvc); err != nil {
		t.Fatalf("snapshot PVC not created: %v", err)
	}
	var cronJob batchv1.CronJob
	if err := c.Get(ctx, client.ObjectKey{Name: "kakao-agent-backup", Namespace: "agents"}, &cronJob); err != nil {
		t.Fatalf("backup CronJob not created: %v", err)
	}
	if cronJob.Spec.Schedule != "17 * * * *" || cronJob.Spec.Suspend == nil || *cronJob.Spec.Suspend {
		t.Fatalf("CronJob schedule/suspend wrong: %+v", cronJob.Spec)
	}

	var got typeclawv1alpha1.TypeClawInstance
	if err := c.Get(ctx, client.ObjectKeyFromObject(in), &got); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	cond := condition(&got.Status, ConditionBackupReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonBackupPending {
		t.Fatalf("BackupReady = %+v, want False/%s before any Job exists", cond, reasonBackupPending)
	}
	if got.Status.Backup != nil {
		t.Fatalf("status.backup = %+v, want nil before first success", got.Status.Backup)
	}
}

func TestBackupHealthyTracksLatestSucceededJob(t *testing.T) {
	completed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	in := backupEnabledInstance(nil)
	job := snapshotJob("kakao-agent-backup-19200001", completed)
	r, _ := backupReconcilerFor(t, in, job)

	backupReconcile(t, r, in.Name)

	var got typeclawv1alpha1.TypeClawInstance
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(in), &got); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	cond := condition(&got.Status, ConditionBackupReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reasonBackupsHealthy {
		t.Fatalf("BackupReady = %+v, want True/%s", cond, reasonBackupsHealthy)
	}
	if got.Status.Backup == nil ||
		got.Status.Backup.LatestSnapshot != job.Name ||
		got.Status.Backup.LastSnapshotTime == nil ||
		!got.Status.Backup.LastSnapshotTime.Time.Equal(completed) {
		t.Fatalf("status.backup = %+v, want LatestSnapshot=%q LastSnapshotTime=%v",
			got.Status.Backup, job.Name, completed)
	}
}

func TestBackupFailedOverridesOlderSuccess(t *testing.T) {
	base := time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)
	in := backupEnabledInstance(nil)
	succeeded := snapshotJob("kakao-agent-backup-11000001", base)

	failedAt := base.Add(time.Hour)
	failed := snapshotJob("kakao-agent-backup-12000001", failedAt)
	failed.Status.Conditions = nil
	failed.Status.CompletionTime = nil
	failed.Status.Conditions = append(failed.Status.Conditions, batchv1.JobCondition{
		Type:    batchv1.JobFailed,
		Status:  corev1.ConditionTrue,
		Message: `Job has reached the specified backoff limit`,
	})

	r, _ := backupReconcilerFor(t, in, succeeded, failed)
	backupReconcile(t, r, in.Name)

	var got typeclawv1alpha1.TypeClawInstance
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(in), &got); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	cond := condition(&got.Status, ConditionBackupReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonBackupFailed {
		t.Fatalf("BackupReady = %+v, want False/%s when latest Job failed", cond, reasonBackupFailed)
	}
	// The most recent success stays recorded even while the latest run failed.
	if got.Status.Backup == nil || got.Status.Backup.LatestSnapshot != succeeded.Name {
		t.Fatalf("status.backup = %+v, want last success %q retained", got.Status.Backup, succeeded.Name)
	}
}

func TestRestoreCreatesJobExactlyOnce(t *testing.T) {
	in := backupEnabledInstance(func(i *typeclawv1alpha1.TypeClawInstance) {
		i.Spec.Suspend = true
		i.Annotations = map[string]string{RestoreAnnotation: "kakao-agent-backup-123.tar.gz"}
	})
	r, _ := backupReconcilerFor(t, in)

	backupReconcile(t, r, in.Name)

	ctx := context.Background()
	restoreName := resources.RestoreJobName(in.Name, "kakao-agent-backup-123.tar.gz")
	var job batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Name: restoreName, Namespace: "agents"}, &job); err != nil {
		t.Fatalf("restore Job %s not created: %v", restoreName, err)
	}

	// Second reconcile with the annotation unchanged must not duplicate work.
	backupReconcile(t, r, in.Name)
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace("agents")); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	count := 0
	for _, j := range jobs.Items {
		if j.Name == restoreName {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("restore Job duplicated: found %d copies of %s", count, restoreName)
	}

	var got typeclawv1alpha1.TypeClawInstance
	if err := r.Get(ctx, client.ObjectKeyFromObject(in), &got); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if got.Status.RestoreLastProcessed != "kakao-agent-backup-123.tar.gz" {
		t.Fatalf("restoreLastProcessed = %q, want processed value", got.Status.RestoreLastProcessed)
	}
}

func TestRestoreRefusedWhileNotSuspended(t *testing.T) {
	in := backupEnabledInstance(func(i *typeclawv1alpha1.TypeClawInstance) {
		i.Annotations = map[string]string{RestoreAnnotation: "snap.tar.gz"}
	})
	r, _ := backupReconcilerFor(t, in)

	backupReconcile(t, r, in.Name)

	ctx := context.Background()
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{
		Name: resources.RestoreJobName(in.Name, "snap.tar.gz"), Namespace: "agents"}, &job)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("restore Job must not be created while running, get err=%v", err)
	}
	var got typeclawv1alpha1.TypeClawInstance
	if err := r.Get(ctx, client.ObjectKeyFromObject(in), &got); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	cond := condition(&got.Status, ConditionBackupReady)
	if cond == nil || cond.Reason != reasonRestoreBlockedSuspended {
		t.Fatalf("BackupReady = %+v, want reason %s", cond, reasonRestoreBlockedSuspended)
	}
	if got.Status.RestoreLastProcessed != "" {
		t.Fatalf("restoreLastProcessed = %q, refused restores must stay unprocessed", got.Status.RestoreLastProcessed)
	}
}

func TestRestoreTargetNotEmptySurfacesExitCode78(t *testing.T) {
	snapshot := "snap.tar.gz"
	in := backupEnabledInstance(func(i *typeclawv1alpha1.TypeClawInstance) {
		i.Spec.Suspend = true
		i.Annotations = map[string]string{RestoreAnnotation: snapshot}
	})
	failed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.RestoreJobName(in.Name, snapshot),
			Namespace: "agents",
		},
		Status: batchv1.JobStatus{},
	}
	failed.Status.Conditions = append(failed.Status.Conditions, batchv1.JobCondition{
		Type:    batchv1.JobFailed,
		Status:  corev1.ConditionTrue,
		Message: `Job has reached the specified backoff limit: restore aborted: public workspace target not empty (exit code 78)`,
	})

	r, _ := backupReconcilerFor(t, in, failed)
	backupReconcile(t, r, in.Name)

	var got typeclawv1alpha1.TypeClawInstance
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(in), &got); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	cond := condition(&got.Status, ConditionBackupReady)
	if cond == nil || cond.Reason != reasonRestoreTargetNotEmpty {
		t.Fatalf("BackupReady = %+v, want reason %s on exit 78", cond, reasonRestoreTargetNotEmpty)
	}
	if got.Status.RestoreLastProcessed != snapshot {
		t.Fatalf("restoreLastProcessed = %q, want %q (recorded regardless of outcome)",
			got.Status.RestoreLastProcessed, snapshot)
	}
}
