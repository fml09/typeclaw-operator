/*
Copyright 2026 fml09.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resources

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/desktop"
	"github.com/fml09/typeclaw-operator/internal/netblocks"
)

const (
	// EgressPublicWeb renders the PublicWeb destination universe (CONTEXT.md):
	// public DNS names and globally routable Internet addresses after
	// excluding private, special-use, cluster, node, metadata, and
	// control-plane destinations.
	EgressPublicWeb = "PublicWeb"

	// EgressUnrestricted removes egress filtering entirely; the runtime may
	// reach any destination, including cluster-internal services.
	EgressUnrestricted = "Unrestricted"

	// DNSSystemNamespace hosts the cluster DNS workload every PublicWeb
	// policy must reach for name resolution.
	DNSSystemNamespace = "kube-system"
)

// NetworkPolicy renders the externally enforced traffic boundary of one
// TypeClaw Instance. The policy always declares both Ingress and Egress
// regardless of defaults: an absent spec.network means PublicWeb egress with
// same-namespace-only ingress, never "no policy".
//
// apiServerIPs carries the discovered Kubernetes API server endpoints; when
// SelfConfig observation is on, the relay sidecar needs exactly these
// destinations on 443 to reach the API from inside a PublicWeb policy.
// desktopGatewayIP is the Personal Desktop Gateway Service's cluster IP, empty
// until the Service exists; both are cluster observations rather than spec, so
// they arrive as arguments instead of being read from the Instance.
func NetworkPolicy(
	instance *typeclawv1alpha1.TypeClawInstance,
	apiServerIPs []string,
	desktopGatewayIP string,
) *networkingv1.NetworkPolicy {
	labels := Labels(instance)
	egress := netEgressRules(instance.Spec.Network.Egress, apiServerIPs...)
	if desktop.Enabled(instance) {
		// PublicWeb deliberately excludes cluster-internal destinations, so
		// without this rule every typed action the agent sends to its own
		// Personal Desktop would be dropped by its own boundary.
		egress = append(egress, desktop.RuntimeGatewayEgressRule(instance, desktopGatewayIP))
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: netIngressRules(instance),
			Egress:  egress,
		},
	}
}

// netIngressRules admits the server port from same-namespace peers always,
// plus one explicit CIDR rule per spec.network.IngressCIDRs entry.
func netIngressRules(instance *typeclawv1alpha1.TypeClawInstance) []networkingv1.NetworkPolicyIngressRule {
	rules := []networkingv1.NetworkPolicyIngressRule{
		{
			From: []networkingv1.NetworkPolicyPeer{
				// Empty podSelector selects every Pod in the policy's own
				// namespace.
				{PodSelector: &metav1.LabelSelector{}},
			},
			Ports: []networkingv1.NetworkPolicyPort{{Port: netIntOrStr(ContainerPort)}},
		},
	}
	for _, cidr := range instance.Spec.Network.IngressCIDRs {
		if cidr == "" {
			continue
		}
		rules = append(rules, networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{
				{IPBlock: &networkingv1.IPBlock{CIDR: cidr}},
			},
			Ports: []networkingv1.NetworkPolicyPort{{Port: netIntOrStr(ContainerPort)}},
		})
	}
	return rules
}

// netEgressRules maps the declared destination universe onto policy rules. An
// empty value means PublicWeb: unset specs render the safe default, not an
// unfiltered policy.
func netEgressRules(egress string, apiServerIPs ...string) []networkingv1.NetworkPolicyEgressRule {
	var rules []networkingv1.NetworkPolicyEgressRule
	switch egress {
	case EgressUnrestricted:
		rules = []networkingv1.NetworkPolicyEgressRule{
			{
				To: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}},
					{IPBlock: &networkingv1.IPBlock{CIDR: "::/0"}},
				},
			},
		}
	default:
		// PublicWeb: cluster DNS first, then globally routable space with
		// the special-use carve-outs.
		rules = []networkingv1.NetworkPolicyEgressRule{
			{
				To: []networkingv1.NetworkPolicyPeer{
					{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": DNSSystemNamespace},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k8s-app": "kube-dns"},
						},
					},
				},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: netProtocolPtr(corev1.ProtocolUDP), Port: netIntOrStr(53)},
					{Protocol: netProtocolPtr(corev1.ProtocolTCP), Port: netIntOrStr(53)},
				},
			},
			{
				To: []networkingv1.NetworkPolicyPeer{
					{
						IPBlock: &networkingv1.IPBlock{
							CIDR:   "0.0.0.0/0",
							Except: netblocks.PublicWebV4Except,
						},
					},
					{
						IPBlock: &networkingv1.IPBlock{
							CIDR:   "::/0",
							Except: netblocks.PublicWebV6Except,
						},
					},
				},
			},
		}
	}
	if len(apiServerIPs) > 0 {
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(apiServerIPs))
		for _, ip := range apiServerIPs {
			peers = append(peers, networkingv1.NetworkPolicyPeer{
				IPBlock: &networkingv1.IPBlock{CIDR: ip + "/32"},
			})
		}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{
				Port:     netIntOrStr(443),
				Protocol: netProtocolPtr(corev1.ProtocolTCP),
			}},
			To: peers,
		})
	}
	return rules
}

// netIntOrStr builds the port value every NetworkPolicy rule uses.
func netIntOrStr(v int32) *intstr.IntOrString {
	s := intstr.FromInt32(v)
	return &s
}

func netProtocolPtr(p corev1.Protocol) *corev1.Protocol { return &p }
