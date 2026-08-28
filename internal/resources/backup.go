// Package resources renders the Kubernetes workload half of a TypeClaw
// Instance. The backup builders here cover the scheduled snapshot pipeline
// (destination PVC plus pruning CronJob) and the guarded one-shot restore
// Job described in docs/adr/0004.
package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strconv"
	"strings"

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

// PublicWorkspaceMountPath is the only Agent Folder subtree visible to a
// backup workload. Mounting it with SubPath prevents backup code from opening
// sibling credential files even when the source PVC contains them.
const PublicWorkspaceMountPath = "/workspace"

const publicWorkspaceSubPath = "workspace"

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

// SnapshotPVC renders the destination volume receiving public workspace
// snapshots, sized by spec.backup.snapshotVolume.
func SnapshotPVC(instance *typeclawv1alpha1.TypeClawInstance) *corev1.PersistentVolumeClaim {
	claim := claimTemplate(SnapshotPVCName(instance.Name), instance.Spec.Backup.SnapshotVolume)
	claim.Namespace = instance.Namespace
	claim.Labels = Labels(instance)
	return &claim
}

// BackupCronJob renders the scheduled snapshot driver: one busybox Job per
// tick that tars only the public Agent Folder workspace and prunes snapshots
// beyond retention. spec.suspend mirrors the Instance so suspending an
// Instance also pauses snapshots while the runtime is scaled to zero.
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

// backupScript archives only the public workspace under the spawning Job's
// name, then prunes oldest archives beyond RETENTION. The excludes apply to
// both the workspace root and nested directories.
const backupScript = `tar czf "/snapshots/${JOB_NAME}.tar.gz" \
  --exclude='./.env' --exclude='./secrets.json' --exclude='./auth.json' \
  --exclude='*/.env' --exclude='*/secrets.json' --exclude='*/auth.json' \
  -C /workspace .
ls -1t /snapshots/*.tar.gz | tail -n +$((RETENTION+1)) | xargs -r rm -f`

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func restoreArchivePath(snapshotArchive string) string {
	if snapshotArchive == "" || strings.ContainsAny(snapshotArchive, `/\\`) {
		return shellQuote(SnapshotsMountPath + "/invalid-archive")
	}
	return shellQuote(SnapshotsMountPath + "/" + snapshotArchive)
}

// RestoreJob renders the one-shot Job unpacking a public workspace snapshot
// into the public workspace subtree. The guard exits with custom code 78 when
// the target is non-empty, so a restore can never silently overwrite a live
// workspace; the controller surfaces that exit code separately.
func RestoreJob(instance *typeclawv1alpha1.TypeClawInstance, snapshotArchive string) *batchv1.Job {

	restorePath := shellQuote(PublicWorkspaceMountPath)
	script := fmt.Sprintf(`[ -z "$(ls -A %s 2>/dev/null)" ] || { echo "restore aborted: public workspace target not empty (exit code 78)" >&2; exit 78; }
tar xzf %s --exclude='./.env' --exclude='./secrets.json' --exclude='./auth.json' --exclude='*/.env' --exclude='*/secrets.json' --exclude='*/auth.json' -C %s`, restorePath, restoreArchivePath(snapshotArchive), restorePath)
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
// escalation, all capabilities dropped, and no API-server token. The Agent
// Folder PVC is mounted only at its public workspace SubPath; credential
// siblings are not visible to the backup process.
// The source claim follows the StatefulSet's volumeClaimTemplate naming
// (<template>-<instance>-0 = agent-folder-<instance>-0).
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
				{
					Name:      "agent-folder",
					MountPath: PublicWorkspaceMountPath,
					SubPath:   publicWorkspaceSubPath,
					ReadOnly:  agentReadOnly,
				},
				{Name: "snapshots", MountPath: SnapshotsMountPath},
			},
		}},
		Volumes: []corev1.Volume{
			{
				Name: "agent-folder",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: fmt.Sprintf("agent-folder-%s-0", instance.Name),
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
