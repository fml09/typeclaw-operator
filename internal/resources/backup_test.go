package resources

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func backupInstanceForRender() *typeclawv1alpha1.TypeClawInstance {
	size := resource.MustParse("10Gi")
	class := "fast-ssd"
	return &typeclawv1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "kakao-agent", Namespace: "agents"},
		Spec: typeclawv1alpha1.TypeClawInstanceSpec{
			Suspend: true,
			Backup: &typeclawv1alpha1.BackupSpec{
				Schedule:       "17 * * * *",
				Retention:      5,
				SnapshotVolume: typeclawv1alpha1.VolumeClaimSpec{Size: size, StorageClassName: &class},
			},
		},
	}
}

func TestSnapshotPVCRendersClaimFromBackupSpec(t *testing.T) {
	in := backupInstanceForRender()
	pvc := SnapshotPVC(in)

	if pvc.Name != "kakao-agent-snapshots" {
		t.Fatalf("SnapshotPVC() name = %q, want kakao-agent-snapshots", pvc.Name)
	}
	modes := pvc.Spec.AccessModes
	if len(modes) != 1 || modes[0] != corev1.ReadWriteOnce {
		t.Fatalf("SnapshotPVC() accessModes = %v, want [ReadWriteOnce]", modes)
	}
	got := pvc.Spec.Resources.Requests.Storage().String()
	if got != "10Gi" {
		t.Fatalf("SnapshotPVC() storage = %q, want 10Gi", got)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-ssd" {
		t.Fatalf("SnapshotPVC() storageClass = %v, want fast-ssd", pvc.Spec.StorageClassName)
	}
	if pvc.Labels["app.kubernetes.io/instance"] != in.Name {
		t.Fatalf("SnapshotPVC() missing instance label: %v", pvc.Labels)
	}
}

func TestBackupCronJobMirrorsSpecAndSuspend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		suspend bool
	}{
		{name: "running", suspend: false},
		{name: "suspended", suspend: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := backupInstanceForRender()
			in.Spec.Suspend = tc.suspend
			cj := BackupCronJob(in)

			if cj.Name != "kakao-agent-backup" {
				t.Fatalf("BackupCronJob() name = %q, want kakao-agent-backup", cj.Name)
			}
			if cj.Spec.Schedule != "17 * * * *" {
				t.Fatalf("Schedule = %q, want spec value", cj.Spec.Schedule)
			}
			if cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
				t.Fatalf("ConcurrencyPolicy = %v, want Forbid", cj.Spec.ConcurrencyPolicy)
			}
			if cj.Spec.SuccessfulJobsHistoryLimit == nil || *cj.Spec.SuccessfulJobsHistoryLimit != 3 {
				t.Fatalf("SuccessfulJobsHistoryLimit = %v, want 3", cj.Spec.SuccessfulJobsHistoryLimit)
			}
			if cj.Spec.FailedJobsHistoryLimit == nil || *cj.Spec.FailedJobsHistoryLimit != 1 {
				t.Fatalf("FailedJobsHistoryLimit = %v, want 1", cj.Spec.FailedJobsHistoryLimit)
			}
			if cj.Spec.Suspend == nil || *cj.Spec.Suspend != tc.suspend {
				t.Fatalf("Suspend = %v, want %t", cj.Spec.Suspend, tc.suspend)
			}
			pod := cj.Spec.JobTemplate.Spec.Template.Spec
			if img := pod.Containers[0].Image; img != BackupImage {
				t.Fatalf("image = %q, want %q", img, BackupImage)
			}
		})
	}
}

func TestBackupCronJobScriptAndMounts(t *testing.T) {
	in := backupInstanceForRender()
	cj := BackupCronJob(in)

	cmd := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command
	script := cmd[len(cmd)-1]
	if !strings.Contains(script, `tar czf "/snapshots/${JOB_NAME}.tar.gz" -C /agent .`) {
		t.Fatalf("script missing archive step:\n%s", script)
	}
	if !strings.Contains(script, `tail -n +$((RETENTION+1))`) {
		t.Fatalf("script missing retention pruning:\n%s", script)
	}

	container := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	var retention, jobName string
	for _, env := range container.Env {
		switch env.Name {
		case "RETENTION":
			retention = env.Value
		case "JOB_NAME":
			if env.ValueFrom == nil || env.ValueFrom.FieldRef == nil ||
				env.ValueFrom.FieldRef.FieldPath != "metadata.name" {
				t.Fatal("JOB_NAME must come from downward API metadata.name")
			}
			jobName = "downward"
		}
	}
	if retention != "5" {
		t.Fatalf("RETENTION = %q, want 5 (spec.retention)", retention)
	}
	if jobName == "" {
		t.Fatal("JOB_NAME env missing")
	}

	mounts := map[string]corev1.VolumeMount{}
	for _, m := range container.VolumeMounts {
		mounts[m.MountPath] = m
	}
	agent, ok := mounts["/agent"]
	if !ok || !agent.ReadOnly {
		t.Fatalf("/agent mount = %+v, want read-only", agent)
	}
	snapshots, ok := mounts["/snapshots"]
	if !ok || snapshots.ReadOnly {
		t.Fatalf("/snapshots mount = %+v, want read-write", snapshots)
	}
	volumes := map[string]corev1.Volume{}
	for _, v := range cj.Spec.JobTemplate.Spec.Template.Spec.Volumes {
		volumes[v.Name] = v
	}
	if got := volumes["agent-folder"].PersistentVolumeClaim.ClaimName; got != "kakao-agent-agent-folder-0" {
		t.Fatalf("agent volume claim = %q, want kakao-agent-agent-folder-0", got)
	}
	if got := volumes["snapshots"].PersistentVolumeClaim.ClaimName; got != "kakao-agent-snapshots" {
		t.Fatalf("snapshots volume claim = %q, want kakao-agent-snapshots", got)
	}
}

// TestBackupCronJobRestrictedFloor pins the Restricted Workload floor on the
// rendered snapshot podspec.
func TestBackupCronJobRestrictedFloor(t *testing.T) {
	assertRestrictedFloor(t, BackupCronJob(backupInstanceForRender()).Spec.JobTemplate.Spec.Template.Spec)
}

func TestRestoreJobGuardsNonEmptyTarget(t *testing.T) {
	in := backupInstanceForRender()
	job := RestoreJob(in, "kakao-agent-backup-12345.tar.gz")

	if job.Name != RestoreJobName(in.Name, "kakao-agent-backup-12345.tar.gz") {
		t.Fatalf("RestoreJob() name %q does not match RestoreJobName()", job.Name)
	}
	if !strings.HasPrefix(job.Name, "kakao-agent-restore-") || len(job.Name) <= len("kakao-agent-restore-") {
		t.Fatalf("RestoreJob() name %q lacks a short hash suffix", job.Name)
	}
	if RestoreJob(in, "other.tar.gz").Name == job.Name {
		t.Fatal("distinct snapshots must map to distinct restore Jobs")
	}

	cmd := job.Spec.Template.Spec.Containers[0].Command
	script := cmd[len(cmd)-1]
	if !strings.Contains(script, `[ ! -e /agent/typeclaw.json ] ||`) || !strings.Contains(script, "exit 78") {
		t.Fatalf("script missing empty-target guard:\n%s", script)
	}
	if !strings.Contains(script, `tar xzf "/snapshots/kakao-agent-backup-12345.tar.gz" -C /agent`) {
		t.Fatalf("script missing unpack of requested archive:\n%s", script)
	}

	var agent corev1.VolumeMount
	found := false
	for _, m := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/agent" {
			agent, found = m, true
		}
	}
	if !found || agent.ReadOnly {
		t.Fatalf("/agent mount = %+v, want read-write for restore", agent)
	}
	assertRestrictedFloor(t, job.Spec.Template.Spec)
}

// assertRestrictedFloor enforces the Restricted Workload floor shared by
// every backup workload: fixed non-root identity, Localhost seccomp profile,
// no privilege escalation, all capabilities dropped, no API token.
func assertRestrictedFloor(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	sc := pod.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
		sc.RunAsUser == nil || *sc.RunAsUser != RuntimeUID ||
		sc.RunAsGroup == nil || *sc.RunAsGroup != RuntimeGID ||
		sc.FSGroup == nil || *sc.FSGroup != RuntimeGID {
		t.Fatalf("pod securityContext violates Restricted identity floor: %+v", sc)
	}
	if sc.SeccompProfile == nil ||
		sc.SeccompProfile.Type != corev1.SeccompProfileTypeLocalhost ||
		sc.SeccompProfile.LocalhostProfile == nil ||
		*sc.SeccompProfile.LocalhostProfile != SeccompLocalhostProfile {
		t.Fatalf("seccomp profile must pin %q: %+v", SeccompLocalhostProfile, sc.SeccompProfile)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("automountServiceAccountToken must be false")
	}
	for _, c := range pod.Containers {
		cc := c.SecurityContext
		if cc == nil || cc.AllowPrivilegeEscalation == nil || *cc.AllowPrivilegeEscalation {
			t.Fatalf("container %q must forbid privilege escalation", c.Name)
		}
		if cc.Capabilities == nil || len(cc.Capabilities.Drop) != 1 || cc.Capabilities.Drop[0] != "ALL" {
			t.Fatalf("container %q must drop ALL capabilities", c.Name)
		}
	}
}
