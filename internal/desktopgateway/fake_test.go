package desktopgateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	testNamespace     = "desktops"
	testName          = "inst-desktop"
	testIssuer        = "https://login.tailscale.com"
	testSubject       = "alice@example.com"
	testAgentToken    = "agent-token-0123456789abcdef"
	testGuestToken    = "guest-token-0123456789abcdef"
	testProxyToken    = "proxy-token-0123456789abcdef"
	testDevToken      = "dev-token-0123456789abcdefghijkl"
	testConsoleOrigin = "https://desktop.example"
)

var (
	vmResource  = schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachines"}
	vmiResource = schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachineinstances"}
)

func vmNotFound() error  { return apierrors.NewNotFound(vmResource, testName) }
func vmiNotFound() error { return apierrors.NewNotFound(vmiResource, testName) }

// fakeKubeVirt is the in-memory KubeVirtClient the handler tests drive. Every
// method is a field so a test states only the answers it cares about; the rest
// behave as an absent desktop.
type fakeKubeVirt struct {
	getVM      func(ctx context.Context, namespace, name string) (*VirtualMachine, error)
	getVMI     func(ctx context.Context, namespace, name string) (*VirtualMachineInstance, error)
	start      func(ctx context.Context, namespace, name string) error
	stop       func(ctx context.Context, namespace, name string) error
	screenshot func(ctx context.Context, namespace, name string) ([]byte, error)
	dialVNC    func(ctx context.Context, namespace, name string) (VNCStream, error)

	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeKubeVirt) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[name]++
}

func (f *fakeKubeVirt) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeKubeVirt) GetVM(ctx context.Context, namespace, name string) (*VirtualMachine, error) {
	f.record("GetVM")
	if f.getVM == nil {
		return nil, vmNotFound()
	}
	return f.getVM(ctx, namespace, name)
}

func (f *fakeKubeVirt) GetVMI(ctx context.Context, namespace, name string) (*VirtualMachineInstance, error) {
	f.record("GetVMI")
	if f.getVMI == nil {
		return nil, vmiNotFound()
	}
	return f.getVMI(ctx, namespace, name)
}

func (f *fakeKubeVirt) Start(ctx context.Context, namespace, name string) error {
	f.record("Start")
	if f.start == nil {
		return nil
	}
	return f.start(ctx, namespace, name)
}

func (f *fakeKubeVirt) Stop(ctx context.Context, namespace, name string) error {
	f.record("Stop")
	if f.stop == nil {
		return nil
	}
	return f.stop(ctx, namespace, name)
}

func (f *fakeKubeVirt) Screenshot(ctx context.Context, namespace, name string) ([]byte, error) {
	f.record("Screenshot")
	if f.screenshot == nil {
		return nil, vmiNotFound()
	}
	return f.screenshot(ctx, namespace, name)
}

func (f *fakeKubeVirt) DialVNC(ctx context.Context, namespace, name string) (VNCStream, error) {
	f.record("DialVNC")
	if f.dialVNC == nil {
		return nil, vmiNotFound()
	}
	return f.dialVNC(ctx, namespace, name)
}

// blockUntilDone is the shape used to prove a handler bounds its own KubeVirt
// call: the client never answers, so only the handler's deadline can end it.
func blockUntilDone[T any](ctx context.Context) (T, error) {
	var zero T
	<-ctx.Done()
	return zero, ctx.Err()
}

func testConfig() Config {
	return Config{
		AgentListenAddress:   "127.0.0.1:0",
		ConsoleListenAddress: "127.0.0.1:0",
		Namespace:            testNamespace,
		Name:                 testName,
		OS:                   "linux",
		OwnerIssuer:          testIssuer,
		OwnerSubject:         testSubject,
		ConsoleAuthMode:      ConsoleAuthTailscale,
		AllowedLogins:        []string{testSubject},
		AuthProxyToken:       testProxyToken,
		DevAccessToken:       testDevToken,
		AgentToken:           testAgentToken,
		GuestAgentToken:      testGuestToken,
		GuestAgentAddress:    "http://desktop-agent.invalid",
		ConsoleURL:           testConsoleOrigin,
		NoVNCDir:             "/opt/novnc",
	}
}

func newTestGateway(t *testing.T, cfg Config, kubevirt KubeVirtClient) *Gateway {
	t.Helper()
	if kubevirt == nil {
		kubevirt = &fakeKubeVirt{}
	}
	g, err := New(cfg, kubevirt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// agentRequest builds a request carrying the plugin bearer, the only
// credential the agent listener accepts.
func agentRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	request.Header.Set("Authorization", "Bearer "+testAgentToken)
	return request
}

// consoleRequest builds a request carrying the identity the Tailscale operator
// proxy asserts. Mutations additionally need a same-origin header.
func consoleRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set(headerForwardedProto, "https")
	request.Header.Set(headerTailscaleLogin, testSubject)
	return request
}

func withOrigin(request *http.Request) *http.Request {
	request.Header.Set("Origin", "https://"+request.Host)
	return request
}
