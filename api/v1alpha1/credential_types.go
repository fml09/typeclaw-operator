package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CredentialOperation identifies a protocol-specific, typed operation. The
// operator deliberately exposes no generic shell, URL, or header operation.
type CredentialOperation string

const (
	CredentialOperationGitHubCreateIssue CredentialOperation = "github.createIssue"
)

// CredentialAccessMode is selected by administrator policy, never by the
// model-controlled request. Deny is the fail-closed default.
type CredentialAccessMode string

const (
	CredentialAccessDeny          CredentialAccessMode = "Deny"
	CredentialAccessConfirm       CredentialAccessMode = "Confirm"
	CredentialAccessPreAuthorized CredentialAccessMode = "PreAuthorized"
	CredentialAccessBypass        CredentialAccessMode = "Bypass"
)

// CredentialPhase describes the one-shot invocation state machine.
type CredentialPhase string

const (
	CredentialPhasePending         CredentialPhase = "Pending"
	CredentialPhasePendingApproval CredentialPhase = "PendingApproval"
	CredentialPhaseTicketConsumed  CredentialPhase = "TicketConsumed"
	CredentialPhaseRunning         CredentialPhase = "Running"
	CredentialPhaseSucceeded       CredentialPhase = "Succeeded"
	CredentialPhaseFailed          CredentialPhase = "Failed"
	CredentialPhaseDenied          CredentialPhase = "Denied"
	CredentialPhaseUnknownOutcome  CredentialPhase = "UnknownOutcome"
)

// CredentialDecision is the only decision an administrator may append through
// a CredentialApproval. The Kubernetes API audit record supplies identity.
type CredentialDecision string

const (
	CredentialDecisionApprove CredentialDecision = "Approve"
	CredentialDecisionDeny    CredentialDecision = "Deny"
)

// CredentialSecretRef binds a policy to one administrator-created immutable
// Secret. The operator reads only Secret metadata; it never reads Secret.Data.
type CredentialSecretRef struct {
	// Name is the immutable Secret name in the TypeClaw Instance namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Key is the only key projected into a Credential Runner.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Key string `json:"key"`

	// UID and ResourceVersion bind the request to the exact Secret identity
	// observed by the controller before the Runner is created.
	// +kubebuilder:validation:MinLength=1
	UID string `json:"uid"`
	// +kubebuilder:validation:MinLength=1
	ResourceVersion string `json:"resourceVersion"`

	// Immutable must be true. Mutable name-only Secret projection is not a
	// supported credential binding.
	Immutable bool `json:"immutable"`
}

// CredentialConsumerSpec is an administrator-declared use of one credential.
// It is an allow-list, not a plugin identity or a place to put secret bytes.
type CredentialConsumerSpec struct {
	// Name identifies the approved consumer, for example github.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9.-]*$`
	Name string `json:"name"`

	// Operations are exact typed operation identifiers.
	// +kubebuilder:validation:MinItems=1
	Operations []CredentialOperation `json:"operations"`

	// AccessMode selects the administrator policy for this consumer.
	// +kubebuilder:validation:Enum=Deny;Confirm;PreAuthorized;Bypass
	AccessMode CredentialAccessMode `json:"accessMode"`

	// AllowedRepositories is an exact repository allow-list for GitHub
	// operations. Empty means no repository is authorized.
	// +optional
	AllowedRepositories []string `json:"allowedRepositories,omitempty"`
}

// CredentialPolicySpec grants approved consumers access to one exact Secret.
type CredentialPolicySpec struct {
	Secret CredentialSecretRef `json:"secret"`

	// Consumers is the complete administrator allow-list.
	// +kubebuilder:validation:MinItems=1
	Consumers []CredentialConsumerSpec `json:"consumers"`
	// AllowBypass is the independent administrator gate for Bypass mode.
	// Without it, a consumer declaring Bypass is denied.
	// +optional
	AllowBypass bool `json:"allowBypass,omitempty"`
}

// GitHubCreateIssueSpec is the typed input for github.createIssue. The API
// intentionally has no URL, arbitrary headers, or shell command field.
type GitHubCreateIssueSpec struct {
	// Repository is owner/name and is checked against policy.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`
	Repository string `json:"repository"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Title string `json:"title"`

	// +kubebuilder:validation:MaxLength=65536
	Body string `json:"body"`
}

// CredentialRequestSpec is the broker-produced, immutable intent record. The
// broker stores only a digest of the one-shot ticket; the ticket itself is
// returned once to the caller and never persisted in Kubernetes.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="credential request intent is immutable"
type CredentialRequestSpec struct {
	// Instance names the TypeClaw Instance whose policy authorizes this use.
	// +kubebuilder:validation:MinLength=1
	Instance string `json:"instance"`

	// Consumer selects an administrator-declared Credential Consumer.
	// +kubebuilder:validation:MinLength=1
	Consumer string `json:"consumer"`

	// Operation is restricted to the typed operation set.
	// +kubebuilder:validation:Enum=github.createIssue
	Operation CredentialOperation `json:"operation"`
	// TicketDigest is SHA-256(ticket), never the ticket itself.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	TicketDigest string `json:"ticketDigest"`

	// ExpiresAt bounds the one-shot ticket lifetime.
	ExpiresAt metav1.Time `json:"expiresAt"`
	// SecretBinding snapshots the exact administrator-approved Secret
	// metadata into the immutable request. Rotation therefore invalidates
	// pending and reusable tickets instead of silently switching credentials.
	SecretBinding CredentialSecretRef `json:"secretBinding"`

	// Repository, Title, and Body are the typed github.createIssue payload.
	// They intentionally remain flat in the wire schema.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`
	Repository string `json:"repository"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Title string `json:"title"`
	// +kubebuilder:validation:MaxLength=65536
	Body string `json:"body"`
}

// GitHubIssueResult is the bounded, non-secret result returned by the Runner.
type GitHubIssueResult struct {
	Number int64  `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
}

// CredentialRequestStatus contains only state, identities, and sanitized
// result/error data. It never contains a ticket or credential bytes.
type CredentialRequestStatus struct {
	// +kubebuilder:validation:Enum=Pending;PendingApproval;TicketConsumed;Running;Succeeded;Failed;Denied;UnknownOutcome
	Phase CredentialPhase `json:"phase,omitempty"`

	RunnerName string `json:"runnerName,omitempty"`
	PodUID     string `json:"podUID,omitempty"`
	SpecDigest string `json:"specDigest,omitempty"`

	SecretUID             string `json:"secretUID,omitempty"`
	SecretResourceVersion string `json:"secretResourceVersion,omitempty"`

	// +optional
	TicketConsumedAt *metav1.Time `json:"ticketConsumedAt,omitempty"`

	// +optional
	Result *GitHubIssueResult `json:"result,omitempty"`

	// ErrorCode is a bounded code, not raw Runner output.
	ErrorCode string `json:"errorCode,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=typeclaw
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Runner",type="string",JSONPath=`.status.runnerName`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`
type CredentialRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CredentialRequestSpec   `json:"spec"`
	Status CredentialRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CredentialRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CredentialRequest `json:"items"`
}

// CredentialApprovalSpec is a separate append-only administrator decision. The
// request controller never accepts approval through CredentialRequest.status.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="credential approval is immutable"
type CredentialApprovalSpec struct {
	// RequestName is the exact namespaced CredentialRequest being decided.
	// +kubebuilder:validation:MinLength=1
	RequestName string `json:"requestName"`

	// +kubebuilder:validation:Enum=Approve;Deny
	Decision CredentialDecision `json:"decision"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories=typeclaw
// +kubebuilder:printcolumn:name="Request",type="string",JSONPath=`.spec.requestName`
// +kubebuilder:printcolumn:name="Decision",type="string",JSONPath=`.spec.decision`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`
type CredentialApproval struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CredentialApprovalSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type CredentialApprovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CredentialApproval `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&CredentialRequest{}, &CredentialRequestList{},
		&CredentialApproval{}, &CredentialApprovalList{},
	)
}
