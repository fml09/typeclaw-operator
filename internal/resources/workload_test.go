package resources

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func instance(name string, mutate func(*typeclawv1alpha1.TypeClawInstance)) *typeclawv1alpha1.TypeClawInstance {
	in := &typeclawv1alpha1.TypeClawInstance{}
	in.Name = name
	in.Namespace = "agents"
	if mutate != nil {
		mutate(in)
	}
	return in
}
func TestResolveRuntimeImage(t *testing.T) {
	tests := []struct {
		name string
		spec typeclawv1alpha1.TypeClawInstanceSpec
		want string
	}{
		{
			name: "version pairs with default fork repository",
			spec: typeclawv1alpha1.TypeClawInstanceSpec{Runtime: typeclawv1alpha1.RuntimeSpec{Version: "0.48.7"}},
			want: "ghcr.io/fml09/typeclaw-runtime:0.48.7",
		},
		{
			name: "empty spec falls back to tracked default release",
			spec: typeclawv1alpha1.TypeClawInstanceSpec{},
			want: "ghcr.io/fml09/typeclaw-runtime:" + typeclawv1alpha1.DefaultRuntimeVersion,
		},
		{
			name: "explicit image override wins over version",
			spec: typeclawv1alpha1.TypeClawInstanceSpec{
				Runtime: typeclawv1alpha1.RuntimeSpec{Version: "0.48.7", Image: "ghcr.io/fml09/typeclaw-runtime@sha256:abc"},
			},
			want: "ghcr.io/fml09/typeclaw-runtime@sha256:abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveRuntimeImage(tt.spec); got != tt.want {
				t.Fatalf("ResolveRuntimeImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatefulSetEncodesManagedRuntimeContract(t *testing.T) {
	in := instance("kakao-agent", nil)
	sts, err := StatefulSet(in)
	if err != nil {
		t.Fatalf("StatefulSet() error: %v", err)
	}

	pod := sts.Spec.Template.Spec

	if got := *sts.Spec.Replicas; got != 1 {
		t.Errorf("replicas = %d, want single-active 1", got)
	}

	podCtx := pod.SecurityContext
	if !*podCtx.RunAsNonRoot || *podCtx.RunAsUser != RuntimeUID || *podCtx.RunAsGroup != RuntimeGID || *podCtx.FSGroup != RuntimeGID {
		t.Errorf("pod security context does not pin non-root identity 65532: %+v", podCtx)
	}
	profile := podCtx.SeccompProfile
	if profile.Type != corev1.SeccompProfileTypeLocalhost || profile.LocalhostProfile == nil || *profile.LocalhostProfile != SeccompLocalhostProfile {
		// ADR 0001: failure never degrades Localhost to RuntimeDefault/Unconfined.
		t.Errorf("seccomp must be the immutable Localhost bwrap profile, got %+v", profile)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		// ADR 0001: the data plane receives no Kubernetes credential.
		t.Errorf("service account token automount must be disabled")
	}
	if *pod.TerminationGracePeriodSeconds <= 0 {
		t.Errorf("grace period must allow awaited teardown, got %d", *pod.TerminationGracePeriodSeconds)
	}

	runtime := pod.Containers[0]
	ctx := runtime.SecurityContext
	if !*ctx.ReadOnlyRootFilesystem || *ctx.AllowPrivilegeEscalation {
		t.Errorf("runtime must run read-only root without privilege escalation: %+v", ctx)
	}
	if len(ctx.Capabilities.Drop) == 0 || ctx.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities must be fully dropped, got %+v", ctx.Capabilities)
	}

	env := map[string]string{}
	for _, e := range runtime.Env {
		env[e.Name] = e.Value
	}
	if env["TYPECLAW_DEPLOYMENT_PROFILE"] != "managed" {
		t.Errorf("deployment profile env missing, got %v", env)
	}
	if env["TYPECLAW_RUNTIME_ID"] != "agents/kakao-agent" {
		t.Errorf("stable runtime identity env wrong, got %q", env["TYPECLAW_RUNTIME_ID"])
	}
	if env["TYPECLAW_MANAGED_CONTROL_DIR"] != ManagedControlDir {
		t.Errorf("control dir env wrong, got %q", env["TYPECLAW_MANAGED_CONTROL_DIR"])
	}
	if _, ok := env["TZ"]; ok {
		t.Errorf("unset runtime timezone must not add TZ, got %q", env["TZ"])
	}

	if p := runtime.LivenessProbe.HTTPGet; p.Path != "/health/live" || p.Port.IntValue() != ContainerPort {
		t.Errorf("liveness probe must target /health/live on %d, got %+v", ContainerPort, p)
	}
	if p := runtime.ReadinessProbe.HTTPGet; p.Path != "/health/ready" || p.Port.IntValue() != ContainerPort {
		t.Errorf("readiness probe must target /health/ready on %d, got %+v", ContainerPort, p)
	}
}

func TestStatefulSetInjectsRuntimeTimezone(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Runtime.Timezone = "Asia/Seoul"
	})
	sts, err := StatefulSet(in)
	if err != nil {
		t.Fatalf("StatefulSet() error: %v", err)
	}

	var found *corev1.EnvVar
	for i := range sts.Spec.Template.Spec.Containers[0].Env {
		env := &sts.Spec.Template.Spec.Containers[0].Env[i]
		if env.Name == "TZ" {
			found = env
			break
		}
	}
	if found == nil {
		t.Fatal("runtime timezone must render TZ")
	}
	if found.Value != "Asia/Seoul" || found.ValueFrom != nil {
		t.Fatalf("runtime timezone env = %+v, want literal Asia/Seoul", *found)
	}
}

func TestStatefulSetVolumesAndMounts(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Storage.RuntimeHome = &typeclawv1alpha1.VolumeClaimSpec{Size: resource.MustParse("2Gi")}
	})
	sts, err := StatefulSet(in)
	if err != nil {
		t.Fatalf("StatefulSet() error: %v", err)
	}
	pod := sts.Spec.Template.Spec

	claimNames := map[string]bool{}
	for _, c := range sts.Spec.VolumeClaimTemplates {
		claimNames[c.Name] = true
		if len(c.Spec.AccessModes) != 1 || c.Spec.AccessModes[0] != corev1.ReadWriteOnce {
			t.Errorf("claim %s must be ReadWriteOnce", c.Name)
		}
	}
	if !claimNames["agent-folder"] {
		t.Errorf("agent-folder PVC template missing: %v", claimNames)
	}
	if !claimNames["runtime-home"] {
		t.Errorf("configured runtime-home must be a durable PVC template")
	}

	mounts := map[string]string{}
	for _, m := range pod.Containers[0].VolumeMounts {
		mounts[m.MountPath] = m.Name
	}
	for path, vol := range map[string]string{
		AgentMountPath:       "agent-folder",
		RuntimeHomeMountPath: "runtime-home",
		ManagedControlDir:    "managed-control",
		"/tmp":               "runtime-tmp",
		"/dev/shm":           "browser-shm",
	} {
		if mounts[path] != vol {
			t.Errorf("mount %s = %q, want %q", path, mounts[path], vol)
		}
	}

	tmpVol := pod.Volumes[1]
	if tmpVol.EmptyDir == nil || tmpVol.EmptyDir.Medium != corev1.StorageMediumMemory || tmpVol.EmptyDir.SizeLimit.IsZero() {
		t.Errorf("/tmp must be sized memory-backed emptyDir for Xvfb, got %+v", tmpVol)
	}
	shmVol := pod.Volumes[2]
	if shmVol.EmptyDir == nil || shmVol.EmptyDir.SizeLimit.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("browser shared memory must stay at 512Mi limit, got %+v", shmVol)
	}
}

func TestStatefulSetSuspendScalesToZero(t *testing.T) {
	in := instance("paused", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Suspend = true
	})
	sts, err := StatefulSet(in)
	if err != nil {
		t.Fatalf("StatefulSet() error: %v", err)
	}
	if *sts.Spec.Replicas != 0 {
		t.Fatalf("suspend must scale to zero, got %d", *sts.Spec.Replicas)
	}
	claims := sts.Spec.VolumeClaimTemplates
	if len(claims) == 0 || claims[0].Name != "agent-folder" {
		t.Fatalf("suspend must keep Agent Folder storage intact")
	}
}

func TestStatefulSetRuntimeHomeDefaultsToEphemeral(t *testing.T) {
	sts, err := StatefulSet(instance("ephemeral", nil))
	if err != nil {
		t.Fatalf("StatefulSet() error: %v", err)
	}
	var homeClaim bool
	for _, c := range sts.Spec.VolumeClaimTemplates {
		if c.Name == "runtime-home" {
			homeClaim = true
		}
	}
	if homeClaim {
		t.Fatalf("unset runtime-home must not create a PVC template")
	}
	found := false
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name == "runtime-home" && v.EmptyDir != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("unset runtime-home must still mount an emptyDir at %s", RuntimeHomeMountPath)
	}
}

func TestServiceExposesFixedServerPort(t *testing.T) {
	svc := Service(instance("kakao-agent", nil))
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("TUI exposure must default to ClusterIP, got %q", svc.Spec.Type)
	}
	port := svc.Spec.Ports[0]
	if port.Port != ContainerPort || port.TargetPort.IntValue() != ContainerPort {
		t.Errorf("service port mismatch: %+v", port)
	}
	if svc.Spec.Selector["app.kubernetes.io/instance"] != "kakao-agent" {
		t.Errorf("selector must target this instance: %+v", svc.Spec.Selector)
	}
}

func TestStatefulSetRelaySidecarWiring(t *testing.T) {
	sts, err := StatefulSet(instance("kakao-agent", nil))
	if err != nil {
		t.Fatalf("StatefulSet() error: %v", err)
	}
	pod := sts.Spec.Template.Spec
	if len(pod.Containers) != 2 {
		t.Fatalf("default must run runtime plus relay sidecar, got %d containers", len(pod.Containers))
	}
	sidecar := pod.Containers[1]
	if sidecar.Name != RelayContainerName || sidecar.Image != DefaultOperatorImage {
		t.Errorf("sidecar misrendered: name=%q image=%q", sidecar.Name, sidecar.Image)
	}
	if pod.ServiceAccountName != "kakao-agent-relay" {
		t.Errorf("pod must use the dedicated relay identity, got %q", pod.ServiceAccountName)
	}
	var tokenVolume bool
	for _, v := range pod.Volumes {
		if v.Name == RelayTokenVolumeName && v.Projected != nil {
			tokenVolume = true
		}
	}
	if !tokenVolume {
		t.Errorf("projected relay token volume missing")
	}

	disabled := instance("quiet", func(in *typeclawv1alpha1.TypeClawInstance) {
		f := false
		in.Spec.RestartRelay = &f
	})
	stsOff, err := StatefulSet(disabled)
	if err != nil {
		t.Fatalf("StatefulSet() error: %v", err)
	}
	podOff := stsOff.Spec.Template.Spec
	if len(podOff.Containers) != 1 || podOff.ServiceAccountName != "" {
		t.Errorf("disabled relay must drop sidecar and identity: containers=%d sa=%q",
			len(podOff.Containers), podOff.ServiceAccountName)
	}
}

func TestStatefulSetClaimRetentionPolicy(t *testing.T) {
	defaults, err := StatefulSet(instance("kakao-agent", nil))
	if err != nil {
		t.Fatalf("StatefulSet() error: %v", err)
	}
	policy := defaults.Spec.PersistentVolumeClaimRetentionPolicy
	if policy == nil || policy.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatalf("unset onInstanceDeletion must retain claims on Instance deletion")
	}

	del := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Storage.OnInstanceDeletion = "Delete"
	})
	deleting, err := StatefulSet(del)
	if err != nil {
		t.Fatalf("StatefulSet() error: %v", err)
	}
	policy = deleting.Spec.PersistentVolumeClaimRetentionPolicy
	if policy.WhenDeleted != appsv1.DeletePersistentVolumeClaimRetentionPolicyType {
		t.Fatalf("explicit Delete must map to claim deletion, got %v", policy.WhenDeleted)
	}
	if policy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatalf("suspend-to-zero must never delete claims, got WhenScaled=%v", policy.WhenScaled)
	}
}

func TestStatefulSetPromotedImageWinsDuringRollout(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Status.Update = &typeclawv1alpha1.UpdateStatus{
			Phase:         typeclawv1alpha1.UpdatePhaseUpdating,
			PromotedImage: "ghcr.io/fml09/typeclaw-runtime:0.49.0",
		}
	})
	sts, err := StatefulSet(in)
	if err != nil {
		t.Fatalf("StatefulSet() error: %v", err)
	}
	if got := sts.Spec.Template.Spec.Containers[0].Image; got != "ghcr.io/fml09/typeclaw-runtime:0.49.0" {
		t.Fatalf("active promotion must drive the rendered image, got %q", got)
	}
}
