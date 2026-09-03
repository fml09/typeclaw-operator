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
	"reflect"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func netInstance(mutate func(*typeclawv1alpha1.TypeClawInstance)) *typeclawv1alpha1.TypeClawInstance {
	in := &typeclawv1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "kakao-agent", Namespace: "agents"},
	}
	if mutate != nil {
		mutate(in)
	}
	return in
}

func TestNetworkPolicyDefaultsToPublicWeb(t *testing.T) {
	for name, egress := range map[string]string{"unset": "", "explicit": EgressPublicWeb} {
		t.Run(name, func(t *testing.T) {
			policy := NetworkPolicy(netInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
				in.Spec.Network.Egress = egress
			}), nil, "")

			if policy.Name != "kakao-agent" || policy.Namespace != "agents" {
				t.Fatalf("unexpected object ref %s/%s", policy.Namespace, policy.Name)
			}
			wantLabels := Labels(netInstance(nil))
			if !reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, wantLabels) {
				t.Fatalf("pod selector = %v, want workload labels", policy.Spec.PodSelector.MatchLabels)
			}
			wantTypes := []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			}
			if !reflect.DeepEqual(policy.Spec.PolicyTypes, wantTypes) {
				t.Fatalf("policy types = %v, want both Ingress and Egress", policy.Spec.PolicyTypes)
			}

			// Egress shape: kube-dns rule then carved-out allow-all pair.
			egressRules := policy.Spec.Egress
			if len(egressRules) != 2 {
				t.Fatalf("egress rules = %d, want dns + public web", len(egressRules))
			}
			dns := egressRules[0]
			if len(dns.To) != 1 || dns.To[0].IPBlock != nil {
				t.Fatalf("dns rule peers = %+v, want kube-dns selector only", dns.To)
			}
			peer := dns.To[0]
			gotNS := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
			if gotNS != DNSSystemNamespace || peer.PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
				t.Fatalf("dns selectors wrong: ns=%q pods=%v", gotNS, peer.PodSelector)
			}
			if len(dns.Ports) != 2 {
				t.Fatalf("dns ports = %d, want UDP+TCP 53", len(dns.Ports))
			}

			web := egressRules[1]
			if len(web.To) != 2 || web.To[0].IPBlock == nil || web.To[1].IPBlock == nil {
				t.Fatalf("web rule must carry v4+v6 ipBlocks, got %+v", web.To)
			}
			v4, v6 := web.To[0].IPBlock, web.To[1].IPBlock
			if v4.CIDR != "0.0.0.0/0" {
				t.Fatalf("v4 cidr = %q", v4.CIDR)
			}
			if !reflect.DeepEqual(v4.Except, netPublicWebV4Except) {
				t.Fatalf("v4 except = %v, want exact special-use carve-out list", v4.Except)
			}
			if v6.CIDR != "::/0" || !reflect.DeepEqual(v6.Except, netPublicWebV6Except) {
				t.Fatalf("v6 block = %s except %v", v6.CIDR, v6.Except)
			}

			// Ingress default: same-namespace only, no CIDR rules.
			if len(policy.Spec.Ingress) != 1 {
				t.Fatalf("ingress rules = %d, want same-namespace only", len(policy.Spec.Ingress))
			}
			first := policy.Spec.Ingress[0]
			if first.From[0].PodSelector == nil || len(first.From[0].PodSelector.MatchLabels) != 0 {
				t.Fatalf("default ingress from = %+v, want empty podSelector (same namespace)", first.From)
			}
			if len(first.Ports) != 1 || first.Ports[0].Port.IntValue() != ContainerPort {
				t.Fatalf("ingress ports = %+v, want server port only", first.Ports)
			}
		})
	}
}

func TestNetworkPolicyIngressCIDRsInjectOneRuleEach(t *testing.T) {
	policy := NetworkPolicy(netInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Network.IngressCIDRs = []string{"203.0.113.7/32", "198.51.100.0/24"}
	}), nil, "")
	rules := policy.Spec.Ingress
	if len(rules) != 3 {
		t.Fatalf("ingress rules = %d, want same-namespace + 2 CIDRs", len(rules))
	}
	for i, want := range []string{"203.0.113.7/32", "198.51.100.0/24"} {
		block := rules[i+1].From[0].IPBlock
		if block == nil || block.CIDR != want {
			t.Fatalf("rule %d from = %+v, want ipBlock %s", i+1, rules[i+1].From, want)
		}
		if len(rules[i+1].Ports) != 1 || rules[i+1].Ports[0].Port.IntValue() != ContainerPort {
			t.Fatalf("cidr rule %d ports = %+v, want server port", i+1, rules[i+1].Ports)
		}
	}
}

func TestNetworkPolicyUnrestrictedShape(t *testing.T) {
	policy := NetworkPolicy(netInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Network.Egress = EgressUnrestricted
	}), nil, "")
	if len(policy.Spec.Egress) != 1 {
		t.Fatalf("egress rules = %d, want single allow-all rule", len(policy.Spec.Egress))
	}
	to := policy.Spec.Egress[0].To
	if len(to) != 2 {
		t.Fatalf("allow-all peers = %d, want v4+v6 pair", len(to))
	}
	if to[0].IPBlock.CIDR != "0.0.0.0/0" || to[1].IPBlock.CIDR != "::/0" {
		t.Fatalf("allow-all cidrs = %s, %s", to[0].IPBlock.CIDR, to[1].IPBlock.CIDR)
	}
	for _, peer := range to {
		if len(peer.IPBlock.Except) != 0 {
			t.Fatalf("unrestricted peer %s carries exceptions %v", peer.IPBlock.CIDR, peer.IPBlock.Except)
		}
	}
	if len(policy.Spec.Ingress) != 1 {
		t.Fatalf("unrestricted must not change ingress, got %d rules", len(policy.Spec.Ingress))
	}
}

func TestNetworkPolicyAdmitsTheDesktopGateway(t *testing.T) {
	in := netInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Runtime.Version = typeclawv1alpha1.PersonalDesktopMinimumRuntimeVersion
		in.Spec.PersonalDesktop = &typeclawv1alpha1.PersonalDesktopSpec{
			Enabled:   true,
			Namespace: "typeclaw-desktops",
			Owner:     typeclawv1alpha1.PersonalDesktopOwnerSpec{Subject: "alice@example.com"},
			Image:     typeclawv1alpha1.PersonalDesktopImageSpec{GoldenDataVolume: "ubuntu-golden"},
		}
	})
	policy := NetworkPolicy(in, nil, "10.96.0.7")

	// PublicWeb excludes cluster-internal destinations, so without a rule of
	// its own every typed action would be dropped by the runtime's own
	// boundary.
	rule := policy.Spec.Egress[len(policy.Spec.Egress)-1]
	peer := rule.To[0]
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "typeclaw-desktops" {
		t.Fatalf("gateway namespace peer = %+v", peer.NamespaceSelector)
	}
	if peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "typeclaw-desktop-gateway" {
		t.Fatalf("gateway pod peer = %+v", peer.PodSelector)
	}
	// Egress matches pre-DNAT destinations, so the cluster IP the runtime
	// actually dials needs its own block.
	if rule.To[1].IPBlock == nil || rule.To[1].IPBlock.CIDR != "10.96.0.7/32" {
		t.Fatalf("gateway Service peer = %+v", rule.To[1])
	}
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 8080 {
		t.Fatalf("gateway rule ports = %+v", rule.Ports)
	}
}

func TestNetworkPolicyWithoutADesktopHasNoGatewayRule(t *testing.T) {
	policy := NetworkPolicy(netInstance(nil), nil, "10.96.0.7")
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.PodSelector != nil &&
				peer.PodSelector.MatchLabels["app.kubernetes.io/name"] == "typeclaw-desktop-gateway" {
				t.Fatalf("an Instance with no desktop must not admit a gateway")
			}
		}
	}
}
