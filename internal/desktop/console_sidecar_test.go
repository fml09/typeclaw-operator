package desktop

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func sidecarInstance(mutate ...func(*typeclawv1alpha1.TypeClawInstance)) *typeclawv1alpha1.TypeClawInstance {
	in := &typeclawv1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "kakao-agent", Namespace: "typeclaw"},
		Spec: typeclawv1alpha1.TypeClawInstanceSpec{
			PersonalDesktop: &typeclawv1alpha1.PersonalDesktopSpec{
				Enabled: true,
				Owner:   typeclawv1alpha1.PersonalDesktopOwnerSpec{Subject: "owner@example.com"},
				Image:   typeclawv1alpha1.PersonalDesktopImageSpec{GoldenDataVolume: "ubuntu-golden"},
				Access: &typeclawv1alpha1.PersonalDesktopAccessSpec{
					Tailscale: &typeclawv1alpha1.PersonalDesktopTailscaleAccessSpec{
						Hostname:   "kakao-desktop",
						Mode:       ConsoleModeSidecar,
						AuthSecret: "tailscale-console",
					},
				},
			},
		},
	}
	for _, m := range mutate {
		m(in)
	}
	return in
}

// TestSidecarConsoleIsNotOnThePodNetwork is the security property this mode
// exists for. Every surface that could carry a request from another Pod to the
// console has to be absent, because a NetworkPolicy cannot be relied on to
// close them: on a cluster whose CNI does not enforce policy the object is
// stored and ignored.
func TestSidecarConsoleIsNotOnThePodNetwork(t *testing.T) {
	in := sidecarInstance()

	if got := ConsoleListenAddress(in.Spec.PersonalDesktop); got != "127.0.0.1:8081" {
		t.Fatalf("console listen address = %q, want loopback", got)
	}

	deployment := GatewayDeployment(in, "operator:test", "")
	gateway := deployment.Spec.Template.Spec.Containers[0]
	for _, port := range gateway.Ports {
		if port.ContainerPort == GatewayConsolePort {
			t.Fatal("the console must not be declared as a container port in Sidecar mode")
		}
	}

	for _, port := range GatewayService(in).Spec.Ports {
		if port.Port == GatewayConsolePort {
			t.Fatal("the console must not be published by the Service in Sidecar mode")
		}
	}

	if ingress := ConsoleIngress(in); ingress != nil {
		t.Fatal("Sidecar mode publishes the console itself; no Ingress belongs to it")
	}
}

// TestIngressModeKeepsThePodNetworkSurfaces is the converse, so the change
// above cannot quietly become unconditional.
func TestIngressModeKeepsThePodNetworkSurfaces(t *testing.T) {
	in := sidecarInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Access.Tailscale.Mode = ConsoleModeIngress
	})

	if got := ConsoleListenAddress(in.Spec.PersonalDesktop); got != ":8081" {
		t.Fatalf("console listen address = %q, want the Pod network", got)
	}
	if ConsoleIngress(in) == nil {
		t.Fatal("Ingress mode must still render an Ingress")
	}
	found := false
	for _, port := range GatewayService(in).Spec.Ports {
		if port.Port == GatewayConsolePort {
			found = true
		}
	}
	if !found {
		t.Fatal("Ingress mode must still publish the console port")
	}
}

func TestSidecarRendersTailscaledBesideTheGateway(t *testing.T) {
	in := sidecarInstance()
	pod := GatewayDeployment(in, "operator:test", "").Spec.Template.Spec

	var sidecar *corev1.Container
	for i := range pod.Containers {
		if pod.Containers[i].Name == TailscaleSidecarName {
			sidecar = &pod.Containers[i]
		}
	}
	if sidecar == nil {
		t.Fatal("no tailscaled container was rendered")
	}
	if sidecar.Image != DefaultTailscaleImage {
		t.Fatalf("image = %q, want %q", sidecar.Image, DefaultTailscaleImage)
	}

	env := map[string]corev1.EnvVar{}
	for _, e := range sidecar.Env {
		env[e.Name] = e
	}
	if env["TS_USERSPACE"].Value != "true" {
		t.Error("userspace networking keeps the sidecar unprivileged; it must be on")
	}
	if env["TS_HOSTNAME"].Value != "kakao-desktop" {
		t.Errorf("TS_HOSTNAME = %q, want the spec hostname", env["TS_HOSTNAME"].Value)
	}
	// Observed failure, not a hypothetical: without this the container tries a
	// Secret named "tailscale" even though TS_STATE_DIR is set, and dies with
	// "missing get permission on secret" before tailscaled ever starts.
	kube, declared := env["TS_KUBE_SECRET"]
	if !declared || kube.Value != "" {
		t.Errorf("TS_KUBE_SECRET = %q (declared %t); it must be explicitly empty so state stays on the emptyDir",
			kube.Value, declared)
	}
	if env["TS_STATE_DIR"].Value != TailscaleStateDir {
		t.Errorf("TS_STATE_DIR = %q, want %q", env["TS_STATE_DIR"].Value, TailscaleStateDir)
	}
	// Either credential shape may be the one the Secret carries, so both
	// references must be optional or the Pod cannot start with only one.
	for _, key := range []string{"TS_AUTHKEY", "TS_CLIENT_ID", "TS_CLIENT_SECRET"} {
		ref := env[key].ValueFrom
		if ref == nil || ref.SecretKeyRef == nil {
			t.Fatalf("%s must come from a Secret", key)
		}
		if ref.SecretKeyRef.Name != "tailscale-console" {
			t.Errorf("%s reads Secret %q, want the declared authSecret", key, ref.SecretKeyRef.Name)
		}
		if ref.SecretKeyRef.Optional == nil || !*ref.SecretKeyRef.Optional {
			t.Errorf("%s must be optional so one Secret may carry either credential shape", key)
		}
	}
	if sidecar.SecurityContext == nil || sidecar.SecurityContext.AllowPrivilegeEscalation == nil ||
		*sidecar.SecurityContext.AllowPrivilegeEscalation {
		t.Error("the sidecar must not be allowed to escalate privileges")
	}
}

// TestServeConfigProxiesToLoopback pins the one line that decides whether the
// console is reachable from outside the Pod at all.
func TestServeConfigProxiesToLoopback(t *testing.T) {
	configMap := GatewayServeConfig(sidecarInstance())
	if configMap == nil {
		t.Fatal("Sidecar mode must render a Serve config")
	}
	raw, ok := configMap.Data[ServeConfigKey]
	if !ok {
		t.Fatalf("Serve config is missing key %q", ServeConfigKey)
	}

	var parsed struct {
		TCP map[string]struct {
			HTTPS bool
		}
		Web map[string]struct {
			Handlers map[string]struct {
				Proxy string
			}
		}
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("Serve config is not valid JSON: %v\n%s", err, raw)
	}
	if !parsed.TCP["443"].HTTPS {
		t.Error("tailscaled must terminate TLS on 443")
	}
	proxy := parsed.Web["${TS_CERT_DOMAIN}:443"].Handlers["/"].Proxy
	if !strings.HasPrefix(proxy, "http://127.0.0.1:") {
		t.Fatalf("Serve proxies to %q; it must target loopback", proxy)
	}
}

func TestSidecarModeRequiresAnAuthSecret(t *testing.T) {
	in := sidecarInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Access.Tailscale.AuthSecret = ""
	})
	err := Validate(in)
	if err == nil {
		t.Fatal("Sidecar mode without a credential can never bring the console up; it must not validate")
	}
	if !strings.Contains(err.Error(), "authSecret") {
		t.Fatalf("error %q should name the missing field", err)
	}
}

// TestConsoleURLIsSpecDerivedInSidecarMode matters beyond convenience: an
// address that is only knowable from observed status ends up in the Runtime's
// Pod template and restarts the agent when it changes.
func TestConsoleURLIsSpecDerivedInSidecarMode(t *testing.T) {
	if got := ConsoleURL(sidecarInstance(), nil); got != "https://kakao-desktop" {
		t.Fatalf("console URL = %q, want the spec-derived MagicDNS name", got)
	}
	ingressOnly := sidecarInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Access.Tailscale.Mode = ConsoleModeIngress
	})
	if got := ConsoleURL(ingressOnly, nil); got != "" {
		t.Fatalf("console URL = %q; in Ingress mode it is only knowable by observation", got)
	}
}

// TestNoConsoleNetworkPolicyRuleInSidecarMode: with the listener on loopback
// there is nothing a policy could admit, and writing a rule anyway would imply
// the console is a Pod-network surface.
func TestNoConsoleNetworkPolicyRuleInSidecarMode(t *testing.T) {
	policy := GatewayNetworkPolicy(sidecarInstance(), map[string]string{"app": "runtime"}, nil, "")
	for _, rule := range policy.Spec.Ingress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntValue() == GatewayConsolePort {
				t.Fatal("no ingress rule should name the console port in Sidecar mode")
			}
		}
	}
	if len(policy.Spec.Egress) == 0 {
		t.Fatal("tailscaled needs egress to reach the control plane and DERP")
	}
	public := false
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
				public = true
			}
		}
	}
	if !public {
		t.Fatal("without public egress the tailnet device never registers and the console never appears")
	}
}
