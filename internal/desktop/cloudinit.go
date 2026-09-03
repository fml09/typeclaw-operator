// cloudinit.go assembles the Linux guest's first-boot configuration from the
// assets embedded in the operator image.
//
// The document goes into a Secret and is attached through
// cloudInitNoCloud.secretRef rather than inlined in the VirtualMachine spec:
// it carries the Guest Desktop Agent's bearer token, and anyone able to read a
// VM object in the desktop namespace would otherwise be able to read the
// credential that drives the owner's keyboard.
//
// Every file body travels base64-encoded. The agent source is Python whose
// indentation is significant, and a YAML block scalar takes its indentation
// from the first line of its content — one leading space in an embedded asset
// would reshape the whole document instead of failing loudly.
package desktop

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/guest"
)

const (
	// CloudInitNetworkDataKey is the Secret key KubeVirt reads the NoCloud
	// network configuration from.
	CloudInitNetworkDataKey = "networkdata"

	// CloudInitUserDataKey is the Secret key KubeVirt reads the NoCloud user
	// data from.
	CloudInitUserDataKey = "userdata"

	// guestAgentPath and guestTokenPath are where the Linux guest expects the
	// Guest Desktop Agent and its bearer token; the agent's own default token
	// path is /etc/personal-desktop/agent-token.
	guestAgentPath   = "/usr/local/bin/desktop-agent.py"
	guestTokenPath   = "/etc/personal-desktop/agent-token"
	guestSessionPath = "/usr/local/bin/personal-desktop-session.sh"
)

// linuxPackages is the XFCE desktop plus the tools the X11 backend of the
// Guest Desktop Agent shells out to (xdotool, scrot, wmctrl) and the guest
// agent KubeVirt needs for graceful shutdown.
var linuxPackages = []string{
	"dbus-x11",
	"firefox",
	"lightdm",
	"qemu-guest-agent",
	"scrot",
	"wmctrl",
	"xdotool",
	"xfce4",
	"xfce4-goodies",
}

// CloudInitSecret renders the Linux guest bootstrap. guestToken is the bearer
// the Desktop Gateway presents to the Guest Desktop Agent.
func CloudInitSecret(instance *typeclawv1alpha1.TypeClawInstance, guestToken string) *corev1.Secret {
	names := Names(instance)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.CloudInit,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			CloudInitUserDataKey:    []byte(CloudInitUserData(instance, guestToken)),
			CloudInitNetworkDataKey: []byte(CloudInitNetworkData()),
		},
	}
}

// CloudInitUserData renders the #cloud-config document itself. It is exported
// so tests can assert on the guest contract without decoding a Secret.
func CloudInitUserData(instance *typeclawv1alpha1.TypeClawInstance, guestToken string) string {
	spec := instance.Spec.PersonalDesktop
	names := Names(instance)
	username := Username(spec)
	width, height := Screen(spec)

	var b strings.Builder
	b.WriteString("#cloud-config\n")
	fmt.Fprintf(&b, "hostname: %s\n", yamlString(names.Desktop))
	b.WriteString("manage_etc_hosts: true\n")

	b.WriteString("users:\n")
	b.WriteString("  - default\n")
	fmt.Fprintf(&b, "  - name: %s\n", yamlString(username))
	b.WriteString("    gecos: Personal Desktop\n")
	// LightDM's PAM autologin profile only admits a password-locked account
	// when it is in nopasswdlogin, and the desktop account is deliberately
	// password-locked: nobody signs in to it interactively.
	b.WriteString("    groups: [adm, audio, cdrom, dialout, dip, floppy, nopasswdlogin, plugdev, sudo, video]\n")
	b.WriteString("    lock_passwd: true\n")
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	if spec != nil && spec.Linux != nil && len(spec.Linux.SSHAuthorizedKeys) > 0 {
		b.WriteString("    ssh_authorized_keys:\n")
		for _, key := range spec.Linux.SSHAuthorizedKeys {
			fmt.Fprintf(&b, "      - %s\n", yamlString(key))
		}
	}

	b.WriteString("package_update: true\n")
	b.WriteString("package_upgrade: false\n")
	b.WriteString("packages:\n")
	for _, pkg := range linuxPackages {
		fmt.Fprintf(&b, "  - %s\n", pkg)
	}

	b.WriteString("write_files:\n")
	writeFile(&b, guestAgentPath, "root:root", "0755", guest.DesktopAgentSource, false)
	writeFile(&b, guestSessionPath, "root:root", "0755", linuxSessionScript(width, height), false)
	// Deferred: cloud-init's write-files module runs before users-groups, so
	// chowning the token to the desktop user only succeeds in the final
	// stage. The agent reads this file as that user and nobody else may.
	writeFile(&b, guestTokenPath, username+":"+username, "0600", guestToken, true)
	writeFile(&b, lightdmConfigPath, "root:root", "0644", lightdmConfig(username), false)
	// System-wide autostart rather than the user's home: cloud-init would
	// create ~/.config as root on the way to the entry, and an XFCE session
	// that cannot write its own config directory comes up broken.
	writeFile(&b, autostartPath, "root:root", "0644", autostartEntry(), false)
	// Suppress light-locker for the desktop user.
	//
	// The session script turns the X screensaver and DPMS off once at session
	// start, but xfce4-power-manager comes up afterwards, reads its own xfconf
	// timers and re-arms blanking. When it fires, light-locker locks the seat
	// and switches to the greeter — and the desktop account is lock_passwd, so
	// there is no password that could unlock it. The agent keeps screenshotting
	// and typing at a session nobody can reach, and nothing in the Instance
	// status says why.
	//
	// It is deferred and user-owned on purpose. The light-locker package is
	// installed later in the same boot, and a non-deferred write into
	// /etc/xdg/autostart is clobbered when it lands.
	writeFile(&b, lightLockerOverridePath(username), username+":"+username, "0644",
		lightLockerOverride(), true)

	// Ubuntu's lightdm package prompts for the default display manager when
	// gdm3 is already installed; an unattended first boot would stall there.
	b.WriteString("bootcmd:\n")
	b.WriteString("  - [sh, -c, \"echo 'lightdm shared/default-x-display-manager select lightdm' | debconf-set-selections\"]\n")

	b.WriteString("runcmd:\n")
	fmt.Fprintf(&b, "  - [usermod, -aG, nopasswdlogin, %s]\n", yamlString(username))
	b.WriteString("  - [systemctl, enable, qemu-guest-agent.service]\n")
	b.WriteString("  - [systemctl, start, qemu-guest-agent.service]\n")
	b.WriteString("  - [systemctl, set-default, graphical.target]\n")
	b.WriteString("  - [systemctl, enable, lightdm.service]\n")

	// The desktop packages install into a system that booted to a text
	// target; one reboot is what brings up the graphical session the agent
	// needs, and cloud-init runs only once so this cannot loop.
	b.WriteString("power_state:\n")
	b.WriteString("  delay: now\n")
	b.WriteString("  mode: reboot\n")
	b.WriteString("  message: Reboot after first-boot desktop installation\n")
	b.WriteString("  condition: true\n")
	return b.String()
}

const (
	lightdmConfigPath = "/etc/lightdm/lightdm.conf.d/50-personal-desktop.conf"
	autostartPath     = "/etc/xdg/autostart/typeclaw-desktop-agent.desktop"
)

// lightdmConfig autologs the desktop user into XFCE and starts X with screen
// blanking and DPMS off. A blanked screen returns black frames to the model,
// and a locked session refuses every typed action.
func lightdmConfig(username string) string {
	return "[Seat:*]\n" +
		"autologin-user=" + username + "\n" +
		"autologin-user-timeout=0\n" +
		"user-session=xfce\n" +
		"xserver-command=X -s 0 -dpms\n"
}

// linuxSessionScript substitutes the fixed display geometry into the embedded
// session bootstrap.
func linuxSessionScript(width, height int32) string {
	script := strings.ReplaceAll(guest.LinuxSessionScript, "@@SCREEN_WIDTH@@", fmt.Sprint(width))
	return strings.ReplaceAll(script, "@@SCREEN_HEIGHT@@", fmt.Sprint(height))
}

// autostartEntry starts the session script from inside the XFCE session, so
// the Guest Desktop Agent inherits the session's DISPLAY instead of running as
// a system service with no desktop to drive.
func autostartEntry() string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=TypeClaw Personal Desktop\n" +
		"Comment=Typed computer-use actions for the TypeClaw agent\n" +
		"Exec=" + guestSessionPath + "\n" +
		"Terminal=false\n"
}

func writeFile(b *strings.Builder, path, owner, permissions, content string, deferred bool) {
	fmt.Fprintf(b, "  - path: %s\n", yamlString(path))
	fmt.Fprintf(b, "    owner: %s\n", yamlString(owner))
	fmt.Fprintf(b, "    permissions: %s\n", yamlString(permissions))
	b.WriteString("    encoding: b64\n")
	if deferred {
		b.WriteString("    defer: true\n")
	}
	fmt.Fprintf(b, "    content: %s\n", yamlString(base64.StdEncoding.EncodeToString([]byte(content))))
}

// yamlString emits a YAML double-quoted scalar. JSON string encoding produces
// exactly that: every escape JSON emits is also a YAML double-quoted escape,
// so an administrator-supplied username or SSH key cannot terminate the scalar
// and inject cloud-init directives.
func yamlString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		// json.Marshal of a string fails only on invalid UTF-8, which YAML
		// cannot carry either; emit an empty scalar rather than raw bytes.
		return `""`
	}
	return string(encoded)
}

// CloudInitNetworkData renders the guest's netplan, matching the interface by
// name rather than by hardware address.
//
// Supplying this at all is the point. Without a networkData document cloud-init
// falls back to generating its own, and that fallback pins the interface it
// found on first boot by MAC:
//
//	ethernets:
//	  enp1s0:
//	    match: {macaddress: "52:54:00:12:34:56"}
//
// It writes that to /etc/netplan on the persistent root disk. A desktop's
// normal lifecycle is the owner powering it off and on (runStrategy Manual),
// and every power-on builds a new VMI with a freshly generated MAC unless one
// is pinned in the spec. The guest then boots with a netplan that matches no
// interface present, never brings the link up, never DHCPs, and has no
// address — while the desktop agent inside it runs perfectly and answers
// nothing. Matching on "en*" instead survives any MAC the hypervisor picks, so
// a cloned disk needs no pinned address, and an adopted one stops depending on
// the caller remembering to pin the right one.
func CloudInitNetworkData() string {
	return "version: 2\n" +
		"ethernets:\n" +
		"  primary:\n" +
		"    match:\n" +
		"      name: \"en*\"\n" +
		"    dhcp4: true\n" +
		"    dhcp6: false\n"
}

// lightLockerOverridePath is the per-user autostart entry that shadows the
// system-wide light-locker one. A user entry of the same basename wins over
// /etc/xdg/autostart, which is how a desktop session is meant to be overridden.
func lightLockerOverridePath(username string) string {
	return "/home/" + username + "/.config/autostart/light-locker.desktop"
}

// lightLockerOverride is a hidden desktop entry: Hidden=true tells the session
// to skip the entry it shadows entirely, rather than run it and hope it exits.
func lightLockerOverride() string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=Light Locker\n" +
		"Hidden=true\n"
}
