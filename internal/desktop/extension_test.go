package desktop

import (
	"testing"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/extensions"
)

func TestExtensionConfigMapLivesBesideTheInstance(t *testing.T) {
	configMap := ExtensionConfigMap(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
	}))

	// The runtime Pod projects it, and a Pod can only project a ConfigMap
	// from its own namespace, so this one object stays with the Instance even
	// when the desktop lives elsewhere.
	if configMap.Namespace != "agents" {
		t.Fatalf("extension ConfigMap namespace = %q, want the Instance namespace", configMap.Namespace)
	}
	if configMap.Name != "kakao-agent-desktop-extension" {
		t.Fatalf("extension ConfigMap name = %q", configMap.Name)
	}
	if configMap.Data[ExtensionKey] != extensions.PersonalDesktopComputerUse {
		t.Fatalf("the projected plugin is not the embedded Platform Extension")
	}
	if ExtensionEntrypoint != ExtensionMountPath+"/"+ExtensionKey {
		t.Fatalf("the entrypoint %q does not address the mounted key", ExtensionEntrypoint)
	}
}
