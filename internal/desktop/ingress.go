// ingress.go publishes the Desktop Console on the owner's tailnet.
//
// The Tailscale Kubernetes operator turns this Ingress into a proxy device and
// injects the Tailscale-User-Login header, overwriting anything the client
// sent — that header is the console's whole authentication story. Funnel is
// never enabled: it would expose the console to the public Internet and strips
// exactly the identity header the console trusts, so a Funnel-published
// console would be an unauthenticated remote desktop.
package desktop

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// ConsoleIngress renders the console publication, or nil when the console is
// not published. An unpublished desktop is still fully usable by the agent.
func ConsoleIngress(instance *typeclawv1alpha1.TypeClawInstance) *networkingv1.Ingress {
	access := TailscaleAccess(instance.Spec.PersonalDesktop)
	if access == nil || access.Hostname == "" {
		return nil
	}
	// Sidecar mode publishes the console from tailscaled inside the Gateway
	// Pod, so there is no Ingress and no proxy Pod in another namespace.
	if ConsoleSidecar(instance.Spec.PersonalDesktop) {
		return nil
	}
	names := Names(instance)
	ingressClass := TailscaleIngressClass

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.ConsoleIngress,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			// The Tailscale operator reads the TLS host as the MagicDNS label
			// of the device it creates; the certificate is issued for
			// <hostname>.<tailnet>.ts.net.
			TLS: []networkingv1.IngressTLS{{Hosts: []string{access.Hostname}}},
			DefaultBackend: &networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{
					Name: names.Gateway,
					Port: networkingv1.ServiceBackendPort{Name: GatewayConsolePortName},
				},
			},
		},
	}
	if tags := joinNonEmpty(access.Tags); tags != "" {
		ingress.Annotations = map[string]string{TailscaleTagsAnnotation: tags}
	}
	return ingress
}

// ConsoleURL reports the console address for an Instance.
//
// In Sidecar mode it is known from the spec the moment the desktop is
// declared: tailscaled serves the device's own MagicDNS name, so there is
// nothing to observe and nothing to wait for. That matters beyond tidiness — a
// console address knowable only from observed status ends up feeding the
// Runtime's Pod template from status, and then every transition of it restarts
// the agent outside the chart's quiesce window.
//
// In Ingress mode the address genuinely is an observation, because the
// Tailscale operator names the device; the caller passes the Ingress it read.
func ConsoleURL(instance *typeclawv1alpha1.TypeClawInstance, ingress *networkingv1.Ingress) string {
	if ConsoleSidecar(instance.Spec.PersonalDesktop) {
		return "https://" + TailscaleAccess(instance.Spec.PersonalDesktop).Hostname
	}
	return ConsoleURLFrom(ingress)
}

// ConsoleURLFrom derives the reported console address from the Ingress status
// the Tailscale operator fills in. Before the device exists the status is
// empty and the desktop reports no console URL rather than a guess.
func ConsoleURLFrom(ingress *networkingv1.Ingress) string {
	if ingress == nil {
		return ""
	}
	for _, entry := range ingress.Status.LoadBalancer.Ingress {
		if entry.Hostname != "" {
			return "https://" + entry.Hostname
		}
	}
	return ""
}

// TailscaleProxyPodSelector selects the proxy Pod the Tailscale operator
// creates for this console Ingress. The operator stamps the parent resource
// name onto that Pod, which is the only way to admit the proxy without
// admitting every Pod in the Tailscale namespace.
func TailscaleProxyPodSelector(instance *typeclawv1alpha1.TypeClawInstance) map[string]string {
	return map[string]string{TailscaleParentResourceLabel: Names(instance).ConsoleIngress}
}
