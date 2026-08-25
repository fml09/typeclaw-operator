// Package resources renders the Kubernetes workload half of a TypeClaw
// Instance. The backup builders here cover the scheduled snapshot pipeline
// (destination PVC plus pruning CronJob) and the guarded one-shot restore
// Job described in docs/adr/0004.
package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// BackupImage is the pinned single-binary toolbox image executing snapshot
// and restore scripts. Upgrades are explicit edits, never floating tags.
const BackupImage = "busybox:1.37.0"

// SnapshotsMountPath carries the snapshot destination volume inside the
// snapshot and restore workloads.
const SnapshotsMountPath = "/snapshots"

// componentLabelKey splits rendered resources by capability. It duplicates
// the key used by the BackupController's Job selector; the pair is kept in
// lockstep by backup_cronjob_test.go and backup_controller_test.go.
const componentLabelKey = "app.kubernetes.io/component"

// backupComponent is the component value shared by the snapshot CronJob and
// every snapshot Job it spawns.
const backupComponent = "backup"

// SnapshotPVCName is the destination volume receiving Agent Folder snapshots.
func SnapshotPVCName(instanceName string) string {
	return instanceName + "-snapshots"
}

// BackupCronJobName is the scheduled snapshot driver for one Instance.
func BackupCronJobName(instanceName string) string {
	return instanceName + "-backup"
}

// RestoreJobName derives the deterministic per-snapshot restore Job name;
// the short hash keeps distinct restore requests from colliding while a
// repeated request reuses (never duplicates) its Job.
func RestoreJobName(instanceName, snapshotArchive string) string {
	sum := sha256.Sum256([]byte(snapshotArchive))
	return fmt.Sprintf("%s-restore-%s", instanceName, hex.EncodeToString(sum[:4]))
}

// SnapshotPVC renders the destination volume receiving tar snapshots of the
// Agent Folder, sized by spec.backup.snapshotVolume.
func SnapshotPVC(instance *typeclawv1alpha1.TypeClawInstance) *corev1.PersistentVolumeClaim {
	claim := claimTemplate(SnapshotPVCName(instance.Name), instance.Spec.Backup.SnapshotVolume)
	claim.Namespace = instance.Namespace
	claim.Labels = Labels(instance)
	return &claim
}

// BackupCronJob renders the scheduled snapshot driver: one busybox Job per
// tick that tars the Agent Folder and prunes snapshots beyond retention.
// spec.suspend mirrors the Instance so suspending an Instance also pauses
// snapshots while the runtime is scaled to zero.
func BackupCronJob(instance *typeclawv1alpha1.TypeClawInstance) *batchv1.CronJob {
	labels := Labels(instance)
	labels[componentLabelKey] = backupComponent
	retention := strconv.FormatInt(int64(instance.Spec.Backup.Retention), 10)
	env := []corev1.EnvVar{
		{Name: "RETENTION", Value: retention},
		// JOB_NAME names the archive after the spawning Job, giving every
		// run a unique, chronologically sortable snapshot file.
		{Name: "JOB_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
		}},
	}
	suspend := instance.Spec.Suspend
	historyLimit := func(v int32) *int32 { return &v }

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BackupCronJobName(instance.Name),
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   instance.Spec.Backup.Schedule,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: historyLimit(3),
			FailedJobsHistoryLimit:     historyLimit(1),
			Suspend:                    &suspend,
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec:       backupPodSpec(instance, "snapshot", backupScript, true, env),
					},
				},
			},
		},
	}
}

// backupScript archives the read-only Agent Folder under the spawning
// Job's name, then prunes oldest archives beyond RETENTION.
const backupScript = `tar czf "/snapshots/${JOB_NAME}.tar.gz" -C /agent .
ls -1t /snapshots/*.tar.gz | tail -n +$((RETENTION+1)) | xargs -r rm -f`

// RestoreJob renders the one-shot Job unpacking a snapshot archive onto the
// Agent Folder. The guard exits with custom code 78 when the target already
// holds a runtime configuration, so a restore can never silently overwrite
// a live Agent Folder; the controller surfaces that exit code separately.
func RestoreJob(instance *typeclawv1alpha1.TypeClawInstance, snapshotArchive string) *batchv1.Job {

	script := fmt.Sprintf(`[ ! -e %[2]s/typeclaw.json ] || { echo "restore aborted: Agent Folder target not empty (exit code 78)" >&2; exit 78; }
tar xzf "/snapshots/%[1]s" -C %[2]s`, snapshotArchive, AgentMountPath)
	labels := Labels(instance)
	labels[componentLabelKey] = "restore"

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RestoreJobName(instance.Name, snapshotArchive),
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       backupPodSpec(instance, "restore", script, false, nil),
			},
		},
	}
}

// backupPodSpec renders the Restricted Workload floor shared by every
// backup-capable workload (ADR 0001): fixed non-root identity 65532, the
// administrator-installed Localhost seccomp profile, no privilege
// escalation, all capabilities dropped, and no API-server token. The agent
// folder claim follows the StatefulSet's volumeClaimTemplate naming
// (<instance>-agent-folder-0).
func backupPodSpec(
	instance *typeclawv1alpha1.TypeClawInstance,
	containerName, script string,
	agentReadOnly bool,
	extraEnv []corev1.EnvVar,
) corev1.PodSpec {
	return corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyOnFailure,
		AutomountServiceAccountToken: boolRef(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: boolRef(true),
			RunAsUser:    int64Ref(RuntimeUID),
			RunAsGroup:   int64Ref(RuntimeGID),
			FSGroup:      int64Ref(RuntimeGID),
			SeccompProfile: &corev1.SeccompProfile{
				Type:             corev1.SeccompProfileTypeLocalhost,
				LocalhostProfile: strRef(SeccompLocalhostProfile),
			},
		},
		Containers: []corev1.Container{{
			Name:    containerName,
			Image:   BackupImage,
			Command: []string{"/bin/sh", "-ec", script},
			Env:     extraEnv,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolRef(false),
				ReadOnlyRootFilesystem:   boolRef(true),
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"ALL"},
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "agent-folder", MountPath: AgentMountPath, ReadOnly: agentReadOnly},
				{Name: "snapshots", MountPath: SnapshotsMountPath},
			},
		}},
		Volumes: []corev1.Volume{
			{
				Name: "agent-folder",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: fmt.Sprintf("%s-agent-folder-0", instance.Name),
					},
				},
			},
			{
				Name: "snapshots",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: SnapshotPVCName(instance.Name),
					},
				},
			},
		},
	}
}
