package desktopgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Gateway serves one Personal Desktop on two listeners that share one control
// registry. Nothing about the desktop is derived here: its name, namespace,
// owner, and both bearer tokens arrive as configuration from the operator.
type Gateway struct {
	config   Config
	kubevirt KubeVirtClient
	controls *controlRegistry
	logger   *slog.Logger
	bootID   string

	// devLoopbackListener records that the console listener actually bound a
	// loopback address (ticket #19). Dev-mode authentication is refused until
	// the process has proven that, never on the strength of a request header.
	devLoopbackListener bool

	statusTimeout          time.Duration
	screenshotTimeout      time.Duration
	screenshotWriteTimeout time.Duration
	vncReadinessTimeout    time.Duration
	powerTimeout           time.Duration
	guestAgentTimeout      time.Duration
	agentLeaseTTL          time.Duration

	screenshotConcurrency int
	screenshotOnce        sync.Once
	screenshotSlots       chan struct{}

	guestClientOnce sync.Once
	guestClient     *http.Client
}

// New builds a Gateway for one desktop. Each process boot gets a fresh
// gatewayBootID so a client can tell a restart (which drops every in-memory
// lease) from a lease it still holds.
func New(cfg Config, kubevirt KubeVirtClient, logger *slog.Logger) (*Gateway, error) {
	if kubevirt == nil {
		return nil, errors.New("a KubeVirt client is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	bootID, err := newGatewayBootID()
	if err != nil {
		return nil, fmt.Errorf("create Gateway boot ID: %w", err)
	}
	return &Gateway{
		config:                cfg,
		kubevirt:              kubevirt,
		controls:              newControlRegistry(),
		logger:                logger,
		bootID:                bootID,
		statusTimeout:         cfg.StatusTimeout,
		screenshotTimeout:     cfg.ScreenshotTimeout,
		powerTimeout:          cfg.PowerTimeout,
		guestAgentTimeout:     cfg.GuestAgentTimeout,
		agentLeaseTTL:         cfg.AgentLeaseTTL,
		screenshotConcurrency: cfg.ScreenshotConcurrency,
	}, nil
}

func newGatewayBootID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (g *Gateway) effectiveStatusTimeout() time.Duration {
	return effectiveTimeout(g.statusTimeout, defaultStatusTimeout)
}

func (g *Gateway) effectiveScreenshotTimeout() time.Duration {
	return effectiveTimeout(g.screenshotTimeout, defaultScreenshotTimeout)
}

func (g *Gateway) effectiveScreenshotWriteTimeout() time.Duration {
	return effectiveTimeout(g.screenshotWriteTimeout, defaultScreenshotWriteTimeout)
}

func (g *Gateway) effectiveVNCReadinessTimeout() time.Duration {
	return effectiveTimeout(g.vncReadinessTimeout, defaultVNCReadinessTimeout)
}

func (g *Gateway) effectivePowerTimeout() time.Duration {
	return effectiveTimeout(g.powerTimeout, defaultPowerTimeout)
}

func (g *Gateway) effectiveGuestAgentTimeout() time.Duration {
	return effectiveTimeout(g.guestAgentTimeout, defaultGuestAgentTimeout)
}

func (g *Gateway) effectiveAgentLeaseTTL() time.Duration {
	return effectiveTimeout(g.agentLeaseTTL, defaultAgentLeaseTTL)
}

// tryAcquireScreenshotSlot admits a capture or refuses it outright. Queueing
// would convert a slow framebuffer into an unbounded backlog of requests that
// all outlive their own deadlines.
func (g *Gateway) tryAcquireScreenshotSlot() bool {
	g.screenshotOnce.Do(func() {
		concurrency := g.screenshotConcurrency
		if concurrency <= 0 {
			concurrency = defaultScreenshotConcurrency
		}
		g.screenshotSlots = make(chan struct{}, concurrency)
	})
	select {
	case g.screenshotSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *Gateway) releaseScreenshotSlot() {
	<-g.screenshotSlots
}

// authenticator resolves the caller of one request on one listener. It writes
// the 401 itself so handlers only branch on success.
type authenticator func(w http.ResponseWriter, r *http.Request) (identity, bool)

func (g *Gateway) agentIdentity(w http.ResponseWriter, r *http.Request) (identity, bool) {
	id, err := g.authenticateAgent(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required", err)
		return identity{}, false
	}
	return id, true
}

func (g *Gateway) humanIdentity(w http.ResponseWriter, r *http.Request) (identity, bool) {
	id, err := g.authenticateHuman(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required", err)
		return identity{}, false
	}
	return id, true
}

// AgentHandler serves the plugin-facing listener: liveness, status, the input
// lease, the Guest Desktop Agent proxy, and power.
func (g *Gateway) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /api/me", g.handleMe(g.agentIdentity))
	mux.HandleFunc("POST /api/control/acquire", g.handleControlAcquire)
	mux.HandleFunc("POST /api/control/release", g.handleControlRelease)
	mux.HandleFunc("POST /api/agent/screenshot", g.handleAgentScreenshot)
	mux.HandleFunc("POST /api/agent/{action}", g.handleAgentAction)
	mux.HandleFunc("GET /api/agent/windows", g.handleAgentWindows)
	mux.HandleFunc("POST /api/power/{action}", g.handlePower(g.agentIdentity))
	return securityHeaders(mux)
}

// ConsoleHandler serves the human-facing listener: the Desktop Console page,
// noVNC, view-only screenshots, the RFB WebSocket, and power.
func (g *Gateway) ConsoleHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", g.handleMe(g.humanIdentity))
	mux.HandleFunc("GET /api/screenshot", g.handleScreenshot)
	mux.HandleFunc("GET /api/vnc", g.handleVNC)
	mux.HandleFunc("POST /api/power/{action}", g.handlePower(g.humanIdentity))
	mux.Handle("GET /novnc/", http.StripPrefix("/novnc/", http.FileServer(http.Dir(g.config.NoVNCDir))))
	ui, err := fs.Sub(consoleUI, "static")
	if err != nil {
		panic(fmt.Sprintf("open embedded Desktop Console: %v", err))
	}
	mux.Handle("GET /", http.FileServer(http.FS(ui)))
	return securityHeaders(mux)
}

func (g *Gateway) handleMe(authenticate authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := authenticate(w, r)
		if !ok {
			return
		}
		readCtx, cancelRead := context.WithTimeout(r.Context(), g.effectiveStatusTimeout())
		defer cancelRead()

		response := map[string]any{
			"desktopName":          g.config.Name,
			"namespace":            g.config.Namespace,
			"os":                   g.config.OS,
			"actor":                string(id.actor),
			"authMode":             id.authMode,
			"gatewayBootID":        g.bootID,
			"consoleURL":           g.config.ConsoleURL,
			"agentLeaseTtlSeconds": int(g.effectiveAgentLeaseTTL().Seconds()),
		}
		if id.login != "" {
			response["login"] = id.login
		}

		vm, err := g.kubevirt.GetVM(readCtx, g.config.Namespace, g.config.Name)
		switch {
		case err == nil:
			response["vmExists"] = true
			response["vmPrintableStatus"] = vm.PrintableStatus
		case apierrors.IsNotFound(err):
			response["vmExists"] = false
		default:
			writeError(w, kubeVirtReadStatus(err), "read VirtualMachine status", err)
			return
		}

		vmi, err := g.kubevirt.GetVMI(readCtx, g.config.Namespace, g.config.Name)
		switch {
		case err == nil:
			response["vmiExists"] = true
			response["vmiPhase"] = vmi.Phase
			response["vmiUID"] = vmi.UID
		case apierrors.IsNotFound(err):
			response["vmiExists"] = false
		default:
			writeError(w, kubeVirtReadStatus(err), "read VirtualMachineInstance status", err)
			return
		}

		owner, generation, active := g.controls.status(g.config.Name)
		response["controlActive"] = active
		response["controlGeneration"] = generation
		if active {
			response["controlActor"] = string(owner)
		}
		controlBlocked, powerActive, powerAction := g.controls.powerStatus(g.config.Name)
		response["controlBlocked"] = controlBlocked
		response["powerOperationActive"] = powerActive
		response["powerRecoveryRequired"] = controlBlocked && !powerActive
		if powerActive {
			response["powerOperationAction"] = powerAction
		}
		writeJSON(w, http.StatusOK, response)
	}
}

// handleScreenshot serves the console's view-only frame from KubeVirt. The VMI
// is re-read after the capture: a frame that belongs to a VMI that has since
// been replaced would be a picture of a machine the viewer is not looking at.
func (g *Gateway) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.humanIdentity(w, r); !ok {
		return
	}
	if !g.tryAcquireScreenshotSlot() {
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, "too many concurrent screenshot requests", nil)
		return
	}
	defer g.releaseScreenshotSlot()
	readCtx, cancelRead := context.WithTimeout(r.Context(), g.effectiveScreenshotTimeout())
	defer cancelRead()

	before, err := g.kubevirt.GetVMI(readCtx, g.config.Namespace, g.config.Name)
	if err != nil {
		writeError(w, screenshotReadStatus(err), "desktop framebuffer is not ready", err)
		return
	}
	if before.Phase != VirtualMachineInstanceRunning || before.Deleting {
		writeError(w, http.StatusConflict, "desktop framebuffer is not ready", errors.New("VMI is not stably running"))
		return
	}
	raw, err := g.kubevirt.Screenshot(readCtx, g.config.Namespace, g.config.Name)
	if err != nil {
		writeError(w, screenshotReadStatus(err), "desktop framebuffer is not ready", err)
		return
	}
	after, err := g.kubevirt.GetVMI(readCtx, g.config.Namespace, g.config.Name)
	if err != nil {
		writeError(w, screenshotReadStatus(err), "re-read VirtualMachineInstance after framebuffer capture", err)
		return
	}
	if after.UID != before.UID || after.Phase != VirtualMachineInstanceRunning || after.Deleting {
		writeError(w, http.StatusConflict, "desktop framebuffer changed; observe again", errors.New("VMI changed while the framebuffer was captured"))
		return
	}
	shot, err := transformScreenshot(readCtx, raw, r.URL.Query())
	if err != nil {
		writeError(w, screenshotTransformStatus(err), "transform desktop screenshot", err)
		return
	}
	if err := readCtx.Err(); err != nil {
		writeError(w, screenshotReadStatus(err), "desktop framebuffer deadline exceeded", err)
		return
	}
	g.writeScreenshotResponse(w, shot, after.UID)
}

func (g *Gateway) writeScreenshotResponse(w http.ResponseWriter, shot screenshot, vmiUID string) {
	responseController := http.NewResponseController(w)
	if err := responseController.SetWriteDeadline(time.Now().Add(g.effectiveScreenshotWriteTimeout())); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		writeError(w, http.StatusServiceUnavailable, "set screenshot response deadline", err)
		return
	}
	defer func() {
		if err := responseController.SetWriteDeadline(time.Time{}); err != nil &&
			!errors.Is(err, http.ErrNotSupported) && g.logger != nil {
			g.logger.Warn("clear screenshot response deadline", "error", err)
		}
	}()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", shot.contentType)
	w.Header().Set("X-Framebuffer-Width", strconv.Itoa(shot.frameWidth))
	w.Header().Set("X-Framebuffer-Height", strconv.Itoa(shot.frameHeight))
	w.Header().Set("X-Encoded-Width", strconv.Itoa(shot.encodedWidth))
	w.Header().Set("X-Encoded-Height", strconv.Itoa(shot.encodedHeight))
	w.Header().Set("X-VMI-UID", vmiUID)
	w.Header().Set("X-Gateway-Boot-ID", g.bootID)
	_, generation, _ := g.controls.status(g.config.Name)
	w.Header().Set("X-Control-Generation", strconv.FormatUint(generation, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(shot.body); err != nil {
		if g.logger != nil {
			g.logger.Warn("write screenshot response", "desktop", g.config.Name, "error", err)
		}
		return
	}
	if err := responseController.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) && g.logger != nil {
		g.logger.Warn("flush screenshot response", "desktop", g.config.Name, "error", err)
	}
}

func (g *Gateway) handleControlAcquire(w http.ResponseWriter, r *http.Request) {
	id, ok := g.agentIdentity(w, r)
	if !ok {
		return
	}
	if !g.requireMutationOrigin(w, r, id) {
		return
	}
	granted, err := g.controls.acquireAgent(g.config.Name, g.effectiveAgentLeaseTTL())
	if err != nil {
		switch {
		case errors.Is(err, errControlBlocked):
			writeError(w, http.StatusConflict, "desktop control is blocked by its power state", err)
		case errors.Is(err, errControlBusy):
			writeError(w, http.StatusConflict, "another actor controls this desktop", err)
		default:
			writeError(w, http.StatusServiceUnavailable, "input control unavailable", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"desktopName":       g.config.Name,
		"gatewayBootID":     g.bootID,
		"controlGeneration": granted.generation,
		"actor":             string(id.actor),
		"leaseTtlSeconds":   int(g.effectiveAgentLeaseTTL().Seconds()),
	})
}

func (g *Gateway) handleControlRelease(w http.ResponseWriter, r *http.Request) {
	id, ok := g.agentIdentity(w, r)
	if !ok {
		return
	}
	if !g.requireMutationOrigin(w, r, id) {
		return
	}
	_, _, active := g.controls.status(g.config.Name)
	if active && !g.controls.releaseAgentOwner(g.config.Name) {
		writeError(w, http.StatusConflict, "a human owns input; the agent cannot release it", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"desktopName": g.config.Name, "released": true})
}

// agentInputActions are the guest paths that carry input. Anything else on
// /api/agent/{action} is a 404 rather than a blind proxy.
var agentInputActions = map[string]bool{"actions": true, "launch": true}

func (g *Gateway) handleAgentAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if !agentInputActions[action] {
		writeError(w, http.StatusNotFound, "unknown agent action", nil)
		return
	}
	id, ok := g.agentIdentity(w, r)
	if !ok {
		return
	}
	if !g.requireMutationOrigin(w, r, id) {
		return
	}
	owner, _, active := g.controls.status(g.config.Name)
	if !active || owner != actorAgent {
		writeError(w, http.StatusConflict, "agent input control is not held; call desktop_acquire first", nil)
		return
	}
	g.controls.touchAgent(g.config.Name)
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read agent action body", err)
		return
	}
	status, body := g.proxyGuestAgent(r.Context(), http.MethodPost, "/"+action, payload)
	proxyGuestAgentResponse(w, status, body)
}

func (g *Gateway) handleAgentScreenshot(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.agentIdentity(w, r); !ok {
		return
	}
	if !g.tryAcquireScreenshotSlot() {
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, "too many concurrent screenshot requests", nil)
		return
	}
	defer g.releaseScreenshotSlot()
	readCtx, cancelRead := context.WithTimeout(r.Context(), g.effectiveScreenshotTimeout())
	defer cancelRead()
	vmi, err := g.kubevirt.GetVMI(readCtx, g.config.Namespace, g.config.Name)
	if err != nil {
		writeError(w, screenshotReadStatus(err), "desktop framebuffer is not ready", err)
		return
	}
	if vmi.Phase != VirtualMachineInstanceRunning || vmi.Deleting {
		writeError(w, http.StatusConflict, "desktop framebuffer is not ready", errors.New("VMI is not stably running"))
		return
	}
	status, payload := g.proxyGuestAgent(readCtx, http.MethodPost, "/screenshot", nil)
	if status != http.StatusOK {
		proxyGuestAgentResponse(w, status, payload)
		return
	}
	shot, err := transformScreenshot(readCtx, payload, r.URL.Query())
	if err != nil {
		writeError(w, screenshotTransformStatus(err), "transform desktop screenshot", err)
		return
	}
	if err := readCtx.Err(); err != nil {
		writeError(w, screenshotReadStatus(err), "desktop framebuffer deadline exceeded", err)
		return
	}
	g.writeScreenshotResponse(w, shot, vmi.UID)
}

func (g *Gateway) handleAgentWindows(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.agentIdentity(w, r); !ok {
		return
	}
	g.controls.touchAgent(g.config.Name)
	readCtx, cancelRead := context.WithTimeout(r.Context(), g.effectiveGuestAgentTimeout())
	defer cancelRead()
	status, payload := g.proxyGuestAgent(readCtx, http.MethodGet, "/windows", nil)
	proxyGuestAgentResponse(w, status, payload)
}

func (g *Gateway) sharedGuestClient() *http.Client {
	g.guestClientOnce.Do(func() {
		g.guestClient = &http.Client{}
	})
	return g.guestClient
}

// proxyGuestAgent forwards one typed action batch or command to the Guest
// Desktop Agent. Guest HTTP failures keep the guest's status and body so the
// plugin can tell a deterministic tool failure (guest 4xx/5xx body without an
// outcome) from an ambiguous dispatch (gateway-authored UnknownOutcome).
func (g *Gateway) proxyGuestAgent(ctx context.Context, method, path string, body []byte) (int, []byte) {
	requestCtx, cancel := context.WithTimeout(ctx, g.effectiveGuestAgentTimeout())
	defer cancel()
	address := strings.TrimRight(g.config.GuestAgentAddress, "/") + path
	request, err := http.NewRequestWithContext(requestCtx, method, address, bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, gatewayUnknownOutcomeBody("build desktop agent request", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+g.config.GuestAgentToken)
	response, err := g.sharedGuestClient().Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return http.StatusGatewayTimeout, gatewayUnknownOutcomeBody("desktop agent did not respond in time", err)
		}
		return http.StatusBadGateway, gatewayUnknownOutcomeBody("desktop agent is unreachable", err)
	}
	defer response.Body.Close()
	// One byte past the processing limit, as the KubeVirt screenshot path
	// does: reading exactly the limit would hand transformScreenshot a
	// truncated frame that still passes its size guard and would then be
	// served as a whole screenshot.
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxScreenshotRawBytes+1))
	if err != nil {
		return http.StatusBadGateway, gatewayUnknownOutcomeBody("read desktop agent response", err)
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	return response.StatusCode, payload
}

func gatewayUnknownOutcomeBody(message string, cause error) []byte {
	return mustJSONBody(map[string]any{
		"error":   message,
		"detail":  cause.Error(),
		"outcome": outcomeUnknown,
	})
}

func mustJSONBody(value map[string]any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"error":"encode gateway response"}`)
	}
	return encoded
}

func proxyGuestAgentResponse(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string, cause error) {
	response := map[string]any{"error": message}
	if cause != nil {
		response["detail"] = cause.Error()
	}
	writeJSON(w, status, response)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob:; connect-src 'self' ws: wss:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}
