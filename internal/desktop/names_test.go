package desktop

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// desktopInstance builds an Instance with an enabled Linux desktop that passes
// Validate and the runtime-version gate; mutate narrows it to the case under
// test.
func desktopInstance(mutate func(*typeclawv1alpha1.TypeClawInstance)) *typeclawv1alpha1.TypeClawInstance {
	in := &typeclawv1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "kakao-agent", Namespace: "agents", UID: "instance-uid"},
		Spec: typeclawv1alpha1.TypeClawInstanceSpec{
			Runtime: typeclawv1alpha1.RuntimeSpec{Version: typeclawv1alpha1.PersonalDesktopMinimumRuntimeVersion},
			PersonalDesktop: &typeclawv1alpha1.PersonalDesktopSpec{
				Enabled: true,
				Owner:   typeclawv1alpha1.PersonalDesktopOwnerSpec{Subject: "alice@example.com"},
				Image:   typeclawv1alpha1.PersonalDesktopImageSpec{GoldenDataVolume: "ubuntu-golden"},
			},
		},
	}
	if mutate != nil {
		mutate(in)
	}
	return in
}

func TestNamesDeriveFromTheInstance(t *testing.T) {
	names := Names(desktopInstance(nil))

	for _, tc := range []struct{ got, want string }{
		{names.Desktop, "kakao-agent-desktop"},
		{names.Tokens, "kakao-agent-desktop-tokens"},
		{names.RootVolume, "kakao-agent-desktop-root"},
		{names.GoldenVolume, "ubuntu-golden"},
		{names.CloudInit, "kakao-agent-desktop-cloudinit"},
		{names.Sysprep, "kakao-agent-desktop-sysprep"},
		{names.AgentService, "kakao-agent-desktop-agent"},
		{names.Gateway, "kakao-agent-desktop-gateway"},
		{names.ConsoleIngress, "kakao-agent-desktop-console"},
		{names.Extension, "kakao-agent-desktop-extension"},
		{names.Namespace, "agents"},
		{names.InstanceNamespace, "agents"},
	} {
		if tc.got != tc.want {
			t.Fatalf("name = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestNamesAdoptExistingRootVolume(t *testing.T) {
	names := Names(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.RootVolume.ExistingDataVolume = "poc-desktop-root"
	}))
	if names.RootVolume != "poc-desktop-root" {
		t.Fatalf("root volume = %q, want the adopted disk", names.RootVolume)
	}
}

func TestNamespaceAndCrossNamespace(t *testing.T) {
	same := desktopInstance(nil)
	if Namespace(same) != "agents" || CrossNamespace(same) {
		t.Fatalf("empty spec.namespace must mean the Instance namespace")
	}

	dedicated := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
	})
	if Namespace(dedicated) != "typeclaw-desktops" || !CrossNamespace(dedicated) {
		t.Fatalf("dedicated namespace must be reported as cross-namespace")
	}
}

func TestLabelsCarryTheCleanupSelector(t *testing.T) {
	in := desktopInstance(nil)
	labels := Labels(in)

	want := map[string]string{
		"app.kubernetes.io/name":       DesktopAppName,
		"app.kubernetes.io/instance":   "kakao-agent",
		"app.kubernetes.io/managed-by": "typeclaw-operator",
		LabelInstanceUID:               "instance-uid",
		LabelInstanceNamespace:         "agents",
	}
	for key, value := range want {
		if labels[key] != value {
			t.Fatalf("label %s = %q, want %q", key, labels[key], value)
		}
	}

	// The selector is what stands in for an owner reference across
	// namespaces, so it must pin the exact Instance incarnation.
	selector := OwnedBySelector(in)
	if selector[LabelInstanceUID] != "instance-uid" || selector[LabelInstanceNamespace] != "agents" {
		t.Fatalf("cleanup selector = %v, want uid and namespace", selector)
	}
}

func TestGatewayLabelsSelectOnlyTheGatewayPod(t *testing.T) {
	labels := GatewayLabels(desktopInstance(nil))
	if labels["app.kubernetes.io/name"] != GatewayAppName {
		t.Fatalf("gateway name label = %q, want %q", labels["app.kubernetes.io/name"], GatewayAppName)
	}
	if _, found := labels[LabelInstanceUID]; found {
		t.Fatalf("gateway Pod labels must not carry the UID selector: %v", labels)
	}
}

func TestGatewayURLAddressesTheDesktopNamespace(t *testing.T) {
	url := GatewayURL(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
	}))
	if url != "http://kakao-agent-desktop-gateway.typeclaw-desktops.svc:8080" {
		t.Fatalf("gateway URL = %q", url)
	}
}
