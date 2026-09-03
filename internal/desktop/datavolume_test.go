package desktop

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func nested(t *testing.T, obj *unstructured.Unstructured, fields ...string) any {
	t.Helper()
	value, found, err := unstructured.NestedFieldNoCopy(obj.Object, fields...)
	if err != nil || !found {
		t.Fatalf("field %v missing from %v", fields, obj.Object)
	}
	return value
}

func TestGoldenDataVolumeOnlyRenderedForAnImport(t *testing.T) {
	if GoldenDataVolume(desktopInstance(nil)) != nil {
		t.Fatalf("an administrator-seeded golden image must never be rendered")
	}
}

func TestGoldenDataVolumeImportsFromHTTP(t *testing.T) {
	size := resource.MustParse("40Gi")
	class := "fast"
	obj := GoldenDataVolume(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Image.Import = &typeclawv1alpha1.PersonalDesktopImageImportSpec{
			URL:              "https://example.test/ubuntu.img",
			Checksum:         "sha256:beef",
			Size:             size,
			StorageClassName: &class,
		}
	}))

	if obj.GroupVersionKind() != DataVolumeGVK {
		t.Fatalf("gvk = %v, want the CDI DataVolume", obj.GroupVersionKind())
	}
	if obj.GetName() != "ubuntu-golden" {
		t.Fatalf("golden DataVolume name = %q", obj.GetName())
	}
	if got := nested(t, obj, "spec", "source", "http", "url"); got != "https://example.test/ubuntu.img" {
		t.Fatalf("source url = %v", got)
	}
	if got := nested(t, obj, "spec", "source", "http", "checksum"); got != "sha256:beef" {
		t.Fatalf("source checksum = %v", got)
	}
	if got := nested(t, obj, "spec", "storage", "resources", "requests", "storage"); got != "40Gi" {
		t.Fatalf("storage request = %v", got)
	}
	if got := nested(t, obj, "spec", "storage", "storageClassName"); got != "fast" {
		t.Fatalf("storage class = %v", got)
	}
}

func TestRootDataVolumeClonesTheGoldenImage(t *testing.T) {
	obj := RootDataVolume(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
	}))

	if obj.GetName() != "kakao-agent-desktop-root" || obj.GetNamespace() != "typeclaw-desktops" {
		t.Fatalf("root DataVolume ref = %s/%s", obj.GetNamespace(), obj.GetName())
	}
	if got := nested(t, obj, "spec", "source", "pvc", "name"); got != "ubuntu-golden" {
		t.Fatalf("clone source = %v", got)
	}
	if got := nested(t, obj, "spec", "source", "pvc", "namespace"); got != "typeclaw-desktops" {
		t.Fatalf("clone source namespace = %v", got)
	}
	// The default sizes the disk even when the API server never applied the
	// CRD default, so a desktop is never created with a 0-byte root disk.
	if got := nested(t, obj, "spec", "storage", "resources", "requests", "storage"); got != "32Gi" {
		t.Fatalf("default root size = %v, want 32Gi", got)
	}
}

func TestRootDataVolumeIsNotRenderedForAnAdoptedDisk(t *testing.T) {
	obj := RootDataVolume(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.RootVolume.ExistingDataVolume = "poc-desktop-root"
	}))
	if obj != nil {
		t.Fatalf("an adopted disk must never be re-rendered: %v", obj.Object)
	}
}

func TestDataVolumePhaseAccessors(t *testing.T) {
	obj := NewObject(DataVolumeGVK, "d", "ns")
	if DataVolumePhase(obj) != "" || DataVolumeSucceeded(obj) {
		t.Fatalf("a DataVolume with no status must report no phase")
	}
	if err := unstructured.SetNestedField(obj.Object, "Succeeded", "status", "phase"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}
	if !DataVolumeSucceeded(obj) {
		t.Fatalf("Succeeded phase was not recognised")
	}
}
