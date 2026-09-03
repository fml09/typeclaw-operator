// kubevirt.go is the only place in the operator that knows KubeVirt and CDI
// field paths.
//
// The operator does not depend on kubevirt.io/api: those CRDs are optional in
// a cluster, and linking their types would make the manager binary carry a
// schema for objects it may never see. So VirtualMachines, VirtualMachine
// Instances and DataVolumes are built and read as unstructured objects, and
// every field path lives behind a named accessor here — a hand-written
// "status.printableStatus" at a call site is a typo away from silently
// reporting an empty desktop state forever.
package desktop

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersionKinds of the optional virtualization CRDs this feature needs.
var (
	VirtualMachineGVK = schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine",
	}
	VirtualMachineInstanceGVK = schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance",
	}
	DataVolumeGVK = schema.GroupVersionKind{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume",
	}
)

// RequiredGVKs are the mappings that must exist before anything is
// provisioned. A cluster missing any of them has no KubeVirt or CDI install
// and the desktop is reported unavailable rather than retried in a hot loop.
func RequiredGVKs() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{VirtualMachineGVK, DataVolumeGVK}
}

// NewObject builds an empty unstructured object of the given kind. The GVK is
// set before anything else because a client Get or Create on an unstructured
// object with no GVK cannot be routed at all.
func NewObject(gvk schema.GroupVersionKind, name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	return obj
}

// VMPrintableStatus reads the VirtualMachine state KubeVirt shows in
// `kubectl get vm` (Running, Stopped, Provisioning, ...).
func VMPrintableStatus(obj *unstructured.Unstructured) string {
	return nestedString(obj, "status", "printableStatus")
}

// DataVolumePhase reads the CDI import or clone phase (Pending, ImportInProgress,
// CloneInProgress, Succeeded, Failed).
func DataVolumePhase(obj *unstructured.Unstructured) string {
	return nestedString(obj, "status", "phase")
}

// DataVolumeSucceeded reports whether a DataVolume finished populating. The
// desktop is only Ready once its root disk has, because a VM started against
// an unfinished clone boots from an incomplete filesystem.
func DataVolumeSucceeded(obj *unstructured.Unstructured) bool {
	return DataVolumePhase(obj) == "Succeeded"
}

// SetSpec replaces the object's whole spec. Renderers re-derive the full
// desired spec on every reconcile, so a partial merge would let a field from a
// previous Instance generation survive forever.
func SetSpec(obj *unstructured.Unstructured, spec map[string]any) {
	if obj.Object == nil {
		obj.Object = map[string]any{}
	}
	obj.Object["spec"] = spec
}

// Spec returns the object's spec map, or nil when it has none.
func Spec(obj *unstructured.Unstructured) map[string]any {
	if obj == nil || obj.Object == nil {
		return nil
	}
	spec, _ := obj.Object["spec"].(map[string]any)
	return spec
}

func nestedString(obj *unstructured.Unstructured, fields ...string) string {
	if obj == nil {
		return ""
	}
	value, found, err := unstructured.NestedString(obj.Object, fields...)
	if err != nil || !found {
		return ""
	}
	return value
}
