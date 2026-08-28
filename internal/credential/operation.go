package credential

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	v1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

const (
	RunnerCredentialFile           = "/var/run/typeclaw/credential/token"
	RunnerResultPath               = "/dev/termination-log"
	RunnerSPIFFETrustDomain        = "typeclaw.local"
	RunnerSPIFFEVolumeName         = "spiffe-workload-api"
	RunnerSPIFFEMountPath          = "/spiffe-workload-api"
	RunnerSPIFFEEndpoint           = "unix:///spiffe-workload-api/spire-agent.sock"
	RunnerNetworkHost              = "api.github.com"
	TicketTTL                      = 5 * time.Minute
	ErrorCodeCredentialUnavailable = "credential_unavailable"
	ErrorCodePolicyDenied          = "policy_denied"
	ErrorCodeApprovalDenied        = "approval_denied"
	ErrorCodeSecretBinding         = "secret_binding_invalid"
	ErrorCodeSPIFFEUnavailable     = "spiffe_unavailable"
	ErrorCodeNetworkUnavailable    = "network_authority_unavailable"
	ErrorCodeRunnerFailed          = "runner_failed"
	ErrorCodeResultInvalid         = "result_invalid"
	ErrorCodeTicketExpired         = "ticket_expired"
)

var (
	ErrPolicyDenied   = errors.New("credential policy denied operation")
	ErrMalformedInput = errors.New("malformed typed operation")
	ErrUnsupported    = errors.New("unsupported credential operation")
	ErrInvalidTicket  = errors.New("invalid invocation ticket")
	ErrSecretBinding  = errors.New("invalid credential secret binding")
)

// ValidateGitHubCreateIssue is shared by the broker and Runner. It accepts
// only the fields needed by the fixed GitHub operation; callers cannot supply
// a URL, HTTP headers, or a command.
func ValidateGitHubCreateIssue(repository, title, body string) error {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || !validRepositoryPart(parts[0]) || !validRepositoryPart(parts[1]) {
		return fmt.Errorf("%w: repository", ErrMalformedInput)
	}
	if len(title) == 0 || len(title) > 256 || strings.IndexByte(title, '\x00') >= 0 {
		return fmt.Errorf("%w: title", ErrMalformedInput)
	}
	if len(body) > 65536 || strings.IndexByte(body, '\x00') >= 0 {
		return fmt.Errorf("%w: body", ErrMalformedInput)
	}
	return nil
}

func validRepositoryPart(part string) bool {
	if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
		return false
	}
	for _, r := range part {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// ValidateSecretBinding verifies the metadata-only binding required before a
// Runner may receive a credential. It deliberately does not inspect Secret.Data.
func ValidateSecretBinding(ref v1alpha1.CredentialSecretRef) error {
	if ref.Name == "" || ref.Key == "" || ref.UID == "" || ref.ResourceVersion == "" || !ref.Immutable {
		return ErrSecretBinding
	}
	return nil
}

// AuthorizePolicy resolves one exact administrator-declared consumer and
// operation. An empty policy, unknown consumer, unknown operation, denied
// mode, or repository outside the allow-list fails closed.
func AuthorizePolicy(policy *v1alpha1.CredentialPolicySpec, consumer string, operation v1alpha1.CredentialOperation, repository string) (v1alpha1.CredentialAccessMode, error) {
	if policy == nil {
		return v1alpha1.CredentialAccessDeny, ErrPolicyDenied
	}
	if err := ValidateSecretBinding(policy.Secret); err != nil {
		return v1alpha1.CredentialAccessDeny, ErrPolicyDenied
	}
	if operation != v1alpha1.CredentialOperationGitHubCreateIssue {
		return v1alpha1.CredentialAccessDeny, ErrUnsupported
	}
	if err := ValidateGitHubCreateIssue(repository, "title", ""); err != nil {
		return v1alpha1.CredentialAccessDeny, ErrMalformedInput
	}
	for _, candidate := range policy.Consumers {
		if candidate.Name != consumer {
			continue
		}
		allowedOperation := false
		for _, allowed := range candidate.Operations {
			if allowed == operation {
				allowedOperation = true
				break
			}
		}
		if !allowedOperation || !allowedRepository(candidate.AllowedRepositories, repository) {
			return v1alpha1.CredentialAccessDeny, ErrPolicyDenied
		}
		switch candidate.AccessMode {
		case v1alpha1.CredentialAccessConfirm,
			v1alpha1.CredentialAccessPreAuthorized:
			return candidate.AccessMode, nil
		case v1alpha1.CredentialAccessBypass:
			if policy.AllowBypass {
				return candidate.AccessMode, nil
			}
			return v1alpha1.CredentialAccessDeny, ErrPolicyDenied
		default:
			return v1alpha1.CredentialAccessDeny, ErrPolicyDenied
		}
	}
	return v1alpha1.CredentialAccessDeny, ErrPolicyDenied
}

func allowedRepository(allowList []string, repository string) bool {
	for _, allowed := range allowList {
		if allowed == repository {
			return true
		}
	}
	return false
}

// NewTicket creates the one-shot opaque handle returned to a broker caller.
// Only its digest is persisted in Kubernetes.
func NewTicket(source io.Reader) (string, string, error) {
	if source == nil {
		source = rand.Reader
	}
	buf := make([]byte, 32)
	if _, err := io.ReadFull(source, buf); err != nil {
		return "", "", err
	}
	ticket := base64.RawURLEncoding.EncodeToString(buf)
	return ticket, TicketDigest(ticket), nil
}

func TicketDigest(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

func ValidateTicket(ticket string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil || len(decoded) != 32 {
		return ErrInvalidTicket
	}
	return nil
}

// RequestName deterministically maps a ticket digest to one Kubernetes object
// name. Reusing a ticket therefore addresses the same request and cannot spawn
// a second Runner.
func RequestName(ticketDigest string) string {
	if len(ticketDigest) > 24 {
		ticketDigest = ticketDigest[:24]
	}
	return "credential-" + ticketDigest
}

func IsTerminal(phase v1alpha1.CredentialPhase) bool {
	switch phase {
	case v1alpha1.CredentialPhaseSucceeded,
		v1alpha1.CredentialPhaseFailed,
		v1alpha1.CredentialPhaseDenied,
		v1alpha1.CredentialPhaseUnknownOutcome:
		return true
	default:
		return false
	}
}

// RunnerResult is the bounded termination-message contract. ErrorCode is
// intentionally an enum-like code; upstream response bodies never cross it.
type RunnerResult struct {
	Result    *v1alpha1.GitHubIssueResult `json:"result,omitempty"`
	ErrorCode string                      `json:"errorCode,omitempty"`
}

func EncodeRunnerResult(result RunnerResult) ([]byte, error) {
	if result.Result == nil && !validErrorCode(result.ErrorCode) {
		return nil, errors.New("runner result has no bounded outcome")
	}
	if result.Result != nil && result.ErrorCode != "" {
		return nil, errors.New("runner result has multiple outcomes")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 4096 {
		return nil, errors.New("runner result exceeds termination message limit")
	}
	return encoded, nil
}

func DecodeRunnerResult(raw string) (RunnerResult, error) {
	var result RunnerResult
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return RunnerResult{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return RunnerResult{}, errors.New("runner result contains trailing data")
	}
	if result.Result == nil && !validErrorCode(result.ErrorCode) {
		return RunnerResult{}, errors.New("runner result has no bounded outcome")
	}
	if result.Result != nil {
		if result.ErrorCode != "" || result.Result.Number <= 0 || result.Result.URL == "" || result.Result.Title == "" {
			return RunnerResult{}, errors.New("runner result is incomplete")
		}
	}
	return result, nil
}

func validErrorCode(code string) bool {
	switch code {
	case ErrorCodeCredentialUnavailable,
		ErrorCodePolicyDenied,
		ErrorCodeApprovalDenied,
		ErrorCodeSecretBinding,
		ErrorCodeSPIFFEUnavailable,
		ErrorCodeNetworkUnavailable,
		ErrorCodeRunnerFailed,
		ErrorCodeResultInvalid,
		ErrorCodeTicketExpired:
		return true
	default:
		return false
	}
}
