// datavolume.go renders the two CDI disks behind one Personal Desktop: the
// sealed golden image every desktop clones from, and the per-desktop root
// disk.
//
// Neither disk is a dataVolumeTemplate on the VirtualMachine. A template would
// make the VM their owner, so deleting or recreating the VM — an ordinary
// operation for a desktop that is resized, retemplated, or moved — would take
// the owner's whole root filesystem with it. Standalone DataVolumes outlive
// the VM by construction, and only the explicit onInstanceDeletion policy
// removes the root disk.
package desktop

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// GoldenDataVolume renders the import of the sealed golden root image. It is
// rendered only when spec.image.import is set and the DataVolume is absent:
// the golden image is shared by every desktop cloned from it, so the operator
// creates it at most once and never updates or deletes it.
func GoldenDataVolume(instance *typeclawv1alpha1.TypeClawInstance) *unstructured.Unstructured {
	spec := instance.Spec.PersonalDesktop
	if spec == nil || spec.Image.Import == nil {
		return nil
	}
	names := Names(instance)
	// resource.Quantity renders through a pointer receiver, so the value has
	// to be named before it can be formatted.
	size := GoldenVolumeSize(spec.Image.Import)
	obj := NewObject(DataVolumeGVK, names.GoldenVolume, names.Namespace)
	obj.SetLabels(Labels(instance))
	SetSpec(obj, map[string]any{
		"source": map[string]any{
			"http": map[string]any{
				"url":      spec.Image.Import.URL,
				"checksum": spec.Image.Import.Checksum,
			},
		},
		"storage": storageSpec(size.String(), spec.Image.Import.StorageClassName),
	})
	return obj
}

// RootDataVolume renders the per-desktop root disk as a clone of the golden
// image. It returns nil when spec.rootVolume.existingDataVolume adopts a disk
// the operator did not create — an adopted disk is never resized or replaced,
// which is what makes migrating an earlier PoC desktop non-destructive.
func RootDataVolume(instance *typeclawv1alpha1.TypeClawInstance) *unstructured.Unstructured {
	spec := instance.Spec.PersonalDesktop
	if spec == nil || spec.RootVolume.ExistingDataVolume != "" {
		return nil
	}
	names := Names(instance)
	size := RootVolumeSize(spec)
	obj := NewObject(DataVolumeGVK, names.RootVolume, names.Namespace)
	obj.SetLabels(Labels(instance))
	SetSpec(obj, map[string]any{
		"source": map[string]any{
			"pvc": map[string]any{
				"name":      names.GoldenVolume,
				"namespace": names.Namespace,
			},
		},
		"storage": storageSpec(size.String(), spec.RootVolume.StorageClassName),
	})
	return obj
}

// storageSpec builds the CDI storage block shared by both disks. volumeMode is
// pinned to Filesystem because the guest images are qcow2 files CDI converts
// into a filesystem-backed disk image.
func storageSpec(size string, storageClassName *string) map[string]any {
	storage := map[string]any{
		"accessModes": []any{"ReadWriteOnce"},
		"resources": map[string]any{
			"requests": map[string]any{"storage": size},
		},
		"volumeMode": "Filesystem",
	}
	if storageClassName != nil && *storageClassName != "" {
		storage["storageClassName"] = *storageClassName
	}
	return storage
}
