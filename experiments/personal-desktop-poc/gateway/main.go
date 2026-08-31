package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	kvcorev1 "kubevirt.io/client-go/kubevirt/typed/core/v1"
	"kubevirt.io/client-go/subresources"
)

//go:embed static/index.html
var webUI embed.FS

type config struct {
	address              string
	namespace            string
	typeClawInstanceUID  string
	ownerHashKey         string
	agentTokenKey        string
	authProxyToken       string
	devAccessToken       string
	allowInsecureDevAuth bool
	noVNCDir             string
	agentPort            int
	agentAddressOverride string
	agentTimeout         time.Duration
	agentLeaseTTL        time.Duration
	agentToken           string
}

type actorKind string

const (
	actorHuman actorKind = "human"
	actorAgent actorKind = "agent"
)

type identity struct {
	issuer      string
	subject     string
	actor       actorKind
	authMode    string
	desktopName string
}

type controller struct {
	actor      actorKind
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	closeOnce  sync.Once
	// ttl > 0 marks a lease acquired over HTTP by the Agent plugin. Such a
	// lease has no socket lifecycle and must expire when the owner stops
	// refreshing it; RFB controllers never set it.
	ttl       time.Duration
	lastTouch atomic.Int64
}

func (c *controller) closeDone() {
	c.closeOnce.Do(func() { close(c.done) })
}

type controlRegistry struct {
	mu             sync.Mutex
	active         map[string]*controller
	generations    map[string]uint64
	controlBlocked map[string]bool
	powerOps       map[string]*powerOperation
	takeovers      map[string]*takeoverReservation
	draining       bool
}

type powerOperation struct {
	action       string
	priorBlocked bool
}

type powerOutcome int

const (
	powerSucceeded powerOutcome = iota
	powerRejected
	powerUnknown
)

type takeoverReservation struct{}

var (
	errControlBusy            = errors.New("another actor controls this desktop")
	errControlBlocked         = errors.New("desktop control is blocked by its power state")
	errPowerBusy              = errors.New("another power operation is in progress")
	errPowerRecoveryRequired  = errors.New("desktop power recovery requires an explicit start")
	errRevocationTimed        = errors.New("the previous controller did not revoke in time")
	errNotAuthenticated       = errors.New("missing trusted desktop identity")
	errInvalidScreenshotQuery = errors.New("invalid screenshot query")
	errScreenshotCannotFit    = errors.New("screenshot cannot fit the requested byte cap")
	errScreenshotTooLarge     = errors.New("screenshot exceeds gateway processing limits")
	dnsLabelPattern           = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	instanceUIDPattern        = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
)

const (
	defaultStatusTimeout          = 5 * time.Second
	defaultScreenshotTimeout      = 12 * time.Second
	defaultScreenshotWriteTimeout = 5 * time.Second
	defaultVNCReadinessTimeout    = 5 * time.Second
	defaultPowerTimeout           = 15 * time.Second
	defaultScreenshotConcurrency  = 3
	defaultAgentPort              = 9876
	defaultAgentTimeout           = 45 * time.Second
	defaultAgentLeaseTTL          = 120 * time.Second
	maxScreenshotRawBytes         = 16 << 20
	maxFramebufferDimension       = 4096
	maxFramebufferPixels          = 4096 * 2160
)

func newControlRegistry() *controlRegistry {
	return &controlRegistry{
		active:         make(map[string]*controller),
		generations:    make(map[string]uint64),
		controlBlocked: make(map[string]bool),
		powerOps:       make(map[string]*powerOperation),
		takeovers:      make(map[string]*takeoverReservation),
	}
}

func (r *controlRegistry) acquire(ctx context.Context, desktop string, actor actorKind, takeover bool) (*controller, error) {
	r.mu.Lock()
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if r.draining {
		r.mu.Unlock()
		return nil, errControlBlocked
	}
	if r.controlBlocked[desktop] {
		r.mu.Unlock()
		return nil, errControlBlocked
	}
	if r.takeovers[desktop] != nil {
		r.mu.Unlock()
		return nil, errControlBusy
	}
	current := r.active[desktop]
	if current == nil {
		granted := r.grantLocked(ctx, desktop, actor)
		r.mu.Unlock()
		return granted, nil
	}

	canTakeOver := takeover && actor == actorHuman && current.actor == actorAgent
	if !canTakeOver {
		r.mu.Unlock()
		return nil, errControlBusy
	}

	reservation := &takeoverReservation{}
	r.takeovers[desktop] = reservation
	previousDone := current.done
	current.cancel()
	r.mu.Unlock()

	timer := time.NewTimer(3 * time.Second)
	var waitErr error
	select {
	case <-previousDone:
		stopTimer(timer)
	case <-timer.C:
		waitErr = errRevocationTimed
	case <-ctx.Done():
		stopTimer(timer)
		waitErr = ctx.Err()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.takeovers[desktop] != reservation {
		return nil, errControlBusy
	}
	if waitErr == nil {
		waitErr = ctx.Err()
	}
	if waitErr != nil {
		delete(r.takeovers, desktop)
		return nil, waitErr
	}
	if r.controlBlocked[desktop] {
		delete(r.takeovers, desktop)
		return nil, errControlBlocked
	}
	if r.active[desktop] != nil {
		delete(r.takeovers, desktop)
		return nil, errControlBusy
	}
	delete(r.takeovers, desktop)
	return r.grantLocked(ctx, desktop, actor), nil
}

func (r *controlRegistry) grantLocked(ctx context.Context, desktop string, actor actorKind) *controller {
	r.generations[desktop]++
	leaseCtx, cancel := context.WithCancel(ctx)
	granted := &controller{
		actor:      actor,
		generation: r.generations[desktop],
		ctx:        leaseCtx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	r.active[desktop] = granted
	return granted
}

// acquireAgent grants the exclusive control lease to the Agent without an RFB
// connection. The lease is anchored to context.Background() because the HTTP
// request that creates it ends immediately; the expire loop below owns its
// lifetime instead.
func (r *controlRegistry) acquireAgent(desktop string, ttl time.Duration) (*controller, error) {
	r.mu.Lock()
	if r.draining || r.controlBlocked[desktop] {
		r.mu.Unlock()
		return nil, errControlBlocked
	}
	if r.takeovers[desktop] != nil || r.active[desktop] != nil {
		r.mu.Unlock()
		return nil, errControlBusy
	}
	granted := r.grantLocked(context.Background(), desktop, actorAgent)
	granted.ttl = ttl
	granted.lastTouch.Store(time.Now().UnixNano())
	r.mu.Unlock()
	go granted.expireLoop(r, desktop)
	return granted, nil
}

func (c *controller) expireLoop(r *controlRegistry, desktop string) {
	interval := c.ttl / 4
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			r.release(desktop, c)
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, c.lastTouch.Load())) > c.ttl {
				r.release(desktop, c)
				return
			}
		}
	}
}

// touchAgent extends the idle deadline of an HTTP agent lease. View-only agent
// calls also count: a model observing or listing windows is still driving the
// session.
func (r *controlRegistry) touchAgent(desktop string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.active[desktop]; current != nil && current.actor == actorAgent && current.ttl > 0 {
		current.lastTouch.Store(time.Now().UnixNano())
	}
}

// releaseAgentOwner releases the desktop only when an HTTP agent lease owns
// it. A human controller cannot be released by the agent.
func (r *controlRegistry) releaseAgentOwner(desktop string) bool {
	r.mu.Lock()
	current := r.active[desktop]
	if current == nil || current.actor != actorAgent {
		r.mu.Unlock()
		return false
	}
	r.mu.Unlock()
	r.release(desktop, current)
	return true
}

func (r *controlRegistry) beginPower(desktop, action string) (*powerOperation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return nil, errControlBlocked
	}
	if r.active[desktop] != nil {
		return nil, errControlBusy
	}
	if r.takeovers[desktop] != nil {
		return nil, errControlBusy
	}
	if r.powerOps[desktop] != nil {
		return nil, errPowerBusy
	}
	if r.controlBlocked[desktop] && action != "start" {
		return nil, errPowerRecoveryRequired
	}
	operation := &powerOperation{action: action, priorBlocked: r.controlBlocked[desktop]}
	r.powerOps[desktop] = operation
	r.controlBlocked[desktop] = true
	return operation, nil
}

func (r *controlRegistry) finishPower(desktop string, operation *powerOperation, outcome powerOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.powerOps[desktop] != operation {
		return
	}
	delete(r.powerOps, desktop)
	if r.draining {
		r.controlBlocked[desktop] = true
		return
	}
	if outcome == powerSucceeded {
		if operation.action == "start" {
			delete(r.controlBlocked, desktop)
		}
		return
	}
	if outcome == powerUnknown {
		return
	}
	if operation.priorBlocked {
		r.controlBlocked[desktop] = true
	} else {
		delete(r.controlBlocked, desktop)
	}
}

func (r *controlRegistry) revokeAll(ctx context.Context) {
	r.mu.Lock()
	r.draining = true
	controllers := make([]*controller, 0, len(r.active))
	for desktop, current := range r.active {
		r.controlBlocked[desktop] = true
		controllers = append(controllers, current)
		current.cancel()
	}
	for desktop := range r.takeovers {
		r.controlBlocked[desktop] = true
		delete(r.takeovers, desktop)
	}
	for desktop := range r.powerOps {
		r.controlBlocked[desktop] = true
	}
	r.mu.Unlock()
	for _, current := range controllers {
		select {
		case <-current.done:
		case <-ctx.Done():
			return
		}
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (r *controlRegistry) release(desktop string, granted *controller) {
	r.mu.Lock()
	if r.active[desktop] == granted {
		delete(r.active, desktop)
	}
	r.mu.Unlock()
	granted.closeDone()
}

func (r *controlRegistry) status(desktop string) (actorKind, uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.active[desktop]
	if current == nil {
		return "", r.generations[desktop], false
	}
	return current.actor, current.generation, true
}

func (r *controlRegistry) powerStatus(desktop string) (blocked, active bool, action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	operation := r.powerOps[desktop]
	if operation == nil {
		return r.controlBlocked[desktop], false, ""
	}
	return r.controlBlocked[desktop], true, operation.action
}

type gateway struct {
	config                 config
	virt                   kubecli.KubevirtClient
	controls               *controlRegistry
	logger                 *slog.Logger
	bootID                 string
	statusTimeout          time.Duration
	screenshotTimeout      time.Duration
	screenshotWriteTimeout time.Duration
	vncReadinessTimeout    time.Duration
	powerTimeout           time.Duration
	agentTimeout           time.Duration
	agentLeaseTTL          time.Duration
	screenshotConcurrency  int
	screenshotOnce         sync.Once
	screenshotSlots        chan struct{}
	agentClientOnce        sync.Once
	agentClient            *http.Client
}

func effectiveTimeout(configured, fallback time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return fallback
}

func (g *gateway) effectiveStatusTimeout() time.Duration {
	return effectiveTimeout(g.statusTimeout, defaultStatusTimeout)
}

func (g *gateway) effectiveScreenshotTimeout() time.Duration {
	return effectiveTimeout(g.screenshotTimeout, defaultScreenshotTimeout)
}

func (g *gateway) effectiveScreenshotWriteTimeout() time.Duration {
	return effectiveTimeout(g.screenshotWriteTimeout, defaultScreenshotWriteTimeout)
}

func (g *gateway) effectiveVNCReadinessTimeout() time.Duration {
	return effectiveTimeout(g.vncReadinessTimeout, defaultVNCReadinessTimeout)
}

func (g *gateway) effectivePowerTimeout() time.Duration {
	if g.powerTimeout > 0 {
		return g.powerTimeout
	}
	return defaultPowerTimeout
}

func (g *gateway) effectiveAgentTimeout() time.Duration {
	return effectiveTimeout(g.agentTimeout, defaultAgentTimeout)
}

func (g *gateway) effectiveAgentLeaseTTL() time.Duration {
	return effectiveTimeout(g.agentLeaseTTL, defaultAgentLeaseTTL)
}

func (g *gateway) tryAcquireScreenshotSlot() bool {
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

func (g *gateway) releaseScreenshotSlot() {
	<-g.screenshotSlots
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	virt, err := kubecli.GetKubevirtClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "create KubeVirt client: %v\n", err)
		os.Exit(2)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	bootID, err := newGatewayBootID()
	if err != nil {
		fmt.Fprintf(os.Stderr, "create Gateway boot ID: %v\n", err)
		os.Exit(2)
	}
	g := &gateway{config: cfg, virt: virt, controls: newControlRegistry(), logger: logger, bootID: bootID}

	server := &http.Server{
		Addr:              cfg.address,
		Handler:           g.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("starting experimental personal desktop gateway",
		"address", cfg.address,
		"namespace", cfg.namespace,
		"insecureDevAuth", cfg.allowInsecureDevAuth,
		"bootID", bootID,
	)
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()

	shutdownSignal, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("gateway stopped", "error", err)
			os.Exit(1)
		}
	case <-shutdownSignal.Done():
		logger.Info("revoking desktop control before shutdown")
		revokeCtx, cancelRevoke := context.WithTimeout(context.Background(), 3*time.Second)
		g.controls.revokeAll(revokeCtx)
		cancelRevoke()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
		cancelShutdown()
	}
}

func newGatewayBootID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (g *gateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /api/me", g.handleMe)
	mux.HandleFunc("GET /api/screenshot", g.handleScreenshot)
	mux.HandleFunc("POST /api/control/acquire", g.handleControlAcquire)
	mux.HandleFunc("POST /api/control/release", g.handleControlRelease)
	mux.HandleFunc("POST /api/agent/screenshot", g.handleAgentScreenshot)
	mux.HandleFunc("POST /api/agent/{action}", g.handleAgentAction)
	mux.HandleFunc("GET /api/agent/windows", g.handleAgentWindows)
	mux.HandleFunc("POST /api/power/{action}", g.handlePower)
	mux.HandleFunc("GET /api/vnc", g.handleVNC)
	mux.Handle("GET /novnc/", http.StripPrefix("/novnc/", http.FileServer(http.Dir(g.config.noVNCDir))))
	ui, err := fs.Sub(webUI, "static")
	if err != nil {
		panic(fmt.Sprintf("open embedded web UI: %v", err))
	}
	mux.Handle("GET /", http.FileServer(http.FS(ui)))
	return securityHeaders(mux)
}

func loadConfig() (config, error) {
	cfg := config{
		address:              envOr("LISTEN_ADDRESS", ":8080"),
		namespace:            envOr("DESKTOP_NAMESPACE", "personal-desktop-poc"),
		typeClawInstanceUID:  strings.TrimSpace(os.Getenv("TYPECLAW_INSTANCE_UID")),
		ownerHashKey:         os.Getenv("OWNER_HASH_KEY"),
		agentTokenKey:        os.Getenv("POC_AGENT_TOKEN_KEY"),
		authProxyToken:       os.Getenv("POC_AUTH_PROXY_TOKEN"),
		devAccessToken:       os.Getenv("POC_DEV_ACCESS_TOKEN"),
		allowInsecureDevAuth: strings.EqualFold(strings.TrimSpace(os.Getenv("ALLOW_INSECURE_DEV_AUTH")), "true"),
		noVNCDir:             envOr("NOVNC_DIR", "/opt/novnc"),
		agentPort:            defaultAgentPort,
		agentAddressOverride: strings.TrimSpace(os.Getenv("DESKTOP_AGENT_ADDRESS")),
		agentToken:           strings.TrimSpace(os.Getenv("DESKTOP_AGENT_TOKEN")),
	}
	if cfg.agentToken != "" && len(cfg.agentToken) < 24 {
		return config{}, errors.New("DESKTOP_AGENT_TOKEN must contain at least 24 bytes when set")
	}
	if value := strings.TrimSpace(os.Getenv("DESKTOP_AGENT_PORT")); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return config{}, errors.New("DESKTOP_AGENT_PORT must be an integer between 1 and 65535")
		}
		cfg.agentPort = port
	}
	if value := strings.TrimSpace(os.Getenv("DESKTOP_AGENT_TIMEOUT")); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return config{}, errors.New("DESKTOP_AGENT_TIMEOUT must be a positive duration")
		}
		cfg.agentTimeout = duration
	}
	if value := strings.TrimSpace(os.Getenv("DESKTOP_AGENT_LEASE_TTL")); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return config{}, errors.New("DESKTOP_AGENT_LEASE_TTL must be a positive duration")
		}
		cfg.agentLeaseTTL = duration
	}
	if cfg.typeClawInstanceUID == "" {
		return config{}, errors.New("TYPECLAW_INSTANCE_UID is required")
	}
	if len(cfg.typeClawInstanceUID) > 253 || !instanceUIDPattern.MatchString(cfg.typeClawInstanceUID) {
		return config{}, errors.New("TYPECLAW_INSTANCE_UID contains unsupported characters")
	}
	if len(cfg.namespace) > 63 || !dnsLabelPattern.MatchString(cfg.namespace) {
		return config{}, errors.New("DESKTOP_NAMESPACE must be a lowercase DNS label of at most 63 characters")
	}
	if len(cfg.ownerHashKey) < 32 {
		return config{}, errors.New("OWNER_HASH_KEY must contain at least 32 bytes")
	}
	if len(cfg.agentTokenKey) < 32 {
		return config{}, errors.New("POC_AGENT_TOKEN_KEY must contain at least 32 bytes")
	}
	if len(cfg.authProxyToken) < 24 {
		return config{}, errors.New("POC_AUTH_PROXY_TOKEN must contain at least 24 bytes")
	}
	if cfg.allowInsecureDevAuth && len(cfg.devAccessToken) < 32 {
		return config{}, errors.New("POC_DEV_ACCESS_TOKEN must contain at least 32 bytes when ALLOW_INSECURE_DEV_AUTH=true")
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func (g *gateway) authenticate(r *http.Request) (identity, error) {
	issuer := r.Header.Get("X-Personal-Desktop-Issuer")
	subject := r.Header.Get("X-Personal-Desktop-Subject")
	authMode := ""
	if issuer == "" && subject == "" && g.config.allowInsecureDevAuth &&
		isLoopbackRequestHost(r.Host) && constantTimeEqual(r.URL.Query().Get("devToken"), g.config.devAccessToken) {
		issuer = r.URL.Query().Get("issuer")
		subject = r.URL.Query().Get("subject")
		authMode = "INSECURE-loopback-query-parameters"
	}
	if issuer == "" || subject == "" || len(issuer) > 512 || len(subject) > 512 || strings.ContainsAny(issuer+subject, "\r\n") {
		return identity{}, errNotAuthenticated
	}

	actor := actorHuman
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		const prefix = "Bearer "
		expected := agentToken(g.config.agentTokenKey, issuer, subject, g.config.typeClawInstanceUID)
		if !strings.HasPrefix(authorization, prefix) || !constantTimeEqual(strings.TrimPrefix(authorization, prefix), expected) {
			return identity{}, errNotAuthenticated
		}
		actor = actorAgent
		authMode = "owner-scoped-agent-bearer"
	} else if authMode == "INSECURE-loopback-query-parameters" {
		// Explicitly enabled local-development path. Never expose this mode.
	} else if constantTimeEqual(r.Header.Get("X-Personal-Desktop-Proxy-Token"), g.config.authProxyToken) {
		authMode = "trusted-oidc-proxy"
	} else {
		return identity{}, errNotAuthenticated
	}

	return identity{
		issuer:      issuer,
		subject:     subject,
		actor:       actor,
		authMode:    authMode,
		desktopName: desktopName(g.config.ownerHashKey, issuer, subject, g.config.typeClawInstanceUID),
	}, nil
}

func constantTimeEqual(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func desktopName(key, issuer, subject, instanceUID string) string {
	canonical := "v1\n" + issuer + "\n" + subject + "\n" + instanceUID + "\n"
	digest := hmac.New(sha256.New, []byte(key))
	_, _ = digest.Write([]byte(canonical))
	return "pd-" + hex.EncodeToString(digest.Sum(nil))[:20]
}

func agentToken(key, issuer, subject, instanceUID string) string {
	canonical := "agent-v1\n" + issuer + "\n" + subject + "\n" + instanceUID + "\n"
	digest := hmac.New(sha256.New, []byte(key))
	_, _ = digest.Write([]byte(canonical))
	return hex.EncodeToString(digest.Sum(nil))
}

// desktopAgentToken is the bearer the Gateway presents to the desktop-agent
// HTTP service inside the guest VM. Both sides derive it from OWNER_HASH_KEY
// with a distinct canonical prefix, so no additional secret is distributed.
func desktopAgentToken(key, issuer, subject, instanceUID string) string {
	canonical := "desktop-agent-v1\n" + issuer + "\n" + subject + "\n" + instanceUID + "\n"
	digest := hmac.New(sha256.New, []byte(key))
	_, _ = digest.Write([]byte(canonical))
	return hex.EncodeToString(digest.Sum(nil))
}

func (g *gateway) handleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := g.requireIdentity(w, r)
	if !ok {
		return
	}
	readCtx, cancelRead := context.WithTimeout(r.Context(), g.effectiveStatusTimeout())
	defer cancelRead()

	response := map[string]any{
		"desktopName":   id.desktopName,
		"namespace":     g.config.namespace,
		"actor":         id.actor,
		"authMode":      id.authMode,
		"gatewayBootID": g.bootID,
		"experimental":  true,
		"persistence":   "whole-root PVC survives VM stop/start; memory and running processes do not",
	}

	vm, err := g.virt.VirtualMachine(g.config.namespace).Get(readCtx, id.desktopName, metav1.GetOptions{})
	switch {
	case err == nil:
		response["vmExists"] = true
		response["vmPrintableStatus"] = string(vm.Status.PrintableStatus)
	case apierrors.IsNotFound(err):
		response["vmExists"] = false
	default:
		writeError(w, kubeVirtReadStatus(err), "read VirtualMachine status", err)
		return
	}

	vmi, err := g.virt.VirtualMachineInstance(g.config.namespace).Get(readCtx, id.desktopName, metav1.GetOptions{})
	switch {
	case err == nil:
		response["vmiExists"] = true
		response["vmiPhase"] = string(vmi.Status.Phase)
		response["vmiUID"] = string(vmi.UID)
	case apierrors.IsNotFound(err):
		response["vmiExists"] = false
	default:
		writeError(w, kubeVirtReadStatus(err), "read VirtualMachineInstance status", err)
		return
	}

	owner, generation, active := g.controls.status(id.desktopName)
	response["controlActive"] = active
	response["controlGeneration"] = generation
	if active {
		response["controlActor"] = owner
	}
	controlBlocked, powerActive, powerAction := g.controls.powerStatus(id.desktopName)
	response["controlBlocked"] = controlBlocked
	response["powerOperationActive"] = powerActive
	response["powerRecoveryRequired"] = controlBlocked && !powerActive
	if powerActive {
		response["powerOperationAction"] = powerAction
	}
	writeJSON(w, http.StatusOK, response)
}

func (g *gateway) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	id, ok := g.requireIdentity(w, r)
	if !ok {
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
	vmiBefore, err := g.virt.VirtualMachineInstance(g.config.namespace).Get(readCtx, id.desktopName, metav1.GetOptions{})
	if err != nil {
		writeError(w, screenshotReadStatus(err), "desktop framebuffer is not ready", err)
		return
	}
	if vmiBefore.Status.Phase != kubevirtv1.Running || vmiBefore.DeletionTimestamp != nil {
		writeError(w, http.StatusConflict, "desktop framebuffer is not ready", errors.New("VMI is not stably running"))
		return
	}
	image, err := g.virt.VirtualMachineInstance(g.config.namespace).Screenshot(
		readCtx, id.desktopName, &kubevirtv1.ScreenshotOptions{MoveCursor: false},
	)
	if err != nil {
		writeError(w, screenshotReadStatus(err), "desktop framebuffer is not ready", err)
		return
	}
	vmiAfter, err := g.virt.VirtualMachineInstance(g.config.namespace).Get(readCtx, id.desktopName, metav1.GetOptions{})
	if err != nil {
		writeError(w, screenshotReadStatus(err), "re-read VirtualMachineInstance after framebuffer capture", err)
		return
	}
	if vmiAfter.UID != vmiBefore.UID || vmiAfter.Status.Phase != kubevirtv1.Running || vmiAfter.DeletionTimestamp != nil {
		writeError(w, http.StatusConflict, "desktop framebuffer changed; observe again", errors.New("VMI changed while the framebuffer was captured"))
		return
	}
	encoded, contentType, frameWidth, frameHeight, encodedWidth, encodedHeight, err := transformScreenshot(readCtx, image, r.URL.Query())
	if err != nil {
		writeError(w, screenshotTransformStatus(err), "transform desktop screenshot", err)
		return
	}
	if err := readCtx.Err(); err != nil {
		writeError(w, screenshotReadStatus(err), "desktop framebuffer deadline exceeded", err)
		return
	}
	g.writeScreenshotResponse(w, encoded, contentType, frameWidth, frameHeight, encodedWidth, encodedHeight, string(vmiAfter.UID), id.desktopName)
}

func kubeVirtReadStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

func screenshotReadStatus(err error) int {
	switch {
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		return http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled),
		apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err), apierrors.IsTooManyRequests(err):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func screenshotTransformStatus(err error) int {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable
	case errors.Is(err, errInvalidScreenshotQuery):
		return http.StatusBadRequest
	case errors.Is(err, errScreenshotCannotFit):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadGateway
	}
}

func (g *gateway) writeScreenshotResponse(
	w http.ResponseWriter,
	encoded []byte,
	contentType string,
	frameWidth, frameHeight, encodedWidth, encodedHeight int,
	vmiUID, desktop string,
) {
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
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Framebuffer-Width", strconv.Itoa(frameWidth))
	w.Header().Set("X-Framebuffer-Height", strconv.Itoa(frameHeight))
	w.Header().Set("X-Encoded-Width", strconv.Itoa(encodedWidth))
	w.Header().Set("X-Encoded-Height", strconv.Itoa(encodedHeight))
	w.Header().Set("X-VMI-UID", vmiUID)
	w.Header().Set("X-Gateway-Boot-ID", g.bootID)
	_, generation, _ := g.controls.status(desktop)
	w.Header().Set("X-Control-Generation", strconv.FormatUint(generation, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(encoded); err != nil {
		if g.logger != nil {
			g.logger.Warn("write screenshot response", "desktop", desktop, "error", err)
		}
		return
	}
	if err := responseController.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) && g.logger != nil {
		g.logger.Warn("flush screenshot response", "desktop", desktop, "error", err)
	}
}

// requireAgentIdentity admits only owner-scoped agent bearers. Human browser
// identities have their own paths (web UI, RFB takeover) and must never reach
// the guest agent.
func (g *gateway) requireAgentIdentity(w http.ResponseWriter, r *http.Request) (identity, bool) {
	id, ok := g.requireIdentity(w, r)
	if !ok {
		return identity{}, false
	}
	if id.actor != actorAgent {
		writeError(w, http.StatusForbidden, "an agent bearer credential is required", nil)
		return identity{}, false
	}
	return id, true
}

func (g *gateway) handleControlAcquire(w http.ResponseWriter, r *http.Request) {
	id, ok := g.requireAgentIdentity(w, r)
	if !ok {
		return
	}
	if !requireMutationOrigin(w, r, id, g.config.allowInsecureDevAuth) {
		return
	}
	granted, err := g.controls.acquireAgent(id.desktopName, g.effectiveAgentLeaseTTL())
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
		"desktopName":       id.desktopName,
		"gatewayBootID":     g.bootID,
		"controlGeneration": granted.generation,
		"actor":             string(id.actor),
		"leaseTtlSeconds":   int(g.effectiveAgentLeaseTTL().Seconds()),
	})
}

func (g *gateway) handleControlRelease(w http.ResponseWriter, r *http.Request) {
	id, ok := g.requireAgentIdentity(w, r)
	if !ok {
		return
	}
	if !requireMutationOrigin(w, r, id, g.config.allowInsecureDevAuth) {
		return
	}
	_, _, active := g.controls.status(id.desktopName)
	if active && !g.controls.releaseAgentOwner(id.desktopName) {
		writeError(w, http.StatusConflict, "a human owns input; the agent cannot release it", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"desktopName": id.desktopName, "released": true})
}

var agentInputActions = map[string]bool{"click": true, "type": true, "key": true, "scroll": true, "launch": true}

func (g *gateway) handleAgentAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if !agentInputActions[action] {
		writeError(w, http.StatusNotFound, "unknown agent action", nil)
		return
	}
	id, ok := g.requireAgentIdentity(w, r)
	if !ok {
		return
	}
	if !requireMutationOrigin(w, r, id, g.config.allowInsecureDevAuth) {
		return
	}
	owner, _, active := g.controls.status(id.desktopName)
	if !active || owner != actorAgent {
		writeError(w, http.StatusConflict, "agent input control is not held; call desktop_acquire first", nil)
		return
	}
	g.controls.touchAgent(id.desktopName)
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read agent action body", err)
		return
	}
	status, body := g.proxyAgent(r.Context(), id, http.MethodPost, "/"+action, payload)
	proxyAgentResponse(w, status, body)
}

func (g *gateway) handleAgentScreenshot(w http.ResponseWriter, r *http.Request) {
	id, ok := g.requireAgentIdentity(w, r)
	if !ok {
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
	vmi, err := g.virt.VirtualMachineInstance(g.config.namespace).Get(readCtx, id.desktopName, metav1.GetOptions{})
	if err != nil {
		writeError(w, screenshotReadStatus(err), "desktop framebuffer is not ready", err)
		return
	}
	if vmi.Status.Phase != kubevirtv1.Running || vmi.DeletionTimestamp != nil {
		writeError(w, http.StatusConflict, "desktop framebuffer is not ready", errors.New("VMI is not stably running"))
		return
	}
	status, payload := g.proxyAgent(readCtx, id, http.MethodPost, "/screenshot", nil)
	if status != http.StatusOK {
		proxyAgentResponse(w, status, payload)
		return
	}
	encoded, contentType, frameWidth, frameHeight, encodedWidth, encodedHeight, err := transformScreenshot(readCtx, payload, r.URL.Query())
	if err != nil {
		writeError(w, screenshotTransformStatus(err), "transform desktop screenshot", err)
		return
	}
	if err := readCtx.Err(); err != nil {
		writeError(w, screenshotReadStatus(err), "desktop framebuffer deadline exceeded", err)
		return
	}
	g.writeScreenshotResponse(w, encoded, contentType, frameWidth, frameHeight, encodedWidth, encodedHeight, string(vmi.UID), id.desktopName)
}

func (g *gateway) handleAgentWindows(w http.ResponseWriter, r *http.Request) {
	id, ok := g.requireAgentIdentity(w, r)
	if !ok {
		return
	}
	g.controls.touchAgent(id.desktopName)
	readCtx, cancelRead := context.WithTimeout(r.Context(), g.effectiveAgentTimeout())
	defer cancelRead()
	status, payload := g.proxyAgent(readCtx, id, http.MethodGet, "/windows", nil)
	proxyAgentResponse(w, status, payload)
}

func (g *gateway) agentBaseURL(desktop string) string {
	if g.config.agentAddressOverride != "" {
		return strings.TrimRight(g.config.agentAddressOverride, "/")
	}
	return fmt.Sprintf("http://%s-agent.%s.svc:%d", desktop, g.config.namespace, g.config.agentPort)
}

func (g *gateway) sharedAgentClient() *http.Client {
	g.agentClientOnce.Do(func() {
		g.agentClient = &http.Client{}
	})
	return g.agentClient
}

// proxyAgent forwards one typed action to the desktop-agent service in the
// guest. Agent HTTP failures keep the agent's status and body so the plugin
// can tell a deterministic tool failure (agent 4xx/5xx body without an
// outcome) from an ambiguous dispatch (gateway-authored outcome field).
func (g *gateway) proxyAgent(ctx context.Context, id identity, method, path string, body []byte) (int, []byte) {
	requestCtx, cancel := context.WithTimeout(ctx, g.effectiveAgentTimeout())
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, g.agentBaseURL(id.desktopName)+path, bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, gatewayUnknownOutcomeBody("build desktop agent request", err)
	}
	request.Header.Set("Content-Type", "application/json")
	bearer := g.config.agentToken
	if bearer == "" {
		bearer = desktopAgentToken(g.config.ownerHashKey, id.issuer, id.subject, g.config.typeClawInstanceUID)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := g.sharedAgentClient().Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return http.StatusGatewayTimeout, gatewayUnknownOutcomeBody("desktop agent did not respond in time", err)
		}
		return http.StatusBadGateway, gatewayUnknownOutcomeBody("desktop agent is unreachable", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxScreenshotRawBytes))
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
		"outcome": "UnknownOutcome",
	})
}

func mustJSONBody(value map[string]any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"error":"encode gateway response"}`)
	}
	return encoded
}

func proxyAgentResponse(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func transformScreenshot(ctx context.Context, raw []byte, query url.Values) ([]byte, string, int, int, int, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", 0, 0, 0, 0, err
	}
	if len(raw) > maxScreenshotRawBytes {
		return nil, "", 0, 0, 0, 0, fmt.Errorf("%w: raw PNG is %d bytes", errScreenshotTooLarge, len(raw))
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", 0, 0, 0, 0, fmt.Errorf("decode KubeVirt PNG metadata: %w", err)
	}
	if configuration.Width <= 0 || configuration.Height <= 0 ||
		configuration.Width > maxFramebufferDimension || configuration.Height > maxFramebufferDimension ||
		configuration.Width > maxFramebufferPixels/configuration.Height {
		return nil, "", 0, 0, 0, 0, fmt.Errorf(
			"%w: framebuffer is %dx%d",
			errScreenshotTooLarge,
			configuration.Width,
			configuration.Height,
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, "", 0, 0, 0, 0, err
	}
	if query.Get("format") == "" || query.Get("format") == "png" {
		return raw, "image/png", configuration.Width, configuration.Height, configuration.Width, configuration.Height, nil
	}
	if query.Get("format") != "jpeg" {
		return nil, "", 0, 0, 0, 0, fmt.Errorf("%w: format must be png or jpeg", errInvalidScreenshotQuery)
	}

	maxWidth, err := boundedInt(query.Get("maxWidth"), 1024, 320, 1600, "maxWidth")
	if err != nil {
		return nil, "", 0, 0, 0, 0, err
	}
	quality, err := boundedInt(query.Get("quality"), 65, 30, 85, "quality")
	if err != nil {
		return nil, "", 0, 0, 0, 0, err
	}
	maxBytes, err := boundedInt(query.Get("maxBytes"), 180_000, 50_000, 500_000, "maxBytes")
	if err != nil {
		return nil, "", 0, 0, 0, 0, err
	}

	source, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", 0, 0, 0, 0, fmt.Errorf("decode KubeVirt PNG: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, "", 0, 0, 0, 0, err
	}
	width := configuration.Width
	height := configuration.Height
	if width > maxWidth {
		height = max(1, height*maxWidth/width)
		width = maxWidth
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, "", 0, 0, 0, 0, err
		}
		resized, err := nearestNeighbor(ctx, source, width, height)
		if err != nil {
			return nil, "", 0, 0, 0, 0, err
		}
		for {
			if err := ctx.Err(); err != nil {
				return nil, "", 0, 0, 0, 0, err
			}
			var output bytes.Buffer
			if err := jpeg.Encode(&output, resized, &jpeg.Options{Quality: quality}); err != nil {
				return nil, "", 0, 0, 0, 0, fmt.Errorf("encode JPEG: %w", err)
			}
			if err := ctx.Err(); err != nil {
				return nil, "", 0, 0, 0, 0, err
			}
			if output.Len() <= maxBytes {
				return output.Bytes(), "image/jpeg", configuration.Width, configuration.Height, width, height, nil
			}
			if quality <= 35 {
				break
			}
			quality -= 10
		}
		if width <= 320 {
			return nil, "", 0, 0, 0, 0, fmt.Errorf("%w: maxBytes=%d", errScreenshotCannotFit, maxBytes)
		}
		width = max(320, width*4/5)
		height = max(1, configuration.Height*width/configuration.Width)
		quality = 55
	}
}

func boundedInt(raw string, fallback, minimum, maximum int, name string) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%w: %s must be between %d and %d", errInvalidScreenshotQuery, name, minimum, maximum)
	}
	return value, nil
}

func nearestNeighbor(ctx context.Context, source image.Image, width, height int) (*image.RGBA, error) {
	destination := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := source.Bounds()
	for y := 0; y < height; y++ {
		if y%16 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		sourceY := bounds.Min.Y + y*bounds.Dy()/height
		for x := 0; x < width; x++ {
			sourceX := bounds.Min.X + x*bounds.Dx()/width
			destination.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	return destination, nil
}

func (g *gateway) handlePower(w http.ResponseWriter, r *http.Request) {
	id, ok := g.requireIdentity(w, r)
	if !ok {
		return
	}
	if !requireMutationOrigin(w, r, id, g.config.allowInsecureDevAuth) {
		return
	}
	action := r.PathValue("action")
	if action != "start" && action != "stop" {
		writeError(w, http.StatusNotFound, "unknown power action", nil)
		return
	}
	operation, err := g.controls.beginPower(id.desktopName, action)
	if err != nil {
		if errors.Is(err, errPowerRecoveryRequired) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":          "desktop power recovery requires an explicit start",
				"detail":         err.Error(),
				"desktopName":    id.desktopName,
				"action":         action,
				"retrySafe":      false,
				"controlBlocked": true,
				"recoveryAction": "start",
			})
			return
		}
		writeError(w, http.StatusConflict, "power operation unavailable", err)
		return
	}
	powerCtx, cancelPower := context.WithTimeout(r.Context(), g.effectivePowerTimeout())
	defer cancelPower()
	switch action {
	case "start":
		err = g.virt.VirtualMachine(g.config.namespace).Start(powerCtx, id.desktopName, &kubevirtv1.StartOptions{})
	case "stop":
		err = g.virt.VirtualMachine(g.config.namespace).Stop(powerCtx, id.desktopName, &kubevirtv1.StopOptions{})
	}
	idempotent := false
	if action == "start" && apierrors.IsConflict(err) {
		vm, vmErr := g.virt.VirtualMachine(g.config.namespace).Get(powerCtx, id.desktopName, metav1.GetOptions{})
		if vmErr != nil {
			err = fmt.Errorf("confirm VirtualMachine after start conflict: %w", vmErr)
		} else if vmi, vmiErr := g.virt.VirtualMachineInstance(g.config.namespace).Get(powerCtx, id.desktopName, metav1.GetOptions{}); vmiErr != nil {
			err = fmt.Errorf("confirm VirtualMachineInstance after start conflict: %w", vmiErr)
		} else if stableRunningAfterStartConflict(vm, vmi) {
			err = nil
			idempotent = true
		}
	}
	outcome := powerSucceeded
	if err != nil {
		outcome = powerUnknown
		if definitivePowerRejection(err) {
			outcome = powerRejected
		}
	}
	g.controls.finishPower(id.desktopName, operation, outcome)
	if err != nil {
		if outcome == powerUnknown {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":          "KubeVirt power outcome is unknown; desktop control remains blocked",
				"detail":         err.Error(),
				"desktopName":    id.desktopName,
				"action":         action,
				"outcome":        "UnknownOutcome",
				"retrySafe":      false,
				"controlBlocked": true,
			})
			return
		}
		writeError(w, kubeVirtOperationStatus(err), "KubeVirt power operation failed", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"desktopName": id.desktopName, "action": action, "idempotent": idempotent})
}

func stableRunningAfterStartConflict(vm *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance) bool {
	return vm != nil &&
		vm.DeletionTimestamp == nil &&
		vm.Status.PrintableStatus == kubevirtv1.VirtualMachineStatusRunning &&
		len(vm.Status.StateChangeRequests) == 0 &&
		vmi != nil &&
		vmi.Status.Phase == kubevirtv1.Running &&
		vmi.DeletionTimestamp == nil
}

func definitivePowerRejection(err error) bool {
	switch apierrors.ReasonForError(err) {
	case metav1.StatusReasonNotFound,
		metav1.StatusReasonUnauthorized,
		metav1.StatusReasonBadRequest,
		metav1.StatusReasonInvalid,
		metav1.StatusReasonMethodNotAllowed,
		metav1.StatusReasonNotAcceptable,
		metav1.StatusReasonRequestEntityTooLarge,
		metav1.StatusReasonUnsupportedMediaType:
		return true
	default:
		return false
	}
}

func kubeVirtOperationStatus(err error) int {
	switch {
	case apierrors.IsNotFound(err):
		return http.StatusNotFound
	case apierrors.IsConflict(err):
		return http.StatusConflict
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func (g *gateway) handleVNC(w http.ResponseWriter, r *http.Request) {
	id, ok := g.requireIdentity(w, r)
	if !ok {
		return
	}
	if !requireMutationOrigin(w, r, id, g.config.allowInsecureDevAuth) {
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
	vmi, err := g.virt.VirtualMachineInstance(g.config.namespace).Get(readinessCtx, id.desktopName, metav1.GetOptions{})
	cancelReadiness()
	if err != nil {
		status := kubeVirtReadStatus(err)
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			status = http.StatusConflict
		}
		writeError(w, status, "desktop is not ready for control", err)
		return
	}
	if vmi.Status.Phase != kubevirtv1.Running || vmi.DeletionTimestamp != nil {
		writeError(w, http.StatusConflict, "desktop is not ready for control", errors.New("VMI is not stably running"))
		return
	}

	granted, err := g.controls.acquire(relayCtx, id.desktopName, id.actor, takeover)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, errRevocationTimed) {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, "input control unavailable", err)
		return
	}
	defer g.controls.release(id.desktopName, granted)

	openCtx, cancelOpen := context.WithTimeout(granted.ctx, 10*time.Second)
	stream, err := openKubeVirtVNC(openCtx, g.virt.Config(), g.config.namespace, id.desktopName)
	cancelOpen()
	if err != nil {
		writeError(w, vncStreamHTTPStatus(err), "open KubeVirt VNC stream", err)
		return
	}

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 5 * time.Second,
		Subprotocols:     []string{"binary"},
		CheckOrigin: func(request *http.Request) bool {
			return mutationOriginAllowed(request, id, g.config.allowInsecureDevAuth)
		},
	}
	w.Header().Set("X-Control-Generation", fmt.Sprint(granted.generation))
	w.Header().Set("X-Gateway-Boot-ID", g.bootID)
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		closeUnstartedStream(stream)
		return
	}
	client.SetReadLimit(1 << 20)
	_ = client.SetReadDeadline(time.Now().Add(45 * time.Second))
	client.SetPongHandler(func(string) error {
		return client.SetReadDeadline(time.Now().Add(45 * time.Second))
	})

	g.logger.Info("desktop control granted", "desktop", id.desktopName, "actor", id.actor, "generation", granted.generation)
	err = relayVNC(client, stream, granted)
	relayCancel()
	g.logger.Info("desktop control released", "desktop", id.desktopName, "actor", id.actor, "generation", granted.generation, "reason", fmt.Sprint(err))
}

type vncDialRoundTripper struct {
	dialer     *websocket.Dialer
	connection chan<- *websocket.Conn
	done       <-chan struct{}
}

func (d *vncDialRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	connection, response, err := d.dialer.DialContext(request.Context(), request.URL.String(), request.Header)
	if err != nil {
		return response, err
	}
	select {
	case d.connection <- connection:
	case <-request.Context().Done():
		_ = connection.Close()
		return response, request.Context().Err()
	}
	<-d.done
	_ = connection.Close()
	return response, nil
}

type vncOpenError struct {
	statusCode int
	err        error
}

func (e *vncOpenError) Error() string      { return e.err.Error() }
func (e *vncOpenError) Unwrap() error      { return e.err }
func (e *vncOpenError) GetStatusCode() int { return e.statusCode }

func openKubeVirtVNC(ctx context.Context, config *rest.Config, namespace, name string) (kvcorev1.StreamInterface, error) {
	if config == nil {
		return nil, errors.New("KubeVirt REST config is unavailable")
	}
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, fmt.Errorf("build KubeVirt VNC TLS config: %w", err)
	}
	proxy := http.ProxyFromEnvironment
	if config.Proxy != nil {
		proxy = config.Proxy
	}
	done := make(chan struct{})
	connections := make(chan *websocket.Conn)
	dialer := &websocket.Dialer{
		Proxy:            proxy,
		TLSClientConfig:  tlsConfig,
		HandshakeTimeout: 10 * time.Second,
		WriteBufferSize:  kvcorev1.WebsocketMessageBufferSize,
		ReadBufferSize:   kvcorev1.WebsocketMessageBufferSize,
		Subprotocols:     []string{subresources.PlainStreamProtocolName},
	}
	roundTripper, err := rest.HTTPWrappersForConfig(config, &vncDialRoundTripper{
		dialer: dialer, connection: connections, done: done,
	})
	if err != nil {
		return nil, fmt.Errorf("build KubeVirt VNC transport: %w", err)
	}
	query := url.Values{"preserveSession": {"true"}}
	request, err := kvcorev1.RequestFromConfig(config, "virtualmachineinstances", name, namespace, "vnc", query)
	if err != nil {
		return nil, fmt.Errorf("build KubeVirt VNC request: %w", err)
	}
	request = request.WithContext(ctx)
	errorsFromDial := make(chan error, 1)
	go func() {
		response, roundTripErr := roundTripper.RoundTrip(request)
		if roundTripErr == nil {
			errorsFromDial <- nil
			return
		}
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
			roundTripErr = kvcorev1.EnrichError(roundTripErr, response)
			if response.Body != nil {
				_ = response.Body.Close()
			}
		}
		errorsFromDial <- &vncOpenError{statusCode: statusCode, err: roundTripErr}
	}()

	select {
	case connection := <-connections:
		stream := kvcorev1.NewWebsocketStreamer(connection, done)
		if err := ctx.Err(); err != nil {
			closeUnstartedStream(stream)
			return nil, err
		}
		return stream, nil
	case dialErr := <-errorsFromDial:
		// DialContext may report its socket timeout concurrently with the
		// caller context. Preserve the caller-visible cancellation contract.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var timedOut interface{ Timeout() bool }
		if deadline, ok := ctx.Deadline(); ok &&
			errors.As(dialErr, &timedOut) && timedOut.Timeout() &&
			time.Until(deadline) <= 10*time.Millisecond {
			return nil, context.DeadlineExceeded
		}
		if dialErr == nil {
			return nil, errors.New("KubeVirt VNC transport ended before a connection was established")
		}
		return nil, dialErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func closeUnstartedStream(stream kvcorev1.StreamInterface) {
	_ = stream.AsConn().Close()
	_ = stream.Stream(kvcorev1.StreamOptions{In: bytes.NewReader(nil), Out: io.Discard})
}

func relayVNC(client *websocket.Conn, stream kvcorev1.StreamInterface, granted *controller) error {
	upstream := stream.AsConn()
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
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-granted.ctx.Done():
				_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "control revoked"), time.Now().Add(time.Second))
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

	return stream.Stream(kvcorev1.StreamOptions{
		In:  &binaryWebSocketReader{conn: client},
		Out: &binaryWebSocketWriter{conn: client},
	})
}

type binaryWebSocketReader struct {
	conn    *websocket.Conn
	current io.Reader
}

func (r *binaryWebSocketReader) Read(buffer []byte) (int, error) {
	for {
		if r.current == nil {
			messageType, reader, err := r.conn.NextReader()
			if err != nil {
				return 0, err
			}
			if messageType != websocket.BinaryMessage {
				continue
			}
			r.current = reader
		}
		count, err := r.current.Read(buffer)
		if errors.Is(err, io.EOF) {
			r.current = nil
			if count > 0 {
				return count, nil
			}
			continue
		}
		return count, err
	}
}

type binaryWebSocketWriter struct {
	conn *websocket.Conn
}

func (w *binaryWebSocketWriter) Write(payload []byte) (int, error) {
	if err := w.conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		return 0, err
	}
	return len(payload), nil
}

type httpStatusCoder interface {
	GetStatusCode() int
}

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
		// These are Gateway ServiceAccount failures, not caller auth failures.
		return http.StatusBadGateway
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func sameOrigin(r *http.Request, allowInsecureDevAuth bool) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // TypeClaw/non-browser RFB clients do not send Origin.
	}
	parsed, ok := matchingRequestOrigin(origin, r.Host)
	if !ok {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	return parsed.Scheme == "http" && allowInsecureDevAuth && isLoopbackHost(parsed.Hostname())
}

func matchingRequestOrigin(origin, requestHost string) (*url.URL, bool) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, false
	}
	if !strings.EqualFold(parsed.Host, requestHost) {
		return nil, false
	}
	return parsed, true
}

func mutationOriginAllowed(r *http.Request, id identity, allowInsecureDevAuth bool) bool {
	origin := r.Header.Get("Origin")
	if id.actor == actorAgent && id.authMode == "owner-scoped-agent-bearer" {
		if origin == "" {
			return true
		}
		parsed, ok := matchingRequestOrigin(origin, r.Host)
		return ok && (parsed.Scheme == "https" || parsed.Scheme == "http")
	}
	return origin != "" && sameOrigin(r, allowInsecureDevAuth)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackRequestHost(requestHost string) bool {
	parsed, err := url.Parse("//" + requestHost)
	return err == nil && parsed.User == nil && parsed.Hostname() != "" && isLoopbackHost(parsed.Hostname())
}

func requireMutationOrigin(w http.ResponseWriter, r *http.Request, id identity, allowInsecureDevAuth bool) bool {
	if !mutationOriginAllowed(r, id, allowInsecureDevAuth) {
		writeError(w, http.StatusForbidden, "same-origin browser request required", nil)
		return false
	}
	return true
}

func (g *gateway) requireIdentity(w http.ResponseWriter, r *http.Request) (identity, bool) {
	id, err := g.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required", err)
		return identity{}, false
	}
	return id, true
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

func init() {
	// FileServer redirects directories based on OS paths; keep the configured
	// noVNC root absolute so a container working-directory change cannot alter it.
	if value := os.Getenv("NOVNC_DIR"); value != "" && !filepath.IsAbs(value) {
		fmt.Fprintln(os.Stderr, "NOVNC_DIR must be an absolute path")
		os.Exit(2)
	}
}
