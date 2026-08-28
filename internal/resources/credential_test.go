package resources

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/credential"
)

func credentialInstance() *v1alpha1.TypeClawInstance {
	return &v1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "kakao-agent", Namespace: "agents"},
		Spec: v1alpha1.TypeClawInstanceSpec{CredentialPolicy: &v1alpha1.CredentialPolicySpec{
			Secret: v1alpha1.CredentialSecretRef{
				Name: "github-credential-1", Key: "token", UID: "uid-1", ResourceVersion: "17", Immutable: true,
			},
			Consumers: []v1alpha1.CredentialConsumerSpec{{
				Name: "github", Operations: []v1alpha1.CredentialOperation{v1alpha1.CredentialOperationGitHubCreateIssue},
				AccessMode: v1alpha1.CredentialAccessPreAuthorized, AllowedRepositories: []string{"fml09/typeclaw"},
			}},
		}},
	}
}

func credentialRequest() *v1alpha1.CredentialRequest {
	return &v1alpha1.CredentialRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "credential-1234567890abcdef12345678", Namespace: "agents"},
		Spec: v1alpha1.CredentialRequestSpec{
			Instance: "kakao-agent", Consumer: "github", Operation: v1alpha1.CredentialOperationGitHubCreateIssue,
			TicketDigest: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			ExpiresAt:    metav1.Time{Time: time.Now().Add(time.Hour)},
			SecretBinding: v1alpha1.CredentialSecretRef{
				Name: "github-credential-1", Key: "token", UID: "uid-1", ResourceVersion: "17", Immutable: true,
			},
			Repository: "fml09/typeclaw",
			Title:      "hello",
			Body:       "world",
		},
	}
}

func TestCredentialRunnerJobContainsOnlyBoundSecretAndTypedInputs(t *testing.T) {
	job, err := CredentialRunnerJob(credentialInstance(), credentialRequest())
	if err != nil {
		t.Fatalf("CredentialRunnerJob() error: %v", err)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("BackoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatalf("Runner must be one-shot and tokenless: restart=%q automount=%v", pod.RestartPolicy, pod.AutomountServiceAccountToken)
	}
	container := pod.Containers[0]
	if container.Command == nil || len(container.Command) != 1 || container.Command[0] != "/credential-runner" {
		t.Fatalf("Runner command must be fixed binary, got %v", container.Command)
	}
	if len(container.EnvFrom) != 0 {
		t.Fatalf("Runner must not use EnvFrom for credentials: %+v", container.EnvFrom)
	}
	env := map[string]string{}
	for _, value := range container.Env {
		env[value.Name] = value.Value
	}
	if env["TYPECLAW_CREDENTIAL_FILE"] != credential.RunnerCredentialFile || env["TYPECLAW_GITHUB_REPOSITORY"] != "fml09/typeclaw" {
		t.Fatalf("typed Runner env = %v", env)
	}
	if env["SPIFFE_ENDPOINT_SOCKET"] != credential.RunnerSPIFFEEndpoint {
		t.Fatalf("SPIFFE endpoint = %q", env["SPIFFE_ENDPOINT_SOCKET"])
	}
	if env["TYPECLAW_CREDENTIAL_FILE"] == "uid-1" || env["TYPECLAW_CREDENTIAL_FILE"] == "token" {
		t.Fatal("Runner env must contain a path, never credential data")
	}
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == AgentMountPath || mount.MountPath == "/home/typeclaw" {
			t.Fatalf("Runner must not mount runtime state: %+v", mount)
		}
	}
	if len(pod.Volumes) != 2 {
		t.Fatalf("Runner must have one Secret volume and one SPIFFE volume: %+v", pod.Volumes)
	}
	var secret *corev1.SecretVolumeSource
	var spiffe *corev1.CSIVolumeSource
	for _, volume := range pod.Volumes {
		if volume.Secret != nil {
			secret = volume.Secret
		}
		if volume.CSI != nil {
			spiffe = volume.CSI
		}
	}
	if secret == nil || spiffe == nil || spiffe.Driver != "csi.spiffe.io" || spiffe.ReadOnly == nil || !*spiffe.ReadOnly {
		t.Fatalf("Runner volume sources = secret=%+v spiffe=%+v", secret, spiffe)
	}
	if secret.SecretName != "github-credential-1" || len(secret.Items) != 1 || secret.Items[0].Key != "token" || secret.Items[0].Path != "token" {
		t.Fatalf("Secret projection = %+v", secret)
	}
	if job.Annotations["typeclaw.fml09.io/credential-secret-uid"] != "uid-1" ||
		job.Annotations["typeclaw.fml09.io/credential-secret-resource-version"] != "17" {
		t.Fatalf("metadata binding annotations = %v", job.Annotations)
	}
	if job.Annotations["typeclaw.fml09.io/network-authority-required"] != credential.RunnerNetworkHost {
		t.Fatalf("network authority annotation = %q", job.Annotations["typeclaw.fml09.io/network-authority-required"])
	}
}

func TestCredentialRunnerNetworkPolicyDoesNotInheritRuntimePolicy(t *testing.T) {
	instance := credentialInstance()
	request := credentialRequest()
	policy := CredentialRunnerNetworkPolicy(instance, request)
	if policy.Spec.PodSelector.MatchLabels["app.kubernetes.io/instance"] != "" {
		t.Fatalf("Runner selector must not inherit runtime labels: %+v", policy.Spec.PodSelector)
	}
	if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].Ports) != 2 {
		t.Fatalf("Runner egress must be DNS-only until external Network Authority grants GitHub: %+v", policy.Spec.Egress)
	}
	if policy.Annotations["typeclaw.fml09.io/required-network-hosts"] != credential.RunnerNetworkHost {
		t.Fatalf("required network host annotation = %q", policy.Annotations["typeclaw.fml09.io/required-network-hosts"])
	}
}

func TestCredentialRunnerRejectsMissingPolicy(t *testing.T) {
	instance := credentialInstance()
	instance.Spec.CredentialPolicy = nil
	if _, err := CredentialRunnerJob(instance, credentialRequest()); err == nil {
		t.Fatal("Runner without administrator credential policy must fail closed")
	}
}

func TestCredentialRunnerKeepsRestrictedFloor(t *testing.T) {
	job, err := CredentialRunnerJob(credentialInstance(), credentialRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertRestrictedFloor(t, job.Spec.Template.Spec)
	if got := job.Spec.Template.Spec.Containers[0].TerminationMessagePolicy; got != corev1.TerminationMessageReadFile {
		t.Fatalf("termination message policy = %q", got)
	}
	if got := job.Spec.Template.Spec.Containers[0].TerminationMessagePath; got != credential.RunnerResultPath {
		t.Fatalf("termination message path = %q", got)
	}
	if got := job.Spec.Template.Spec.RestartPolicy; got != corev1.RestartPolicyNever {
		t.Fatalf("restart policy = %q", got)
	}
	if got := job.Spec.BackoffLimit; got == nil || *got != 0 {
		t.Fatalf("backoff limit = %v", got)
	}
}
