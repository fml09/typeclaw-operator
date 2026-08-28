package credential

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

const (
	GitHubCreateIssuePath = "/v1/github/create-issue"
	defaultBrokerBodySize = 128 << 10
)

// Broker is the capability gateway between a TypeClaw-facing typed tool and
// the Kubernetes control plane. It stores only typed intent and a ticket
// digest; it never reads or transports Secret.Data.
type Broker struct {
	Reader       client.Reader
	Writer       client.Writer
	TicketSource io.Reader
	TrustDomain  string
	Now          func() time.Time
	MaxBodySize  int64
}

type githubCreateIssueRequest struct {
	Repository string `json:"repository"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Ticket     string `json:"ticket,omitempty"`
}

type brokerResponse struct {
	State       v1alpha1.CredentialPhase    `json:"state"`
	RequestName string                      `json:"requestName"`
	Ticket      string                      `json:"ticket,omitempty"`
	Result      *v1alpha1.GitHubIssueResult `json:"result,omitempty"`
	ErrorCode   string                      `json:"errorCode,omitempty"`
}

// ServeHTTP accepts exactly one typed operation. Unknown JSON fields are
// rejected, which makes attempts to smuggle URL, header, or shell authority
// fail before a CredentialRequest is created.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != GitHubCreateIssuePath {
		writeBrokerError(w, http.StatusNotFound, "not_found")
		return
	}
	identity, err := identityFromRequest(r, b.trustDomain())
	if err != nil {
		writeBrokerError(w, http.StatusUnauthorized, "spiffe_required")
		return
	}
	limit := b.MaxBodySize
	if limit <= 0 {
		limit = defaultBrokerBodySize
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	var input githubCreateIssueRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeBrokerError(w, http.StatusBadRequest, "typed_request_invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeBrokerError(w, http.StatusBadRequest, "typed_request_invalid")
		return
	}
	if err := ValidateGitHubCreateIssue(input.Repository, input.Title, input.Body); err != nil {
		writeBrokerError(w, http.StatusBadRequest, "typed_request_invalid")
		return
	}

	if input.Ticket != "" {
		b.handleRetry(w, r.Context(), identity, input)
		return
	}
	b.handleNew(w, r.Context(), identity, input)
}

type brokerIdentity struct {
	Namespace string
	Instance  string
}

func identityFromRequest(r *http.Request, trustDomain string) (brokerIdentity, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return brokerIdentity{}, errors.New("peer certificate required")
	}
	var identity *brokerIdentity
	for _, cert := range r.TLS.PeerCertificates {
		for _, uri := range cert.URIs {
			candidate, err := ParseSPIFFEID(uri, trustDomain)
			if err != nil {
				continue
			}
			if identity != nil {
				return brokerIdentity{}, errors.New("peer has multiple TypeClaw SPIFFE identities")
			}
			identity = &candidate
		}
	}
	if identity == nil {
		return brokerIdentity{}, errors.New("trusted TypeClaw SPIFFE identity required")
	}
	return *identity, nil
}

// ParseSPIFFEID accepts only the broker's exact identity grammar:
// spiffe://<trust-domain>/typeclaw/ns/<namespace>/instance/<name>.
func ParseSPIFFEID(uri *url.URL, trustDomain string) (brokerIdentity, error) {
	if uri == nil || uri.Scheme != "spiffe" || uri.Host != trustDomain || uri.RawQuery != "" || uri.Fragment != "" {
		return brokerIdentity{}, errors.New("invalid SPIFFE identity")
	}
	parts := strings.Split(strings.Trim(uri.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "typeclaw" || parts[1] != "ns" || parts[3] != "instance" || parts[2] == "" || parts[4] == "" {
		return brokerIdentity{}, errors.New("invalid SPIFFE identity")
	}
	return brokerIdentity{Namespace: parts[2], Instance: parts[4]}, nil
}

func (b *Broker) trustDomain() string {
	if b.TrustDomain != "" {
		return b.TrustDomain
	}
	return RunnerSPIFFETrustDomain
}
func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *Broker) handleNew(w http.ResponseWriter, ctx context.Context, identity brokerIdentity, input githubCreateIssueRequest) {
	if b.Reader == nil || b.Writer == nil {
		writeBrokerError(w, http.StatusServiceUnavailable, "broker_unavailable")
		return
	}
	instance := &v1alpha1.TypeClawInstance{}
	if err := b.Reader.Get(ctx, types.NamespacedName{Namespace: identity.Namespace, Name: identity.Instance}, instance); err != nil {
		if apierrors.IsNotFound(err) {
			writeBrokerError(w, http.StatusForbidden, ErrorCodePolicyDenied)
		} else {
			writeBrokerError(w, http.StatusServiceUnavailable, "broker_unavailable")
		}
		return
	}
	policy := instance.Spec.CredentialPolicy
	if _, err := AuthorizePolicy(
		policy,
		"github",
		v1alpha1.CredentialOperationGitHubCreateIssue,
		input.Repository,
	); err != nil {
		writeBrokerError(w, http.StatusForbidden, ErrorCodePolicyDenied)
		return
	}
	ticket, digest, err := NewTicket(b.TicketSource)
	if err != nil {
		writeBrokerError(w, http.StatusServiceUnavailable, "ticket_unavailable")
		return
	}
	request := &v1alpha1.CredentialRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RequestName(digest),
			Namespace: identity.Namespace,
			Labels: map[string]string{
				"typeclaw.fml09.io/instance":           identity.Instance,
				"typeclaw.fml09.io/credential-request": RequestName(digest),
			},
		},
		Spec: v1alpha1.CredentialRequestSpec{
			Instance:      identity.Instance,
			Consumer:      "github",
			Operation:     v1alpha1.CredentialOperationGitHubCreateIssue,
			TicketDigest:  digest,
			ExpiresAt:     metav1.Time{Time: b.now().Add(TicketTTL)},
			SecretBinding: policy.Secret,
			Repository:    input.Repository,
			Title:         input.Title,
			Body:          input.Body,
		},
	}
	if err := b.Writer.Create(ctx, request); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A cryptographically random collision must not turn into a second
			// invocation. The caller can retry with a fresh ticket.
			writeBrokerError(w, http.StatusConflict, "ticket_collision")
		} else {
			writeBrokerError(w, http.StatusServiceUnavailable, "broker_unavailable")
		}
		return
	}
	writeBrokerJSON(w, http.StatusAccepted, brokerResponse{
		State:       v1alpha1.CredentialPhasePending,
		RequestName: request.Name,
		Ticket:      ticket,
	})
}

func (b *Broker) handleRetry(w http.ResponseWriter, ctx context.Context, identity brokerIdentity, input githubCreateIssueRequest) {
	if b.Reader == nil {
		writeBrokerError(w, http.StatusServiceUnavailable, "broker_unavailable")
		return
	}
	if err := ValidateTicket(input.Ticket); err != nil {
		writeBrokerError(w, http.StatusUnauthorized, "invalid_ticket")
		return
	}
	digest := TicketDigest(input.Ticket)
	request := &v1alpha1.CredentialRequest{}
	if err := b.Reader.Get(ctx, types.NamespacedName{Namespace: identity.Namespace, Name: RequestName(digest)}, request); err != nil {
		if apierrors.IsNotFound(err) {
			writeBrokerError(w, http.StatusNotFound, string(v1alpha1.CredentialPhaseUnknownOutcome))
		} else {
			writeBrokerError(w, http.StatusServiceUnavailable, "broker_unavailable")
		}
		return
	}
	if request.Spec.ExpiresAt.Time.IsZero() || !b.now().Before(request.Spec.ExpiresAt.Time) {
		writeBrokerError(w, http.StatusGone, ErrorCodeTicketExpired)
		return
	}
	if request.Spec.Instance != identity.Instance ||
		request.Spec.Consumer != "github" ||
		request.Spec.Operation != v1alpha1.CredentialOperationGitHubCreateIssue ||
		request.Spec.Repository != input.Repository ||
		request.Spec.Title != input.Title ||
		request.Spec.Body != input.Body ||
		request.Spec.TicketDigest != digest {
		writeBrokerError(w, http.StatusForbidden, "ticket_mismatch")
		return
	}
	writeRequestStatus(w, request, input.Ticket)
}

func writeRequestStatus(w http.ResponseWriter, request *v1alpha1.CredentialRequest, ticket string) {
	phase := request.Status.Phase
	if phase == "" {
		phase = v1alpha1.CredentialPhasePending
	}
	status := http.StatusAccepted
	switch phase {
	case v1alpha1.CredentialPhaseSucceeded:
		status = http.StatusOK
	case v1alpha1.CredentialPhaseDenied:
		status = http.StatusForbidden
	case v1alpha1.CredentialPhaseFailed, v1alpha1.CredentialPhaseUnknownOutcome:
		status = http.StatusConflict
	}
	writeBrokerJSON(w, status, brokerResponse{
		State:       phase,
		RequestName: request.Name,
		Ticket:      ticket,
		Result:      request.Status.Result,
		ErrorCode:   request.Status.ErrorCode,
	})
}

func writeBrokerError(w http.ResponseWriter, status int, code string) {
	writeBrokerJSON(w, status, struct {
		ErrorCode string `json:"errorCode"`
	}{ErrorCode: code})
}

func writeBrokerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
