package desktop

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func envOf(t *testing.T, deployment *appsv1.Deployment, name string) corev1.EnvVar {
	t.Helper()
	for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
		if env.Name == name {
			return env
		}
	}
	t.Fatalf("gateway env %s is not set", name)
	return corev1.EnvVar{}
}

func TestGatewayRoleIsScopedToOneDesktop(t *testing.T) {
	role := GatewayRole(desktopInstance(nil))

	if len(role.Rules) != 3 {
		t.Fatalf("gateway Role rules = %d, want the three KubeVirt rules", len(role.Rules))
	}
	for _, rule := range role.Rules {
		// The credential is only defensible because it names one VM; a rule
		// without resourceNames would grant the whole namespace.
		if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "kakao-agent-desktop" {
			t.Fatalf("rule %+v is not restricted to this desktop", rule)
		}
	}
	if role.Rules[2].Verbs[0] != "update" {
		t.Fatalf("power subresources need update, got %v", role.Rules[2].Verbs)
	}
}

func TestGatewayRoleBindingBindsTheGatewayIdentity(t *testing.T) {
	binding := GatewayRoleBinding(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
	}))

	if binding.RoleRef.Name != "kakao-agent-desktop-gateway" || binding.RoleRef.Kind != "Role" {
		t.Fatalf("roleRef = %+v", binding.RoleRef)
	}
	subject := binding.Subjects[0]
	if subject.Kind != "ServiceAccount" || subject.Namespace != "typeclaw-desktops" {
		t.Fatalf("subject = %+v", subject)
	}
}

func TestGatewayDeploymentIsASingleRestrictedReplica(t *testing.T) {
	deployment := GatewayDeployment(desktopInstance(nil), "ghcr.io/fml09/typeclaw-operator:test", "")

	if *deployment.Spec.Replicas != 1 {
		t.Fatalf("replicas = %d, want 1", *deployment.Spec.Replicas)
	}
	// Input leases live in the gateway's memory; two replicas would both
	// believe they hold the exclusive Input Controller lease.
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %s, want Recreate", deployment.Spec.Strategy.Type)
	}

	pod := deployment.Spec.Template.Spec
	if pod.ServiceAccountName != "kakao-agent-desktop-gateway" {
		t.Fatalf("service account = %q", pod.ServiceAccountName)
	}
	if pod.AutomountServiceAccountToken == nil || !*pod.AutomountServiceAccountToken {
		t.Fatalf("the gateway is the one component that needs a Kubernetes credential")
	}
	if *pod.SecurityContext.RunAsUser != 65532 || !*pod.SecurityContext.RunAsNonRoot {
		t.Fatalf("pod security context = %+v", pod.SecurityContext)
	}
	if pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("seccomp = %+v", pod.SecurityContext.SeccompProfile)
	}

	container := pod.Containers[0]
	if container.Image != "ghcr.io/fml09/typeclaw-operator:test" {
		t.Fatalf("image = %q", container.Image)
	}
	if len(container.Command) != 1 || container.Command[0] != GatewayCommand {
		t.Fatalf("command = %v", container.Command)
	}
	if !*container.SecurityContext.ReadOnlyRootFilesystem || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("container security context = %+v", container.SecurityContext)
	}
	if len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("capabilities = %+v", container.SecurityContext.Capabilities)
	}
	if container.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("readiness probe = %+v", container.ReadinessProbe)
	}
	ports := map[string]int32{}
	for _, port := range container.Ports {
		ports[port.Name] = port.ContainerPort
	}
	if ports[GatewayAgentPortName] != GatewayAgentPort || ports[GatewayConsolePortName] != GatewayConsolePort {
		t.Fatalf("container ports = %v", ports)
	}
}

func TestGatewayDeploymentEnvironmentMatchesTheGatewayContract(t *testing.T) {
	in := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
		in.Spec.PersonalDesktop.Gateway = &typeclawv1alpha1.PersonalDesktopGatewaySpec{AgentLeaseTTLSeconds: 300}
		in.Spec.PersonalDesktop.Access = &typeclawv1alpha1.PersonalDesktopAccessSpec{
			Tailscale: &typeclawv1alpha1.PersonalDesktopTailscaleAccessSpec{
				Hostname:      "kakao-desktop",
				AllowedLogins: []string{"alice@example.com", "bob@example.com"},
			},
		}
	})
	deployment := GatewayDeployment(in, "operator:test", "https://kakao-desktop.tail.ts.net")

	for name, want := range map[string]string{
		"DESKTOP_NAMESPACE":        "typeclaw-desktops",
		"DESKTOP_NAME":             "kakao-agent-desktop",
		"DESKTOP_OS":               "linux",
		"OWNER_ISSUER":             "https://login.tailscale.com",
		"OWNER_SUBJECT":            "alice@example.com",
		"CONSOLE_AUTH_MODE":        "tailscale",
		"LISTEN_ADDRESS":           ":8080",
		"CONSOLE_LISTEN_ADDRESS":   ":8081",
		"AGENT_LEASE_TTL":          "300s",
		"TAILSCALE_ALLOWED_LOGINS": "alice@example.com,bob@example.com",
		"CONSOLE_URL":              "https://kakao-desktop.tail.ts.net",
	} {
		if got := envOf(t, deployment, name); got.Value != want {
			t.Fatalf("env %s = %q, want %q", name, got.Value, want)
		}
	}

	for name, key := range map[string]string{
		"AGENT_TOKEN":       TokenKeyAgent,
		"GUEST_AGENT_TOKEN": TokenKeyGuest,
	} {
		ref := envOf(t, deployment, name).ValueFrom
		if ref == nil || ref.SecretKeyRef == nil {
			t.Fatalf("env %s must come from the token Secret", name)
		}
		if ref.SecretKeyRef.Name != "kakao-agent-desktop-tokens" || ref.SecretKeyRef.Key != key {
			t.Fatalf("env %s secretKeyRef = %+v", name, ref.SecretKeyRef)
		}
	}
}

func TestGatewayDeploymentOmitsOptionalEnvironment(t *testing.T) {
	deployment := GatewayDeployment(desktopInstance(nil), "operator:test", "")
	for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
		switch env.Name {
		case "CONSOLE_URL", "TAILSCALE_ALLOWED_LOGINS":
			t.Fatalf("env %s must be unset when nothing declares it", env.Name)
		case "GUEST_AGENT_ADDRESS":
			// The gateway derives the agent address from DESKTOP_NAME and
			// DESKTOP_NAMESPACE; a second derivation could desynchronise.
			t.Fatalf("the guest agent address must stay derived, not configured")
		}
	}
}

func TestGatewayImageHonoursTheOverride(t *testing.T) {
	in := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Gateway = &typeclawv1alpha1.PersonalDesktopGatewaySpec{Image: "example.test/gateway:pinned"}
	})
	if got := GatewayImage(in, "operator:test"); got != "example.test/gateway:pinned" {
		t.Fatalf("GatewayImage() = %q", got)
	}
	if got := GatewayImage(desktopInstance(nil), "operator:test"); got != "operator:test" {
		t.Fatalf("GatewayImage() = %q, want the operator image", got)
	}
}

func TestGatewayServicePublishesBothListeners(t *testing.T) {
	service := GatewayService(desktopInstance(nil))

	if service.Spec.Selector["app.kubernetes.io/name"] != GatewayAppName {
		t.Fatalf("selector = %v, want the gateway Pod labels", service.Spec.Selector)
	}
	ports := map[string]int32{}
	for _, port := range service.Spec.Ports {
		ports[port.Name] = port.Port
	}
	if ports[GatewayAgentPortName] != GatewayAgentPort || ports[GatewayConsolePortName] != GatewayConsolePort {
		t.Fatalf("service ports = %v", ports)
	}
}

func TestGatewayServiceAccountIsPerDesktop(t *testing.T) {
	sa := GatewayServiceAccount(desktopInstance(nil))
	if sa.Name != "kakao-agent-desktop-gateway" || sa.Labels[LabelInstanceUID] != "instance-uid" {
		t.Fatalf("gateway ServiceAccount = %s labels %v", sa.Name, sa.Labels)
	}
}
