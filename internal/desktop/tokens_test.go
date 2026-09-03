package desktop

import (
	"encoding/hex"
	"testing"

	corev1 "k8s.io/api/core/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func TestNewTokenSetGenerates64HexCharacters(t *testing.T) {
	tokens, err := NewTokenSet(desktopInstance(nil), nil)
	if err != nil {
		t.Fatalf("NewTokenSet() error: %v", err)
	}
	for name, value := range map[string]string{"agent": tokens.Agent, "guest": tokens.Guest} {
		if len(value) != 64 {
			t.Fatalf("%s token length = %d, want 64", name, len(value))
		}
		if _, err := hex.DecodeString(value); err != nil {
			t.Fatalf("%s token is not hex: %v", name, err)
		}
	}
	if tokens.Agent == tokens.Guest {
		t.Fatalf("agent and guest bearers must be distinct secrets")
	}
	if tokens.WindowsPassword != "" {
		t.Fatalf("a Linux desktop has no autologon password: %q", tokens.WindowsPassword)
	}
}

func TestNewTokenSetPreservesExistingTokens(t *testing.T) {
	in := desktopInstance(nil)
	first, err := NewTokenSet(in, nil)
	if err != nil {
		t.Fatalf("NewTokenSet() error: %v", err)
	}

	// The guest wrote its copy of the bearer to disk at first boot and never
	// reads it again; regenerating here would strand the Gateway behind a
	// token the VM does not recognise.
	second, err := NewTokenSet(in, TokensSecret(in, first))
	if err != nil {
		t.Fatalf("NewTokenSet() error: %v", err)
	}
	if second.Agent != first.Agent || second.Guest != first.Guest {
		t.Fatalf("tokens rotated across reconciles: %+v -> %+v", first, second)
	}
}

func TestNewTokenSetFillsOnlyMissingKeys(t *testing.T) {
	in := desktopInstance(nil)
	existing := &corev1.Secret{Data: map[string][]byte{TokenKeyAgent: []byte("kept-agent-token")}}

	tokens, err := NewTokenSet(in, existing)
	if err != nil {
		t.Fatalf("NewTokenSet() error: %v", err)
	}
	if tokens.Agent != "kept-agent-token" {
		t.Fatalf("agent token = %q, want the stored value", tokens.Agent)
	}
	if len(tokens.Guest) != 64 {
		t.Fatalf("missing guest token was not generated: %q", tokens.Guest)
	}
}

func TestNewTokenSetAddsWindowsPassword(t *testing.T) {
	in := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.OS = OSWindows
	})
	tokens, err := NewTokenSet(in, nil)
	if err != nil {
		t.Fatalf("NewTokenSet() error: %v", err)
	}
	if len(tokens.WindowsPassword) != 64 {
		t.Fatalf("windows autologon password = %q, want 64 hex characters", tokens.WindowsPassword)
	}

	secret := TokensSecret(in, tokens)
	if string(secret.Data[TokenKeyWindowsPassword]) != tokens.WindowsPassword {
		t.Fatalf("windows password is not carried by the Secret")
	}
}

func TestNewTokenSetPreservesTheWindowsPasswordAcrossAnOSChange(t *testing.T) {
	windows := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.OS = OSWindows
	})
	first, err := NewTokenSet(windows, nil)
	if err != nil {
		t.Fatalf("NewTokenSet() error: %v", err)
	}

	// Sysprep wrote this password into the guest's local account and never
	// runs again on that disk, so a Windows -> Linux -> Windows round trip
	// must not reach a state where the Secret advertises a password the guest
	// rejects and nobody recorded the change.
	linux := desktopInstance(nil)
	viaLinux, err := NewTokenSet(linux, TokensSecret(windows, first))
	if err != nil {
		t.Fatalf("NewTokenSet() error: %v", err)
	}
	if viaLinux.WindowsPassword != first.WindowsPassword {
		t.Fatalf("the stored autologon password was dropped on a Linux reconcile")
	}

	back, err := NewTokenSet(windows, TokensSecret(linux, viaLinux))
	if err != nil {
		t.Fatalf("NewTokenSet() error: %v", err)
	}
	if back.WindowsPassword != first.WindowsPassword {
		t.Fatalf("autologon password rotated: %q -> %q", first.WindowsPassword, back.WindowsPassword)
	}
}

func TestTokensSecretShape(t *testing.T) {
	in := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
	})
	secret := TokensSecret(in, TokenSet{Agent: "a", Guest: "g"})

	if secret.Name != "kakao-agent-desktop-tokens" || secret.Namespace != "typeclaw-desktops" {
		t.Fatalf("token Secret ref = %s/%s", secret.Namespace, secret.Name)
	}
	if _, found := secret.Data[TokenKeyWindowsPassword]; found {
		t.Fatalf("a Linux desktop must carry no autologon password")
	}
	if secret.Labels[LabelInstanceUID] != "instance-uid" {
		t.Fatalf("token Secret is not selectable for cleanup: %v", secret.Labels)
	}
}

func TestMirroredAgentTokenSecretCarriesOnlyThePluginBearer(t *testing.T) {
	in := desktopInstance(func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.PersonalDesktop.Namespace = "typeclaw-desktops"
	})
	mirror := MirroredAgentTokenSecret(in, "agent-bearer")

	if mirror.Namespace != "agents" {
		t.Fatalf("mirror namespace = %q, want the Instance namespace", mirror.Namespace)
	}
	if len(mirror.Data) != 1 || string(mirror.Data[TokenKeyAgent]) != "agent-bearer" {
		t.Fatalf("mirror data = %v, want the agent bearer alone", mirror.Data)
	}
}
