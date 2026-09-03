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
						RunAsNonRoot: boolPtr(true),
						RunAsUser:    int64Ptr(gatewayUID),
						RunAsGroup:   int64Ptr(gatewayUID),
						// Without this, an emptyDir arrives owned by root with
						// mode 0755 and the unprivileged process cannot write
						// it. That is not a theoretical papercut: tailscaled
						// refuses to start at all on an unwritable state
						// directory, reporting only "cannot start backend when
						// state store is unhealthy".
						FSGroup:        int64Ptr(gatewayUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Volumes:    gatewayVolumes(instance),
					Containers: gatewayContainers(instance, operatorImage, consoleURL),
				},
			},
		},
	}
}

// gatewayContainers renders the Gateway Pod's containers. In Sidecar mode a
// tailscaled joins it, sharing the Pod's network namespace so it can reach a
// console listener that is bound to loopback and therefore unreachable from
// every other Pod in the cluster.
func gatewayContainers(instance *typeclawv1alpha1.TypeClawInstance, operatorImage, consoleURL string) []corev1.Container {
	containers := []corev1.Container{gatewayContainer(instance, operatorImage, consoleURL)}
	if sidecar := tailscaleSidecar(instance); sidecar != nil {
		containers = append(containers, *sidecar)
	}
	return containers
}

func gatewayContainer(instance *typeclawv1alpha1.TypeClawInstance, operatorImage, consoleURL string) corev1.Container {
	return corev1.Container{
		Name:            "gateway",
		Image:           GatewayImage(instance, operatorImage),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{GatewayCommand},
		Env:             gatewayEnv(instance, consoleURL),
		Ports:           gatewayContainerPorts(instance),
		LivenessProbe:   gatewayProbe(),
		ReadinessProbe:  gatewayProbe(),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
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
		{Name: "CONSOLE_LISTEN_ADDRESS", Value: ConsoleListenAddress(spec)},
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

// GatewayService publishes the in-cluster listeners.
//
// In Ingress mode that is both of them, on separate ports because the
// NetworkPolicy admits entirely different peers to each: the runtime Pod on
// the agent port, the Tailscale proxy on the console port. In Sidecar mode the
// console is not on the Pod network at all, so publishing a console port here
// would route traffic to a socket that is not listening — and, worse, would
// suggest the console is meant to be reachable from the cluster.
func GatewayService(instance *typeclawv1alpha1.TypeClawInstance) *corev1.Service {
	names := Names(instance)
	ports := []corev1.ServicePort{{
		Name:       GatewayAgentPortName,
		Port:       GatewayAgentPort,
		TargetPort: intstr.FromString(GatewayAgentPortName),
		Protocol:   corev1.ProtocolTCP,
	}}
	if !ConsoleSidecar(instance.Spec.PersonalDesktop) {
		ports = append(ports, corev1.ServicePort{
			Name:       GatewayConsolePortName,
			Port:       GatewayConsolePort,
			TargetPort: intstr.FromString(GatewayConsolePortName),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Gateway,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: GatewayLabels(instance),
			Ports:    ports,
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

// gatewayContainerPorts publishes the listeners that are actually on the Pod
// network. In Sidecar mode the console is bound to loopback, so naming it as a
// container port would advertise reachability the Pod deliberately does not
// have.
func gatewayContainerPorts(instance *typeclawv1alpha1.TypeClawInstance) []corev1.ContainerPort {
	ports := []corev1.ContainerPort{
		{Name: GatewayAgentPortName, ContainerPort: GatewayAgentPort, Protocol: corev1.ProtocolTCP},
	}
	if !ConsoleSidecar(instance.Spec.PersonalDesktop) {
		ports = append(ports, corev1.ContainerPort{
			Name: GatewayConsolePortName, ContainerPort: GatewayConsolePort, Protocol: corev1.ProtocolTCP,
		})
	}
	return ports
}

// gatewayVolumes renders the volumes the tailscaled sidecar needs, and nothing
// at all in Ingress mode. The state directory is an emptyDir rather than a
// Kubernetes Secret on purpose: keeping tailscaled's state out of the API
// server is what lets this sidecar exist without widening the Gateway's
// Kubernetes credential, which ADR 0007 already records as contested. The cost
// is that the tailnet device must be ephemeral, so it is cleaned up when the
// Pod goes away instead of lingering and taking its MagicDNS name with it.
func gatewayVolumes(instance *typeclawv1alpha1.TypeClawInstance) []corev1.Volume {
	if !ConsoleSidecar(instance.Spec.PersonalDesktop) {
		return nil
	}
	names := Names(instance)
	return []corev1.Volume{
		{
			Name: "serve-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: names.ServeConfig},
				},
			},
		},
		{Name: "tailscale-state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tailscale-tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
}

// tailscaleSidecar renders the tailscaled that fronts the console, or nil when
// the console is published some other way or not at all.
//
// It runs in userspace networking mode, which is what lets it keep the same
// unprivileged security context as the gateway beside it: Serve terminates
// tailnet TLS and proxies to a local port, so nothing here needs NET_ADMIN or
// a tun device.
func tailscaleSidecar(instance *typeclawv1alpha1.TypeClawInstance) *corev1.Container {
	spec := instance.Spec.PersonalDesktop
	if !ConsoleSidecar(spec) {
		return nil
	}
	access := TailscaleAccess(spec)

	env := []corev1.EnvVar{
		{Name: "TS_USERSPACE", Value: "true"},
		{Name: "TS_HOSTNAME", Value: access.Hostname},
		{Name: "TS_STATE_DIR", Value: TailscaleStateDir},
		// Empty on purpose, and required: inside Kubernetes the container
		// stores tailscaled's state in a Secret named "tailscale" unless this
		// is cleared, and it does that even when TS_STATE_DIR is set. It then
		// fails to start for want of get/update on that Secret — RBAC this
		// Gateway deliberately does not have, because keeping tailscaled's
		// state off the API server is what lets the console sidecar exist
		// without widening the one Kubernetes credential in this data plane.
		{Name: "TS_KUBE_SECRET", Value: ""},
		{Name: "TS_SERVE_CONFIG", Value: ServeConfigMountPath + "/" + ServeConfigKey},
	}
	// Both credential shapes are declared optional so one Secret may carry
	// either a reusable auth key or an OAuth client, and the container picks
	// whichever is present. A required reference to the shape that is absent
	// would leave the Pod stuck in CreateContainerConfigError instead.
	for _, key := range []string{"TS_AUTHKEY", "TS_CLIENT_ID", "TS_CLIENT_SECRET"} {
		env = append(env, corev1.EnvVar{Name: key, ValueFrom: optionalTokenRef(access.AuthSecret, key)})
	}
	if tags := joinNonEmpty(access.Tags); tags != "" {
		env = append(env, corev1.EnvVar{Name: "TS_EXTRA_ARGS", Value: "--advertise-tags=" + tags})
	}

	return &corev1.Container{
		Name:            TailscaleSidecarName,
		Image:           TailscaleImage(instance),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             env,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "serve-config", MountPath: ServeConfigMountPath, ReadOnly: true},
			{Name: "tailscale-state", MountPath: TailscaleStateVolumePath},
			{Name: "tailscale-tmp", MountPath: "/tmp"},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// GatewayServeConfig renders the tailscaled Serve configuration, or nil in
// Ingress mode. The shape matches what the Tailscale Kubernetes operator
// writes for its own Ingress proxies: TLS terminated on 443 for the device's
// certificate domain, everything proxied to one backend. Here the backend is
// loopback, which is the entire point.
func GatewayServeConfig(instance *typeclawv1alpha1.TypeClawInstance) *corev1.ConfigMap {
	if !ConsoleSidecar(instance.Spec.PersonalDesktop) {
		return nil
	}
	names := Names(instance)
	config := `{"TCP":{"443":{"HTTPS":true}},"Web":{"${TS_CERT_DOMAIN}:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:` +
		itoa32(GatewayConsolePort) + `/"}}}}}`
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.ServeConfig,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Data: map[string]string{ServeConfigKey: config},
	}
}

// TailscaleImage resolves the tailscaled image for the console sidecar.
func TailscaleImage(instance *typeclawv1alpha1.TypeClawInstance) string {
	if access := TailscaleAccess(instance.Spec.PersonalDesktop); access != nil && access.Image != "" {
		return access.Image
	}
	return DefaultTailscaleImage
}

func optionalTokenRef(secretName, key string) *corev1.EnvVarSource {
	optional := true
	return &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			Key:                  key,
			Optional:             &optional,
		},
	}
}
