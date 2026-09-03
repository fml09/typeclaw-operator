// gateway.go renders the Desktop Gateway: the one component of this feature
// that holds a Kubernetes credential.
//
// KubeVirt exposes VNC, screenshots and power as Kubernetes subresources, so
// there is no way to relay a console without a token that can call them. ADR
// 0007 accepts that and bounds it here: a dedicated ServiceAccount, a Role
// restricted with resourceNames to this one VirtualMachine, and nothing else.
// The Managed Runtime and the computer-use plugin still hold no Kubernetes
// credential at all — they reach the desktop only through this process, with a
// bearer token that means nothing to the API server.
package desktop

import (
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

const (
	// GatewayCommand is the gateway binary baked into the operator image.
	GatewayCommand = "/desktop-gateway"

	// gatewayUID is the distroless nonroot identity of the operator image.
	gatewayUID = 65532
)

// GatewayImage resolves the Desktop Gateway image. The gateway binary ships
// inside the operator image, so manager and gateway upgrade together unless an
// administrator pins an override.
func GatewayImage(instance *typeclawv1alpha1.TypeClawInstance, operatorImage string) string {
	if spec := instance.Spec.PersonalDesktop; spec != nil && spec.Gateway != nil && spec.Gateway.Image != "" {
		return spec.Gateway.Image
	}
	return operatorImage
}

// GatewayServiceAccount is the identity the gateway's Role is bound to.
func GatewayServiceAccount(instance *typeclawv1alpha1.TypeClawInstance) *corev1.ServiceAccount {
	names := Names(instance)
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Gateway,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
	}
}

// GatewayRole grants exactly the KubeVirt subresources the console needs, on
// exactly this desktop. Every rule is resourceNames-restricted, so the
// credential is worthless against any other VM in the namespace.
func GatewayRole(instance *typeclawv1alpha1.TypeClawInstance) *rbacv1.Role {
	names := Names(instance)
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Gateway,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{"kubevirt.io"},
				Resources:     []string{"virtualmachines", "virtualmachineinstances"},
				ResourceNames: []string{names.Desktop},
				Verbs:         []string{"get"},
			},
			{
				APIGroups:     []string{"subresources.kubevirt.io"},
				Resources:     []string{"virtualmachineinstances/vnc", "virtualmachineinstances/vnc/screenshot"},
				ResourceNames: []string{names.Desktop},
				Verbs:         []string{"get"},
			},
			{
				APIGroups:     []string{"subresources.kubevirt.io"},
				Resources:     []string{"virtualmachines/start", "virtualmachines/stop"},
				ResourceNames: []string{names.Desktop},
				Verbs:         []string{"update"},
			},
		},
	}
}

// GatewayRoleBinding binds the gateway identity to GatewayRole.
func GatewayRoleBinding(instance *typeclawv1alpha1.TypeClawInstance) *rbacv1.RoleBinding {
	names := Names(instance)
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Gateway,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     names.Gateway,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      names.Gateway,
			Namespace: names.Namespace,
		}},
	}
}

// GatewayDeployment renders the gateway workload. consoleURL is the published
// console address once the Ingress reports one, and is empty before that.
func GatewayDeployment(instance *typeclawv1alpha1.TypeClawInstance, operatorImage, consoleURL string) *appsv1.Deployment {
	names := Names(instance)
	replicas := int32(1)
	automount := true

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Gateway,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			// Recreate, not RollingUpdate: input leases live in the gateway's
			// memory, and two replicas would each believe they hold the
			// exclusive Input Controller lease on the same desktop.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: GatewayLabels(instance)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: GatewayLabels(instance)},
				Spec: corev1.PodSpec{
					ServiceAccountName: names.Gateway,
					// The only Kubernetes credential in this data plane, and
					// the reason the Role above is resourceNames-scoped.
					AutomountServiceAccountToken: &automount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   boolPtr(true),
						RunAsUser:      int64Ptr(gatewayUID),
						RunAsGroup:     int64Ptr(gatewayUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            "gateway",
						Image:           GatewayImage(instance, operatorImage),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{GatewayCommand},
						Env:             gatewayEnv(instance, consoleURL),
						Ports: []corev1.ContainerPort{
							{Name: GatewayAgentPortName, ContainerPort: GatewayAgentPort, Protocol: corev1.ProtocolTCP},
							{Name: GatewayConsolePortName, ContainerPort: GatewayConsolePort, Protocol: corev1.ProtocolTCP},
						},
						LivenessProbe:  gatewayProbe(),
						ReadinessProbe: gatewayProbe(),
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolPtr(false),
							ReadOnlyRootFilesystem:   boolPtr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

// gatewayEnv renders the configuration table the gateway reads at startup.
// GUEST_AGENT_ADDRESS is deliberately absent: the gateway derives
// <DESKTOP_NAME>-agent.<DESKTOP_NAMESPACE>.svc, which is exactly the agent
// Service this package renders, and one derivation is harder to desynchronise
// than two.
func gatewayEnv(instance *typeclawv1alpha1.TypeClawInstance, consoleURL string) []corev1.EnvVar {
	spec := instance.Spec.PersonalDesktop
	names := Names(instance)

	env := []corev1.EnvVar{
		{Name: "DESKTOP_NAMESPACE", Value: names.Namespace},
		{Name: "DESKTOP_NAME", Value: names.Desktop},
		{Name: "DESKTOP_OS", Value: GuestOSName(spec)},
		{Name: "OWNER_ISSUER", Value: OwnerIssuer(spec)},
		{Name: "OWNER_SUBJECT", Value: ownerSubject(spec)},
		{Name: "CONSOLE_AUTH_MODE", Value: "tailscale"},
		{Name: "LISTEN_ADDRESS", Value: ":" + itoa32(GatewayAgentPort)},
		{Name: "CONSOLE_LISTEN_ADDRESS", Value: ":" + itoa32(GatewayConsolePort)},
		{Name: "AGENT_LEASE_TTL", Value: itoa32(AgentLeaseTTLSeconds(spec)) + "s"},
		{Name: "AGENT_TOKEN", ValueFrom: tokenRef(names.Tokens, TokenKeyAgent)},
		{Name: "GUEST_AGENT_TOKEN", ValueFrom: tokenRef(names.Tokens, TokenKeyGuest)},
	}
	if access := TailscaleAccess(spec); access != nil && len(access.AllowedLogins) > 0 {
		env = append(env, corev1.EnvVar{
			Name:  "TAILSCALE_ALLOWED_LOGINS",
			Value: joinNonEmpty(access.AllowedLogins),
		})
	}
	if consoleURL != "" {
		env = append(env, corev1.EnvVar{Name: "CONSOLE_URL", Value: consoleURL})
	}
	return env
}

// GatewayService publishes both listeners in-cluster. They stay separate
// ports because the NetworkPolicy admits entirely different peers to each:
// the runtime Pod on the agent port, the Tailscale proxy on the console port.
func GatewayService(instance *typeclawv1alpha1.TypeClawInstance) *corev1.Service {
	names := Names(instance)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Gateway,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: GatewayLabels(instance),
			Ports: []corev1.ServicePort{
				{
					Name:       GatewayAgentPortName,
					Port:       GatewayAgentPort,
					TargetPort: intstr.FromString(GatewayAgentPortName),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       GatewayConsolePortName,
					Port:       GatewayConsolePort,
					TargetPort: intstr.FromString(GatewayConsolePortName),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

func gatewayProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz",
				Port: intstr.FromString(GatewayAgentPortName),
			},
		},
		PeriodSeconds: 10,
	}
}

func tokenRef(secretName, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			Key:                  key,
		},
	}
}

func ownerSubject(spec *typeclawv1alpha1.PersonalDesktopSpec) string {
	if spec == nil {
		return ""
	}
	return spec.Owner.Subject
}

func joinNonEmpty(values []string) string {
	joined := ""
	for _, value := range values {
		if value == "" {
			continue
		}
		if joined != "" {
			joined += ","
		}
		joined += value
	}
	return joined
}

func boolPtr(v bool) *bool    { return &v }
func int64Ptr(v int64) *int64 { return &v }
func itoa32(v int32) string   { return strconv.FormatInt(int64(v), 10) }
