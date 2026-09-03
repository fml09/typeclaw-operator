package desktop

import (
	"strings"
	"testing"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/guest"
)

func windowsInstance(mutate func(*typeclawv1alpha1.TypeClawInstance)) *typeclawv1alpha1.TypeClawInstance {
	return desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.OS = OSWindows
		if mutate != nil {
			mutate(in)
		}
	})
}

func TestSysprepSecretCarriesTheWholeFirstBootPayload(t *testing.T) {
	secret, err := SysprepSecret(windowsInstance(nil), "guest-bearer", "autologon-password")
	if err != nil {
		t.Fatalf("SysprepSecret() error: %v", err)
	}

	if secret.Name != "kakao-agent-desktop-sysprep" || secret.Namespace != "agents" {
		t.Fatalf("sysprep Secret ref = %s/%s", secret.Namespace, secret.Name)
	}
	for _, key := range []string{SysprepUnattendKey, SysprepSetupScriptKey, SysprepAgentKey, SysprepAgentTokenKey} {
		if len(secret.Data[key]) == 0 {
			t.Fatalf("sysprep Secret is missing key %s", key)
		}
	}
	if string(secret.Data[SysprepAgentKey]) != guest.DesktopAgentSource {
		t.Fatalf("the delivered agent is not the embedded one")
	}
	if string(secret.Data[SysprepAgentTokenKey]) != "guest-bearer" {
		t.Fatalf("the guest bearer is not delivered to the Windows guest")
	}
}

func TestSysprepUnattendFillsTheAnswerFile(t *testing.T) {
	in := windowsInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Windows = &typeclawv1alpha1.PersonalDesktopWindowsSpec{Username: "operator"}
		in.Spec.PersonalDesktop.Screen = &typeclawv1alpha1.PersonalDesktopScreenSpec{Width: 1600, Height: 900}
	})
	secret, err := SysprepSecret(in, "guest-bearer", "autologon-password")
	if err != nil {
		t.Fatalf("SysprepSecret() error: %v", err)
	}
	unattend := string(secret.Data[SysprepUnattendKey])

	for _, want := range []string{
		"<Name>operator</Name>",
		"<Value>autologon-password</Value>",
		"<HorizontalResolution>1600</HorizontalResolution>",
		"<VerticalResolution>900</VerticalResolution>",
	} {
		if !strings.Contains(unattend, want) {
			t.Fatalf("answer file is missing %s:\n%s", want, unattend)
		}
	}
}

func TestSysprepUnattendEscapesAndTruncatesForWindows(t *testing.T) {
	unattend, err := renderUnattend(`op&rator`, `p<ss`, "a-very-long-desktop-name", 1280, 800)
	if err != nil {
		t.Fatalf("renderUnattend() error: %v", err)
	}
	if !strings.Contains(unattend, "op&amp;rator") || !strings.Contains(unattend, "p&lt;ss") {
		t.Fatalf("the answer file is XML; unescaped input would make Windows reject it:\n%s", unattend)
	}
	// A ComputerName longer than the 15-character NetBIOS limit fails the
	// specialize pass and strands the desktop at OOBE.
	if !strings.Contains(unattend, "<ComputerName>a-very-long-des</ComputerName>") {
		t.Fatalf("hostname was not truncated to a NetBIOS name:\n%s", unattend)
	}
}

func TestNetbiosNameNeverEndsInAHyphen(t *testing.T) {
	if got := netbiosName("kakao-agent-de-sktop"); got != "kakao-agent-de" {
		t.Fatalf("netbiosName() = %q, want a name with no trailing hyphen", got)
	}
}

func TestSysprepSetupScriptBakesThePinnedInterpreter(t *testing.T) {
	secret, err := SysprepSecret(windowsInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Windows = &typeclawv1alpha1.PersonalDesktopWindowsSpec{
			PythonInstaller: &typeclawv1alpha1.PersonalDesktopWindowsInstallerSpec{
				URL:    "https://example.test/python.exe",
				SHA256: "abcdef",
			},
		}
	}), "guest-bearer", "autologon-password")
	if err != nil {
		t.Fatalf("SysprepSecret() error: %v", err)
	}
	script := string(secret.Data[SysprepSetupScriptKey])

	if strings.Contains(script, "@@PD_PYTHON_URL@@") || strings.Contains(script, "@@PD_PYTHON_SHA256@@") {
		t.Fatalf("setup script placeholders survived substitution")
	}
	if !strings.Contains(script, "https://example.test/python.exe") || !strings.Contains(script, "abcdef") {
		t.Fatalf("the declared interpreter download was not baked in")
	}
}

func TestSysprepSetupScriptFallsBackToTheTrackedInterpreter(t *testing.T) {
	secret, err := SysprepSecret(windowsInstance(nil), "guest-bearer", "autologon-password")
	if err != nil {
		t.Fatalf("SysprepSecret() error: %v", err)
	}
	script := string(secret.Data[SysprepSetupScriptKey])
	if !strings.Contains(script, typeclawv1alpha1.PersonalDesktopDefaultPythonSHA256) {
		t.Fatalf("the tracked interpreter digest is not baked into the setup script")
	}
}
