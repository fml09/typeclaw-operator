package desktop

import (
	"strings"
	"testing"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func TestValidateAcceptsADeclaredDesktop(t *testing.T) {
	if err := Validate(desktopInstance(nil)); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
	// An Instance with no desktop block is valid; the controller simply has
	// nothing to converge.
	if err := Validate(&typeclawv1alpha1.TypeClawInstance{}); err != nil {
		t.Fatalf("Validate() on an Instance with no desktop: %v", err)
	}
}

func TestValidateRejectsIncoherentDesktops(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*typeclawv1alpha1.TypeClawInstance)
		want   string
	}{
		{
			name:   "owner subject is required",
			mutate: func(in *typeclawv1alpha1.TypeClawInstance) { in.Spec.PersonalDesktop.Owner.Subject = "" },
			want:   "owner.subject",
		},
		{
			name:   "golden image is required",
			mutate: func(in *typeclawv1alpha1.TypeClawInstance) { in.Spec.PersonalDesktop.Image.GoldenDataVolume = "" },
			want:   "image.goldenDataVolume",
		},
		{
			name: "windows golden images are built, never imported",
			mutate: func(in *typeclawv1alpha1.TypeClawInstance) {
				in.Spec.PersonalDesktop.OS = OSWindows
				in.Spec.PersonalDesktop.Image.Import = &typeclawv1alpha1.PersonalDesktopImageImportSpec{
					URL: "https://example.test/win.img", Checksum: "sha256:beef",
				}
			},
			want: "os=Windows",
		},
		{
			name: "an import needs a source",
			mutate: func(in *typeclawv1alpha1.TypeClawInstance) {
				in.Spec.PersonalDesktop.Image.Import = &typeclawv1alpha1.PersonalDesktopImageImportSpec{Checksum: "sha256:beef"}
			},
			want: "image.import.url",
		},
		{
			name: "an import needs a checksum",
			mutate: func(in *typeclawv1alpha1.TypeClawInstance) {
				in.Spec.PersonalDesktop.Image.Import = &typeclawv1alpha1.PersonalDesktopImageImportSpec{URL: "https://example.test/u.img"}
			},
			want: "image.import.checksum",
		},
		{
			name: "declared access needs a hostname",
			mutate: func(in *typeclawv1alpha1.TypeClawInstance) {
				in.Spec.PersonalDesktop.Access = &typeclawv1alpha1.PersonalDesktopAccessSpec{}
			},
			want: "access.tailscale.hostname",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(desktopInstance(tt.mutate))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want an error naming %s", err, tt.want)
			}
		})
	}
}

func TestRuntimeSupportsExtensionsGate(t *testing.T) {
	tests := []struct {
		name    string
		runtime typeclawv1alpha1.RuntimeSpec
		want    bool
	}{
		{"minimum release passes", typeclawv1alpha1.RuntimeSpec{Version: "0.52.0"}, true},
		{"newer release passes", typeclawv1alpha1.RuntimeSpec{Version: "1.0.0"}, true},
		{"prerelease of the minimum passes", typeclawv1alpha1.RuntimeSpec{Version: "0.52.0-rc.1"}, true},
		{"older release fails", typeclawv1alpha1.RuntimeSpec{Version: "0.51.9"}, false},
		// The tracked default predates Platform Extensions, so an Instance
		// that pins nothing is gated too.
		{"unset version uses the tracked default", typeclawv1alpha1.RuntimeSpec{}, false},
		// A digest is not comparable to a release number; pinning an image is
		// the administrator asserting the runtime is new enough.
		{"explicit image skips the gate", typeclawv1alpha1.RuntimeSpec{Image: "ghcr.io/fml09/typeclaw-runtime@sha256:abc"}, true},
		{"unparseable version fails closed", typeclawv1alpha1.RuntimeSpec{Version: "nightly"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
				in.Spec.Runtime = tt.runtime
			})
			if got := RuntimeSupportsExtensions(in); got != tt.want {
				t.Fatalf("RuntimeSupportsExtensions() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRuntimeTooOldMessageNamesBothVersions(t *testing.T) {
	in := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Runtime = typeclawv1alpha1.RuntimeSpec{Version: "0.48.9"}
	})
	message := RuntimeTooOldMessage(in)
	if !strings.Contains(message, "0.48.9") ||
		!strings.Contains(message, typeclawv1alpha1.PersonalDesktopMinimumRuntimeVersion) {
		t.Fatalf("condition message = %q", message)
	}
	if got := EffectiveRuntimeVersion(in); got != "0.48.9" {
		t.Fatalf("EffectiveRuntimeVersion() = %q", got)
	}
}

func TestEnabledRequiresTheFlag(t *testing.T) {
	if Enabled(&typeclawv1alpha1.TypeClawInstance{}) {
		t.Fatalf("an Instance with no desktop block declares no desktop")
	}
	off := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Enabled = false
	})
	if Enabled(off) {
		t.Fatalf("enabled=false declares no running desktop")
	}
}
