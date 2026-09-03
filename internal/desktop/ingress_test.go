package desktop

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func tailscaleInstance(mutate func(*typeclawv1alpha1.PersonalDesktopTailscaleAccessSpec)) *typeclawv1alpha1.TypeClawInstance {
	return desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		access := &typeclawv1alpha1.PersonalDesktopTailscaleAccessSpec{Hostname: "kakao-desktop"}
		if mutate != nil {
			mutate(access)
		}
		in.Spec.PersonalDesktop.Access = &typeclawv1alpha1.PersonalDesktopAccessSpec{Tailscale: access}
	})
}

func TestConsoleIngressIsAbsentWithoutDeclaredAccess(t *testing.T) {
	// An unpublished desktop is still fully usable by the agent.
	if ConsoleIngress(desktopInstance(nil)) != nil {
		t.Fatalf("a console must not be published unless access declares it")
	}
}

func TestConsoleIngressPublishesOnTheTailnet(t *testing.T) {
	ingress := ConsoleIngress(tailscaleInstance(func(access *typeclawv1alpha1.PersonalDesktopTailscaleAccessSpec) {
		access.Tags = []string{"tag:typeclaw-desktop", ""}
	}))

	if ingress.Name != "kakao-agent-desktop-console" {
		t.Fatalf("console Ingress name = %q", ingress.Name)
	}
	if *ingress.Spec.IngressClassName != TailscaleIngressClass {
		t.Fatalf("ingress class = %q", *ingress.Spec.IngressClassName)
	}
	if len(ingress.Spec.TLS) != 1 || ingress.Spec.TLS[0].Hosts[0] != "kakao-desktop" {
		t.Fatalf("tls hosts = %+v", ingress.Spec.TLS)
	}
	backend := ingress.Spec.DefaultBackend.Service
	if backend.Name != "kakao-agent-desktop-gateway" || backend.Port.Name != GatewayConsolePortName {
		t.Fatalf("default backend = %+v", backend)
	}
	if ingress.Annotations[TailscaleTagsAnnotation] != "tag:typeclaw-desktop" {
		t.Fatalf("tags annotation = %q", ingress.Annotations[TailscaleTagsAnnotation])
	}
	// Funnel would publish the console to the public Internet and strip the
	// identity header the console authenticates with.
	if _, found := ingress.Annotations["tailscale.com/funnel"]; found {
		t.Fatalf("funnel must never be enabled: %v", ingress.Annotations)
	}
}

func TestConsoleIngressOmitsAnEmptyTagAnnotation(t *testing.T) {
	ingress := ConsoleIngress(tailscaleInstance(nil))
	if _, found := ingress.Annotations[TailscaleTagsAnnotation]; found {
		t.Fatalf("no tags declared, yet the annotation was set: %v", ingress.Annotations)
	}
}

func TestConsoleURLFromReadsTheAccessProviderStatus(t *testing.T) {
	if ConsoleURLFrom(nil) != "" {
		t.Fatalf("a missing Ingress reports no console URL")
	}
	ingress := ConsoleIngress(tailscaleInstance(nil))
	if ConsoleURLFrom(ingress) != "" {
		t.Fatalf("before the tailnet device exists there is no URL to report")
	}
	ingress.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{
		{Hostname: "kakao-desktop.tailnet.ts.net"},
	}
	if got := ConsoleURLFrom(ingress); got != "https://kakao-desktop.tailnet.ts.net" {
		t.Fatalf("ConsoleURLFrom() = %q", got)
	}
}

func TestTailscaleProxyPodSelectorNamesTheConsoleIngress(t *testing.T) {
	selector := TailscaleProxyPodSelector(desktopInstance(nil))
	if selector[TailscaleParentResourceLabel] != "kakao-agent-desktop-console" {
		t.Fatalf("proxy selector = %v", selector)
	}
}
