package desktopgateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func TestServeRunsBothListenersAndShutsDownGracefully(t *testing.T) {
	cfg := testConfig()
	cfg.NoVNCDir = t.TempDir()
	g := newTestGateway(t, cfg, &fakeKubeVirt{})
	agentListener, consoleListener := listenLoopback(t), listenLoopback(t)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- g.Serve(ctx, agentListener, consoleListener) }()

	if body := getBody(t, "http://"+agentListener.Addr().String()+"/healthz"); body != "ok\n" {
		t.Fatalf("agent listener /healthz = %q", body)
	}
	if body := getBody(t, "http://"+consoleListener.Addr().String()+"/"); !strings.Contains(body, `id="control"`) {
		t.Fatalf("console listener did not serve the console page")
	}
	// The console page is not reachable on the agent listener, and /healthz is
	// not part of the console surface.
	if status := getStatus(t, "http://"+agentListener.Addr().String()+"/"); status != http.StatusNotFound {
		t.Fatalf("agent listener root status = %d, want 404", status)
	}
	if status := getStatus(t, "http://"+consoleListener.Addr().String()+"/healthz"); status == http.StatusOK {
		t.Fatal("the console listener answered a liveness probe")
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve() = %v, want a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve() did not return after cancellation")
	}
}

// Shutdown revokes before it closes: a controller must learn it lost input
// rather than discovering a dead socket.
func TestServeRevokesControlBeforeShutdown(t *testing.T) {
	g := newTestGateway(t, testConfig(), &fakeKubeVirt{})
	agentListener, consoleListener := listenLoopback(t), listenLoopback(t)
	granted, err := g.controls.acquireAgent(testName, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- g.Serve(ctx, agentListener, consoleListener) }()
	cancel()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve() did not return after cancellation")
	}

	select {
	case <-granted.ctx.Done():
	default:
		t.Fatal("shutdown left the agent lease live")
	}
	if blocked, _, _ := g.controls.powerStatus(testName); !blocked {
		t.Fatal("a drained gateway still admits control")
	}
}

// Ticket #19: the console listener's bound address is the evidence, so a dev
// gateway must refuse to serve when that address is reachable from elsewhere.
func TestServeRefusesDevModeOnANonLoopbackConsoleListener(t *testing.T) {
	cfg := testConfig()
	cfg.ConsoleAuthMode = ConsoleAuthDev
	g := newTestGateway(t, cfg, &fakeKubeVirt{})

	wildcard, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer wildcard.Close()

	err = g.Serve(context.Background(), listenLoopback(t), wildcard)
	if err == nil {
		t.Fatal("dev mode served a console listener bound to every interface")
	}
	if g.devLoopbackListener {
		t.Fatal("a refused dev listener still marked the process as loopback-bound")
	}
}

func TestServeMarksTheDevLoopbackListener(t *testing.T) {
	cfg := testConfig()
	cfg.ConsoleAuthMode = ConsoleAuthDev
	cfg.NoVNCDir = t.TempDir()
	g := newTestGateway(t, cfg, &fakeKubeVirt{})
	consoleListener := listenLoopback(t)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- g.Serve(ctx, listenLoopback(t), consoleListener) }()

	target := "http://" + consoleListener.Addr().String() + "/api/me?devToken=" + testDevToken
	if status := getStatus(t, target); status != http.StatusOK {
		t.Fatalf("dev console /api/me status = %d, want 200", status)
	}
	// The same token from a non-loopback Host is still refused.
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "gateway.desktops.svc"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dev console with a cluster Host = %d, want 401", response.StatusCode)
	}

	cancel()
	<-served
}

func TestListenBindsBothAddresses(t *testing.T) {
	cfg := testConfig()
	agent, console, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	defer console.Close()
	if agent.Addr().String() == console.Addr().String() {
		t.Fatal("both listeners bound the same address")
	}

	cfg.AgentListenAddress = "256.256.256.256:1"
	if _, _, err := Listen(cfg); err == nil {
		t.Fatal("an unbindable agent address was accepted")
	}
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}
