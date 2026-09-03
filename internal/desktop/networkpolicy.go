// networkpolicy.go draws the traffic boundary around the Desktop Gateway.
//
// The gateway is the single hinge of this feature: it holds a Kubernetes
// credential, it can drive the owner's keyboard, and it is reachable from two
// very different peers. So the policy is written from the peer side — the
// Managed Runtime may reach the agent port and nothing else, the Tailscale
// proxy may reach the console port and nothing else — rather than as a
// port-only rule that would let any Pod in either namespace take control of
// the desktop.
package desktop

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/netblocks"
)

// dnsNamespace hosts the cluster DNS workload the gateway resolves through.
const dnsNamespace = "kube-system"

// GatewayNetworkPolicy renders the Desktop Gateway's boundary.
//
// runtimeSelector is the Managed Runtime Pod's label set (owned by the
// workload renderer). apiServerIPs and agentServiceIP are discovered by the
// controller: egress rules match pre-DNAT destinations, so a Service the
// gateway reaches by name needs its ClusterIP admitted explicitly.
func GatewayNetworkPolicy(
	instance *typeclawv1alpha1.TypeClawInstance,
	runtimeSelector map[string]string,
	apiServerIPs []string,
	agentServiceIP string,
) *networkingv1.NetworkPolicy {
	names := Names(instance)

	ingress := []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: namespaceSelector(names.InstanceNamespace),
			PodSelector:       &metav1.LabelSelector{MatchLabels: runtimeSelector},
		}},
		Ports: []networkingv1.NetworkPolicyPort{tcpPort(GatewayAgentPort)},
	}}
	// The console rule exists only in Ingress mode. In Sidecar mode the console
	// listener is bound to loopback, so no NetworkPolicy could admit anything
	// to it and none is needed: the network namespace is the boundary. Writing
	// the rule anyway would imply the console is a Pod-network surface.
	if access := TailscaleAccess(instance.Spec.PersonalDesktop); access != nil &&
		!ConsoleSidecar(instance.Spec.PersonalDesktop) {
		ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: namespaceSelector(TailscaleOperatorNamespace(access)),
				PodSelector:       &metav1.LabelSelector{MatchLabels: TailscaleProxyPodSelector(instance)},
			}},
			Ports: []networkingv1.NetworkPolicyPort{tcpPort(GatewayConsolePort)},
		})
	}

	// In Sidecar mode the Gateway Pod also runs tailscaled, which has to reach
	// the Tailscale control plane and DERP relays to bring the console up at
	// all. Without this rule the console silently never appears on a cluster
	// that enforces NetworkPolicy — the Pod is healthy and the tailnet device
	// simply never registers.
	tailscaleEgress := []networkingv1.NetworkPolicyEgressRule(nil)
	if ConsoleSidecar(instance.Spec.PersonalDesktop) {
		tailscaleEgress = append(tailscaleEgress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{
				{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: netblocks.PublicWebV4Except}},
				{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: netblocks.PublicWebV6Except}},
			},
		})
	}

	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: namespaceSelector(dnsNamespace),
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"k8s-app": "kube-dns"},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: protocolPtr(corev1.ProtocolUDP), Port: portPtr(53)},
				{Protocol: protocolPtr(corev1.ProtocolTCP), Port: portPtr(53)},
			},
		},
		{
			// The guest agent lives behind the VM's masquerade interface, so
			// the peer is the virt-launcher Pod of exactly this desktop.
			To: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{DomainLabel: names.Desktop},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{tcpPort(GuestAgentPort)},
		},
	}
	if agentServiceIP != "" {
		egress[len(egress)-1].To = append(egress[len(egress)-1].To, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: agentServiceIP + "/32"},
		})
	}
	if peers := ipPeers(apiServerIPs); len(peers) > 0 {
		// VNC, screenshots and power are Kubernetes subresources; without the
		// API server the console is a blank page.
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    peers,
			Ports: []networkingv1.NetworkPolicyPort{tcpPort(443)},
		})
	}

	egress = append(egress, tailscaleEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Gateway,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: GatewayLabels(instance)},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: ingress,
			Egress:  egress,
		},
	}
}

// RuntimeGatewayEgressRule is the counterpart rendered into the Instance's own
// NetworkPolicy: without it the runtime's PublicWeb egress would drop every
// packet to the gateway, since the gateway is a cluster-internal destination
// PublicWeb deliberately excludes.
func RuntimeGatewayEgressRule(
	instance *typeclawv1alpha1.TypeClawInstance,
	gatewayServiceIP string,
) networkingv1.NetworkPolicyEgressRule {
	names := Names(instance)
	rule := networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: namespaceSelector(names.Namespace),
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     GatewayAppName,
					"app.kubernetes.io/instance": instance.Name,
				},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{tcpPort(GatewayAgentPort)},
	}
	if gatewayServiceIP != "" {
		// A pod-selector peer is the durable expression of intent, but egress
		// matches pre-DNAT destinations: the runtime resolves the gateway by
		// Service name, so the ClusterIP needs its own /32 the way the
		// existing API-server rule already documents.
		rule.To = append(rule.To, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: gatewayServiceIP + "/32"},
		})
	}
	return rule
}

func namespaceSelector(namespace string) *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace},
	}
}

func ipPeers(ips []string) []networkingv1.NetworkPolicyPeer {
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(ips))
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: ip + "/32"},
		})
	}
	return peers
}

func tcpPort(port int32) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{
		Protocol: protocolPtr(corev1.ProtocolTCP),
		Port:     portPtr(port),
	}
}

func portPtr(port int32) *intstr.IntOrString {
	value := intstr.FromInt32(port)
	return &value
}

func protocolPtr(protocol corev1.Protocol) *corev1.Protocol { return &protocol }
