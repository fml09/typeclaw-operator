// sysprep.go assembles the Windows guest's first-boot payload.
//
// KubeVirt attaches this Secret as a CD-ROM, so the answer file, the
// first-logon script, the Guest Desktop Agent and its bearer token all reach a
// sysprepped golden image without the operator ever building a custom image
// and without the credential appearing in the VirtualMachine spec.
package desktop

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/guest"
)

// Keys of the sysprep Secret. The names are the file names Windows sees on the
// attached CD-ROM: the unattend pass looks for autounattend/unattend.xml, and
// the first-logon command searches the drives for setup.ps1.
const (
	SysprepUnattendKey    = "unattend.xml"
	SysprepSetupScriptKey = "setup.ps1"
	SysprepAgentKey       = "desktop_agent.py"
	SysprepAgentTokenKey  = "agent-token"
)

// unattendTemplate is parsed once: the embedded answer file is a compile-time
// constant, so a parse failure is a build-time defect, not a cluster event.
var unattendTemplate = template.Must(
	template.New("unattend").Parse(guest.WindowsUnattendTemplate),
)

// SysprepSecret renders the Windows first-boot payload. guestToken is the
// bearer the Desktop Gateway presents to the Guest Desktop Agent; password is
// the autologon password of the interactive user.
func SysprepSecret(instance *typeclawv1alpha1.TypeClawInstance, guestToken, password string) (*corev1.Secret, error) {
	spec := instance.Spec.PersonalDesktop
	names := Names(instance)
	width, height := Screen(spec)

	unattend, err := renderUnattend(Username(spec), password, names.Desktop, width, height)
	if err != nil {
		return nil, err
	}
	pythonURL, pythonSHA256 := PythonInstaller(spec)

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Sysprep,
			Namespace: names.Namespace,
			Labels:    Labels(instance),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			SysprepUnattendKey:    []byte(unattend),
			SysprepSetupScriptKey: []byte(windowsSetupScript(pythonURL, pythonSHA256)),
			SysprepAgentKey:       []byte(guest.DesktopAgentSource),
			SysprepAgentTokenKey:  []byte(guestToken),
		},
	}, nil
}

// renderUnattend fills the answer file. The hostname is truncated to the 15
// characters a Windows NetBIOS computer name allows; a longer ComputerName
// makes the specialize pass fail and leaves the desktop stuck at OOBE.
func renderUnattend(username, password, hostname string, width, height int32) (string, error) {
	var out bytes.Buffer
	// text/template performs no escaping, and the answer file is XML: an
	// administrator-chosen username carrying & or < would produce a document
	// Windows silently refuses, leaving the desktop stuck at OOBE.
	err := unattendTemplate.Execute(&out, struct {
		Username string
		Password string
		Width    int32
		Height   int32
		Hostname string
	}{
		Username: xmlEscape(username),
		Password: xmlEscape(password),
		Width:    width,
		Height:   height,
		Hostname: xmlEscape(netbiosName(hostname)),
	})
	if err != nil {
		return "", fmt.Errorf("render Windows answer file: %w", err)
	}
	return out.String(), nil
}

// windowsSetupScript bakes the interpreter download and its digest into the
// first-logon script, so the guest verifies what it downloads without the
// operator having to pass environment through the sysprep CD-ROM.
func windowsSetupScript(pythonURL, pythonSHA256 string) string {
	script := strings.ReplaceAll(guest.WindowsSetupScript, "@@PD_PYTHON_URL@@", pythonURL)
	return strings.ReplaceAll(script, "@@PD_PYTHON_SHA256@@", pythonSHA256)
}

func xmlEscape(value string) string {
	var out bytes.Buffer
	if err := xml.EscapeText(&out, []byte(value)); err != nil {
		return ""
	}
	return out.String()
}

const netbiosNameLimit = 15

func netbiosName(name string) string {
	if len(name) <= netbiosNameLimit {
		return name
	}
	// A NetBIOS name may not end in a hyphen, which truncation can produce.
	return strings.TrimRight(name[:netbiosNameLimit], "-")
}
