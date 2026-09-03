package desktopgateway

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

var errNotAuthenticated = errors.New("missing trusted desktop identity")

// Header names the console listener reads. The tailscale header is set by the
// Tailscale Kubernetes operator's proxy; the X-Personal-Desktop-* headers are
// read only in trusted-proxy mode.
const (
	headerTailscaleLogin = "Tailscale-User-Login"
	headerForwardedProto = "X-Forwarded-Proto"
	headerProxyIssuer    = "X-Personal-Desktop-Issuer"
	headerProxySubject   = "X-Personal-Desktop-Subject"
	headerProxyToken     = "X-Personal-Desktop-Proxy-Token"
)

// Auth mode labels reported in /api/me.
const (
	authModeAgentBearer  = "agent-bearer"
	authModeTailscale    = "tailscale"
	authModeTrustedProxy = "trusted-proxy"
	authModeDev          = "INSECURE-loopback-dev-token"
)

// identity is the authenticated party behind one request. The gateway serves
// exactly one desktop and one owner, so an identity carries no desktop name:
// it only records which side of the boundary the request came from.
type identity struct {
	issuer   string
	subject  string
	login    string
	actor    actorKind
	authMode string
}

// authenticateAgent admits the plugin bearer and nothing else. It runs only on
// the agent listener, so a human identity asserted by the console's access
// provider can never reach an agent endpoint.
func (g *Gateway) authenticateAgent(r *http.Request) (identity, error) {
	const prefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, prefix) ||
		!constantTimeEqual(strings.TrimPrefix(authorization, prefix), g.config.AgentToken) {
		return identity{}, errNotAuthenticated
	}
	return identity{
		issuer:   g.config.OwnerIssuer,
		subject:  g.config.OwnerSubject,
		actor:    actorAgent,
		authMode: authModeAgentBearer,
	}, nil
}

// authenticateHuman establishes the console identity for the configured mode.
// It never inspects Authorization: the agent bearer is not a console
// credential, and accepting it here would let the plugin drive the human's
// exclusive input path.
func (g *Gateway) authenticateHuman(r *http.Request) (identity, error) {
	switch g.config.ConsoleAuthMode {
	case ConsoleAuthTailscale:
		return g.authenticateTailscale(r)
	case ConsoleAuthTrustedProxy:
		return g.authenticateTrustedProxy(r)
	case ConsoleAuthDev:
		return g.authenticateDev(r)
	default:
		return identity{}, errNotAuthenticated
	}
}

// authenticateTailscale trusts Tailscale-User-Login because the Tailscale
// Kubernetes operator's proxy deletes any client-supplied copy of that header
// before setting its own from the tailnet identity of the connection. That
// deletion is the whole trust assumption of this mode: the console port is
// reachable only from the operator namespace by NetworkPolicy, so no other
// writer of the header can reach this listener.
func (g *Gateway) authenticateTailscale(r *http.Request) (identity, error) {
	// The proxy always terminates TLS and forwards the original scheme. A
	// request that does not claim https did not come through it.
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get(headerForwardedProto)), "https") {
		return identity{}, errNotAuthenticated
	}
	login := strings.TrimSpace(r.Header.Get(headerTailscaleLogin))
	if login == "" || len(login) > maxIdentityBytes {
		return identity{}, errNotAuthenticated
	}
	if !allowedLogin(g.config.AllowedLogins, login) {
		return identity{}, errNotAuthenticated
	}
	return identity{
		issuer:   g.config.OwnerIssuer,
		subject:  login,
		login:    login,
		actor:    actorHuman,
		authMode: authModeTailscale,
	}, nil
}

// authenticateTrustedProxy accepts an authenticating proxy's asserted tuple
// only alongside the shared token, and only when the tuple is the owner's.
func (g *Gateway) authenticateTrustedProxy(r *http.Request) (identity, error) {
	if !constantTimeEqual(r.Header.Get(headerProxyToken), g.config.AuthProxyToken) {
		return identity{}, errNotAuthenticated
	}
	issuer := strings.TrimSpace(r.Header.Get(headerProxyIssuer))
	subject := strings.TrimSpace(r.Header.Get(headerProxySubject))
	if issuer == "" || subject == "" ||
		issuer != g.config.OwnerIssuer || subject != g.config.OwnerSubject {
		return identity{}, errNotAuthenticated
	}
	return identity{
		issuer:   issuer,
		subject:  subject,
		login:    subject,
		actor:    actorHuman,
		authMode: authModeTrustedProxy,
	}, nil
}

// authenticateDev is the local-development path (ticket #19). Locality is
// established by the address the console listener bound, which the process
// verified at startup; the request Host is an additional condition only,
// because any in-cluster client can send Host: localhost.
func (g *Gateway) authenticateDev(r *http.Request) (identity, error) {
	if !g.devLoopbackListener {
		return identity{}, errNotAuthenticated
	}
	if !isLoopbackRequestHost(r.Host) {
		return identity{}, errNotAuthenticated
	}
	if !constantTimeEqual(r.URL.Query().Get("devToken"), g.config.DevAccessToken) {
		return identity{}, errNotAuthenticated
	}
	return identity{
		issuer:   g.config.OwnerIssuer,
		subject:  g.config.OwnerSubject,
		login:    g.config.OwnerSubject,
		actor:    actorHuman,
		authMode: authModeDev,
	}, nil
}

func allowedLogin(allowed []string, login string) bool {
	for _, candidate := range allowed {
		if candidate == login {
			return true
		}
	}
	return false
}

func constantTimeEqual(got, want string) bool {
	if len(got) != len(want) || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// allowInsecureOrigin reports whether plain-HTTP origins may be accepted. Only
// the loopback-bound dev listener qualifies.
func (g *Gateway) allowInsecureOrigin() bool {
	return g.config.ConsoleAuthMode == ConsoleAuthDev && g.devLoopbackListener
}

func sameOrigin(r *http.Request, allowInsecureOrigin bool) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // Non-browser RFB clients do not send Origin.
	}
	parsed, ok := matchingRequestOrigin(origin, r.Host)
	if !ok {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	return parsed.Scheme == "http" && allowInsecureOrigin && isLoopbackHost(parsed.Hostname())
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

// mutationOriginAllowed is the CSRF boundary. A browser-borne human identity
// must prove same-origin; the agent bearer is not ambiently attached by a
// browser, so an absent Origin is allowed for it and a mismatched one is not.
func (g *Gateway) mutationOriginAllowed(r *http.Request, id identity) bool {
	origin := r.Header.Get("Origin")
	if id.actor == actorAgent {
		if origin == "" {
			return true
		}
		parsed, ok := matchingRequestOrigin(origin, r.Host)
		return ok && (parsed.Scheme == "https" || parsed.Scheme == "http")
	}
	return origin != "" && sameOrigin(r, g.allowInsecureOrigin())
}

func (g *Gateway) requireMutationOrigin(w http.ResponseWriter, r *http.Request, id identity) bool {
	if !g.mutationOriginAllowed(r, id) {
		writeError(w, http.StatusForbidden, "same-origin browser request required", nil)
		return false
	}
	return true
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
