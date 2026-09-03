package desktop

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// namedEntry finds one entry of a KubeVirt list-of-maps keyed by "name",
// which is how disks, volumes and interfaces are addressed in a VM spec.
func namedEntry(t *testing.T, entries any, name string) map[string]any {
	t.Helper()
	list, ok := entries.([]any)
	if !ok {
		t.Fatalf("expected a list, got %T", entries)
	}
	for _, entry := range list {
		item, ok := entry.(map[string]any)
		if ok && item["name"] == name {
			return item
		}
	}
	t.Fatalf("no entry named %q in %v", name, entries)
	return nil
}

func TestVMLinuxShape(t *testing.T) {
	vm := VM(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Resources.CPUCores = 4
		in.Spec.PersonalDesktop.Resources.Memory = resource.MustParse("8Gi")
		in.Spec.PersonalDesktop.NodeSelector = map[string]string{"kubevirt.io/schedulable": "true"}
	}))

	if vm.GroupVersionKind() != VirtualMachineGVK {
		t.Fatalf("gvk = %v", vm.GroupVersionKind())
	}
	if got := nested(t, vm, "spec", "runStrategy"); got != "Manual" {
		t.Fatalf("runStrategy = %v, want Manual so power stays with the owner", got)
	}
	labels := nested(t, vm, "spec", "template", "metadata", "labels").(map[string]any)
	if labels[DomainLabel] != "kakao-agent-desktop" {
		t.Fatalf("template domain label = %v", labels[DomainLabel])
	}
	if got := nested(t, vm, "spec", "template", "spec", "architecture"); got != "amd64" {
		t.Fatalf("architecture = %v", got)
	}
	if got := nested(t, vm, "spec", "template", "spec", "terminationGracePeriodSeconds"); got != int64(90) {
		t.Fatalf("grace period = %v", got)
	}
	selector := nested(t, vm, "spec", "template", "spec", "nodeSelector").(map[string]any)
	if selector["kubevirt.io/schedulable"] != "true" {
		t.Fatalf("node selector = %v", selector)
	}

	domain := nested(t, vm, "spec", "template", "spec", "domain").(map[string]any)
	if got := nested(t, vm, "spec", "template", "spec", "domain", "cpu", "cores"); got != int64(4) {
		t.Fatalf("cpu cores = %v", got)
	}
	if got := nested(t, vm, "spec", "template", "spec", "domain", "resources", "requests", "memory"); got != "8Gi" {
		t.Fatalf("memory request = %v", got)
	}
	if got := nested(t, vm, "spec", "template", "spec", "domain", "machine", "type"); got != "q35" {
		t.Fatalf("machine type = %v", got)
	}

	devices := domain["devices"].(map[string]any)
	// An absolute pointer is what makes "click at (x, y)" mean the same thing
	// to the model and to the guest.
	tablet := namedEntry(t, devices["inputs"], "tablet")
	if tablet["type"] != "tablet" || tablet["bus"] != "usb" {
		t.Fatalf("tablet input = %v", tablet)
	}
	iface := namedEntry(t, devices["interfaces"], "default")
	port := namedEntry(t, iface["ports"], "desktop-agent")
	if port["port"] != int64(GuestAgentPort) || port["protocol"] != "TCP" {
		t.Fatalf("masquerade port = %v", port)
	}

	namedEntry(t, devices["disks"], "rootdisk")
	namedEntry(t, devices["disks"], "cloudinit")
	volumes := nested(t, vm, "spec", "template", "spec", "volumes")
	root := namedEntry(t, volumes, "rootdisk")
	if root["dataVolume"].(map[string]any)["name"] != "kakao-agent-desktop-root" {
		t.Fatalf("root volume = %v", root)
	}
	cloudInit := namedEntry(t, volumes, "cloudinit")
	secretRef := cloudInit["cloudInitNoCloud"].(map[string]any)["secretRef"].(map[string]any)
	if secretRef["name"] != "kakao-agent-desktop-cloudinit" {
		t.Fatalf("cloud-init secretRef = %v", secretRef)
	}
	// The guest bearer must never be readable from the VM object itself.
	if _, inline := cloudInit["cloudInitNoCloud"].(map[string]any)["userData"]; inline {
		t.Fatalf("cloud-init must not be inlined in the VM spec: %v", cloudInit)
	}
}

func TestVMAdoptsExistingRootVolume(t *testing.T) {
	vm := VM(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.RootVolume.ExistingDataVolume = "poc-desktop-root"
	}))
	root := namedEntry(t, nested(t, vm, "spec", "template", "spec", "volumes"), "rootdisk")
	if root["dataVolume"].(map[string]any)["name"] != "poc-desktop-root" {
		t.Fatalf("adopted disk not referenced: %v", root)
	}
}

func TestVMWindowsShape(t *testing.T) {
	vm := VM(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.OS = OSWindows
		in.Spec.PersonalDesktop.Windows = &typeclawv1alpha1.PersonalDesktopWindowsSpec{
			VirtioContainerDisk: "quay.io/kubevirt/virtio-container-disk:test",
		}
	}))

	domain := nested(t, vm, "spec", "template", "spec", "domain").(map[string]any)
	// Secure Boot would reject the unsigned virtio drivers the guest loads
	// from the attached CD-ROM.
	if got := nested(t, vm, "spec", "template", "spec", "domain", "firmware", "bootloader", "efi", "secureBoot"); got != false {
		t.Fatalf("secureBoot = %v, want false", got)
	}
	devices := domain["devices"].(map[string]any)
	if _, found := devices["tpm"]; !found {
		t.Fatalf("Windows requires a TPM device: %v", devices)
	}
	features := domain["features"].(map[string]any)
	hyperv, found := features["hyperv"].(map[string]any)
	if !found {
		t.Fatalf("hyperv enlightenments missing: %v", features)
	}
	if got := hyperv["spinlocks"].(map[string]any)["spinlocks"]; got != int64(8191) {
		t.Fatalf("spinlocks = %v", got)
	}
	if _, found := domain["clock"].(map[string]any)["utc"]; !found {
		t.Fatalf("Windows guests need a UTC clock: %v", domain["clock"])
	}

	virtio := namedEntry(t, nested(t, vm, "spec", "template", "spec", "volumes"), "virtiocontainerdisk")
	if virtio["containerDisk"].(map[string]any)["image"] != "quay.io/kubevirt/virtio-container-disk:test" {
		t.Fatalf("virtio containerDisk = %v", virtio)
	}
	sysprep := namedEntry(t, nested(t, vm, "spec", "template", "spec", "volumes"), "sysprep")
	if sysprep["sysprep"].(map[string]any)["secret"].(map[string]any)["name"] != "kakao-agent-desktop-sysprep" {
		t.Fatalf("sysprep secret = %v", sysprep)
	}
	for _, name := range []string{"virtiocontainerdisk", "sysprep"} {
		disk := namedEntry(t, devices["disks"], name)
		if _, found := disk["cdrom"]; !found {
			t.Fatalf("%s must be attached as a CD-ROM: %v", name, disk)
		}
	}
	if list, ok := nested(t, vm, "spec", "template", "spec", "volumes").([]any); ok {
		for _, entry := range list {
			if entry.(map[string]any)["name"] == "cloudinit" {
				t.Fatalf("a Windows guest must not receive cloud-init")
			}
		}
	}
}

func TestVMDefaultsWhenTheAPIServerNeverAppliedThem(t *testing.T) {
	vm := VM(desktopInstance(nil))
	if got := nested(t, vm, "spec", "template", "spec", "domain", "cpu", "cores"); got != int64(2) {
		t.Fatalf("default cores = %v, want 2", got)
	}
	if got := nested(t, vm, "spec", "template", "spec", "domain", "resources", "requests", "memory"); got != "4Gi" {
		t.Fatalf("default memory = %v, want 4Gi", got)
	}
}

func TestAgentServiceForwardsTheTypedActionPort(t *testing.T) {
	service := AgentService(desktopInstance(nil))

	if service.Name != "kakao-agent-desktop-agent" {
		t.Fatalf("agent Service name = %q", service.Name)
	}
	if service.Spec.Selector[DomainLabel] != "kakao-agent-desktop" {
		t.Fatalf("agent Service selector = %v, want the VM's domain", service.Spec.Selector)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != GuestAgentPort {
		t.Fatalf("agent Service ports = %+v", service.Spec.Ports)
	}
}

func TestVMPrintableStatusAccessor(t *testing.T) {
	vm := NewObject(VirtualMachineGVK, "d", "ns")
	if VMPrintableStatus(vm) != "" {
		t.Fatalf("a VM with no status must report nothing")
	}
	if err := unstructured.SetNestedField(vm.Object, "Running", "status", "printableStatus"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}
	if VMPrintableStatus(vm) != "Running" {
		t.Fatalf("printable status accessor did not read status.printableStatus")
	}
}

// A desktop cloned from a golden image must not pin a MAC: KubeVirt assigns
// one and the guest's first boot writes matching network configuration.
func TestVMOmitsTheMACAddressWhenUnset(t *testing.T) {
	vm := VM(desktopInstance(nil))

	iface := firstInterface(t, vm)
	if _, pinned := iface["macAddress"]; pinned {
		t.Fatalf("interface pinned a MAC address without one being declared: %v", iface)
	}
}

// Adopting a disk that has already booted is the case the field exists for:
// the guest's persisted netplan matches by MAC, so the address has to come
// back exactly as the disk remembers it.
func TestVMPinsTheDeclaredMACAddressForAnAdoptedDisk(t *testing.T) {
	const declared = "72:23:c8:e8:e9:be"
	vm := VM(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.MACAddress = declared
		in.Spec.PersonalDesktop.RootVolume.ExistingDataVolume = "pd-def2f73d11aa05e5ab3d-root"
	}))

	iface := firstInterface(t, vm)
	if got := iface["macAddress"]; got != declared {
		t.Fatalf("interface macAddress = %v, want %q", got, declared)
	}
	if _, ok := iface["masquerade"]; !ok {
		t.Fatalf("pinning a MAC dropped the masquerade binding: %v", iface)
	}
}

func firstInterface(t *testing.T, vm *unstructured.Unstructured) map[string]any {
	t.Helper()
	interfaces, found, err := unstructured.NestedSlice(vm.Object,
		"spec", "template", "spec", "domain", "devices", "interfaces")
	if err != nil || !found || len(interfaces) != 1 {
		t.Fatalf("interfaces = %v (found %v, err %v), want exactly one", interfaces, found, err)
	}
	iface, ok := interfaces[0].(map[string]any)
	if !ok {
		t.Fatalf("interface is %T, want map", interfaces[0])
	}
	return iface
}
