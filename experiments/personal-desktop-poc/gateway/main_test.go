package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/mock/gomock"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	kvcorev1 "kubevirt.io/client-go/kubevirt/typed/core/v1"
)

func TestDesktopNameUsesCanonicalHMAC(t *testing.T) {
	got := desktopName("kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk", "https://issuer.example", "subject-123", "instance-uid")
	const want = "pd-25c349c305c913d248f8"
	if got != want {
		t.Fatalf("desktopName() = %q, want %q", got, want)
	}
}

func TestAgentTokenUsesCanonicalHMAC(t *testing.T) {
	got := agentToken("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "https://issuer.example", "subject-123", "instance-uid")
	const want = "dd561d00cb72bf4c4c78df18608ab60de1102d86f47d0640bb24d2c54b9a7f2c"
	if got != want {
		t.Fatalf("agentToken() = %q, want %q", got, want)
	}
}

func TestGatewayBootIDIsFreshAndOpaque(t *testing.T) {
	first, err := newGatewayBootID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newGatewayBootID()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || len(second) != 32 {
		t.Fatalf("boot ID lengths = %d and %d, want 32", len(first), len(second))
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("boot ID is not hexadecimal: %q", first)
	}
	if first == second {
		t.Fatal("two Gateway boots received the same ID")
	}
}

func TestRootServesEmbeddedControlUI(t *testing.T) {
	g := gateway{config: config{noVNCDir: t.TempDir()}}
	response := httptest.NewRecorder()
	g.handler().ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	if response.Code != 200 {
		t.Fatalf("GET / status = %d, want 200", response.Code)
	}
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`id="control"`)) {
		t.Fatalf("GET / did not serve the Personal Desktop UI")
	}
}

func TestAgentTokenIsBoundToExactOwnerTuple(t *testing.T) {
	const (
		issuer      = "https://issuer.example"
		subject     = "subject-123"
		instanceUID = "instance-uid"
		tokenKey    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		proxyToken  = "pppppppppppppppppppppppp"
	)
	g := gateway{config: config{
		typeClawInstanceUID: instanceUID,
		ownerHashKey:        "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk",
		agentTokenKey:       tokenKey,
		authProxyToken:      proxyToken,
	}}

	token := agentToken(tokenKey, issuer, subject, instanceUID)
	request := httptest.NewRequest("GET", "/api/me", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Personal-Desktop-Issuer", issuer)
	request.Header.Set("X-Personal-Desktop-Subject", subject)
	got, err := g.authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.actor != actorAgent || got.authMode != "owner-scoped-agent-bearer" {
		t.Fatalf("authenticated identity = %#v", got)
	}

	request.Header.Set("X-Personal-Desktop-Subject", "another-subject")
	if _, err := g.authenticate(request); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("reusing an owner token for another subject: error = %v, want %v", err, errNotAuthenticated)
	}
}

func TestTrustedHeadersRequireProxyCredential(t *testing.T) {
	g := gateway{config: config{
		typeClawInstanceUID: "instance-uid",
		ownerHashKey:        "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk",
		agentTokenKey:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		authProxyToken:      "pppppppppppppppppppppppp",
	}}
	request := httptest.NewRequest("GET", "/api/me", nil)
	request.Header.Set("X-Personal-Desktop-Issuer", "https://issuer.example")
	request.Header.Set("X-Personal-Desktop-Subject", "subject-123")
	if _, err := g.authenticate(request); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("uncredentialed trusted headers: error = %v, want %v", err, errNotAuthenticated)
	}

	request.Header.Set("X-Personal-Desktop-Proxy-Token", g.config.authProxyToken)
	got, err := g.authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.actor != actorHuman || got.authMode != "trusted-oidc-proxy" {
		t.Fatalf("authenticated identity = %#v", got)
	}
}

func TestDevQueryAuthRequiresLoopbackHostAndSecret(t *testing.T) {
	const devToken = "dddddddddddddddddddddddddddddddd"
	g := gateway{config: config{
		typeClawInstanceUID:  "instance-uid",
		ownerHashKey:         "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk",
		agentTokenKey:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		authProxyToken:       "pppppppppppppppppppppppp",
		devAccessToken:       devToken,
		allowInsecureDevAuth: true,
	}}

	request := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/me?issuer=https%3A%2F%2Fissuer.example&subject=subject-123&devToken="+devToken, nil)
	got, err := g.authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.actor != actorHuman || got.authMode != "INSECURE-loopback-query-parameters" {
		t.Fatalf("authenticated identity = %#v", got)
	}

	wrongToken := request.Clone(request.Context())
	wrongToken.URL.RawQuery = "issuer=https%3A%2F%2Fissuer.example&subject=subject-123&devToken=wrong"
	if _, err := g.authenticate(wrongToken); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("wrong dev token: error = %v, want %v", err, errNotAuthenticated)
	}

	serviceHost := request.Clone(request.Context())
	serviceHost.Host = "personal-desktop-gateway.personal-desktop-poc.svc"
	serviceHost.URL.Host = serviceHost.Host
	if _, err := g.authenticate(serviceHost); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("non-loopback dev auth: error = %v, want %v", err, errNotAuthenticated)
	}
}

func TestPowerMutationOriginPolicy(t *testing.T) {
	config := config{
		typeClawInstanceUID:  "instance-uid",
		ownerHashKey:         "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk",
		agentTokenKey:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		authProxyToken:       "pppppppppppppppppppppppp",
		devAccessToken:       "dddddddddddddddddddddddddddddddd",
		allowInsecureDevAuth: true,
	}
	gateway := gateway{config: config}

	tests := []struct {
		name      string
		configure func(*http.Request)
		want      bool
	}{
		{
			name: "trusted proxy rejects hostile origin",
			configure: func(request *http.Request) {
				request.Header.Set("X-Personal-Desktop-Issuer", "https://issuer.example")
				request.Header.Set("X-Personal-Desktop-Subject", "subject-123")
				request.Header.Set("X-Personal-Desktop-Proxy-Token", config.authProxyToken)
				request.Header.Set("Origin", "https://hostile.example")
			},
			want: false,
		},
		{
			name: "insecure query auth rejects hostile origin",
			configure: func(request *http.Request) {
				request.URL.Scheme = "http"
				request.URL.Host = "localhost:8080"
				request.Host = "localhost:8080"
				request.URL.RawQuery = "issuer=https%3A%2F%2Fissuer.example&subject=subject-123&devToken=" + config.devAccessToken
				request.Header.Set("Origin", "https://hostile.example")
			},
			want: false,
		},
		{
			name: "owner bearer permits non-browser request without origin",
			configure: func(request *http.Request) {
				const issuer, subject = "https://issuer.example", "subject-123"
				request.Header.Set("X-Personal-Desktop-Issuer", issuer)
				request.Header.Set("X-Personal-Desktop-Subject", subject)
				request.Header.Set("Authorization", "Bearer "+agentToken(config.agentTokenKey, issuer, subject, config.typeClawInstanceUID))
			},
			want: true,
		},
		{
			name: "owner bearer rejects hostile browser origin",
			configure: func(request *http.Request) {
				const issuer, subject = "https://issuer.example", "subject-123"
				request.Header.Set("X-Personal-Desktop-Issuer", issuer)
				request.Header.Set("X-Personal-Desktop-Subject", subject)
				request.Header.Set("Authorization", "Bearer "+agentToken(config.agentTokenKey, issuer, subject, config.typeClawInstanceUID))
				request.Header.Set("Origin", "https://hostile.example")
			},
			want: false,
		},
		{
			name: "owner bearer permits same-origin browser request",
			configure: func(request *http.Request) {
				const issuer, subject = "https://issuer.example", "subject-123"
				request.Header.Set("X-Personal-Desktop-Issuer", issuer)
				request.Header.Set("X-Personal-Desktop-Subject", subject)
				request.Header.Set("Authorization", "Bearer "+agentToken(config.agentTokenKey, issuer, subject, config.typeClawInstanceUID))
				request.Header.Set("Origin", "https://desktop.example")
			},
			want: true,
		},
		{
			name: "owner bearer permits same-authority in-cluster HTTP origin",
			configure: func(request *http.Request) {
				const issuer, subject = "https://issuer.example", "subject-123"
				request.URL.Scheme = "http"
				request.Host = "gateway.personal-desktop-poc.svc"
				request.Header.Set("X-Personal-Desktop-Issuer", issuer)
				request.Header.Set("X-Personal-Desktop-Subject", subject)
				request.Header.Set("Authorization", "Bearer "+agentToken(config.agentTokenKey, issuer, subject, config.typeClawInstanceUID))
				request.Header.Set("Origin", "http://gateway.personal-desktop-poc.svc")
			},
			want: true,
		},
		{
			name: "same authority over insecure HTTP is not same origin",
			configure: func(request *http.Request) {
				request.Header.Set("X-Personal-Desktop-Issuer", "https://issuer.example")
				request.Header.Set("X-Personal-Desktop-Subject", "subject-123")
				request.Header.Set("X-Personal-Desktop-Proxy-Token", config.authProxyToken)
				request.Header.Set("Origin", "http://desktop.example")
			},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "https://desktop.example/api/power/start", nil)
			test.configure(request)
			id, err := gateway.authenticate(request)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			if got := requireMutationOrigin(response, request, id, config.allowInsecureDevAuth); got != test.want {
				t.Fatalf("requireMutationOrigin() = %v, want %v", got, test.want)
			}
			if !test.want && response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", response.Code)
			}
		})
	}
}

func TestSameOriginAllowsHTTPOnlyForExplicitLoopbackDevMode(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/vnc", nil)
	request.Header.Set("Origin", "http://localhost:8080")
	if sameOrigin(request, false) {
		t.Fatal("loopback HTTP origin was accepted without insecure dev mode")
	}
	if !sameOrigin(request, true) {
		t.Fatal("loopback HTTP origin was rejected in explicit insecure dev mode")
	}

	request = httptest.NewRequest(http.MethodGet, "http://192.0.2.10:8080/api/vnc", nil)
	request.Header.Set("Origin", "http://192.0.2.10:8080")
	if sameOrigin(request, true) {
		t.Fatal("non-loopback HTTP origin was accepted in insecure dev mode")
	}
}

func TestHumanVNCRejectsOriginlessAndNonUpgradeRequestsBeforeTakeover(t *testing.T) {
	const (
		issuer     = "https://issuer.example"
		subject    = "subject-123"
		proxyToken = "pppppppppppppppppppppppp"
	)
	cfg := config{
		typeClawInstanceUID: "instance-uid",
		ownerHashKey:        "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk",
		agentTokenKey:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		authProxyToken:      proxyToken,
	}
	desktop := desktopName(cfg.ownerHashKey, issuer, subject, cfg.typeClawInstanceUID)
	controls := newControlRegistry()
	agent, err := controls.acquire(context.Background(), desktop, actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	defer controls.release(desktop, agent)
	g := gateway{config: cfg, controls: controls}

	request := httptest.NewRequest(http.MethodGet, "https://desktop.example/api/vnc?takeover=1", nil)
	request.Header.Set("X-Personal-Desktop-Issuer", issuer)
	request.Header.Set("X-Personal-Desktop-Subject", subject)
	request.Header.Set("X-Personal-Desktop-Proxy-Token", proxyToken)
	response := httptest.NewRecorder()
	g.handleVNC(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("originless human VNC status = %d, want 403", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "https://desktop.example/api/vnc?takeover=1", nil)
	request.Header.Set("X-Personal-Desktop-Issuer", issuer)
	request.Header.Set("X-Personal-Desktop-Subject", subject)
	request.Header.Set("X-Personal-Desktop-Proxy-Token", proxyToken)
	request.Header.Set("Origin", "https://desktop.example")
	response = httptest.NewRecorder()
	g.handleVNC(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("non-WebSocket human VNC status = %d, want 400", response.Code)
	}

	select {
	case <-agent.ctx.Done():
		t.Fatal("rejected human VNC request canceled the active Agent controller")
	default:
	}
	owner, generation, active := controls.status(desktop)
	if !active || owner != actorAgent || generation != agent.generation {
		t.Fatalf("controller after rejected takeover = (%q, %d, %v), want active Agent generation %d", owner, generation, active, agent.generation)
	}
}

func TestKubeVirtOperationStatusPreservesCallerVisibleSemantics(t *testing.T) {
	resource := schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachines"}
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "missing VM", err: apierrors.NewNotFound(resource, "pd-missing"), want: 404},
		{name: "lifecycle conflict", err: apierrors.NewConflict(resource, "pd-busy", errors.New("busy")), want: 409},
		{name: "upstream failure", err: errors.New("transport failed"), want: 502},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := kubeVirtOperationStatus(test.err); got != test.want {
				t.Fatalf("kubeVirtOperationStatus() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestScreenshotErrorStatusSeparatesRetrySemantics(t *testing.T) {
	resource := schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachineinstances"}
	readCases := []struct {
		err  error
		want int
	}{
		{apierrors.NewNotFound(resource, "pd-a"), 409},
		{apierrors.NewConflict(resource, "pd-a", errors.New("changed")), 409},
		{apierrors.NewServiceUnavailable("temporary"), 503},
		{errors.New("transport"), 502},
	}
	for _, test := range readCases {
		if got := screenshotReadStatus(test.err); got != test.want {
			t.Errorf("screenshotReadStatus(%v) = %d, want %d", test.err, got, test.want)
		}
	}

	transformCases := []struct {
		err  error
		want int
	}{
		{context.DeadlineExceeded, 503},
		{errInvalidScreenshotQuery, 400},
		{errScreenshotCannotFit, 422},
		{errors.New("invalid upstream PNG"), 502},
	}
	for _, test := range transformCases {
		if got := screenshotTransformStatus(test.err); got != test.want {
			t.Errorf("screenshotTransformStatus(%v) = %d, want %d", test.err, got, test.want)
		}
	}

	if _, err := boundedInt("not-an-integer", 10, 1, 20, "example"); !errors.Is(err, errInvalidScreenshotQuery) {
		t.Fatalf("boundedInt invalid input = %v, want invalid-query classification", err)
	}
}

func TestScreenshotAdmissionRejectsInsteadOfQueueingPastTheCap(t *testing.T) {
	g := gateway{screenshotConcurrency: 2}
	if !g.tryAcquireScreenshotSlot() || !g.tryAcquireScreenshotSlot() {
		t.Fatal("screenshot admission rejected below its configured cap")
	}
	if g.tryAcquireScreenshotSlot() {
		t.Fatal("screenshot admission queued or admitted a request above its cap")
	}
	g.releaseScreenshotSlot()
	if !g.tryAcquireScreenshotSlot() {
		t.Fatal("released screenshot capacity was not reusable")
	}
	g.releaseScreenshotSlot()
	g.releaseScreenshotSlot()
}

func TestMeBoundsKubeVirtStatusRead(t *testing.T) {
	const (
		issuer     = "https://issuer.example"
		subject    = "subject-123"
		tokenKey   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		ownerKey   = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
		instanceID = "instance-uid"
		namespace  = "personal-desktop-poc"
	)
	desktop := desktopName(ownerKey, issuer, subject, instanceID)
	ctrl := gomock.NewController(t)
	virt := kubecli.NewMockKubevirtClient(ctrl)
	virtualMachines := kubecli.NewMockVirtualMachineInterface(ctrl)
	virt.EXPECT().VirtualMachine(namespace).Return(virtualMachines)
	virtualMachines.EXPECT().Get(gomock.Any(), desktop, gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ string, _ metav1.GetOptions) (*kubevirtv1.VirtualMachine, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)

	g := gateway{
		config: config{
			namespace:           namespace,
			typeClawInstanceUID: instanceID,
			ownerHashKey:        ownerKey,
			agentTokenKey:       tokenKey,
		},
		virt:          virt,
		controls:      newControlRegistry(),
		statusTimeout: 20 * time.Millisecond,
	}
	request := httptest.NewRequest(http.MethodGet, "https://desktop.example/api/me", nil)
	request.Header.Set("X-Personal-Desktop-Issuer", issuer)
	request.Header.Set("X-Personal-Desktop-Subject", subject)
	request.Header.Set("Authorization", "Bearer "+agentToken(tokenKey, issuer, subject, instanceID))
	response := httptest.NewRecorder()
	started := time.Now()
	g.handleMe(response, request)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded status request took %v, want less than one second", elapsed)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("timed-out status = %d, want 503", response.Code)
	}
}

func TestScreenshotDeadlineReleasesAdmissionSlot(t *testing.T) {
	const (
		issuer     = "https://issuer.example"
		subject    = "subject-123"
		tokenKey   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		ownerKey   = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
		instanceID = "instance-uid"
		namespace  = "personal-desktop-poc"
	)
	desktop := desktopName(ownerKey, issuer, subject, instanceID)
	ctrl := gomock.NewController(t)
	virt := kubecli.NewMockKubevirtClient(ctrl)
	virtualMachineInstances := kubecli.NewMockVirtualMachineInstanceInterface(ctrl)
	virt.EXPECT().VirtualMachineInstance(namespace).Return(virtualMachineInstances)
	virtualMachineInstances.EXPECT().Get(gomock.Any(), desktop, gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ string, _ metav1.GetOptions) (*kubevirtv1.VirtualMachineInstance, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)

	g := gateway{
		config: config{
			namespace:           namespace,
			typeClawInstanceUID: instanceID,
			ownerHashKey:        ownerKey,
			agentTokenKey:       tokenKey,
		},
		virt:                  virt,
		controls:              newControlRegistry(),
		screenshotTimeout:     20 * time.Millisecond,
		screenshotConcurrency: 1,
	}
	request := httptest.NewRequest(http.MethodGet, "https://desktop.example/api/screenshot", nil)
	request.Header.Set("X-Personal-Desktop-Issuer", issuer)
	request.Header.Set("X-Personal-Desktop-Subject", subject)
	request.Header.Set("Authorization", "Bearer "+agentToken(tokenKey, issuer, subject, instanceID))
	response := httptest.NewRecorder()
	started := time.Now()
	g.handleScreenshot(response, request)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded screenshot request took %v, want less than one second", elapsed)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("timed-out screenshot status = %d, want 503", response.Code)
	}
	if !g.tryAcquireScreenshotSlot() {
		t.Fatal("timed-out screenshot did not release its admission slot")
	}
	g.releaseScreenshotSlot()
}

func TestVNCReadinessLookupHasIndependentDeadline(t *testing.T) {
	const (
		issuer     = "https://issuer.example"
		subject    = "subject-123"
		tokenKey   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		ownerKey   = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
		instanceID = "instance-uid"
		namespace  = "personal-desktop-poc"
	)
	desktop := desktopName(ownerKey, issuer, subject, instanceID)
	ctrl := gomock.NewController(t)
	virt := kubecli.NewMockKubevirtClient(ctrl)
	virtualMachineInstances := kubecli.NewMockVirtualMachineInstanceInterface(ctrl)
	virt.EXPECT().VirtualMachineInstance(namespace).Return(virtualMachineInstances)
	virtualMachineInstances.EXPECT().Get(gomock.Any(), desktop, gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ string, _ metav1.GetOptions) (*kubevirtv1.VirtualMachineInstance, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)

	g := gateway{
		config: config{
			namespace:           namespace,
			typeClawInstanceUID: instanceID,
			ownerHashKey:        ownerKey,
			agentTokenKey:       tokenKey,
		},
		virt:                virt,
		controls:            newControlRegistry(),
		vncReadinessTimeout: 20 * time.Millisecond,
	}
	request := httptest.NewRequest(http.MethodGet, "https://desktop.example/api/vnc", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("X-Personal-Desktop-Issuer", issuer)
	request.Header.Set("X-Personal-Desktop-Subject", subject)
	request.Header.Set("Authorization", "Bearer "+agentToken(tokenKey, issuer, subject, instanceID))
	response := httptest.NewRecorder()
	started := time.Now()
	g.handleVNC(response, request)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded VNC readiness request took %v, want less than one second", elapsed)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("timed-out VNC readiness status = %d, want 503", response.Code)
	}
}

type deadlineResponseWriter struct {
	header        http.Header
	status        int
	writeDeadline time.Time
	writeCalled   bool
}

func (w *deadlineResponseWriter) Header() http.Header {
	return w.header
}

func (w *deadlineResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return nil
}

func (w *deadlineResponseWriter) Write(_ []byte) (int, error) {
	w.writeCalled = true
	if delay := time.Until(w.writeDeadline); delay > 0 {
		time.Sleep(delay)
	}
	return 0, context.DeadlineExceeded
}

func TestScreenshotWriteDeadlineReleasesAdmissionSlot(t *testing.T) {
	const (
		issuer     = "https://issuer.example"
		subject    = "subject-123"
		tokenKey   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		ownerKey   = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
		instanceID = "instance-uid"
		namespace  = "personal-desktop-poc"
	)
	desktop := desktopName(ownerKey, issuer, subject, instanceID)
	ctrl := gomock.NewController(t)
	virt := kubecli.NewMockKubevirtClient(ctrl)
	virtualMachineInstances := kubecli.NewMockVirtualMachineInstanceInterface(ctrl)
	virt.EXPECT().VirtualMachineInstance(namespace).Return(virtualMachineInstances).Times(3)
	runningVMI := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{UID: "vmi-a"},
		Status:     kubevirtv1.VirtualMachineInstanceStatus{Phase: kubevirtv1.Running},
	}
	virtualMachineInstances.EXPECT().Get(gomock.Any(), desktop, gomock.Any()).Return(runningVMI, nil).Times(2)
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	virtualMachineInstances.EXPECT().Screenshot(gomock.Any(), desktop, gomock.Any()).Return(raw.Bytes(), nil)

	g := gateway{
		config: config{
			namespace:           namespace,
			typeClawInstanceUID: instanceID,
			ownerHashKey:        ownerKey,
			agentTokenKey:       tokenKey,
		},
		virt:                   virt,
		controls:               newControlRegistry(),
		screenshotWriteTimeout: 20 * time.Millisecond,
		screenshotConcurrency:  1,
	}
	request := httptest.NewRequest(http.MethodGet, "https://desktop.example/api/screenshot", nil)
	request.Header.Set("X-Personal-Desktop-Issuer", issuer)
	request.Header.Set("X-Personal-Desktop-Subject", subject)
	request.Header.Set("Authorization", "Bearer "+agentToken(tokenKey, issuer, subject, instanceID))
	response := &deadlineResponseWriter{header: make(http.Header)}
	started := time.Now()
	g.handleScreenshot(response, request)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded screenshot write took %v, want less than one second", elapsed)
	}
	if response.status != http.StatusOK || !response.writeCalled {
		t.Fatalf("screenshot response = status %d, writeCalled %v", response.status, response.writeCalled)
	}
	if !g.tryAcquireScreenshotSlot() {
		t.Fatal("timed-out screenshot write did not release its admission slot")
	}
	g.releaseScreenshotSlot()
}

type statusCodeError int

func (e statusCodeError) Error() string      { return "status error" }
func (e statusCodeError) GetStatusCode() int { return int(e) }

func TestVNCStreamHTTPStatusPreservesReadinessAndAuthorization(t *testing.T) {
	cases := []struct {
		upstream int
		want     int
	}{
		{upstream: 400, want: 409},
		{upstream: 404, want: 409},
		{upstream: 401, want: 502},
		{upstream: 403, want: 502},
		{upstream: 503, want: 503},
		{upstream: 500, want: 502},
	}
	for _, test := range cases {
		if got := vncStreamHTTPStatus(statusCodeError(test.upstream)); got != test.want {
			t.Errorf("vncStreamHTTPStatus(%d) = %d, want %d", test.upstream, got, test.want)
		}
	}
}

func TestHumanTakeoverWaitsForAgentRelease(t *testing.T) {
	registry := newControlRegistry()
	agent, err := registry.acquire(context.Background(), "pd-a", actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		<-agent.ctx.Done()
		registry.release("pd-a", agent)
		close(released)
	}()

	human, err := registry.acquire(context.Background(), "pd-a", actorHuman, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("human grant activated before the agent grant was released")
	}
	if human.generation <= agent.generation {
		t.Fatalf("generation did not increase: agent=%d human=%d", agent.generation, human.generation)
	}
	if _, err := registry.acquire(context.Background(), "pd-a", actorHuman, true); !errors.Is(err, errControlBusy) {
		t.Fatalf("second human acquire error = %v, want %v", err, errControlBusy)
	}
	registry.release("pd-a", human)
}

func TestHumanTakeoverReservationBlocksInterveningAgent(t *testing.T) {
	registry := newControlRegistry()
	agent, err := registry.acquire(context.Background(), "pd-a", actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	type acquireResult struct {
		controller *controller
		err        error
	}
	humanResult := make(chan acquireResult, 1)
	go func() {
		controller, acquireErr := registry.acquire(context.Background(), "pd-a", actorHuman, true)
		humanResult <- acquireResult{controller: controller, err: acquireErr}
	}()

	deadline := time.Now().Add(time.Second)
	for {
		registry.mu.Lock()
		reserved := registry.takeovers["pd-a"] != nil
		registry.mu.Unlock()
		if reserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("human takeover did not reserve the desktop")
		}
		time.Sleep(time.Millisecond)
	}

	// Reproduce the exact release window: active is empty, the original waiter
	// has not observed done yet, and the takeover reservation remains installed.
	registry.mu.Lock()
	delete(registry.active, "pd-a")
	registry.mu.Unlock()
	if _, err := registry.acquire(context.Background(), "pd-a", actorAgent, false); !errors.Is(err, errControlBusy) {
		t.Fatalf("intervening agent acquire = %v, want %v", err, errControlBusy)
	}
	agent.closeDone()

	select {
	case result := <-humanResult:
		if result.err != nil {
			t.Fatal(result.err)
		}
		registry.release("pd-a", result.controller)
	case <-time.After(time.Second):
		t.Fatal("reserved human takeover did not complete")
	}
}

func TestCanceledTakeoverCannotWinAgainstSimultaneousRelease(t *testing.T) {
	registry := newControlRegistry()
	agent, err := registry.acquire(context.Background(), "pd-a", actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, acquireErr := registry.acquire(ctx, "pd-a", actorHuman, true)
		result <- acquireErr
	}()

	deadline := time.Now().Add(time.Second)
	for {
		registry.mu.Lock()
		reserved := registry.takeovers["pd-a"] != nil
		registry.mu.Unlock()
		if reserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("human takeover did not reserve the desktop")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	registry.release("pd-a", agent)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("takeover error = %v, want context canceled", err)
	}
	actor, _, active := registry.status("pd-a")
	if active {
		t.Fatalf("canceled takeover left active controller %q", actor)
	}
}

func TestPowerReservationBlocksControlUntilStartSucceeds(t *testing.T) {
	registry := newControlRegistry()
	failedStop, err := registry.beginPower("pd-rollback", "stop")
	if err != nil {
		t.Fatal(err)
	}
	registry.finishPower("pd-rollback", failedStop, powerRejected)
	rollbackController, err := registry.acquire(context.Background(), "pd-rollback", actorAgent, false)
	if err != nil {
		t.Fatalf("failed stop did not roll back control block: %v", err)
	}
	registry.release("pd-rollback", rollbackController)

	stop, err := registry.beginPower("pd-a", "stop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.acquire(context.Background(), "pd-a", actorAgent, false); !errors.Is(err, errControlBlocked) {
		t.Fatalf("acquire during stop = %v, want %v", err, errControlBlocked)
	}
	registry.finishPower("pd-a", stop, powerSucceeded)
	if _, err := registry.acquire(context.Background(), "pd-a", actorAgent, false); !errors.Is(err, errControlBlocked) {
		t.Fatalf("acquire after successful stop = %v, want %v", err, errControlBlocked)
	}

	start, err := registry.beginPower("pd-a", "start")
	if err != nil {
		t.Fatal(err)
	}
	registry.finishPower("pd-a", start, powerRejected)
	if _, err := registry.acquire(context.Background(), "pd-a", actorAgent, false); !errors.Is(err, errControlBlocked) {
		t.Fatalf("acquire after failed start = %v, want %v", err, errControlBlocked)
	}

	start, err = registry.beginPower("pd-a", "start")
	if err != nil {
		t.Fatal(err)
	}
	registry.finishPower("pd-a", start, powerSucceeded)
	controller, err := registry.acquire(context.Background(), "pd-a", actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.beginPower("pd-a", "stop"); !errors.Is(err, errControlBusy) {
		t.Fatalf("stop with active control = %v, want %v", err, errControlBusy)
	}
	registry.release("pd-a", controller)
}

func TestDrainRejectsNewControlAndPower(t *testing.T) {
	registry := newControlRegistry()
	registry.revokeAll(context.Background())
	if _, err := registry.acquire(context.Background(), "pd-new", actorAgent, false); !errors.Is(err, errControlBlocked) {
		t.Fatalf("acquire while draining = %v, want %v", err, errControlBlocked)
	}
	if _, err := registry.beginPower("pd-new", "start"); !errors.Is(err, errControlBlocked) {
		t.Fatalf("power while draining = %v, want %v", err, errControlBlocked)
	}
}

func TestAmbiguousStopKeepsControlBlockedUntilExplicitStartSucceeds(t *testing.T) {
	registry := newControlRegistry()
	stop, err := registry.beginPower("pd-a", "stop")
	if err != nil {
		t.Fatal(err)
	}
	registry.finishPower("pd-a", stop, powerUnknown)
	if _, err := registry.acquire(context.Background(), "pd-a", actorAgent, false); !errors.Is(err, errControlBlocked) {
		t.Fatalf("acquire after ambiguous stop = %v, want %v", err, errControlBlocked)
	}
	if _, err := registry.beginPower("pd-a", "stop"); !errors.Is(err, errPowerRecoveryRequired) {
		t.Fatalf("second stop after ambiguous stop = %v, want %v", err, errPowerRecoveryRequired)
	}
	blocked, active, action := registry.powerStatus("pd-a")
	if !blocked || active || action != "" {
		t.Fatalf("power status after ambiguous stop = (%v, %v, %q), want blocked recovery state", blocked, active, action)
	}

	start, err := registry.beginPower("pd-a", "start")
	if err != nil {
		t.Fatal(err)
	}
	registry.finishPower("pd-a", start, powerSucceeded)
	controller, err := registry.acquire(context.Background(), "pd-a", actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	registry.release("pd-a", controller)

	resource := schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachines"}
	if !definitivePowerRejection(apierrors.NewNotFound(resource, "pd-a")) {
		t.Fatal("NotFound must be a definitive rejection")
	}
	if definitivePowerRejection(apierrors.NewTimeoutError("lost ACK", 1)) {
		t.Fatal("timeout must retain UnknownOutcome semantics")
	}
}

func TestStartConflictIsIdempotentOnlyForStableRunningVM(t *testing.T) {
	vm := &kubevirtv1.VirtualMachine{}
	vm.Status.PrintableStatus = kubevirtv1.VirtualMachineStatusRunning
	vmi := &kubevirtv1.VirtualMachineInstance{}
	vmi.Status.Phase = kubevirtv1.Running
	if !stableRunningAfterStartConflict(vm, vmi) {
		t.Fatal("stable Running VM/VMI was not recognized as an idempotent start")
	}

	vm.Status.StateChangeRequests = []kubevirtv1.VirtualMachineStateChangeRequest{
		{Action: kubevirtv1.StopRequest},
	}
	if stableRunningAfterStartConflict(vm, vmi) {
		t.Fatal("Running VMI with a pending Stop request was treated as stable")
	}
	vm.Status.StateChangeRequests = nil
	now := metav1.Now()
	vmi.DeletionTimestamp = &now
	if stableRunningAfterStartConflict(vm, vmi) {
		t.Fatal("deleting Running VMI was treated as stable")
	}
}

func TestPendingStopConflictCannotClearPowerQuarantine(t *testing.T) {
	const (
		issuer     = "https://issuer.example"
		subject    = "subject-123"
		tokenKey   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		ownerKey   = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
		instanceID = "instance-uid"
		namespace  = "personal-desktop-poc"
	)
	desktop := desktopName(ownerKey, issuer, subject, instanceID)
	ctrl := gomock.NewController(t)
	virt := kubecli.NewMockKubevirtClient(ctrl)
	virtualMachines := kubecli.NewMockVirtualMachineInterface(ctrl)
	virtualMachineInstances := kubecli.NewMockVirtualMachineInstanceInterface(ctrl)
	virt.EXPECT().VirtualMachine(namespace).Return(virtualMachines).Times(2)
	virtualMachines.EXPECT().Start(gomock.Any(), desktop, gomock.Any()).Return(
		apierrors.NewConflict(
			schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachines"},
			desktop,
			errors.New("VM is already running"),
		),
	)
	virtualMachines.EXPECT().Get(gomock.Any(), desktop, gomock.Any()).Return(
		&kubevirtv1.VirtualMachine{Status: kubevirtv1.VirtualMachineStatus{
			PrintableStatus: kubevirtv1.VirtualMachineStatusRunning,
			StateChangeRequests: []kubevirtv1.VirtualMachineStateChangeRequest{
				{Action: kubevirtv1.StopRequest},
			},
		}},
		nil,
	)
	virt.EXPECT().VirtualMachineInstance(namespace).Return(virtualMachineInstances)
	virtualMachineInstances.EXPECT().Get(gomock.Any(), desktop, gomock.Any()).Return(
		&kubevirtv1.VirtualMachineInstance{Status: kubevirtv1.VirtualMachineInstanceStatus{
			Phase: kubevirtv1.Running,
		}},
		nil,
	)

	controls := newControlRegistry()
	stop, err := controls.beginPower(desktop, "stop")
	if err != nil {
		t.Fatal(err)
	}
	controls.finishPower(desktop, stop, powerUnknown)
	g := gateway{
		config: config{
			namespace:           namespace,
			typeClawInstanceUID: instanceID,
			ownerHashKey:        ownerKey,
			agentTokenKey:       tokenKey,
		},
		virt:     virt,
		controls: controls,
	}
	request := httptest.NewRequest(http.MethodPost, "https://desktop.example/api/power/start", nil)
	request.SetPathValue("action", "start")
	request.Header.Set("X-Personal-Desktop-Issuer", issuer)
	request.Header.Set("X-Personal-Desktop-Subject", subject)
	request.Header.Set("Authorization", "Bearer "+agentToken(tokenKey, issuer, subject, instanceID))
	response := httptest.NewRecorder()
	g.handlePower(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("pending-stop start status = %d, want 503 UnknownOutcome", response.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["outcome"] != "UnknownOutcome" || body["controlBlocked"] != true {
		t.Fatalf("pending-stop start response = %#v", body)
	}
	blocked, active, _ := controls.powerStatus(desktop)
	if !blocked || active {
		t.Fatalf("pending-stop start power status = (%v, %v), want quarantined", blocked, active)
	}
}

func TestPowerHandlerBoundsKubeVirtCallAndReleasesReservation(t *testing.T) {
	const (
		issuer     = "https://issuer.example"
		subject    = "subject-123"
		tokenKey   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		ownerKey   = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
		instanceID = "instance-uid"
		namespace  = "personal-desktop-poc"
	)
	ctrl := gomock.NewController(t)
	virt := kubecli.NewMockKubevirtClient(ctrl)
	virtualMachines := kubecli.NewMockVirtualMachineInterface(ctrl)
	virt.EXPECT().VirtualMachine(namespace).Return(virtualMachines)
	virtualMachines.EXPECT().Stop(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ string, _ *kubevirtv1.StopOptions) error {
			<-ctx.Done()
			return ctx.Err()
		},
	)

	controls := newControlRegistry()
	g := gateway{
		config: config{
			namespace:           namespace,
			typeClawInstanceUID: instanceID,
			ownerHashKey:        ownerKey,
			agentTokenKey:       tokenKey,
		},
		virt:         virt,
		controls:     controls,
		powerTimeout: 20 * time.Millisecond,
	}
	request := httptest.NewRequest(http.MethodPost, "https://desktop.example/api/power/stop", nil)
	request.SetPathValue("action", "stop")
	request.Header.Set("X-Personal-Desktop-Issuer", issuer)
	request.Header.Set("X-Personal-Desktop-Subject", subject)
	request.Header.Set("Authorization", "Bearer "+agentToken(tokenKey, issuer, subject, instanceID))
	response := httptest.NewRecorder()
	started := time.Now()
	g.handlePower(response, request)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded power request took %v, want less than one second", elapsed)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("timed-out power status = %d, want 503", response.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["outcome"] != "UnknownOutcome" || body["retrySafe"] != false || body["controlBlocked"] != true {
		t.Fatalf("timed-out power response = %#v", body)
	}

	desktop := desktopName(ownerKey, issuer, subject, instanceID)
	blocked, active, _ := controls.powerStatus(desktop)
	if !blocked || active {
		t.Fatalf("power status after timeout = (%v, %v), want blocked without active reservation", blocked, active)
	}
	if _, err := controls.beginPower(desktop, "stop"); !errors.Is(err, errPowerRecoveryRequired) {
		t.Fatalf("stop after timeout = %v, want %v", err, errPowerRecoveryRequired)
	}
	start, err := controls.beginPower(desktop, "start")
	if err != nil {
		t.Fatalf("explicit start recovery was not allowed: %v", err)
	}
	controls.finishPower(desktop, start, powerSucceeded)
}

type closeBlockingStream struct {
	upstream net.Conn
	peer     net.Conn
	started  chan struct{}
}

func newCloseBlockingStream() *closeBlockingStream {
	upstream, peer := net.Pipe()
	return &closeBlockingStream{upstream: upstream, peer: peer, started: make(chan struct{})}
}

func (s *closeBlockingStream) AsConn() net.Conn { return s.upstream }

func (s *closeBlockingStream) Stream(kvcorev1.StreamOptions) error {
	close(s.started)
	buffer := make([]byte, 1)
	_, err := s.peer.Read(buffer)
	_ = s.peer.Close()
	return err
}

func TestRelayCancellationClosesBlockedKubeVirtStream(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		accepted <- connection
	}))
	defer server.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	downstream := <-accepted

	stream := newCloseBlockingStream()
	ctx, cancel := context.WithCancel(context.Background())
	granted := &controller{ctx: ctx}
	result := make(chan error, 1)
	go func() { result <- relayVNC(downstream, stream, granted) }()

	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("relay did not enter the KubeVirt stream")
	}
	cancel()
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("relay stayed blocked after control cancellation")
	}
}

func TestOpenVNCHandshakeHonorsContext(t *testing.T) {
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := openKubeVirtVNC(ctx, &rest.Config{Host: server.URL}, "personal-desktop-poc", "pd-a")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("openKubeVirtVNC() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled VNC handshake took %v", elapsed)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("upstream VNC request remained stuck after context cancellation")
	}
}

func TestJPEGTransformPreservesFramebufferGeometryAndByteCap(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 1600, 900))
	for y := 0; y < source.Bounds().Dy(); y++ {
		for x := 0; x < source.Bounds().Dx(); x++ {
			source.SetRGBA(x, y, color.RGBA{R: uint8(x % 251), G: uint8(y % 241), B: uint8((x + y) % 239), A: 255})
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, source); err != nil {
		t.Fatal(err)
	}

	encoded, contentType, frameWidth, frameHeight, encodedWidth, encodedHeight, err := transformScreenshot(context.Background(), input.Bytes(), url.Values{
		"format":   {"jpeg"},
		"maxWidth": {"800"},
		"maxBytes": {"100000"},
		"quality":  {"70"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "image/jpeg" || frameWidth != 1600 || frameHeight != 900 {
		t.Fatalf("metadata = (%q, %dx%d), want image/jpeg and 1600x900", contentType, frameWidth, frameHeight)
	}
	if encodedWidth > 800 || encodedHeight <= 0 || len(encoded) > 100000 {
		t.Fatalf("encoded = %dx%d, %d bytes", encodedWidth, encodedHeight, len(encoded))
	}
}

func TestScreenshotTransformRejectsFrameOutsideProcessingLimits(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, maxFramebufferDimension+1, 1))
	var input bytes.Buffer
	if err := png.Encode(&input, source); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _, _, err := transformScreenshot(context.Background(), input.Bytes(), nil)
	if !errors.Is(err, errScreenshotTooLarge) {
		t.Fatalf("oversized screenshot error = %v, want %v", err, errScreenshotTooLarge)
	}
}

func TestScreenshotResizeHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := nearestNeighbor(ctx, image.NewRGBA(image.Rect(0, 0, 100, 100)), 100, 100)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resize error = %v, want context canceled", err)
	}
}
