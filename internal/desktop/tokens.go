// tokens.go owns the three shared secrets of one Personal Desktop.
//
// The tokens are generated once and then preserved verbatim on every later
// reconcile. Rotating them here would look harmless and break the desktop: the
// guest holds its copy in a file written by cloud-init or sysprep at first
// boot and never reads it again, so a new guest token would leave the Gateway
// authenticating against a VM that still expects the old one — recoverable
// only by rebuilding the disk this feature exists to preserve.
package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// tokenBytes yields 64 lowercase hex characters, comfortably above the
// Desktop Gateway's 24-byte minimum for every bearer it accepts.
const tokenBytes = 32

// TokenSet carries the desktop's shared secrets in plaintext, only ever
// between the renderer and the Secret it writes them into.
type TokenSet struct {
	// Agent authenticates the computer-use plugin to the Desktop Gateway.
	Agent string
	// Guest authenticates the Desktop Gateway to the Guest Desktop Agent.
	Guest string
	// WindowsPassword is the autologon password of the Windows interactive
	// user. It is generated only for a Windows desktop, whose account sysprep
	// creates, but it is preserved from then on: a Linux reconcile of the same
	// disk must not drop the password the guest still holds.
	WindowsPassword string
}

// NewTokenSet returns the tokens for one desktop, reusing every value already
// present in existing. existing is nil on the first reconcile.
func NewTokenSet(instance *typeclawv1alpha1.TypeClawInstance, existing *corev1.Secret) (TokenSet, error) {
	// The stored autologon password is read whatever the declared OS is. It
	// is written into the guest's local account by sysprep, which runs once
	// on a disk that is then kept; dropping the key while the desktop is
	// switched to Linux would make a later switch back generate a password
	// the guest rejects and nobody can recover.
	tokens := TokenSet{
		Agent:           existingToken(existing, TokenKeyAgent),
		Guest:           existingToken(existing, TokenKeyGuest),
		WindowsPassword: existingToken(existing, TokenKeyWindowsPassword),
	}

	for _, slot := range []struct {
		name   string
		target *string
		needed bool
	}{
		{TokenKeyAgent, &tokens.Agent, true},
		{TokenKeyGuest, &tokens.Guest, true},
		{TokenKeyWindowsPassword, &tokens.WindowsPassword, OS(instance.Spec.PersonalDesktop) == OSWindows},
	} {
		if !slot.needed || *slot.target != "" {
			continue
		}
		value, err := randomToken()
		if err != nil {
			return TokenSet{}, fmt.Errorf("generate %s: %w", slot.name, err)
		}
		*slot.target = value
	}
	return tokens, nil
}

// TokensSecret renders the desktop-namespace Secret holding the token set.
// It is retained when the feature is disabled so re-enabling resumes the same
// disk with credentials the guest still recognises.
func TokensSecret(instance *typeclawv1alpha1.TypeClawInstance, tokens TokenSet) *corev1.Secret {
	names := Names(instance)
	data := map[string][]byte{
		TokenKeyAgent: []byte(tokens.Agent),
		TokenKeyGuest: []byte(tokens.Guest),
	}
	if tokens.WindowsPassword != "" {
		data[TokenKeyWindowsPassword] = []byte(tokens.WindowsPassword)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Tokens,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// MirroredAgentTokenSecret renders the Instance-namespace copy of the agent
// token. A Pod can only project a Secret from its own namespace, so a desktop
// in a dedicated namespace needs this mirror for the runtime's secretKeyRef to
// resolve. It carries the plugin bearer alone: the guest token and the Windows
// password have no business in the namespace the agent runs in.
func MirroredAgentTokenSecret(instance *typeclawv1alpha1.TypeClawInstance, agentToken string) *corev1.Secret {
	names := Names(instance)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Tokens,
			Namespace: names.InstanceNamespace,
			Labels:    Labels(instance),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{TokenKeyAgent: []byte(agentToken)},
	}
}

func existingToken(secret *corev1.Secret, key string) string {
	if secret == nil {
		return ""
	}
	if value, ok := secret.Data[key]; ok && len(value) > 0 {
		return string(value)
	}
	return ""
}

func randomToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
