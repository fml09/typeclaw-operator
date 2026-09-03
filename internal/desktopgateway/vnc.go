package desktopgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	// vncClientReadLimit caps one inbound RFB message from the browser. RFB
	// client messages are small; anything larger is not a console session.
	vncClientReadLimit = 1 << 20
	// vncClientIdleTimeout is refreshed by every pong, so a browser that has
	// gone away releases the exclusive input lease within one interval.
	vncClientIdleTimeout = 45 * time.Second
	vncPingInterval      = 15 * time.Second
	vncDialTimeout       = 10 * time.Second
)

// handleVNC grants the exclusive input lease and relays RFB between the
// browser and the desktop's KubeVirt VNC subresource. It runs on the console
// listener only: RFB is the human's input path, and the agent drives the
// desktop through typed actions instead.
func (g *Gateway) handleVNC(w http.ResponseWriter, r *http.Request) {
	id, ok := g.humanIdentity(w, r)
	if !ok {
		return
	}
	if !g.requireMutationOrigin(w, r, id) {
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, "WebSocket upgrade required", nil)
		return
	}
	takeover := r.URL.Query().Get("takeover") == "1"
	relayCtx, relayCancel := context.WithCancel(r.Context())
	defer relayCancel()

	readinessCtx, cancelReadiness := context.WithTimeout(r.Context(), g.effectiveVNCReadinessTimeout())
	vmi, err := g.kubevirt.GetVMI(readinessCtx, g.config.Namespace, g.config.Name)
	cancelReadiness()
	if err != nil {
		status := kubeVirtReadStatus(err)
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			status = http.StatusConflict
		}
		writeError(w, status, "desktop is not ready for control", err)
		return
	}
	if vmi.Phase != VirtualMachineInstanceRunning || vmi.Deleting {
		writeError(w, http.StatusConflict, "desktop is not ready for control", errors.New("VMI is not stably running"))
		return
	}

	granted, err := g.controls.acquire(relayCtx, g.config.Name, id.actor, takeover)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, errRevocationTimed) {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, "input control unavailable", err)
		return
	}
	defer g.controls.release(g.config.Name, granted)

	openCtx, cancelOpen := context.WithTimeout(granted.ctx, vncDialTimeout)
	stream, err := g.kubevirt.DialVNC(openCtx, g.config.Namespace, g.config.Name)
	cancelOpen()
	if err != nil {
		writeError(w, vncStreamHTTPStatus(err), "open KubeVirt VNC stream", err)
		return
	}

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 5 * time.Second,
		Subprotocols:     []string{"binary"},
		CheckOrigin: func(request *http.Request) bool {
			return g.mutationOriginAllowed(request, id)
		},
	}
	// The 101 response is assembled by the upgrader from this header alone;
	// anything set on w.Header() is dropped once the connection is hijacked.
	// A client detects a gateway restart from the boot ID it reads here.
	handshake := http.Header{}
	handshake.Set("X-Control-Generation", strconv.FormatUint(granted.generation, 10))
	handshake.Set("X-Gateway-Boot-ID", g.bootID)
	client, err := upgrader.Upgrade(w, r, handshake)
	if err != nil {
		_ = stream.Close()
		return
	}
	client.SetReadLimit(vncClientReadLimit)
	_ = client.SetReadDeadline(time.Now().Add(vncClientIdleTimeout))
	client.SetPongHandler(func(string) error {
		return client.SetReadDeadline(time.Now().Add(vncClientIdleTimeout))
	})

	g.logger.Info("desktop control granted", "desktop", g.config.Name, "actor", id.actor, "generation", granted.generation)
	err = relayVNC(client, stream, granted)
	relayCancel()
	g.logger.Info("desktop control released", "desktop", g.config.Name, "actor", id.actor, "generation", granted.generation, "reason", fmt.Sprint(err))
}

// relayVNC copies RFB in both directions until either side ends or the lease
// is revoked. Revocation closes both sockets rather than waiting for a clean
// protocol shutdown: a controller that has lost input must stop being able to
// send it immediately.
func relayVNC(client *websocket.Conn, upstream VNCStream, granted *controller) error {
	var closeOnce sync.Once
	closeRelay := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	defer closeRelay()

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(vncPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-granted.ctx.Done():
				_ = client.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "control revoked"),
					time.Now().Add(time.Second),
				)
				closeRelay()
				return
			case <-ticker.C:
				if err := client.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
					closeRelay()
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Both copies are buffered so the loser of the race can always report and
	// exit even though nobody reads its result.
	failures := make(chan error, 2)
	go func() {
		_, err := io.Copy(upstream, &binaryWebSocketReader{conn: client})
		failures <- err
	}()
	go func() {
		_, err := io.Copy(&binaryWebSocketWriter{conn: client}, upstream)
		failures <- err
	}()
	err := <-failures
	closeRelay()
	return err
}

type httpStatusCoder interface {
	GetStatusCode() int
}

// vncStreamHTTPStatus translates an upstream VNC upgrade failure for the
// browser. A 401 or 403 from KubeVirt is the gateway ServiceAccount's problem,
// never the console user's, so it must not surface as an auth failure.
func vncStreamHTTPStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return http.StatusServiceUnavailable
	}
	var coded httpStatusCoder
	if !errors.As(err, &coded) {
		return http.StatusBadGateway
	}
	switch coded.GetStatusCode() {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict:
		return http.StatusConflict
	case http.StatusUnauthorized, http.StatusForbidden:
		return http.StatusBadGateway
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}
