package desktop

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

var runtimeSelector = map[string]string{
	"app.kubernetes.io/name":       "typeclaw",
	"app.kubernetes.io/instance":   "kakao-agent",
	"app.kubernetes.io/managed-by": "typeclaw-operator",
}

func hasPort(ports []networkingv1.NetworkPolicyPort, protocol corev1.Protocol, port int32) bool {
	for _, entry := range ports {
		if entry.Port != nil && entry.Port.IntValue() == int(port) &&
			(entry.Protocol == nil || *entry.Protocol == protocol) {
			return true
		}
	}
	return false
}

func TestGatewayNetworkPolicyAdmitsOnlyTheRuntimeOnTheAgentPort(t *testing.T) {
	policy := GatewayNetworkPolicy(desktopInstance(nil), runtimeSelector, nil, "")

	if policy.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"] != GatewayAppName {
		t.Fatalf("policy selects %v, want the gateway Pod", policy.Spec.PodSelector.MatchLabels)
	}
	if len(policy.Spec.Ingress) != 1 {
		t.Fatalf("ingress rules = %d, want the runtime rule alone", len(policy.Spec.Ingress))
	}
	peer := policy.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "agents" {
		t.Fatalf("runtime namespace peer = %+v", peer.NamespaceSelector)
	}
	if peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "typeclaw" {
		t.Fatalf("runtime pod peer = %+v", peer.PodSelector)
	}
	if !hasPort(policy.Spec.Ingress[0].Ports, corev1.ProtocolTCP, GatewayAgentPort) {
		t.Fatalf("runtime rule ports = %+v", policy.Spec.Ingress[0].Ports)
	}
}

func TestGatewayNetworkPolicyAdmitsTheTailscaleProxyOnTheConsolePort(t *testing.T) {
	in := tailscaleInstance(func(access *typeclawv1alpha1.PersonalDesktopTailscaleAccessSpec) {
		access.OperatorNamespace = "ts-system"
	})
	policy := GatewayNetworkPolicy(in, runtimeSelector, nil, "")

	if len(policy.Spec.Ingress) != 2 {
		t.Fatalf("ingress rules = %d, want runtime + console", len(policy.Spec.Ingress))
	}
	console := policy.Spec.Ingress[1]
	peer := console.From[0]
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "ts-system" {
		t.Fatalf("console namespace peer = %+v", peer.NamespaceSelector)
	}
	// Only the proxy Pod of this console, never every Pod in the Tailscale
	// namespace.
	if peer.PodSelector.MatchLabels[TailscaleParentResourceLabel] != "kakao-agent-desktop-console" {
		t.Fatalf("console pod peer = %+v", peer.PodSelector)
	}
	if !hasPort(console.Ports, corev1.ProtocolTCP, GatewayConsolePort) {
		t.Fatalf("console rule ports = %+v", console.Ports)
	}
}

func TestGatewayNetworkPolicyEgressReachesDNSGuestAndAPIServer(t *testing.T) {
	policy := GatewayNetworkPolicy(desktopInstance(nil), runtimeSelector, []string{"10.0.0.1", ""}, "10.96.0.42")

	if len(policy.Spec.Egress) != 3 {
		t.Fatalf("egress rules = %d, want dns + guest agent + API server", len(policy.Spec.Egress))
	}
	dns := policy.Spec.Egress[0]
	if dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Fatalf("dns peer = %+v", dns.To[0])
	}
	if !hasPort(dns.Ports, corev1.ProtocolUDP, 53) || !hasPort(dns.Ports, corev1.ProtocolTCP, 53) {
		t.Fatalf("dns ports = %+v", dns.Ports)
	}

	guest := policy.Spec.Egress[1]
	if guest.To[0].PodSelector.MatchLabels[DomainLabel] != "kakao-agent-desktop" {
		t.Fatalf("guest agent peer = %+v", guest.To[0])
	}
	// Egress matches pre-DNAT destinations, so the agent Service's cluster IP
	// needs its own block alongside the Pod selector.
	if guest.To[1].IPBlock == nil || guest.To[1].IPBlock.CIDR != "10.96.0.42/32" {
		t.Fatalf("agent Service peer = %+v", guest.To[1])
	}
	if !hasPort(guest.Ports, corev1.ProtocolTCP, GuestAgentPort) {
		t.Fatalf("guest agent ports = %+v", guest.Ports)
	}

	api := policy.Spec.Egress[2]
	if len(api.To) != 1 || api.To[0].IPBlock.CIDR != "10.0.0.1/32" {
		t.Fatalf("API server peers = %+v, empty addresses must be dropped", api.To)
	}
	if !hasPort(api.Ports, corev1.ProtocolTCP, 443) {
		t.Fatalf("API server ports = %+v", api.Ports)
	}
}

func TestGatewayNetworkPolicyOmitsUnknownAddresses(t *testing.T) {
	policy := GatewayNetworkPolicy(desktopInstance(nil), runtimeSelector, nil, "")

	if len(policy.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want dns + guest agent only", len(policy.Spec.Egress))
	}
	if len(policy.Spec.Egress[1].To) != 1 {
		t.Fatalf("guest agent peers = %+v, want the Pod selector alone", policy.Spec.Egress[1].To)
	}
}

func TestRuntimeGatewayEgressRulePrefersASelectorPeer(t *testing.T) {
	rule := RuntimeGatewayEgressRule(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
	}), "10.96.0.7")

	peer := rule.To[0]
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "typeclaw-desktops" {
		t.Fatalf("gateway namespace peer = %+v", peer.NamespaceSelector)
	}
	if peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != GatewayAppName {
		t.Fatalf("gateway pod peer = %+v", peer.PodSelector)
	}
	if peer.PodSelector.MatchLabels["app.kubernetes.io/instance"] != "kakao-agent" {
		t.Fatalf("gateway pod peer does not pin the Instance: %+v", peer.PodSelector)
	}
	if rule.To[1].IPBlock == nil || rule.To[1].IPBlock.CIDR != "10.96.0.7/32" {
		t.Fatalf("gateway Service peer = %+v", rule.To[1])
	}
	if !hasPort(rule.Ports, corev1.ProtocolTCP, GatewayAgentPort) {
		t.Fatalf("rule ports = %+v", rule.Ports)
	}
}

func TestRuntimeGatewayEgressRuleWithoutAKnownClusterIP(t *testing.T) {
	rule := RuntimeGatewayEgressRule(desktopInstance(nil), "")
	if len(rule.To) != 1 {
		t.Fatalf("peers = %+v, want the selector peer alone before the Service exists", rule.To)
	}
}
