package credential

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func brokerTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func brokerRequest(t *testing.T, input string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, GitHubCreateIssuePath, bytes.NewBufferString(input))
	uri, err := url.Parse("spiffe://typeclaw.local/typeclaw/ns/agents/instance/kakao-agent")
	if err != nil {
		t.Fatal(err)
	}
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{uri}}}}
	return request
}

func TestParseSPIFFEIDUsesExactRuntimeIdentityGrammar(t *testing.T) {
	good, _ := url.Parse("spiffe://typeclaw.local/typeclaw/ns/agents/instance/kakao-agent")
	identity, err := ParseSPIFFEID(good, "typeclaw.local")
	if err != nil || identity.Namespace != "agents" || identity.Instance != "kakao-agent" {
		t.Fatalf("identity = %+v, err = %v", identity, err)
	}
	for _, raw := range []string{
		"spiffe://other.local/typeclaw/ns/agents/instance/kakao-agent",
		"spiffe://typeclaw.local/typeclaw/ns/agents/instance/kakao-agent/extra",
		"spiffe://typeclaw.local/typeclaw/ns/agents/instance/kakao-agent?x=1",
	} {
		uri, _ := url.Parse(raw)
		if _, err := ParseSPIFFEID(uri, "typeclaw.local"); err == nil {
			t.Fatalf("identity %q must be rejected", raw)
		}
	}
}

func TestBrokerStoresTypedIntentWithoutSecretOrDuplicateOnTicketRetry(t *testing.T) {
	instance := &v1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "kakao-agent", Namespace: "agents"},
		Spec:       v1alpha1.TypeClawInstanceSpec{CredentialPolicy: validPolicy(v1alpha1.CredentialAccessPreAuthorized)},
	}
	kclient := brokerTestClient(t, instance)
	now := time.Unix(1700000000, 0)
	broker := &Broker{
		Reader:       kclient,
		Writer:       kclient,
		TicketSource: bytes.NewReader(bytes.Repeat([]byte{0x11}, 32)),
		TrustDomain:  "typeclaw.local",
		Now:          func() time.Time { return now },
	}
	input := `{"repository":"fml09/typeclaw","title":"hello","body":"world"}`
	response := httptest.NewRecorder()
	broker.ServeHTTP(response, brokerRequest(t, input))
	if response.Code != http.StatusAccepted {
		t.Fatalf("initial status = %d, body=%s", response.Code, response.Body.String())
	}
	var initial brokerResponse
	if err := json.Unmarshal(response.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode initial response: %v", err)
	}
	if initial.Ticket == "" || initial.RequestName == "" {
		t.Fatalf("initial response must contain only opaque ticket metadata: %+v", initial)
	}
	stored := &v1alpha1.CredentialRequest{}
	if err := kclient.Get(t.Context(), types.NamespacedName{Namespace: "agents", Name: initial.RequestName}, stored); err != nil {
		t.Fatalf("request not stored: %v", err)
	}
	if !stored.Spec.ExpiresAt.Time.Equal(time.Unix(1700000300, 0)) {
		t.Fatalf("ticket expiry = %v, want five minutes", stored.Spec.ExpiresAt.Time)
	}
	if stored.Spec.TicketDigest != TicketDigest(initial.Ticket) || stored.Spec.Repository != "fml09/typeclaw" {
		t.Fatalf("stored typed request = %+v", stored.Spec)
	}
	if stored.Spec.SecretBinding != validPolicy(v1alpha1.CredentialAccessPreAuthorized).Secret {
		t.Fatalf("stored Secret binding = %+v", stored.Spec.SecretBinding)
	}
	if stored.Status.Result != nil || stored.Status.ErrorCode != "" {
		t.Fatalf("new request has unexpected result/status: %+v", stored.Status)
	}

	retryInput := `{"repository":"fml09/typeclaw","title":"hello","body":"world","ticket":"` + initial.Ticket + `"}`
	retry := httptest.NewRecorder()
	broker.ServeHTTP(retry, brokerRequest(t, retryInput))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d, body=%s", retry.Code, retry.Body.String())
	}
	var requests v1alpha1.CredentialRequestList
	if err := kclient.List(t.Context(), &requests); err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(requests.Items) != 1 {
		t.Fatalf("ticket retry must not create another request, got %d", len(requests.Items))
	}
	now = now.Add(TicketTTL)
	expired := httptest.NewRecorder()
	broker.ServeHTTP(expired, brokerRequest(t, retryInput))
	if expired.Code != http.StatusGone {
		t.Fatalf("expired ticket status = %d, body=%s", expired.Code, expired.Body.String())
	}
}

func TestBrokerRejectsArbitraryURLAndHeaders(t *testing.T) {
	instance := &v1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "kakao-agent", Namespace: "agents"},
		Spec:       v1alpha1.TypeClawInstanceSpec{CredentialPolicy: validPolicy(v1alpha1.CredentialAccessPreAuthorized)},
	}
	kclient := brokerTestClient(t, instance)
	broker := &Broker{Reader: kclient, Writer: kclient, TicketSource: bytes.NewReader(bytes.Repeat([]byte{0x22}, 32))}
	for _, input := range []string{
		`{"repository":"fml09/typeclaw","title":"hello","url":"https://attacker.example"}`,
		`{"repository":"fml09/typeclaw","title":"hello","headers":{"Authorization":"Bearer x"}}`,
	} {
		response := httptest.NewRecorder()
		broker.ServeHTTP(response, brokerRequest(t, input))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("input %s status = %d, want bad request", input, response.Code)
		}
	}
}
