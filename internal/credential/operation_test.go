package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	v1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func validPolicy(mode v1alpha1.CredentialAccessMode) *v1alpha1.CredentialPolicySpec {
	return &v1alpha1.CredentialPolicySpec{
		Secret: v1alpha1.CredentialSecretRef{
			Name:            "github-credential-1",
			Key:             "token",
			UID:             "secret-uid",
			ResourceVersion: "17",
			Immutable:       true,
		},
		Consumers: []v1alpha1.CredentialConsumerSpec{{
			Name:                "github",
			Operations:          []v1alpha1.CredentialOperation{v1alpha1.CredentialOperationGitHubCreateIssue},
			AccessMode:          mode,
			AllowedRepositories: []string{"fml09/typeclaw"},
		}},
	}
}

func TestAuthorizePolicyRequiresExactTypedGrant(t *testing.T) {
	for _, test := range []struct {
		name      string
		policy    *v1alpha1.CredentialPolicySpec
		consumer  string
		op        v1alpha1.CredentialOperation
		repo      string
		wantMode  v1alpha1.CredentialAccessMode
		wantError bool
	}{
		{
			name:     "preauthorized exact match",
			policy:   validPolicy(v1alpha1.CredentialAccessPreAuthorized),
			consumer: "github", op: v1alpha1.CredentialOperationGitHubCreateIssue,
			repo: "fml09/typeclaw", wantMode: v1alpha1.CredentialAccessPreAuthorized,
		},
		{
			name:     "unknown repository",
			policy:   validPolicy(v1alpha1.CredentialAccessPreAuthorized),
			consumer: "github", op: v1alpha1.CredentialOperationGitHubCreateIssue,
			repo: "fml09/other", wantError: true,
		},
		{
			name:     "arbitrary operation",
			policy:   validPolicy(v1alpha1.CredentialAccessPreAuthorized),
			consumer: "github", op: "http.request", repo: "fml09/typeclaw", wantError: true,
		},
		{
			name:     "deny mode",
			policy:   validPolicy(v1alpha1.CredentialAccessDeny),
			consumer: "github", op: v1alpha1.CredentialOperationGitHubCreateIssue,
			repo: "fml09/typeclaw", wantError: true,
		},
		{
			name:     "bypass requires independent gate",
			policy:   validPolicy(v1alpha1.CredentialAccessBypass),
			consumer: "github", op: v1alpha1.CredentialOperationGitHubCreateIssue,
			repo: "fml09/typeclaw", wantError: true,
		},
		{
			name: "bypass with policy gate",
			policy: func() *v1alpha1.CredentialPolicySpec {
				p := validPolicy(v1alpha1.CredentialAccessBypass)
				p.AllowBypass = true
				return p
			}(),
			consumer: "github", op: v1alpha1.CredentialOperationGitHubCreateIssue,
			repo: "fml09/typeclaw", wantMode: v1alpha1.CredentialAccessBypass,
		},
		{
			name: "missing immutable binding",
			policy: func() *v1alpha1.CredentialPolicySpec {
				p := validPolicy(v1alpha1.CredentialAccessBypass)
				p.Secret.Immutable = false
				return p
			}(),
			consumer: "github", op: v1alpha1.CredentialOperationGitHubCreateIssue,
			repo: "fml09/typeclaw", wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			mode, err := AuthorizePolicy(test.policy, test.consumer, test.op, test.repo)
			if (err != nil) != test.wantError {
				t.Fatalf("AuthorizePolicy() error = %v, want error=%t", err, test.wantError)
			}
			if !test.wantError && mode != test.wantMode {
				t.Fatalf("mode = %q, want %q", mode, test.wantMode)
			}
		})
	}
}

func TestTicketIsOpaqueAndDeterministic(t *testing.T) {
	ticket, digest, err := NewTicket(bytes.NewReader(bytes.Repeat([]byte{0x42}, 32)))
	if err != nil {
		t.Fatalf("NewTicket() error: %v", err)
	}
	if ticket == digest || ticket == "" || digest != TicketDigest(ticket) {
		t.Fatalf("ticket/digest contract broken: ticket=%q digest=%q", ticket, digest)
	}
	if got := RequestName(digest); got != "credential-"+digest[:24] {
		t.Fatalf("request name = %q", got)
	}
}

type recordingDoer struct {
	request *http.Request
}

func (d *recordingDoer) Do(request *http.Request) (*http.Response, error) {
	d.request = request
	return &http.Response{
		StatusCode: http.StatusCreated,
		Body:       io.NopCloser(bytes.NewBufferString(`{"number":7,"html_url":"https://github.com/fml09/typeclaw/issues/7","title":"hello"}`)),
		Header:     make(http.Header),
	}, nil
}

func TestGitHubClientFixesEndpointAndHeaders(t *testing.T) {
	doer := &recordingDoer{}
	result, err := (GitHubClient{Client: doer}).CreateIssue(context.Background(), "token-value", v1alpha1.GitHubCreateIssueSpec{
		Repository: "fml09/typeclaw",
		Title:      "hello",
	})
	if err != nil {
		t.Fatalf("CreateIssue() error: %v", err)
	}
	if result.Number != 7 || doer.request == nil {
		t.Fatalf("unexpected result/request: %+v %#v", result, doer.request)
	}
	if got := doer.request.URL.String(); got != "https://api.github.com/repos/fml09/typeclaw/issues" {
		t.Fatalf("endpoint = %q", got)
	}
	if got := doer.request.Header.Get("Authorization"); got != "Bearer token-value" {
		t.Fatalf("authorization header = %q", got)
	}
	if got := doer.request.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
		t.Fatalf("API version header = %q", got)
	}
	var body map[string]string
	if err := json.NewDecoder(doer.request.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if len(body) != 2 || body["title"] != "hello" || body["body"] != "" {
		t.Fatalf("typed request body = %#v", body)
	}
}

func TestGitHubClientRejectsArbitraryEndpoint(t *testing.T) {
	_, err := (GitHubClient{BaseURL: "https://attacker.example"}).CreateIssue(context.Background(), "token", v1alpha1.GitHubCreateIssueSpec{
		Repository: "fml09/typeclaw", Title: "hello",
	})
	if err == nil {
		t.Fatal("arbitrary endpoint must be rejected")
	}
}

func TestRunnerResultRejectsUnboundedOrUnknownFields(t *testing.T) {
	if _, err := DecodeRunnerResult(`{"result":{"number":1,"url":"https://github.com/fml09/typeclaw/issues/1","title":"ok"},"credential":"leak"}`); err == nil {
		t.Fatal("unknown result fields must be rejected")
	}
	if _, err := EncodeRunnerResult(RunnerResult{Result: &v1alpha1.GitHubIssueResult{Number: 1, URL: "https://github.com/x/y/issues/1", Title: "ok"}}); err != nil {
		t.Fatalf("bounded result must encode: %v", err)
	}
}
