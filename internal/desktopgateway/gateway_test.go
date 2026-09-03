package desktopgateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

func TestConsoleRootServesTheEmbeddedUI(t *testing.T) {
	cfg := testConfig()
	cfg.NoVNCDir = t.TempDir()
	g := newTestGateway(t, cfg, nil)
	response := httptest.NewRecorder()
	g.ConsoleHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://desktop.example/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", response.Code)
	}
	body := response.Body.Bytes()
	for _, id := range []string{`id="control"`, `id="takeover"`, `id="release"`, `id="start"`,
		`id="stop"`, `id="view"`, `id="status"`, `id="fallback"`, `id="screen"`, `id="dev-warning"`} {
		if !bytes.Contains(body, []byte(id)) {
			t.Fatalf("the console page is missing %s", id)
		}
	}
	if header := response.Header().Get("Content-Security-Policy"); header == "" {
		t.Fatal("the console page was served without a Content-Security-Policy")
	}
}

// The console page is not served on the agent listener: only /healthz is
// reachable there without a credential.
func TestAgentListenerServesOnlyHealthzUnauthenticated(t *testing.T) {
	g := newTestGateway(t, testConfig(), nil)
	handler := g.AgentHandler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://gateway.svc/healthz", nil))
	if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
		t.Fatalf("healthz = %d %q", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://gateway.svc/", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("agent listener root status = %d, want 404", response.Code)
	}
}

func TestMeReportsDesktopStateForBothListeners(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		getVM:  func(context.Context, string, string) (*VirtualMachine, error) { return runningVM(), nil },
		getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
	}
	cfg := testConfig()
	cfg.AgentLeaseTTL = 90 * time.Second
	g := newTestGateway(t, cfg, kubevirt)

	agent := httptest.NewRecorder()
	g.AgentHandler().ServeHTTP(agent, agentRequest(t, http.MethodGet, "https://gateway.svc/api/me", nil))
	if agent.Code != http.StatusOK {
		t.Fatalf("agent /api/me = %d, body %s", agent.Code, agent.Body.String())
	}
	body := decodeBody(t, agent)
	for field, want := range map[string]any{
		"desktopName":          testName,
		"namespace":            testNamespace,
		"os":                   "linux",
		"actor":                "agent",
		"authMode":             authModeAgentBearer,
		"consoleURL":           testConsoleOrigin,
		"vmExists":             true,
		"vmPrintableStatus":    VirtualMachineStatusRunning,
		"vmiExists":            true,
		"vmiPhase":             VirtualMachineInstanceRunning,
		"vmiUID":               "vmi-a",
		"controlActive":        false,
		"controlBlocked":       false,
		"powerOperationActive": false,
		"agentLeaseTtlSeconds": float64(90),
	} {
		if body[field] != want {
			t.Errorf("agent /api/me %s = %#v, want %#v", field, body[field], want)
		}
	}
	if body["gatewayBootID"] != g.bootID {
		t.Errorf("gatewayBootID = %v", body["gatewayBootID"])
	}
	if _, present := body["login"]; present {
		t.Error("the agent identity reported a human login")
	}

	console := httptest.NewRecorder()
	g.ConsoleHandler().ServeHTTP(console, consoleRequest(t, http.MethodGet, "https://desktop.example/api/me"))
	if console.Code != http.StatusOK {
		t.Fatalf("console /api/me = %d, body %s", console.Code, console.Body.String())
	}
	body = decodeBody(t, console)
	if body["actor"] != "human" || body["authMode"] != authModeTailscale || body["login"] != testSubject {
		t.Fatalf("console /api/me identity = %#v", body)
	}
}

func TestMeReportsQuarantineAndController(t *testing.T) {
	g := newTestGateway(t, testConfig(), &fakeKubeVirt{})
	if _, err := g.controls.acquireAgent(testName, time.Minute); err != nil {
		t.Fatal(err)
	}
	stop, err := g.controls.beginPower("other-desktop", "stop")
	if err != nil {
		t.Fatal(err)
	}
	g.controls.finishPower("other-desktop", stop, powerUnknown)

	response := httptest.NewRecorder()
	g.AgentHandler().ServeHTTP(response, agentRequest(t, http.MethodGet, "https://gateway.svc/api/me", nil))
	body := decodeBody(t, response)
	if body["vmExists"] != false || body["vmiExists"] != false {
		t.Fatalf("absent desktop reported as existing: %#v", body)
	}
	if body["controlActive"] != true || body["controlActor"] != "agent" {
		t.Fatalf("controller = %#v", body)
	}
	if body["controlBlocked"] != false || body["powerRecoveryRequired"] != false {
		t.Fatalf("another desktop's quarantine leaked: %#v", body)
	}
}

func TestMeBoundsKubeVirtStatusRead(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		getVM: func(ctx context.Context, _, _ string) (*VirtualMachine, error) {
			return blockUntilDone[*VirtualMachine](ctx)
		},
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	g.statusTimeout = 20 * time.Millisecond

	response := httptest.NewRecorder()
	started := time.Now()
	g.AgentHandler().ServeHTTP(response, agentRequest(t, http.MethodGet, "https://gateway.svc/api/me", nil))
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded status request took %v, want less than one second", elapsed)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("timed-out status = %d, want 503", response.Code)
	}
}

func TestConsoleScreenshotPublishesFramebufferGeometry(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		getVMI:     func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
		screenshot: func(context.Context, string, string) ([]byte, error) { return encodePNG(t, 1280, 800), nil },
	}
	g := newTestGateway(t, testConfig(), kubevirt)

	response := httptest.NewRecorder()
	g.ConsoleHandler().ServeHTTP(response, consoleRequest(t, http.MethodGet,
		"https://desktop.example/api/screenshot?format=jpeg&maxWidth=640&maxBytes=200000&quality=60"))
	if response.Code != http.StatusOK {
		t.Fatalf("console screenshot status = %d, body %s", response.Code, response.Body.String())
	}
	header := response.Header()
	if header.Get("Content-Type") != "image/jpeg" ||
		header.Get("X-Framebuffer-Width") != "1280" || header.Get("X-Framebuffer-Height") != "800" ||
		header.Get("X-Encoded-Width") != "640" || header.Get("X-VMI-UID") != "vmi-a" ||
		header.Get("X-Gateway-Boot-ID") != g.bootID || header.Get("X-Control-Generation") != "0" {
		t.Fatalf("screenshot headers = %#v", header)
	}
}

// A frame captured across a VMI replacement pictures a machine the viewer is
// not looking at, so it must be refused rather than shown.
func TestConsoleScreenshotRejectsVMIReplacementDuringCapture(t *testing.T) {
	var reads atomic.Int32
	kubevirt := &fakeKubeVirt{
		getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) {
			if reads.Add(1) == 1 {
				return &VirtualMachineInstance{UID: "vmi-a", Phase: VirtualMachineInstanceRunning}, nil
			}
			return &VirtualMachineInstance{UID: "vmi-b", Phase: VirtualMachineInstanceRunning}, nil
		},
		screenshot: func(context.Context, string, string) ([]byte, error) { return encodePNG(t, 64, 64), nil },
	}
	g := newTestGateway(t, testConfig(), kubevirt)

	response := httptest.NewRecorder()
	g.ConsoleHandler().ServeHTTP(response, consoleRequest(t, http.MethodGet, "https://desktop.example/api/screenshot"))
	if response.Code != http.StatusConflict {
		t.Fatalf("replaced-VMI screenshot status = %d, want 409", response.Code)
	}
}

func TestConsoleScreenshotRefusesAnUnreadyDesktop(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) {
			return &VirtualMachineInstance{UID: "vmi-a", Phase: "Scheduling"}, nil
		},
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	response := httptest.NewRecorder()
	g.ConsoleHandler().ServeHTTP(response, consoleRequest(t, http.MethodGet, "https://desktop.example/api/screenshot"))
	if response.Code != http.StatusConflict {
		t.Fatalf("unready screenshot status = %d, want 409", response.Code)
	}
}

func TestScreenshotDeadlineReleasesAdmissionSlot(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		getVMI: func(ctx context.Context, _, _ string) (*VirtualMachineInstance, error) {
			return blockUntilDone[*VirtualMachineInstance](ctx)
		},
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	g.screenshotTimeout = 20 * time.Millisecond
	g.screenshotConcurrency = 1

	response := httptest.NewRecorder()
	started := time.Now()
	g.ConsoleHandler().ServeHTTP(response, consoleRequest(t, http.MethodGet, "https://desktop.example/api/screenshot"))
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

func TestScreenshotAdmissionCapAnswersTooManyRequests(t *testing.T) {
	g := newTestGateway(t, testConfig(), &fakeKubeVirt{})
	g.screenshotConcurrency = 1
	if !g.tryAcquireScreenshotSlot() {
		t.Fatal("the first admission slot was refused")
	}
	defer g.releaseScreenshotSlot()

	response := httptest.NewRecorder()
	g.ConsoleHandler().ServeHTTP(response, consoleRequest(t, http.MethodGet, "https://desktop.example/api/screenshot"))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("saturated screenshot status = %d, Retry-After %q", response.Code, response.Header().Get("Retry-After"))
	}
}

// deadlineResponseWriter fails its write exactly when the response deadline
// the handler installed elapses.
type deadlineResponseWriter struct {
	header        http.Header
	status        int
	writeDeadline time.Time
	writeCalled   bool
}

func (w *deadlineResponseWriter) Header() http.Header { return w.header }

func (w *deadlineResponseWriter) WriteHeader(status int) { w.status = status }

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
	kubevirt := &fakeKubeVirt{
		getVMI:     func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
		screenshot: func(context.Context, string, string) ([]byte, error) { return encodePNG(t, 2, 2), nil },
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	g.screenshotWriteTimeout = 20 * time.Millisecond
	g.screenshotConcurrency = 1

	response := &deadlineResponseWriter{header: make(http.Header)}
	started := time.Now()
	g.handleScreenshot(response, consoleRequest(t, http.MethodGet, "https://desktop.example/api/screenshot"))
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

func TestAgentControlAcquireReleaseAndOwnership(t *testing.T) {
	cfg := testConfig()
	cfg.AgentLeaseTTL = time.Minute
	g := newTestGateway(t, cfg, &fakeKubeVirt{})
	handler := g.AgentHandler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/control/acquire", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("agent acquire status = %d, body %s", response.Code, response.Body.String())
	}
	var granted struct {
		ControlGeneration uint64 `json:"controlGeneration"`
		GatewayBootID     string `json:"gatewayBootID"`
		LeaseTTLSeconds   int    `json:"leaseTtlSeconds"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &granted); err != nil {
		t.Fatal(err)
	}
	if granted.ControlGeneration == 0 || granted.GatewayBootID == "" || granted.LeaseTTLSeconds != 60 {
		t.Fatalf("acquire response = %s", response.Body.String())
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/control/acquire", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("double acquire status = %d, want 409", response.Code)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/control/release", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("agent release status = %d, body %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/control/acquire", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("re-acquire after release status = %d", response.Code)
	}
}

func TestAgentReleaseRefusesToDropAHumanController(t *testing.T) {
	g := newTestGateway(t, testConfig(), &fakeKubeVirt{})
	human, err := g.controls.acquire(context.Background(), testName, actorHuman, false)
	if err != nil {
		t.Fatal(err)
	}
	defer g.controls.release(testName, human)

	response := httptest.NewRecorder()
	g.AgentHandler().ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/control/release", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("agent release over a human controller = %d, want 409", response.Code)
	}
}

func TestAgentActionRequiresAcquiredControl(t *testing.T) {
	g := newTestGateway(t, testConfig(), &fakeKubeVirt{})
	handler := g.AgentHandler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/agent/actions",
		bytes.NewBufferString(`{"actions":[{"type":"click","x":1,"y":2,"button":"left"}]}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("action batch without acquire status = %d, want 409", response.Code)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/agent/click",
		bytes.NewBufferString(`{}`)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown agent action status = %d, want 404", response.Code)
	}
}

func TestAgentActionForwardsToTheGuestAgentWithTheConfiguredBearer(t *testing.T) {
	var gotAuthorization, gotPath, gotBody string
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"outcome":"Applied"}`))
	}))
	defer guest.Close()

	cfg := testConfig()
	cfg.GuestAgentAddress = guest.URL
	cfg.AgentLeaseTTL = time.Minute
	g := newTestGateway(t, cfg, &fakeKubeVirt{})
	handler := g.AgentHandler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/control/acquire", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("acquire status = %d", response.Code)
	}

	const payload = `{"actions":[{"type":"click","x":10,"y":20,"button":"left","clicks":1},{"type":"type","text":"hello"}]}`
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/agent/actions",
		bytes.NewBufferString(payload)))
	if response.Code != http.StatusOK {
		t.Fatalf("action batch status = %d, body %s", response.Code, response.Body.String())
	}
	if gotPath != "/actions" || gotBody != payload {
		t.Fatalf("guest agent saw path %q body %q", gotPath, gotBody)
	}
	if gotAuthorization != "Bearer "+testGuestToken {
		t.Fatalf("guest agent saw Authorization %q, want the configured guest token", gotAuthorization)
	}
}

func TestAgentActionReportsUnknownOutcomeWhenTheGuestIsUnreachable(t *testing.T) {
	cfg := testConfig()
	cfg.GuestAgentAddress = "http://127.0.0.1:1"
	cfg.AgentLeaseTTL = time.Minute
	g := newTestGateway(t, cfg, &fakeKubeVirt{})
	handler := g.AgentHandler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/control/acquire", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("acquire status = %d", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, agentRequest(t, http.MethodPost, "https://gateway.svc/api/agent/actions",
		bytes.NewBufferString(`{"actions":[{"type":"click","x":1,"y":2,"button":"left"}]}`)))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("action batch status = %d, want 502", response.Code)
	}
	if decodeBody(t, response)["outcome"] != outcomeUnknown {
		t.Fatalf("action batch body = %s", response.Body.String())
	}
}

func TestAgentScreenshotProxiesTheGuestFrame(t *testing.T) {
	raw := encodePNG(t, 1600, 900)
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/screenshot" {
			t.Errorf("guest agent saw path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(raw)
	}))
	defer guest.Close()

	cfg := testConfig()
	cfg.GuestAgentAddress = guest.URL
	kubevirt := &fakeKubeVirt{
		getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
	}
	g := newTestGateway(t, cfg, kubevirt)

	response := httptest.NewRecorder()
	g.AgentHandler().ServeHTTP(response, agentRequest(t, http.MethodPost,
		"https://gateway.svc/api/agent/screenshot?format=jpeg&maxWidth=1024&maxBytes=200000&quality=65", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("agent screenshot status = %d, body %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "image/jpeg" {
		t.Fatalf("agent screenshot content type = %q", contentType)
	}
	if width := response.Header().Get("X-Framebuffer-Width"); width != "1600" {
		t.Fatalf("agent screenshot framebuffer width = %q, want 1600", width)
	}
	if vmiUID := response.Header().Get("X-VMI-UID"); vmiUID != "vmi-a" {
		t.Fatalf("agent screenshot VMI UID = %q", vmiUID)
	}
}

func TestAgentWindowsRefreshesTheLease(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"windows":[]}`))
	}))
	defer guest.Close()

	cfg := testConfig()
	cfg.GuestAgentAddress = guest.URL
	g := newTestGateway(t, cfg, &fakeKubeVirt{})
	granted, err := g.controls.acquireAgent(testName, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer g.controls.release(testName, granted)
	before := granted.lastTouch.Load()

	response := httptest.NewRecorder()
	g.AgentHandler().ServeHTTP(response, agentRequest(t, http.MethodGet, "https://gateway.svc/api/agent/windows", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("windows status = %d, body %s", response.Code, response.Body.String())
	}
	if granted.lastTouch.Load() <= before {
		t.Fatal("a view-only agent call did not refresh the lease")
	}
}

// A guest frame larger than the processing limit must be reported as oversized.
// Reading exactly the limit would truncate it into a decodable PNG and serve it
// as a whole screenshot, which the model has no way to recognize as partial.
func TestAgentScreenshotRefusesAnOversizedGuestFrame(t *testing.T) {
	oversized := bytes.Repeat([]byte("p"), maxScreenshotRawBytes+8)
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(oversized)
	}))
	defer guest.Close()

	cfg := testConfig()
	cfg.GuestAgentAddress = guest.URL
	kubevirt := &fakeKubeVirt{
		getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
	}
	g := newTestGateway(t, cfg, kubevirt)

	response := httptest.NewRecorder()
	g.AgentHandler().ServeHTTP(response, agentRequest(t, http.MethodPost,
		"https://gateway.svc/api/agent/screenshot", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("oversized guest screenshot status = %d, want 502", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("oversized guest screenshot content type = %q, want a gateway error body", contentType)
	}
	detail, _ := decodeBody(t, response)["detail"].(string)
	if !strings.Contains(detail, errScreenshotTooLarge.Error()) {
		t.Fatalf("oversized guest screenshot detail = %q", detail)
	}
}
