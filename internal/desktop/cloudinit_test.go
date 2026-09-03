package desktop

import (
	"encoding/base64"
	"strings"
	"testing"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/guest"
)

func TestCloudInitSecretCarriesTheUserData(t *testing.T) {
	secret := CloudInitSecret(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
	}), "guest-bearer")

	if secret.Name != "kakao-agent-desktop-cloudinit" || secret.Namespace != "typeclaw-desktops" {
		t.Fatalf("cloud-init Secret ref = %s/%s", secret.Namespace, secret.Name)
	}
	if !strings.HasPrefix(string(secret.Data[CloudInitUserDataKey]), "#cloud-config\n") {
		t.Fatalf("userdata is not a cloud-config document")
	}
}

func TestCloudInitUserDataDeliversTheGuestAgent(t *testing.T) {
	userData := CloudInitUserData(desktopInstance(nil), "guest-bearer")

	for _, path := range []string{guestAgentPath, guestTokenPath, guestSessionPath, lightdmConfigPath, autostartPath} {
		if !strings.Contains(userData, `"`+path+`"`) {
			t.Fatalf("cloud-config does not write %s:\n%s", path, userData)
		}
	}
	// Every body travels base64-encoded: the agent is Python whose
	// indentation is significant, and a block scalar would take its
	// indentation from the asset's first line.
	agent := base64.StdEncoding.EncodeToString([]byte(guest.DesktopAgentSource))
	if !strings.Contains(userData, agent) {
		t.Fatalf("the embedded Guest Desktop Agent is not delivered")
	}
	token := base64.StdEncoding.EncodeToString([]byte("guest-bearer"))
	if !strings.Contains(userData, token) {
		t.Fatalf("the guest bearer is not written to the token file")
	}
	if strings.Contains(userData, "guest-bearer") {
		t.Fatalf("the bearer must travel encoded, never as a raw scalar")
	}
	if !strings.Contains(userData, "defer: true") {
		t.Fatalf("the token file must be deferred until the desktop user exists")
	}
	for _, pkg := range linuxPackages {
		if !strings.Contains(userData, "  - "+pkg+"\n") {
			t.Fatalf("package %s missing from the cloud-config", pkg)
		}
	}
	if !strings.Contains(userData, "mode: reboot") {
		t.Fatalf("the first boot must end in the reboot that starts the graphical session")
	}
}

func TestCloudInitUserDataSubstitutesTheScreenGeometry(t *testing.T) {
	in := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Screen = &typeclawv1alpha1.PersonalDesktopScreenSpec{Width: 1920, Height: 1080}
	})
	script := linuxSessionScript(1920, 1080)

	if strings.Contains(script, "@@SCREEN_WIDTH@@") || strings.Contains(script, "@@SCREEN_HEIGHT@@") {
		t.Fatalf("session script placeholders survived substitution:\n%s", script)
	}
	if !strings.Contains(script, `SCREEN_WIDTH="1920"`) || !strings.Contains(script, `SCREEN_HEIGHT="1080"`) {
		t.Fatalf("session script geometry = %s", script)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	if !strings.Contains(CloudInitUserData(in, "guest-bearer"), encoded) {
		t.Fatalf("the substituted session script is not the one delivered")
	}
}

func TestCloudInitUserDataHonoursTheLinuxSpec(t *testing.T) {
	userData := CloudInitUserData(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Linux = &typeclawv1alpha1.PersonalDesktopLinuxSpec{
			Username:          "operator",
			SSHAuthorizedKeys: []string{"ssh-ed25519 AAAA maintenance"},
		}
	}), "guest-bearer")

	if !strings.Contains(userData, `- name: "operator"`) {
		t.Fatalf("the declared desktop user was not created:\n%s", userData)
	}
	if !strings.Contains(userData, `"ssh-ed25519 AAAA maintenance"`) {
		t.Fatalf("maintenance key missing from the cloud-config")
	}
	if !strings.Contains(lightdmConfig("operator"), "autologin-user=operator") {
		t.Fatalf("LightDM does not autologin the declared user")
	}
	if !strings.Contains(userData, base64.StdEncoding.EncodeToString([]byte(lightdmConfig("operator")))) {
		t.Fatalf("the LightDM drop-in delivered is not the one rendered for the user")
	}
}

func TestCloudInitQuotesAdministratorInput(t *testing.T) {
	// A username carrying a quote must terminate as a YAML scalar, not as a
	// new cloud-init directive.
	userData := CloudInitUserData(desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Linux = &typeclawv1alpha1.PersonalDesktopLinuxSpec{Username: `de"sk\top`}
	}), "guest-bearer")

	if !strings.Contains(userData, `- name: "de\"sk\\top"`) {
		t.Fatalf("username was not escaped as a YAML double-quoted scalar:\n%s", userData)
	}
}

// TestNetworkDataMatchesByNameNotMAC pins the fix for a failure that only
// appears on the second power-on, which makes it very easy to ship.
//
// A desktop is powered off and on by its owner (runStrategy Manual), and each
// power-on builds a new VMI whose interface gets a freshly generated MAC
// unless the spec pins one. If the guest's netplan matched on a hardware
// address it would match nothing from then on: no link, no DHCP, no address,
// and a desktop agent that runs perfectly while answering nobody.
func TestNetworkDataMatchesByNameNotMAC(t *testing.T) {
	data := CloudInitNetworkData()
	if strings.Contains(strings.ToLower(data), "macaddress") {
		t.Fatalf("network data must not pin a hardware address:\n%s", data)
	}
	for _, want := range []string{"version: 2", "match:", "name: \"en*\"", "dhcp4: true"} {
		if !strings.Contains(data, want) {
			t.Errorf("network data is missing %q:\n%s", want, data)
		}
	}
}

// TestCloudInitSecretCarriesNetworkData: KubeVirt reads the networkdata key
// through networkDataSecretRef, so the key has to be in the same Secret the VM
// already references.
func TestCloudInitSecretCarriesNetworkData(t *testing.T) {
	secret := CloudInitSecret(sidecarInstance(), "guest-token")
	if _, ok := secret.Data[CloudInitNetworkDataKey]; !ok {
		t.Fatalf("Secret is missing key %q; KubeVirt would fall back to a MAC-pinned netplan", CloudInitNetworkDataKey)
	}
	if _, ok := secret.Data[CloudInitUserDataKey]; !ok {
		t.Fatalf("Secret is missing key %q", CloudInitUserDataKey)
	}
}

// TestLightLockerIsSuppressed: the desktop account has no password
// (lock_passwd), so if light-locker ever locks the seat the session is
// unreachable by anyone, agent or owner, until the VM is power-cycled.
func TestLightLockerIsSuppressed(t *testing.T) {
	data := CloudInitUserData(sidecarInstance(), "guest-token")
	path := lightLockerOverridePath(Username(sidecarInstance().Spec.PersonalDesktop))
	if !strings.Contains(data, path) {
		t.Fatalf("no light-locker override was written:\n%s", data)
	}
	if !strings.Contains(data, "/home/") {
		t.Error("the override must live in the user's home; a /etc/xdg copy is clobbered when the package installs")
	}
	// Locate the entry and confirm it is deferred: cloud-init installs
	// light-locker later in the same boot.
	idx := strings.Index(data, path)
	tail := data[idx:]
	end := strings.Index(tail, "  - path:")
	if end == -1 {
		end = len(tail)
	}
	if !strings.Contains(tail[:end], "defer: true") {
		t.Fatalf("the override must be deferred or the package install overwrites it:\n%s", tail[:end])
	}
}
