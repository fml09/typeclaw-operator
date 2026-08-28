package resources

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/credential"
)

const (
	CredentialRunnerContainerName = "credential-runner"
	CredentialRunnerVolumeName    = "credential"
	CredentialRunnerMountPath     = "/var/run/typeclaw/credential"
	CredentialRunnerComponent     = "credential-runner"
	CredentialRunnerTTLSeconds    = int32(300)
	CredentialRunnerDeadline      = int64(120)
)

// CredentialRunnerJob renders a fresh, one-shot Restricted Workload. It
// references only the administrator-bound Secret metadata in the Instance
// policy and never receives the live Agent Folder or a Kubernetes token.
func CredentialRunnerJob(instance *v1alpha1.TypeClawInstance, request *v1alpha1.CredentialRequest) (*batchv1.Job, error) {
	if instance == nil || request == nil || instance.Spec.CredentialPolicy == nil || request.Spec.Instance != instance.Name {
		return nil, fmt.Errorf("credential runner requires matching Instance, policy, and request")
	}
	if err := credential.ValidateSecretBinding(instance.Spec.CredentialPolicy.Secret); err != nil {
		return nil, err
	}
	if request.Spec.SecretBinding != instance.Spec.CredentialPolicy.Secret {
		return nil, fmt.Errorf("credential request Secret binding changed")
	}
	if err := credential.ValidateSecretBinding(request.Spec.SecretBinding); err != nil {
		return nil, err
	}
	if err := credential.ValidateGitHubCreateIssue(request.Spec.Repository, request.Spec.Title, request.Spec.Body); err != nil {
		return nil, err
	}
	if request.Spec.Operation != v1alpha1.CredentialOperationGitHubCreateIssue {
		return nil, fmt.Errorf("unsupported credential operation")
	}

	ref := request.Spec.SecretBinding
	jobName := CredentialRunnerJobName(request.Name)
	labels := map[string]string{
		componentLabelKey:                       CredentialRunnerComponent,
		"typeclaw.fml09.io/credential-request":  request.Name,
		"typeclaw.fml09.io/credential-instance": instance.Name,
	}
	annotations := map[string]string{
		"typeclaw.fml09.io/credential-secret-uid":              ref.UID,
		"typeclaw.fml09.io/credential-secret-resource-version": ref.ResourceVersion,
		"typeclaw.fml09.io/network-authority-required":         credential.RunnerNetworkHost,
		"typeclaw.fml09.io/spiffe-required":                    "true",
		"spiffe.io/credential-runner-id":                       CredentialRunnerSPIFFEID(instance, request),
	}
	backoffLimit := int32(0)
	ttlSeconds := CredentialRunnerTTLSeconds
	deadline := CredentialRunnerDeadline
	mode := int32(0o440)
	spiffeReadOnly := true

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName,
			Namespace:   instance.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSeconds,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolRef(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolRef(true),
						RunAsUser:    int64Ref(RuntimeUID),
						RunAsGroup:   int64Ref(RuntimeGID),
						FSGroup:      int64Ref(RuntimeGID),
						SeccompProfile: &corev1.SeccompProfile{
							Type:             corev1.SeccompProfileTypeLocalhost,
							LocalhostProfile: strRef(SeccompLocalhostProfile),
						},
					},
					Containers: []corev1.Container{{
						Name:            CredentialRunnerContainerName,
						Image:           DefaultOperatorImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"/credential-runner"},
						Env: []corev1.EnvVar{
							{Name: "TYPECLAW_CREDENTIAL_OPERATION", Value: string(request.Spec.Operation)},
							{Name: "TYPECLAW_GITHUB_REPOSITORY", Value: request.Spec.Repository},
							{Name: "TYPECLAW_GITHUB_TITLE", Value: request.Spec.Title},
							{Name: "TYPECLAW_GITHUB_BODY", Value: request.Spec.Body},
							{Name: "TYPECLAW_CREDENTIAL_FILE", Value: credential.RunnerCredentialFile},
							{Name: "TYPECLAW_SPIFFE_ID", Value: CredentialRunnerSPIFFEID(instance, request)},
							{Name: "SPIFFE_ENDPOINT_SOCKET", Value: credential.RunnerSPIFFEEndpoint},
						},
						TerminationMessagePath:   credential.RunnerResultPath,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolRef(false),
							ReadOnlyRootFilesystem:   boolRef(true),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      CredentialRunnerVolumeName,
								MountPath: CredentialRunnerMountPath,
								ReadOnly:  true,
							},
							{
								Name:      credential.RunnerSPIFFEVolumeName,
								MountPath: credential.RunnerSPIFFEMountPath,
								ReadOnly:  true,
							},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: CredentialRunnerVolumeName,
							VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
								SecretName:  ref.Name,
								Items:       []corev1.KeyToPath{{Key: ref.Key, Path: "token"}},
								DefaultMode: &mode,
							}},
						},
						{
							Name: credential.RunnerSPIFFEVolumeName,
							VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
								Driver:   "csi.spiffe.io",
								ReadOnly: &spiffeReadOnly,
							}},
						},
					},
				},
			},
		},
	}, nil
}

// CredentialRunnerNetworkPolicy is default-deny for the Runner. DNS is the
// only Kubernetes-level exception; an administrator-owned Network Authority
// must separately authorize api.github.com, represented by the Job annotation.
func CredentialRunnerNetworkPolicy(instance *v1alpha1.TypeClawInstance, request *v1alpha1.CredentialRequest) *networkingv1.NetworkPolicy {
	labels := map[string]string{
		componentLabelKey:                       CredentialRunnerComponent,
		"typeclaw.fml09.io/credential-request":  request.Name,
		"typeclaw.fml09.io/credential-instance": instance.Name,
	}
	policyLabels := map[string]string{
		"app.kubernetes.io/managed-by":          "typeclaw-operator",
		"typeclaw.fml09.io/credential-instance": instance.Name,
		componentLabelKey:                       CredentialRunnerComponent,
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CredentialRunnerNetworkPolicyName(request.Name),
			Namespace: instance.Namespace,
			Labels:    policyLabels,
			Annotations: map[string]string{
				"typeclaw.fml09.io/required-network-hosts": credential.RunnerNetworkHost,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": "kube-system",
					}},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"k8s-app": "kube-dns",
					}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: protocolPtr(corev1.ProtocolUDP), Port: netIntOrStr(53)},
					{Protocol: protocolPtr(corev1.ProtocolTCP), Port: netIntOrStr(53)},
				},
			}},
		},
	}
	return policy
}

func CredentialRunnerJobName(requestName string) string {
	return requestName + "-runner"
}

func CredentialRunnerNetworkPolicyName(requestName string) string {
	return requestName + "-egress"
}

func CredentialRunnerSPIFFEID(instance *v1alpha1.TypeClawInstance, request *v1alpha1.CredentialRequest) string {
	return fmt.Sprintf("spiffe://%s/typeclaw/ns/%s/instance/%s/credential-runner/%s",
		credential.RunnerSPIFFETrustDomain, instance.Namespace, instance.Name, request.Name)
}

func protocolPtr(protocol corev1.Protocol) *corev1.Protocol {
	return &protocol
}
