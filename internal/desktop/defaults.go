// defaults.go resolves the effective value of every optional desktop setting.
//
// The CRD carries kubebuilder defaults, but the API server only applies them
// to objects that went through it: a spec built in a test, an Instance created
// before a field existed, or a field a client cleared all reach the renderers
// zero-valued. Rendering a VM with zero cores or a 0x0 screen would produce a
// desktop that boots and is useless, so every renderer reads its inputs
// through this file instead of the spec directly.
package desktop

import (
	"k8s.io/apimachinery/pkg/api/resource"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

const (
	// OSLinux and OSWindows are the guest template selectors.
	OSLinux   = "Linux"
	OSWindows = "Windows"

	defaultScreenWidth      = 1280
	defaultScreenHeight     = 800
	defaultCPUCores         = 2
	defaultUsername         = "desktop"
	defaultOwnerIssuer      = "https://login.tailscale.com"
	defaultTailscaleNS      = "tailscale"
	defaultAgentLeaseTTLSec = 120
)

var (
	defaultMemory     = resource.MustParse("4Gi")
	defaultVolumeSize = resource.MustParse("32Gi")
)

// Enabled reports whether the Instance declares a running Personal Desktop.
func Enabled(instance *typeclawv1alpha1.TypeClawInstance) bool {
	spec := instance.Spec.PersonalDesktop
	return spec != nil && spec.Enabled
}

// OS resolves the guest template selector.
func OS(spec *typeclawv1alpha1.PersonalDesktopSpec) string {
	if spec != nil && spec.OS == OSWindows {
		return OSWindows
	}
	return OSLinux
}

// GuestOSName is the lowercase form the Desktop Gateway and the Guest Desktop
// Agent exchange (DESKTOP_OS).
func GuestOSName(spec *typeclawv1alpha1.PersonalDesktopSpec) string {
	if OS(spec) == OSWindows {
		return "windows"
	}
	return "linux"
}

// Screen resolves the fixed guest display geometry.
func Screen(spec *typeclawv1alpha1.PersonalDesktopSpec) (width, height int32) {
	width, height = defaultScreenWidth, defaultScreenHeight
	if spec == nil || spec.Screen == nil {
		return width, height
	}
	if spec.Screen.Width > 0 {
		width = spec.Screen.Width
	}
	if spec.Screen.Height > 0 {
		height = spec.Screen.Height
	}
	return width, height
}

// CPUCores resolves the guest core count.
func CPUCores(spec *typeclawv1alpha1.PersonalDesktopSpec) int32 {
	if spec != nil && spec.Resources.CPUCores > 0 {
		return spec.Resources.CPUCores
	}
	return defaultCPUCores
}

// Memory resolves the guest memory request.
func Memory(spec *typeclawv1alpha1.PersonalDesktopSpec) resource.Quantity {
	if spec != nil && !spec.Resources.Memory.IsZero() {
		return spec.Resources.Memory
	}
	return defaultMemory.DeepCopy()
}

// RootVolumeSize resolves the cloned root disk size.
func RootVolumeSize(spec *typeclawv1alpha1.PersonalDesktopSpec) resource.Quantity {
	if spec != nil && !spec.RootVolume.Size.IsZero() {
		return spec.RootVolume.Size
	}
	return defaultVolumeSize.DeepCopy()
}

// GoldenVolumeSize resolves the imported golden image size.
func GoldenVolumeSize(importSpec *typeclawv1alpha1.PersonalDesktopImageImportSpec) resource.Quantity {
	if importSpec != nil && !importSpec.Size.IsZero() {
		return importSpec.Size
	}
	return defaultVolumeSize.DeepCopy()
}

// Username resolves the interactive guest account name for the selected OS.
func Username(spec *typeclawv1alpha1.PersonalDesktopSpec) string {
	if spec != nil {
		if OS(spec) == OSWindows {
			if spec.Windows != nil && spec.Windows.Username != "" {
				return spec.Windows.Username
			}
		} else if spec.Linux != nil && spec.Linux.Username != "" {
			return spec.Linux.Username
		}
	}
	return defaultUsername
}

// OwnerIssuer resolves the identity provider the console compares against.
func OwnerIssuer(spec *typeclawv1alpha1.PersonalDesktopSpec) string {
	if spec != nil && spec.Owner.Issuer != "" {
		return spec.Owner.Issuer
	}
	return defaultOwnerIssuer
}

// TailscaleOperatorNamespace resolves where the Tailscale proxy Pods run. The
// console listener is reachable from that namespace alone.
func TailscaleOperatorNamespace(access *typeclawv1alpha1.PersonalDesktopTailscaleAccessSpec) string {
	if access != nil && access.OperatorNamespace != "" {
		return access.OperatorNamespace
	}
	return defaultTailscaleNS
}

// AgentLeaseTTLSeconds resolves the idle TTL of the agent input lease.
func AgentLeaseTTLSeconds(spec *typeclawv1alpha1.PersonalDesktopSpec) int32 {
	if spec != nil && spec.Gateway != nil && spec.Gateway.AgentLeaseTTLSeconds > 0 {
		return spec.Gateway.AgentLeaseTTLSeconds
	}
	return defaultAgentLeaseTTLSec
}

// VirtioContainerDisk resolves the virtio-win containerDisk. Without those
// drivers a Windows guest cannot see its own virtio root disk or NIC.
func VirtioContainerDisk(spec *typeclawv1alpha1.PersonalDesktopSpec) string {
	if spec != nil && spec.Windows != nil && spec.Windows.VirtioContainerDisk != "" {
		return spec.Windows.VirtioContainerDisk
	}
	return typeclawv1alpha1.PersonalDesktopDefaultVirtioContainerDisk
}

// PythonInstaller resolves the interpreter download the Windows first-logon
// script falls back to, together with the digest the guest verifies.
func PythonInstaller(spec *typeclawv1alpha1.PersonalDesktopSpec) (url, sha256 string) {
	if spec != nil && spec.Windows != nil && spec.Windows.PythonInstaller != nil {
		installer := spec.Windows.PythonInstaller
		if installer.URL != "" && installer.SHA256 != "" {
			return installer.URL, installer.SHA256
		}
	}
	return typeclawv1alpha1.PersonalDesktopDefaultPythonURL,
		typeclawv1alpha1.PersonalDesktopDefaultPythonSHA256
}

// TailscaleAccess returns the console publication settings, or nil when the
// console is not published at all (the agent path still works).
func TailscaleAccess(spec *typeclawv1alpha1.PersonalDesktopSpec) *typeclawv1alpha1.PersonalDesktopTailscaleAccessSpec {
	if spec == nil || spec.Access == nil {
		return nil
	}
	return spec.Access.Tailscale
}

// Console exposure modes. They differ in what makes the Tailscale-User-Login
// header trustworthy, which is the console's whole authentication story.
const (
	// ConsoleModeIngress publishes through the Tailscale Kubernetes operator
	// and leans on a NetworkPolicy to keep every other Pod off the console
	// port. Sound only where the CNI enforces NetworkPolicy.
	ConsoleModeIngress = "Ingress"
	// ConsoleModeSidecar runs tailscaled in the Gateway Pod and binds the
	// console to loopback, so reachability is enforced by the network
	// namespace rather than by a policy engine.
	ConsoleModeSidecar = "Sidecar"
)

// ConsoleMode resolves the console exposure mode, defaulting to Ingress to
// match the CRD default for an Instance stored before the field existed.
func ConsoleMode(spec *typeclawv1alpha1.PersonalDesktopSpec) string {
	access := TailscaleAccess(spec)
	if access == nil || access.Mode == "" {
		return ConsoleModeIngress
	}
	return access.Mode
}

// ConsoleSidecar reports whether the Gateway Pod carries its own tailscaled.
// It is false when the console is not published at all, because there is then
// no console to front.
func ConsoleSidecar(spec *typeclawv1alpha1.PersonalDesktopSpec) bool {
	access := TailscaleAccess(spec)
	return access != nil && access.Hostname != "" && ConsoleMode(spec) == ConsoleModeSidecar
}

// ConsoleListenAddress is where the Gateway binds the console listener. In
// Sidecar mode it is loopback: the tailscaled beside it shares the Pod's
// network namespace and reaches it there, while nothing else can reach it at
// all. In Ingress mode the Tailscale proxy is a separate Pod, so the listener
// has to be on the Pod network and a NetworkPolicy is the only guard left.
func ConsoleListenAddress(spec *typeclawv1alpha1.PersonalDesktopSpec) string {
	if ConsoleSidecar(spec) {
		return "127.0.0.1:" + itoa32(GatewayConsolePort)
	}
	return ":" + itoa32(GatewayConsolePort)
}
