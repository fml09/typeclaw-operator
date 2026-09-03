// extension.go projects the computer-use Platform Extension into the Managed
// Runtime.
//
// The plugin source is embedded in the operator image and delivered as a
// ConfigMap mounted read-only, never written into the Agent Folder. That is
// what makes it administrator-owned: an agent can edit anything in its own
// folder, so a plugin that lived there could be rewritten by the very model it
// is supposed to constrain, and upgrading the operator would not upgrade it.
package desktop

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/extensions"
)

// ExtensionConfigMap renders the plugin source into the Instance namespace,
// where the runtime Pod can project it. It is the one desktop object that
// always lives beside the Instance rather than beside the VM.
func ExtensionConfigMap(instance *typeclawv1alpha1.TypeClawInstance) *corev1.ConfigMap {
	names := Names(instance)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Extension,
			Namespace: names.InstanceNamespace,
			Labels:    Labels(instance),
		},
		Data: map[string]string{ExtensionKey: extensions.PersonalDesktopComputerUse},
	}
}
