// Package desktop renders the Kubernetes objects behind one Personal Desktop:
// the KubeVirt VirtualMachine and its disks, the Desktop Gateway that fronts
// it, the console Ingress, the traffic boundary around both, and the guest
// bootstrap payloads. Every function here is pure — it takes a
// TypeClawInstance and returns objects — so the controller owns all cluster
// interaction and every rendered shape is testable without a cluster.
//
// names.go fixes the names and labels. Names are derived from the Instance
// rather than hashed from the owner identity so an administrator can find
// every object of a desktop by reading the Instance name, and the two
// typeclaw.fml09.io/instance-* labels stand in for owner references: a desktop
// may live in another namespace, where Kubernetes garbage collection cannot
// follow an owner reference at all and a finalizer plus a label selector is
// the only cleanup path that works.
package desktop

import (
	"strconv"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

const (
	// Finalizer keeps cross-namespace desktop objects reachable after the
	// Instance is deleted. Owner references cannot cross namespaces, so
	// without it a desktop in a dedicated namespace would outlive its owner
	// with nothing left pointing at it.
	Finalizer = "typeclaw.fml09.io/personal-desktop"

	// LabelInstanceUID and LabelInstanceNamespace identify the owning
	// Instance for label-selected cleanup. The UID distinguishes a recreated
	// Instance from the one whose objects are still being removed.
	LabelInstanceUID       = "typeclaw.fml09.io/instance-uid"
	LabelInstanceNamespace = "typeclaw.fml09.io/instance-namespace"

	// DesktopAppName labels every object of one Personal Desktop.
	DesktopAppName = "typeclaw-desktop"
	// GatewayAppName labels the Desktop Gateway Pods specifically; the
	// NetworkPolicy peers select on it.
	GatewayAppName = "typeclaw-desktop-gateway"

	// TokenKeyAgent is the bearer the computer-use plugin presents to the
	// Desktop Gateway.
	TokenKeyAgent = "agent-token"
	// TokenKeyGuest is the bearer the Gateway presents to the Guest Desktop
	// Agent inside the VM.
	TokenKeyGuest = "guest-token"
	// TokenKeyWindowsPassword is the autologon password of the Windows
	// interactive user. Linux desktops lock the account instead.
	TokenKeyWindowsPassword = "windows-password"

	// GatewayAgentPort carries the plugin API and /healthz.
	GatewayAgentPort = 8080
	// GatewayConsolePort carries the Desktop Console.
	GatewayConsolePort = 8081
	// GuestAgentPort is the Guest Desktop Agent's fixed port inside the VM.
	GuestAgentPort = 9876

	// GatewayAgentPortName and GatewayConsolePortName are referenced by the
	// Service, the Ingress backend, and the probes.
	GatewayAgentPortName   = "agent"
	GatewayConsolePortName = "console"

	// ExtensionMountPath is where the computer-use Platform Extension is
	// projected read-only into the Managed Runtime. It is outside the Agent
	// Folder on purpose: an agent must not be able to edit, shadow, or delete
	// the extension that grants it desktop control.
	ExtensionMountPath = "/opt/typeclaw/extensions/personal-desktop-computer-use"
	// ExtensionEntrypoint is the value of TYPECLAW_PLATFORM_EXTENSIONS.
	ExtensionEntrypoint = ExtensionMountPath + "/index.ts"
	// ExtensionVolumeName names the projected ConfigMap volume.
	ExtensionVolumeName = "desktop-extension"
	// ExtensionKey is the ConfigMap key holding the plugin source.
	ExtensionKey = "index.ts"

	// TailscaleIngressClass publishes the console on the tailnet. Funnel is
	// never enabled: it would expose the console to the public Internet and
	// strips the identity header the console authenticates with.
	TailscaleIngressClass = "tailscale"
	// TailscaleTagsAnnotation applies tailnet tags to the proxy device.
	TailscaleTagsAnnotation = "tailscale.com/tags"
	// TailscaleParentResourceLabel is set by the Tailscale operator on the
	// proxy Pod it creates for an Ingress; the console ingress rule selects
	// exactly that Pod.
	TailscaleParentResourceLabel = "tailscale.com/parent-resource"
)

// NameSet carries every derived name of one Personal Desktop. Callers read
// names from here instead of concatenating suffixes, so a rename is one edit.
type NameSet struct {
	// Instance and InstanceNamespace identify the owning TypeClawInstance.
	Instance          string
	InstanceNamespace string

	// Namespace hosts the VM, disks, Gateway, agent Service, and Ingress.
	Namespace string

	// Desktop is the VirtualMachine name and the kubevirt.io/domain value.
	Desktop string

	Tokens         string
	RootVolume     string
	GoldenVolume   string
	CloudInit      string
	Sysprep        string
	AgentService   string
	Gateway        string
	ConsoleIngress string
	Extension      string
}

// Names derives every object name for one Instance. It is safe on an Instance
// with no personalDesktop block: the names are stable, which is what cleanup
// after a removed block needs.
func Names(instance *typeclawv1alpha1.TypeClawInstance) NameSet {
	desktop := instance.Name + "-desktop"
	names := NameSet{
		Instance:          instance.Name,
		InstanceNamespace: instance.Namespace,
		Namespace:         Namespace(instance),
		Desktop:           desktop,
		Tokens:            desktop + "-tokens",
		RootVolume:        desktop + "-root",
		CloudInit:         desktop + "-cloudinit",
		Sysprep:           desktop + "-sysprep",
		AgentService:      desktop + "-agent",
		Gateway:           desktop + "-gateway",
		ConsoleIngress:    desktop + "-console",
		Extension:         desktop + "-extension",
	}
	if spec := instance.Spec.PersonalDesktop; spec != nil {
		names.GoldenVolume = spec.Image.GoldenDataVolume
		if adopted := spec.RootVolume.ExistingDataVolume; adopted != "" {
			names.RootVolume = adopted
		}
	}
	return names
}

// Namespace resolves where the desktop objects live. Empty means the Instance
// namespace; a dedicated namespace is common because KubeVirt relabels a VM's
// namespace to pod-security enforce=privileged.
func Namespace(instance *typeclawv1alpha1.TypeClawInstance) string {
	if spec := instance.Spec.PersonalDesktop; spec != nil && spec.Namespace != "" {
		return spec.Namespace
	}
	return instance.Namespace
}

// CrossNamespace reports whether the desktop lives outside the Instance
// namespace. Owner references cannot cross namespaces, so this decides
// whether cleanup runs on the finalizer path instead of garbage collection.
func CrossNamespace(instance *typeclawv1alpha1.TypeClawInstance) bool {
	return Namespace(instance) != instance.Namespace
}

// Labels returns the label set carried by every object of one desktop.
func Labels(instance *typeclawv1alpha1.TypeClawInstance) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       DesktopAppName,
		"app.kubernetes.io/instance":   instance.Name,
		"app.kubernetes.io/managed-by": "typeclaw-operator",
		LabelInstanceUID:               string(instance.UID),
		LabelInstanceNamespace:         instance.Namespace,
	}
}

// GatewayLabels returns the Desktop Gateway Pod labels. They differ from
// Labels because NetworkPolicy peers must select the Gateway Pod alone, not
// every Pod of the desktop.
func GatewayLabels(instance *typeclawv1alpha1.TypeClawInstance) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       GatewayAppName,
		"app.kubernetes.io/instance":   instance.Name,
		"app.kubernetes.io/managed-by": "typeclaw-operator",
	}
}

// OwnedBySelector selects every desktop object belonging to exactly this
// Instance incarnation. The UID is part of the selector so a deleted and
// recreated Instance never adopts the previous one's leftovers.
func OwnedBySelector(instance *typeclawv1alpha1.TypeClawInstance) map[string]string {
	return map[string]string{
		LabelInstanceUID:       string(instance.UID),
		LabelInstanceNamespace: instance.Namespace,
	}
}

// GatewayURL is the in-cluster address the computer-use Platform Extension
// posts typed actions to.
func GatewayURL(instance *typeclawv1alpha1.TypeClawInstance) string {
	names := Names(instance)
	return "http://" + names.Gateway + "." + names.Namespace + ".svc:" + strconv.Itoa(GatewayAgentPort)
}
