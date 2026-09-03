// vm.go renders the KubeVirt VirtualMachine and the in-cluster Service that
// reaches the Guest Desktop Agent inside it.
//
// runStrategy is Manual on purpose: a Personal Desktop is a machine its owner
// powers on and off, and any Always-style strategy would fight the Desktop
// Gateway's power endpoints by restarting a desktop the owner just shut down.
// The guest is reached through one declared masquerade port, so the only way
// into the VM from the cluster is the typed-action port the Gateway uses.
package desktop

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// DomainLabel carries the KubeVirt domain name onto the virt-launcher Pod.
// The agent Service and the Gateway's egress rule both select on it, because
// it is the only label that identifies the Pod backing one specific VM.
const DomainLabel = "kubevirt.io/domain"

// VM renders the VirtualMachine for one Personal Desktop.
func VM(instance *typeclawv1alpha1.TypeClawInstance) *unstructured.Unstructured {
	spec := instance.Spec.PersonalDesktop
	names := Names(instance)

	// resource.Quantity renders through a pointer receiver, so the memory
	// request has to be named before it can be formatted.
	memory := Memory(spec)

	obj := NewObject(VirtualMachineGVK, names.Desktop, names.Namespace)
	obj.SetLabels(Labels(instance))

	templateLabels := Labels(instance)
	templateLabels[DomainLabel] = names.Desktop

	domain := map[string]any{
		"cpu": map[string]any{"cores": int64(CPUCores(spec))},
		"devices": map[string]any{
			"autoattachGraphicsDevice": true,
			"disks":                    diskDevices(spec),
			// A tablet gives the guest absolute pointer coordinates. With a
			// relative mouse the model's "click at (x,y)" would land wherever
			// the pointer happened to be, which is the single most common way
			// a computer-use loop goes silently wrong.
			"inputs": []any{
				map[string]any{"name": "tablet", "type": "tablet", "bus": "usb"},
			},
			"interfaces": []any{
				map[string]any{
					"name":       "default",
					"masquerade": map[string]any{},
					"ports": []any{
						map[string]any{
							"name":     "desktop-agent",
							"protocol": "TCP",
							"port":     int64(GuestAgentPort),
						},
					},
				},
			},
		},
		"machine":   map[string]any{"type": "q35"},
		"resources": map[string]any{"requests": map[string]any{"memory": memory.String()}},
	}
	applyGuestPlatform(domain, spec)

	template := map[string]any{
		"metadata": map[string]any{"labels": toAnyMap(templateLabels)},
		"spec": map[string]any{
			"architecture":                  "amd64",
			"domain":                        domain,
			"networks":                      []any{map[string]any{"name": "default", "pod": map[string]any{}}},
			"terminationGracePeriodSeconds": int64(90),
			"volumes":                       volumes(instance),
		},
	}
	if spec != nil && len(spec.NodeSelector) > 0 {
		template["spec"].(map[string]any)["nodeSelector"] = toAnyMap(spec.NodeSelector)
	}

	SetSpec(obj, map[string]any{
		"runStrategy": "Manual",
		"template":    template,
	})
	return obj
}

// diskDevices lists the guest's disks in boot order. Linux takes cloud-init
// on a second virtio disk; Windows takes the virtio driver CD-ROM plus the
// sysprep CD-ROM, because a stock Windows image cannot see a virtio root disk
// until those drivers load.
func diskDevices(spec *typeclawv1alpha1.PersonalDesktopSpec) []any {
	disks := []any{
		map[string]any{"name": "rootdisk", "disk": map[string]any{"bus": "virtio"}},
	}
	if OS(spec) == OSWindows {
		return append(disks,
			map[string]any{"name": "virtiocontainerdisk", "cdrom": map[string]any{"bus": "sata"}},
			map[string]any{"name": "sysprep", "cdrom": map[string]any{"bus": "sata"}},
		)
	}
	return append(disks,
		map[string]any{"name": "cloudinit", "disk": map[string]any{"bus": "virtio"}},
	)
}

// volumes binds the disks to their sources. The root disk references a
// standalone DataVolume rather than a dataVolumeTemplate so deleting the VM
// never deletes the owner's data.
func volumes(instance *typeclawv1alpha1.TypeClawInstance) []any {
	spec := instance.Spec.PersonalDesktop
	names := Names(instance)
	items := []any{
		map[string]any{
			"name":       "rootdisk",
			"dataVolume": map[string]any{"name": names.RootVolume},
		},
	}
	if OS(spec) == OSWindows {
		return append(items,
			map[string]any{
				"name":          "virtiocontainerdisk",
				"containerDisk": map[string]any{"image": VirtioContainerDisk(spec)},
			},
			map[string]any{
				"name": "sysprep",
				"sysprep": map[string]any{
					"secret": map[string]any{"name": names.Sysprep},
				},
			},
		)
	}
	return append(items, map[string]any{
		"name": "cloudinit",
		"cloudInitNoCloud": map[string]any{
			"secretRef": map[string]any{"name": names.CloudInit},
		},
	})
}

// applyGuestPlatform adds the firmware, chipset features and clock a Windows
// guest needs. Modern Windows refuses to install or boot without UEFI and a
// TPM, and without the Hyper-V enlightenments its idle CPU use and timer
// behaviour make an interactive desktop unusable.
func applyGuestPlatform(domain map[string]any, spec *typeclawv1alpha1.PersonalDesktopSpec) {
	if OS(spec) != OSWindows {
		domain["features"] = map[string]any{"acpi": map[string]any{}}
		return
	}
	domain["firmware"] = map[string]any{
		"bootloader": map[string]any{
			// Secure Boot would require a signed OVMF stack and rejects the
			// unsigned virtio drivers the guest loads from the CD-ROM.
			"efi": map[string]any{"secureBoot": false},
		},
	}
	domain["devices"].(map[string]any)["tpm"] = map[string]any{}
	domain["features"] = map[string]any{
		"acpi": map[string]any{},
		"apic": map[string]any{},
		"hyperv": map[string]any{
			"relaxed":   map[string]any{},
			"vapic":     map[string]any{},
			"spinlocks": map[string]any{"spinlocks": int64(8191)},
		},
	}
	domain["clock"] = map[string]any{
		"utc": map[string]any{},
		"timer": map[string]any{
			"hpet":   map[string]any{"present": false},
			"pit":    map[string]any{"tickPolicy": "delay"},
			"rtc":    map[string]any{"tickPolicy": "catchup"},
			"hyperv": map[string]any{},
		},
	}
}

// AgentService forwards the typed-action port to the VM's masquerade
// interface. The Desktop Gateway is the only intended caller; the Guest
// Desktop Agent authenticates it with the guest bearer token.
func AgentService(instance *typeclawv1alpha1.TypeClawInstance) *corev1.Service {
	names := Names(instance)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.AgentService,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{DomainLabel: names.Desktop},
			Ports: []corev1.ServicePort{{
				Name:       "desktop-agent",
				Port:       GuestAgentPort,
				TargetPort: intstr.FromInt32(GuestAgentPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
