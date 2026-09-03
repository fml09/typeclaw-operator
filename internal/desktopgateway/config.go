// Package desktopgateway implements the Desktop Gateway: the operator-managed
// service that fronts exactly one Personal Desktop. It relays the human
// Desktop Console over VNC, serves view-only KubeVirt screenshots, proxies
// typed actions to the Guest Desktop Agent, and arbitrates the exclusive
// Input Controller lease between the human owner and the agent.
//
// The process listens twice. The agent listener carries the plugin bearer and
// nothing else; the console listener carries a human identity asserted by the
// access provider and never accepts the agent bearer. Both share one control
// registry, so a lease taken on either listener is visible on the other.
package desktopgateway

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ConsoleAuthMode selects how the console listener establishes the human
// identity behind a request.
type ConsoleAuthMode string

const (
	// ConsoleAuthTailscale trusts the Tailscale operator proxy's asserted
	// login header. It is the only mode the shipped Ingress path uses.
	ConsoleAuthTailscale ConsoleAuthMode = "tailscale"
	// ConsoleAuthTrustedProxy trusts an authenticating proxy that presents a
	// shared token alongside the issuer/subject it verified.
	ConsoleAuthTrustedProxy ConsoleAuthMode = "trusted-proxy"
	// ConsoleAuthDev authenticates from a query parameter and is only ever
	// permitted on a loopback listener.
	ConsoleAuthDev ConsoleAuthMode = "dev"
)

const (
	// DefaultAgentListenAddress carries the plugin API and /healthz.
	DefaultAgentListenAddress = ":8080"
	// DefaultConsoleListenAddress carries the Desktop Console.
	DefaultConsoleListenAddress = ":8081"
	// DefaultGuestAgentPort is the Guest Desktop Agent's fixed port.
	DefaultGuestAgentPort = 9876
	// DefaultNoVNCDir is where the operator image stages noVNC.
	DefaultNoVNCDir = "/opt/novnc"

	defaultStatusTimeout          = 5 * time.Second
	defaultScreenshotTimeout      = 12 * time.Second
	defaultScreenshotWriteTimeout = 5 * time.Second
	defaultVNCReadinessTimeout    = 5 * time.Second
	defaultPowerTimeout           = 15 * time.Second
	defaultScreenshotConcurrency  = 3
	defaultGuestAgentTimeout      = 45 * time.Second
	defaultAgentLeaseTTL          = 120 * time.Second

	minTokenBytes    = 24
	minDevTokenBytes = 32
	maxIdentityBytes = 512
)

// dnsLabelPattern bounds DESKTOP_NAME and DESKTOP_NAMESPACE. Both are
// interpolated into KubeVirt subresource paths and into the Guest Desktop
// Agent's Service address, so a value that is not a DNS label could reshape a
// request path instead of naming an object.
var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Config is the Desktop Gateway's whole runtime configuration. Every field
// arrives from the operator-rendered Deployment environment; the gateway
// derives no names and no credentials of its own.
type Config struct {
	AgentListenAddress   string
	ConsoleListenAddress string

	Namespace string
	Name      string
	OS        string

	OwnerIssuer  string
	OwnerSubject string

	ConsoleAuthMode ConsoleAuthMode
	AllowedLogins   []string
	AuthProxyToken  string
	DevAccessToken  string

	AgentToken      string
	GuestAgentToken string

	GuestAgentAddress string
	GuestAgentTimeout time.Duration
	AgentLeaseTTL     time.Duration

	ConsoleURL string
	NoVNCDir   string

	StatusTimeout         time.Duration
	ScreenshotTimeout     time.Duration
	PowerTimeout          time.Duration
	ScreenshotConcurrency int
}

// LoadConfig reads the environment table from §2.2 of the Personal Desktop
// contract. lookup is injected so tests never mutate process environment.
func LoadConfig(lookup func(string) string) (Config, error) {
	get := func(name string) string { return strings.TrimSpace(lookup(name)) }
	or := func(name, fallback string) string {
		if value := get(name); value != "" {
			return value
		}
		return fallback
	}

	cfg := Config{
		AgentListenAddress:   or("LISTEN_ADDRESS", DefaultAgentListenAddress),
		ConsoleListenAddress: or("CONSOLE_LISTEN_ADDRESS", DefaultConsoleListenAddress),
		Namespace:            get("DESKTOP_NAMESPACE"),
		Name:                 get("DESKTOP_NAME"),
		OS:                   strings.ToLower(or("DESKTOP_OS", "linux")),
		OwnerIssuer:          get("OWNER_ISSUER"),
		OwnerSubject:         get("OWNER_SUBJECT"),
		ConsoleAuthMode:      ConsoleAuthMode(or("CONSOLE_AUTH_MODE", string(ConsoleAuthTailscale))),
		AuthProxyToken:       get("AUTH_PROXY_TOKEN"),
		DevAccessToken:       get("DEV_ACCESS_TOKEN"),
		AgentToken:           get("AGENT_TOKEN"),
		GuestAgentToken:      get("GUEST_AGENT_TOKEN"),
		GuestAgentAddress:    get("GUEST_AGENT_ADDRESS"),
		ConsoleURL:           get("CONSOLE_URL"),
		NoVNCDir:             or("NOVNC_DIR", DefaultNoVNCDir),
	}

	if cfg.Namespace == "" || len(cfg.Namespace) > 63 || !dnsLabelPattern.MatchString(cfg.Namespace) {
		return Config{}, errors.New("DESKTOP_NAMESPACE must be a lowercase DNS label of at most 63 characters")
	}
	if cfg.Name == "" || len(cfg.Name) > 63 || !dnsLabelPattern.MatchString(cfg.Name) {
		return Config{}, errors.New("DESKTOP_NAME must be a lowercase DNS label of at most 63 characters")
	}
	switch cfg.OS {
	case "linux", "windows", "macos":
	default:
		return Config{}, errors.New("DESKTOP_OS must be linux, windows, or macos")
	}
	if err := validIdentityPart("OWNER_ISSUER", cfg.OwnerIssuer); err != nil {
		return Config{}, err
	}
	if err := validIdentityPart("OWNER_SUBJECT", cfg.OwnerSubject); err != nil {
		return Config{}, err
	}
	if len(cfg.AgentToken) < minTokenBytes {
		return Config{}, fmt.Errorf("AGENT_TOKEN must contain at least %d bytes", minTokenBytes)
	}
	if len(cfg.GuestAgentToken) < minTokenBytes {
		return Config{}, fmt.Errorf("GUEST_AGENT_TOKEN must contain at least %d bytes", minTokenBytes)
	}

	logins, err := parseAllowedLogins(get("TAILSCALE_ALLOWED_LOGINS"), cfg.OwnerSubject)
	if err != nil {
		return Config{}, err
	}
	cfg.AllowedLogins = logins

	switch cfg.ConsoleAuthMode {
	case ConsoleAuthTailscale:
	case ConsoleAuthTrustedProxy:
		if len(cfg.AuthProxyToken) < minTokenBytes {
			return Config{}, fmt.Errorf("AUTH_PROXY_TOKEN must contain at least %d bytes in trusted-proxy console auth mode", minTokenBytes)
		}
	case ConsoleAuthDev:
		if len(cfg.DevAccessToken) < minDevTokenBytes {
			return Config{}, fmt.Errorf("DEV_ACCESS_TOKEN must contain at least %d bytes in dev console auth mode", minDevTokenBytes)
		}
		if err := devConsoleAddressAllowed(cfg.ConsoleListenAddress); err != nil {
			return Config{}, err
		}
	default:
		return Config{}, errors.New("CONSOLE_AUTH_MODE must be tailscale, trusted-proxy, or dev")
	}

	if cfg.GuestAgentAddress == "" {
		cfg.GuestAgentAddress = fmt.Sprintf("http://%s-agent.%s.svc:%d", cfg.Name, cfg.Namespace, DefaultGuestAgentPort)
	}
	if !filepath.IsAbs(cfg.NoVNCDir) {
		// http.FileServer resolves a relative root against the process working
		// directory, so a container that starts elsewhere would serve a
		// different tree than the image staged.
		return Config{}, errors.New("NOVNC_DIR must be an absolute path")
	}

	durations := []struct {
		env    string
		target *time.Duration
	}{
		{"GUEST_AGENT_TIMEOUT", &cfg.GuestAgentTimeout},
		{"AGENT_LEASE_TTL", &cfg.AgentLeaseTTL},
		{"STATUS_TIMEOUT", &cfg.StatusTimeout},
		{"SCREENSHOT_TIMEOUT", &cfg.ScreenshotTimeout},
		{"POWER_TIMEOUT", &cfg.PowerTimeout},
	}
	for _, entry := range durations {
		value, err := parseDuration(entry.env, get(entry.env))
		if err != nil {
			return Config{}, err
		}
		*entry.target = value
	}

	if raw := get("SCREENSHOT_CONCURRENCY"); raw != "" {
		concurrency, err := strconv.Atoi(raw)
		if err != nil || concurrency < 1 || concurrency > 64 {
			return Config{}, errors.New("SCREENSHOT_CONCURRENCY must be an integer between 1 and 64")
		}
		cfg.ScreenshotConcurrency = concurrency
	}
	return cfg, nil
}

func validIdentityPart(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maxIdentityBytes || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s must be at most %d bytes and free of line breaks", name, maxIdentityBytes)
	}
	return nil
}

// parseAllowedLogins resolves TAILSCALE_ALLOWED_LOGINS. The list widens
// console access beyond the owner, so the owner is always a member of the
// result: a list that names other people and forgets the owner would
// otherwise lock the owner out of their own desktop, and the gateway cannot
// tell that apart from a deliberate list.
func parseAllowedLogins(raw, ownerSubject string) ([]string, error) {
	logins := []string{ownerSubject}
	if raw == "" {
		return logins, nil
	}
	listed := 0
	for _, entry := range strings.Split(raw, ",") {
		login := strings.TrimSpace(entry)
		if login == "" {
			continue
		}
		if err := validIdentityPart("TAILSCALE_ALLOWED_LOGINS", login); err != nil {
			return nil, err
		}
		listed++
		if !allowedLogin(logins, login) {
			logins = append(logins, login)
		}
	}
	if listed == 0 {
		return nil, errors.New("TAILSCALE_ALLOWED_LOGINS must list at least one login when set")
	}
	return logins, nil
}

func parseDuration(name, raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

// devConsoleAddressAllowed is the static half of ticket #19: dev mode
// authenticates from a query parameter, so its listener must not be reachable
// from the Pod network at all. The authoritative check runs against the bound
// listener in RequireLoopbackConsoleListener; this one only turns an obvious
// misconfiguration into a startup error before anything binds.
func devConsoleAddressAllowed(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("CONSOLE_LISTEN_ADDRESS must be host:port in dev console auth mode: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("CONSOLE_LISTEN_ADDRESS must bind a loopback address in dev console auth mode")
}

// RequireLoopbackConsoleListener enforces ticket #19 against the address the
// console listener actually bound. A request's Host header is written by
// whoever sent it, so any in-cluster client can claim "localhost"; the bound
// address cannot be forged. Dev mode refuses to serve unless this holds.
func RequireLoopbackConsoleListener(addr net.Addr) error {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP == nil || !tcp.IP.IsLoopback() {
		return fmt.Errorf("dev console auth mode requires a loopback console listener, bound %v", addr)
	}
	return nil
}

func effectiveTimeout(configured, fallback time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return fallback
}
