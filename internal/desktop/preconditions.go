// preconditions.go holds the checks that decide whether a declared desktop can
// be provisioned at all.
//
// They live next to the renderers, and not in the controller, because they are
// statements about the desired state rather than about the cluster: given this
// Instance, is the requested desktop coherent, and can the runtime that will
// drive it actually load the Platform Extension. Both answers are pure
// functions of the spec, so both are decided before a single object is applied.
package desktop

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// Validate reports why a declared Personal Desktop cannot be rendered. The
// error text is surfaced on the Instance condition, so it names the field.
func Validate(instance *typeclawv1alpha1.TypeClawInstance) error {
	spec := instance.Spec.PersonalDesktop
	if spec == nil {
		return nil
	}
	if spec.Owner.Subject == "" {
		return errors.New("spec.personalDesktop.owner.subject is required")
	}
	if spec.Image.GoldenDataVolume == "" {
		return errors.New("spec.personalDesktop.image.goldenDataVolume is required")
	}
	if OS(spec) == OSWindows && spec.Image.Import != nil {
		// A Windows golden image is sysprepped by an administrator; there is
		// no cloud image to fetch, and importing one would produce a disk that
		// never reaches an interactive session.
		return errors.New("spec.personalDesktop.image.import is not supported with os=Windows")
	}
	if spec.Image.Import != nil {
		if spec.Image.Import.URL == "" {
			return errors.New("spec.personalDesktop.image.import.url is required")
		}
		if spec.Image.Import.Checksum == "" {
			return errors.New("spec.personalDesktop.image.import.checksum is required")
		}
	}
	if spec.Access != nil {
		if spec.Access.Tailscale == nil || spec.Access.Tailscale.Hostname == "" {
			return errors.New("spec.personalDesktop.access.tailscale.hostname is required when access is set")
		}
		// Sidecar mode has no other way to reach the tailnet. Failing here
		// keeps the Gateway Pod from being rendered at all, which is better
		// than rendering one whose tailscaled can never authenticate and whose
		// console therefore never appears, with nothing saying why.
		if ConsoleMode(spec) == ConsoleModeSidecar {
			if spec.Access.Tailscale.AuthSecret == "" {
				return errors.New(
					"spec.personalDesktop.access.tailscale.authSecret is required when mode is Sidecar")
			}
			// Without it the operator can only name the console by its short
			// MagicDNS label, which resolves and then fails TLS verification
			// against a certificate issued for the full name. Refusing here is
			// better than publishing a link that looks right and cannot open.
			if spec.Access.Tailscale.Tailnet == "" {
				return errors.New(
					"spec.personalDesktop.access.tailscale.tailnet is required when mode is Sidecar")
			}
		}
	}
	return nil
}

// RuntimeSupportsExtensions reports whether the Managed Runtime this Instance
// will run can load a Platform Extension.
//
// An explicit spec.runtime.image is not comparable to a release number and is
// treated as an administrator's assertion that the image is new enough;
// otherwise the effective version must reach
// PersonalDesktopMinimumRuntimeVersion. An older runtime ignores
// TYPECLAW_PLATFORM_EXTENSIONS entirely, so the desktop would provision
// perfectly and no agent would ever be able to touch it.
func RuntimeSupportsExtensions(instance *typeclawv1alpha1.TypeClawInstance) bool {
	if instance.Spec.Runtime.Image != "" {
		return true
	}
	version := instance.Spec.Runtime.Version
	if version == "" {
		version = typeclawv1alpha1.DefaultRuntimeVersion
	}
	return compareSemver(version, typeclawv1alpha1.PersonalDesktopMinimumRuntimeVersion) >= 0
}

// EffectiveRuntimeVersion reports the release the gate compared, for the
// condition message.
func EffectiveRuntimeVersion(instance *typeclawv1alpha1.TypeClawInstance) string {
	if instance.Spec.Runtime.Image != "" {
		return instance.Spec.Runtime.Image
	}
	if instance.Spec.Runtime.Version != "" {
		return instance.Spec.Runtime.Version
	}
	return typeclawv1alpha1.DefaultRuntimeVersion
}

// RuntimeTooOldMessage explains the gate to whoever reads the condition.
func RuntimeTooOldMessage(instance *typeclawv1alpha1.TypeClawInstance) string {
	return fmt.Sprintf(
		"managed runtime %s predates %s, which is the first release that loads Platform Extensions",
		EffectiveRuntimeVersion(instance),
		typeclawv1alpha1.PersonalDesktopMinimumRuntimeVersion,
	)
}

// compareSemver orders two release numbers. Anything that does not parse as
// major.minor.patch sorts below every real release, so a malformed version
// fails the gate closed rather than provisioning a desktop nothing can drive.
func compareSemver(left, right string) int {
	leftParts, leftOK := semverParts(left)
	rightParts, rightOK := semverParts(right)
	switch {
	case !leftOK && !rightOK:
		return 0
	case !leftOK:
		return -1
	case !rightOK:
		return 1
	}
	for i := range leftParts {
		if leftParts[i] != rightParts[i] {
			if leftParts[i] < rightParts[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func semverParts(version string) ([3]int, bool) {
	var parts [3]int
	// Prerelease and build metadata do not change which minor a release
	// belongs to; 0.52.0-rc.1 is compared as 0.52.0.
	core := version
	if index := strings.IndexAny(core, "-+"); index >= 0 {
		core = core[:index]
	}
	fields := strings.Split(strings.TrimPrefix(core, "v"), ".")
	if len(fields) != 3 {
		return parts, false
	}
	for i, field := range fields {
		value, err := strconv.Atoi(field)
		if err != nil || value < 0 {
			return parts, false
		}
		parts[i] = value
	}
	return parts, true
}
