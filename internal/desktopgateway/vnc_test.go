package desktopgateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/rest"
)

// websocketPair returns the two ends of one live websocket connection.
func websocketPair(t *testing.T) (client *websocket.Conn, server *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		accepted <- conn
	}))
	t.Cleanup(httpServer.Close)

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case server = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("websocket upgrade did not complete")
	}
	// Registered after the server cleanup so both sockets are closed before
	// httptest waits for its connections.
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// blockingStream never answers a read until it is closed, which is how a
// wedged KubeVirt stream behaves when the desktop stops responding.
type blockingStream struct {
	startOnce sync.Once
	started   chan struct{}
	release   chan struct{}
	closeOnce sync.Once
}

func newBlockingStream() *blockingStream {
	return &blockingStream{started: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingStream) Read([]byte) (int, error) {
	s.startOnce.Do(func() { close(s.started) })
	<-s.release
	return 0, io.EOF
}

func (s *blockingStream) Write(payload []byte) (int, error) { return len(payload), nil }

func (s *blockingStream) Close() error {
	s.closeOnce.Do(func() { close(s.release) })
	return nil
}

type pipeStream struct{ net.Conn }

func TestRelayCancellationClosesABlockedKubeVirtStream(t *testing.T) {
	client, server := websocketPair(t)
	defer client.Close()

	stream := newBlockingStream()
	ctx, cancel := context.WithCancel(context.Background())
	granted := &controller{ctx: ctx}
	result := make(chan error, 1)
	go func() { result <- relayVNC(server, stream, granted) }()

	select {
	case <-stream.started:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not enter the KubeVirt stream")
	}
	cancel()
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("relay stayed blocked after control cancellation")
	}
}

func TestRelayCopiesRFBInBothDirections(t *testing.T) {
	client, server := websocketPair(t)
	desktop, gateway := net.Pipe()
	defer desktop.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	granted := &controller{ctx: ctx}
	go func() { _ = relayVNC(server, pipeStream{gateway}, granted) }()

	if err := client.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 12)
	_ = desktop.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(desktop, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "RFB 003.008\n" {
		t.Fatalf("desktop received %q", buffer)
	}

	_ = desktop.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := desktop.Write([]byte("RFB 003.003\n")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	messageType, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.BinaryMessage || string(payload) != "RFB 003.003\n" {
		t.Fatalf("browser received type %d payload %q", messageType, payload)
	}
}

// RFB has no framing of its own, so a reader must join whatever slices the
// peer chose and must treat a close frame as the end of the stream.
func TestBinaryWebSocketReaderReassemblesFramesAndEndsOnClose(t *testing.T) {
	client, server := websocketPair(t)

	for _, fragment := range []string{"RFB ", "003.", "008\n"} {
		if err := client.WriteMessage(websocket.BinaryMessage, []byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	// A text frame is not part of the RFB stream and must be skipped.
	if err := client.WriteMessage(websocket.TextMessage, []byte("ignored")); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteMessage(websocket.BinaryMessage, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	reader := &binaryWebSocketReader{conn: server}
	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	collected, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read to close = %v, want a clean EOF", err)
	}
	if string(collected) != "RFB 003.008\ndone" {
		t.Fatalf("reassembled stream = %q", collected)
	}
}

func TestBinaryWebSocketWriterSendsBinaryFrames(t *testing.T) {
	client, server := websocketPair(t)
	writer := &binaryWebSocketWriter{conn: server}
	count, err := writer.Write([]byte("frame"))
	if err != nil || count != 5 {
		t.Fatalf("Write() = (%d, %v)", count, err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	messageType, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.BinaryMessage || string(payload) != "frame" {
		t.Fatalf("received type %d payload %q", messageType, payload)
	}
}

func TestVNCRejectsOriginlessAndNonUpgradeRequestsBeforeTakeover(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	agent, err := g.controls.acquire(context.Background(), testName, actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	defer g.controls.release(testName, agent)

	response := httptest.NewRecorder()
	g.handleVNC(response, consoleRequest(t, http.MethodGet, "https://desktop.example/api/vnc?takeover=1"))
	if response.Code != http.StatusForbidden {
		t.Fatalf("originless console VNC status = %d, want 403", response.Code)
	}

	response = httptest.NewRecorder()
	g.handleVNC(response, withOrigin(consoleRequest(t, http.MethodGet, "https://desktop.example/api/vnc?takeover=1")))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("non-WebSocket console VNC status = %d, want 400", response.Code)
	}

	select {
	case <-agent.ctx.Done():
		t.Fatal("a rejected console VNC request canceled the active agent controller")
	default:
	}
	owner, generation, active := g.controls.status(testName)
	if !active || owner != actorAgent || generation != agent.generation {
		t.Fatalf("controller after rejected takeover = (%q, %d, %v)", owner, generation, active)
	}
}

func TestVNCRefusesADesktopThatIsNotStablyRunning(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) {
			return &VirtualMachineInstance{UID: "vmi-a", Phase: "Scheduling"}, nil
		},
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	request := withOrigin(consoleRequest(t, http.MethodGet, "https://desktop.example/api/vnc"))
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")

	response := httptest.NewRecorder()
	g.handleVNC(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("unready VNC status = %d, want 409", response.Code)
	}
	if kubevirt.callCount("DialVNC") != 0 {
		t.Fatal("an unready desktop was dialed anyway")
	}
}

func TestVNCReadinessLookupHasIndependentDeadline(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		getVMI: func(ctx context.Context, _, _ string) (*VirtualMachineInstance, error) {
			return blockUntilDone[*VirtualMachineInstance](ctx)
		},
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	g.vncReadinessTimeout = 20 * time.Millisecond

	request := withOrigin(consoleRequest(t, http.MethodGet, "https://desktop.example/api/vnc"))
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")

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

func TestVNCReleasesTheLeaseWhenTheUpstreamDialFails(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		getVMI:  func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
		dialVNC: func(context.Context, string, string) (VNCStream, error) { return nil, statusCodeError(503) },
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	request := withOrigin(consoleRequest(t, http.MethodGet, "https://desktop.example/api/vnc"))
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")

	response := httptest.NewRecorder()
	g.handleVNC(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed dial status = %d, want 503", response.Code)
	}
	if _, _, active := g.controls.status(testName); active {
		t.Fatal("a failed VNC dial left the input lease held")
	}
}

type statusCodeError int

func (e statusCodeError) Error() string      { return "status error" }
func (e statusCodeError) GetStatusCode() int { return int(e) }

func TestVNCStreamHTTPStatusPreservesReadinessAndAuthorization(t *testing.T) {
	cases := []struct {
		upstream int
		want     int
	}{
		{upstream: http.StatusBadRequest, want: http.StatusConflict},
		{upstream: http.StatusNotFound, want: http.StatusConflict},
		{upstream: http.StatusConflict, want: http.StatusConflict},
		{upstream: http.StatusUnauthorized, want: http.StatusBadGateway},
		{upstream: http.StatusForbidden, want: http.StatusBadGateway},
		{upstream: http.StatusServiceUnavailable, want: http.StatusServiceUnavailable},
		{upstream: http.StatusInternalServerError, want: http.StatusBadGateway},
	}
	for _, test := range cases {
		if got := vncStreamHTTPStatus(statusCodeError(test.upstream)); got != test.want {
			t.Errorf("vncStreamHTTPStatus(%d) = %d, want %d", test.upstream, got, test.want)
		}
	}
	if got := vncStreamHTTPStatus(context.DeadlineExceeded); got != http.StatusServiceUnavailable {
		t.Errorf("vncStreamHTTPStatus(deadline) = %d, want 503", got)
	}
	if got := vncStreamHTTPStatus(errors.New("transport")); got != http.StatusBadGateway {
		t.Errorf("vncStreamHTTPStatus(transport) = %d, want 502", got)
	}
}

func TestVNCURLTargetsTheKubeVirtSubresource(t *testing.T) {
	target, err := vncURL("https://10.96.0.1:443", testNamespace, testName)
	if err != nil {
		t.Fatal(err)
	}
	want := "wss://10.96.0.1:443/apis/subresources.kubevirt.io/v1/namespaces/desktops/virtualmachineinstances/inst-desktop/vnc?preserveSession=true"
	if target.String() != want {
		t.Fatalf("vncURL() = %q, want %q", target, want)
	}

	plain, err := vncURL("http://127.0.0.1:8001", testNamespace, testName)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Scheme != "ws" {
		t.Fatalf("plain HTTP host produced scheme %q, want ws", plain.Scheme)
	}
	if _, err := vncURL("", testNamespace, testName); err == nil {
		t.Fatal("an empty API host was accepted")
	}
}

func TestDialKubeVirtVNCOpensAnAuthenticatedBinaryStream(t *testing.T) {
	var gotPath, gotQuery, gotAuthorization, gotSubprotocol string
	upgrader := websocket.Upgrader{Subprotocols: []string{vncSubprotocol}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuthorization = r.Header.Get("Authorization")
		gotSubprotocol = r.Header.Get("Sec-WebSocket-Protocol")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n")); err != nil {
			return
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := dialKubeVirtVNC(ctx, &rest.Config{Host: server.URL, BearerToken: "sa-token"}, testNamespace, testName)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	buffer := make([]byte, 12)
	if _, err := io.ReadFull(stream, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "RFB 003.008\n" {
		t.Fatalf("stream delivered %q", buffer)
	}
	wantPath := "/apis/subresources.kubevirt.io/v1/namespaces/desktops/virtualmachineinstances/inst-desktop/vnc"
	if gotPath != wantPath || gotQuery != "preserveSession=true" {
		t.Fatalf("upstream saw %s?%s", gotPath, gotQuery)
	}
	if gotAuthorization != "Bearer sa-token" {
		t.Fatalf("upstream saw Authorization %q", gotAuthorization)
	}
	if gotSubprotocol != vncSubprotocol {
		t.Fatalf("upstream saw subprotocol %q, want %q", gotSubprotocol, vncSubprotocol)
	}
}

func TestDialKubeVirtVNCReportsTheUpgradeStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "virtualmachineinstances/vnc is forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	_, err := dialKubeVirtVNC(context.Background(), &rest.Config{Host: server.URL}, testNamespace, testName)
	if err == nil {
		t.Fatal("a forbidden upgrade was reported as success")
	}
	if got := vncStreamHTTPStatus(err); got != http.StatusBadGateway {
		t.Fatalf("vncStreamHTTPStatus(forbidden dial) = %d, want 502", got)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("dial error lost the upstream status: %v", err)
	}
}

func TestDialKubeVirtVNCHonorsTheCallerDeadline(t *testing.T) {
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := dialKubeVirtVNC(ctx, &rest.Config{Host: server.URL}, testNamespace, testName)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dialKubeVirtVNC() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled VNC handshake took %v", elapsed)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("the upstream VNC request stayed stuck after context cancellation")
	}
}

// A client detects a gateway restart and its own lease generation from the
// upgrade response, so both headers must survive the hijacked handshake.
func TestVNCHandshakeCarriesTheControlHeaders(t *testing.T) {
	stream := newBlockingStream()
	kubevirt := &fakeKubeVirt{
		getVMI:  func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
		dialVNC: func(context.Context, string, string) (VNCStream, error) { return stream, nil },
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	server := httptest.NewServer(http.HandlerFunc(g.handleVNC))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")

	header := http.Header{}
	header.Set(headerForwardedProto, "https")
	header.Set(headerTailscaleLogin, testSubject)
	header.Set("Origin", "https://"+host)
	client, response, err := websocket.DefaultDialer.Dial("ws://"+host+"/api/vnc", header)
	if err != nil {
		t.Fatalf("console VNC upgrade failed: %v", err)
	}
	defer client.Close()
	defer stream.Close()

	if got := response.Header.Get("X-Gateway-Boot-ID"); got != g.bootID {
		t.Fatalf("handshake X-Gateway-Boot-ID = %q, want %q", got, g.bootID)
	}
	owner, generation, active := g.controls.status(testName)
	if !active || owner != actorHuman {
		t.Fatalf("controller after upgrade = (%q, %v)", owner, active)
	}
	if got := response.Header.Get("X-Control-Generation"); got != strconv.FormatUint(generation, 10) {
		t.Fatalf("handshake X-Control-Generation = %q, want %d", got, generation)
	}
}

// An RFB stream that was severed is not a stream that ended. The release log
// line reports the reason the relay returned, so an abnormal close must stay
// an error instead of being flattened into EOF.
func TestBinaryWebSocketReaderReportsATruncatedStreamAsAFailure(t *testing.T) {
	client, server := websocketPair(t)
	if err := client.WriteMessage(websocket.BinaryMessage, []byte("RFB ")); err != nil {
		t.Fatal(err)
	}
	if err := client.UnderlyingConn().Close(); err != nil {
		t.Fatal(err)
	}

	reader := &binaryWebSocketReader{conn: server}
	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	collected, err := io.ReadAll(reader)
	if string(collected) != "RFB " {
		t.Fatalf("reader delivered %q before the truncation", collected)
	}
	if err == nil {
		t.Fatal("a truncated RFB stream was reported as a clean end")
	}
}

func TestCloseAsEOFAcceptsOnlyAnOrderlyClose(t *testing.T) {
	orderly := []int{websocket.CloseNormalClosure, websocket.CloseGoingAway}
	for _, code := range orderly {
		if err := closeAsEOF(&websocket.CloseError{Code: code}); !errors.Is(err, io.EOF) {
			t.Fatalf("close code %d = %v, want io.EOF", code, err)
		}
	}
	abnormal := []int{websocket.CloseAbnormalClosure, websocket.ClosePolicyViolation, websocket.CloseInternalServerErr}
	for _, code := range abnormal {
		if err := closeAsEOF(&websocket.CloseError{Code: code}); errors.Is(err, io.EOF) {
			t.Fatalf("close code %d was reported as a clean end", code)
		}
	}
	if err := closeAsEOF(errors.New("transport failed")); errors.Is(err, io.EOF) {
		t.Fatal("a transport failure was reported as a clean end")
	}
}
