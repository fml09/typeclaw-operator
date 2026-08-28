// relay.go renders the restart relay attachment for one TypeClaw Instance:
// the sidecar container running the /relay binary baked into the operator
// image, plus the least-privilege Service Account, Role, and Role Binding it
// needs to delete exactly its own Pod and observe its own Instance's config.
// The pod-level Restricted Workload floor (run-as, fsGroup, seccomp,
// automountServiceAccountToken=false) is owned by the StatefulSet pod
// template; this file never relaxes it. Cluster authority reaches the
// sidecar exclusively through the projected service-account token volume
// returned by RelayTokenVolume, verified against the namespace trust bundle
// returned by RelayCAVolume.
package resources

import (
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

const (
	// RelayContainerName is the sidecar container name inside the Instance Pod.
	RelayContainerName = "restart-relay"

	// RelayTokenVolumeName pairs RelayTokenVolume with the sidecar mount.
	RelayTokenVolumeName = "relay-token"

	// RelayTokenMountPath is where the projected service-account token lands.
	RelayTokenMountPath = "/var/run/secrets/typeclaw-relay"

	// RelayTokenFileName is the projected token key under RelayTokenMountPath,
	// giving the canonical mount /var/run/secrets/typeclaw-relay/token.
	RelayTokenFileName = "token"

	// relayControlVolumeName matches the "managed-control" emptyDir already
	// rendered by StatefulSet, so the integrator only inserts the container
	// into the existing pod spec.
	relayControlVolumeName = "managed-control"

	// relayCAVolumeName carries the namespace trust bundle so the relay can
	// verify the API server without pod-level token automounting.
	relayCAVolumeName = "relay-ca"

	// relayCAMountPath mirrors the conventional service-account CA location
	// expected by client-go defaults.
	relayCAMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"

	// relayTokenExpirationSeconds bounds the projected token lifetime; the
	// kubelet rotates it automatically before expiry.
	relayTokenExpirationSeconds = 86400
)

// RelaySidecar renders the restart relay sidecar. It watches the Managed
// Control Dir for restart-request drops and deletes its own Pod when a valid
// request arrives. SelfConfig observation consumes a sanitized observation
// document from the dedicated runtime-to-relay volume (ADR 0005); it never
// mounts or traverses the Agent Folder. The control-dir mount assumes the
// "managed-control" volume already present in the Instance pod spec; callers
// add RelayTokenVolume, RelayCAVolume, and the observation volume to the pod.
func RelaySidecar(runtimeID, controlDir, image string) corev1.Container {
	return corev1.Container{
		Name:            RelayContainerName,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/relay"},
		Env: []corev1.EnvVar{
			{Name: "TYPECLAW_MANAGED_CONTROL_DIR", Value: controlDir},
			{Name: "TYPECLAW_RUNTIME_ID", Value: runtimeID},
			{Name: "TYPECLAW_SELF_CONFIG_OBSERVATION_FILE", Value: SelfConfigObservationFile},
			{
				Name: "POD_NAME",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				}},
			},
			{
				Name: "POD_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				}},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: relayControlVolumeName, MountPath: controlDir},
			{Name: RelayTokenVolumeName, MountPath: RelayTokenMountPath, ReadOnly: true},
			{Name: SelfConfigObservationVolumeName, MountPath: SelfConfigObservationDir, ReadOnly: true},
			{Name: relayCAVolumeName, MountPath: relayCAMountPath, ReadOnly: true},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolRef(false),
			ReadOnlyRootFilesystem:   boolRef(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}
}

// relayResourceName derives the shared name of the relay Service Account,
// Role, and Role Binding for one Instance.
func relayResourceName(instance *typeclawv1alpha1.TypeClawInstance) string {
	return instance.Name + "-relay"
}

// RelayServiceAccount renders the dedicated identity for the relay sidecar.
// The Instance Pod keeps automountServiceAccountToken=false; the sidecar
// receives this identity solely through RelayTokenVolume.
func RelayServiceAccount(instance *typeclawv1alpha1.TypeClawInstance) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      relayResourceName(instance),
			Namespace: instance.Namespace,
			Labels:    Labels(instance),
		},
	}
}

// RelayRole renders a namespace-scoped Role allowing the relay to delete
// exactly its own StatefulSet Pod (<instance>-0) and, for SelfConfig
// observation (ADR 0005), to read its own Instance and fill its selfConfig
// status block. Every grant is resourceNames-restricted; nothing else.
func RelayRole(instance *typeclawv1alpha1.TypeClawInstance) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      relayResourceName(instance),
			Namespace: instance.Namespace,
			Labels:    Labels(instance),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"pods"},
				ResourceNames: []string{instance.Name + "-0"},
				Verbs:         []string{"get", "delete"},
			},
			{
				APIGroups:     []string{typeclawv1alpha1.GroupVersion.Group},
				Resources:     []string{"typeclawinstances"},
				ResourceNames: []string{instance.Name},
				Verbs:         []string{"get"},
			},
			{
				APIGroups:     []string{typeclawv1alpha1.GroupVersion.Group},
				Resources:     []string{"typeclawinstances/status"},
				ResourceNames: []string{instance.Name},
				Verbs:         []string{"get", "patch"},
			},
		},
	}
}

// RelayRoleBinding binds the relay Service Account to RelayRole.
func RelayRoleBinding(instance *typeclawv1alpha1.TypeClawInstance) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      relayResourceName(instance),
			Namespace: instance.Namespace,
			Labels:    Labels(instance),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     relayResourceName(instance),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      relayResourceName(instance),
			Namespace: instance.Namespace,
		}},
	}
}

// RelayTokenVolume renders the projected service-account token volume for
// the relay sidecar. Pod-level automount stays disabled; this projection is
// the only channel carrying the relay identity into the Pod.
func RelayTokenVolume() corev1.Volume {
	return corev1.Volume{
		Name: RelayTokenVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Path:              RelayTokenFileName,
						ExpirationSeconds: int64Ref(relayTokenExpirationSeconds),
					},
				}},
			},
		},
	}
}

// RelayCAVolume mounts the auto-created kube-root-ca.crt ConfigMap so the
// relay can verify the API server while service-account token automounting
// stays disabled at the Pod level.
func RelayCAVolume() corev1.Volume {
	optional := false
	return corev1.Volume{
		Name: relayCAVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
				Items: []corev1.KeyToPath{{
					Key:  "ca.crt",
					Path: "ca.crt",
				}},
				Optional: &optional,
			},
		},
	}
}
