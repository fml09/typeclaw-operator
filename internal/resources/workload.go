// Package resources renders the Kubernetes workload half of a TypeClaw
// Instance. Every builder encodes the upstream managed runtime contract
// (fml09/typeclaw feat/managed-runtime-contract,
// docs/content/docs/internals/managed-runtime.mdx) and the operator security
// decisions in docs/adr/0001 and docs/adr/0003.
package resources

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

const (
	// RuntimeUID and RuntimeGID are the fixed non-root identity of the
	// managed runtime image.
	RuntimeUID = 65532
	RuntimeGID = 65532

	// ContainerPort is TypeClaw's fixed internal server port
	// (src/container/port.ts CONTAINER_PORT); TUI WebSocket, health, and the
	// legacy banner endpoint share it.
	ContainerPort = 8973

	// ManagedControlDir receives atomic restart-request files written by the
	// runtime spool contract.
	ManagedControlDir = "/run/typeclaw-managed"

	// AgentMountPath carries the complete Agent Folder.
	AgentMountPath = "/agent"

	// RuntimeHomeMountPath holds local CLI credentials and channel
	// encryption keys outside the Agent Folder boundary.
	RuntimeHomeMountPath = "/home/typeclaw"

	// DefaultRuntimeRepository pairs with spec.runtime.version (ADR 0003).
	DefaultRuntimeRepository = "ghcr.io/fml09/typeclaw-runtime"

	// SeccompLocalhostProfile is the administrator-installed Localhost
	// profile admitting exactly the bubblewrap syscall set (ADR 0001,
	// Native + LocalBwrap baseline). Clusters without the profile fail
	// closed at Pod start; the operator never degrades to RuntimeDefault
	// or Unconfined.
	SeccompLocalhostProfile = "localhost/profiles/typeclaw/native-localbwrap.json"

	// DefaultOperatorImage carries the restart-relay binary. Version-coupled
	// to the chart appVersion so sidecar and manager upgrade together.
	DefaultOperatorImage = "ghcr.io/fml09/typeclaw-operator:0.1.0"

	gracePeriodSeconds = 120
	tmpMemorySize      = "256Mi"
	shmMemorySize      = "512Mi"
)

// Labels returns the immutable selector labels for an Instance workload.
func Labels(instance *typeclawv1alpha1.TypeClawInstance) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "typeclaw",
		"app.kubernetes.io/instance":   instance.Name,
		"app.kubernetes.io/managed-by": "typeclaw-operator",
	}
}

// ResolveRuntimeImage pins the workload image: an explicit spec.runtime.image
// wins; otherwise the repository is paired with spec.runtime.version, falling
// back to the tracked default release (ADR 0003).
func ResolveRuntimeImage(spec typeclawv1alpha1.TypeClawInstanceSpec) string {
	if spec.Runtime.Image != "" {
		return spec.Runtime.Image
	}
	version := spec.Runtime.Version
	if version == "" {
		version = typeclawv1alpha1.DefaultRuntimeVersion
	}
	return fmt.Sprintf("%s:%s", DefaultRuntimeRepository, version)
}

func claimTemplate(name string, spec typeclawv1alpha1.VolumeClaimSpec) corev1.PersistentVolumeClaim {
	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: spec.Size},
			},
		},
	}
	if spec.StorageClassName != nil {
		claim.Spec.StorageClassName = spec.StorageClassName
	}
	return claim
}

// StatefulSet renders the single-active workload. One replica owns one Agent
// Folder; suspend scales to zero without deleting state.
//
// The upstream contract's reference Pod sketch uses a root ownership-repair
// init container; ADR 0001 forbids that path, so ownership relies on
// fsGroup(65532). The control-directory 0700 guarantee that fsGroup cannot
// establish stays an open item under operator issue #7 rather than being
// solved with privilege here.
func StatefulSet(instance *typeclawv1alpha1.TypeClawInstance) (*appsv1.StatefulSet, error) {
	labels := Labels(instance)

	claims := []corev1.PersistentVolumeClaim{
		claimTemplate("agent-folder", instance.Spec.Storage.AgentFolder),
	}
	volumes := []corev1.Volume{
		{
			Name:         "managed-control",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: "runtime-tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: resource.NewQuantity(mustParseBytes(tmpMemorySize), resource.BinarySI),
			}},
		},
		{
			Name: "browser-shm",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: resource.NewQuantity(mustParseBytes(shmMemorySize), resource.BinarySI),
			}},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: "agent-folder", MountPath: AgentMountPath},
		{Name: "managed-control", MountPath: ManagedControlDir},
		{Name: "runtime-tmp", MountPath: "/tmp"},
		{Name: "browser-shm", MountPath: "/dev/shm"},
	}

	if home := instance.Spec.Storage.RuntimeHome; home != nil {
		claims = append(claims, claimTemplate("runtime-home", *home))
		mounts = append(mounts, corev1.VolumeMount{Name: "runtime-home", MountPath: RuntimeHomeMountPath})
	} else {
		volumes = append(volumes, corev1.Volume{
			Name:         "runtime-home",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "runtime-home", MountPath: RuntimeHomeMountPath})
	}

	relayEnabled := instance.Spec.RestartRelay == nil || *instance.Spec.RestartRelay
	runtimeID := fmt.Sprintf("%s/%s", instance.Namespace, instance.Name)

	replicas := int32(1)
	if instance.Spec.Suspend {
		replicas = 0
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: instance.Name,
			Replicas:    &replicas,
			// Suspend-to-zero must never delete claims; only the explicit
			// onInstanceDeletion policy decides removal.
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: claimRetention(instance),
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     labels["app.kubernetes.io/name"],
					"app.kubernetes.io/instance": labels["app.kubernetes.io/instance"],
				},
			},
			VolumeClaimTemplates: claims,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken:  boolRef(false),
					TerminationGracePeriodSeconds: func() *int64 { v := int64(gracePeriodSeconds); return &v }(),
					ServiceAccountName:            serviceAccountNameFor(relayEnabled, instance),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolRef(true),
						RunAsUser:    int64Ref(RuntimeUID),
						RunAsGroup:   int64Ref(RuntimeGID),
						FSGroup:      int64Ref(RuntimeGID),
						SeccompProfile: seccompProfileFor(
							instance.Spec.Security,
							corev1.SeccompProfile{
								Type:             corev1.SeccompProfileTypeLocalhost,
								LocalhostProfile: strRef(SeccompLocalhostProfile),
							},
						),
					},
					Containers: []corev1.Container{{
						Name:            "runtime",
						Image:           EffectiveRuntimeImage(instance),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env: []corev1.EnvVar{
							{Name: "TYPECLAW_DEPLOYMENT_PROFILE", Value: "managed"},
							{Name: "TYPECLAW_RUNTIME_ID", Value: fmt.Sprintf("%s/%s", instance.Namespace, instance.Name)},
							{Name: "TYPECLAW_MANAGED_CONTROL_DIR", Value: ManagedControlDir},
						},
						Ports: []corev1.ContainerPort{{
							Name:          "server",
							ContainerPort: ContainerPort,
							Protocol:      corev1.ProtocolTCP,
						}},
						LivenessProbe: probe("/health/live"),
						ReadinessProbe: func() *corev1.Probe {
							p := probe("/health/ready")
							p.InitialDelaySeconds = 5
							return p
						}(),
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolRef(false),
							ReadOnlyRootFilesystem:   boolRef(true),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	if relayEnabled {
		sts.Spec.Template.Spec.Containers = append(
			sts.Spec.Template.Spec.Containers,
			RelaySidecar(runtimeID, ManagedControlDir, DefaultOperatorImage),
		)
		sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes, RelayTokenVolume(), RelayCAVolume())
	}
	return sts, nil
}

// seccompProfileFor resolves the declared seccomp posture. Unset or Localhost
// keeps the ADR 0001 baseline; Unconfined is an explicit environment escape
// hatch for clusters that cannot host profiles yet (ADR 0006).
func seccompProfileFor(sec *typeclawv1alpha1.SecuritySpec, baseline corev1.SeccompProfile) *corev1.SeccompProfile {
	if sec != nil && sec.SeccompProfile == "Unconfined" {
		return &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	}
	return &baseline
}

// claimRetention maps the declared deletion policy onto the native StatefulSet
// claim retention. Anything but an explicit Delete retains the Agent Folder.
func claimRetention(instance *typeclawv1alpha1.TypeClawInstance) appsv1.PersistentVolumeClaimRetentionPolicyType {
	if instance.Spec.Storage.OnInstanceDeletion == "Delete" {
		return appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	}
	return appsv1.RetainPersistentVolumeClaimRetentionPolicyType
}

// serviceAccountNameFor points the Pod at the relay identity only when the
// relay runs; the projected token volume sources from this identity.
func serviceAccountNameFor(relayEnabled bool, instance *typeclawv1alpha1.TypeClawInstance) string {
	if !relayEnabled {
		return ""
	}
	return relayResourceName(instance)
}

// Service exposes the runtime server (TUI WebSocket and health endpoints)
// inside the cluster when spec.exposeTUI is not explicitly false.
func Service(instance *typeclawv1alpha1.TypeClawInstance) *corev1.Service {
	labels := Labels(instance)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "server",
				Port:       ContainerPort,
				TargetPort: intstr.FromInt32(ContainerPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func probe(path string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromInt32(ContainerPort),
			},
		},
		PeriodSeconds: 10,
	}
}

func mustParseBytes(s string) int64 {
	q := resource.MustParse(s)
	return q.Value()
}

func boolRef(v bool) *bool { return &v }

func int64Ref(v int64) *int64 { return &v }

func strRef(v string) *string { return &v }
