package desktopgateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentListenerAcceptsOnlyTheConfiguredBearer(t *testing.T) {
	g := newTestGateway(t, testConfig(), nil)

	request := httptest.NewRequest(http.MethodGet, "https://gateway.svc/api/me", nil)
	request.Header.Set("Authorization", "Bearer "+testAgentToken)
	id, err := g.authenticateAgent(request)
	if err != nil {
		t.Fatal(err)
	}
	if id.actor != actorAgent || id.authMode != authModeAgentBearer ||
		id.subject != testSubject || id.issuer != testIssuer {
		t.Fatalf("authenticated identity = %#v", id)
	}

	for _, header := range []string{"", "Bearer wrong-token-0123456789abcdef", "Basic " + testAgentToken, testAgentToken} {
		request.Header.Set("Authorization", header)
		if _, err := g.authenticateAgent(request); !errors.Is(err, errNotAuthenticated) {
			t.Fatalf("Authorization %q: error = %v, want %v", header, err, errNotAuthenticated)
		}
	}
}

// The two listeners carry disjoint credentials: neither one's proof of
// identity may be replayed against the other.
func TestListenersDoNotShareCredentials(t *testing.T) {
	g := newTestGateway(t, testConfig(), &fakeKubeVirt{})

	console := httptest.NewRecorder()
	agentBearerOnConsole := httptest.NewRequest(http.MethodGet, "https://desktop.example/api/me", nil)
	agentBearerOnConsole.Header.Set("Authorization", "Bearer "+testAgentToken)
	g.ConsoleHandler().ServeHTTP(console, agentBearerOnConsole)
	if console.Code != http.StatusUnauthorized {
		t.Fatalf("agent bearer on the console listener = %d, want 401", console.Code)
	}

	agent := httptest.NewRecorder()
	humanOnAgent := httptest.NewRequest(http.MethodGet, "https://gateway.svc/api/me", nil)
	humanOnAgent.Header.Set(headerForwardedProto, "https")
	humanOnAgent.Header.Set(headerTailscaleLogin, testSubject)
	g.AgentHandler().ServeHTTP(agent, humanOnAgent)
	if agent.Code != http.StatusUnauthorized {
		t.Fatalf("console identity on the agent listener = %d, want 401", agent.Code)
	}
}

func TestTailscaleConsoleAuthRequiresAnAllowedLoginOverHTTPS(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedLogins = []string{testSubject, "bob@example.com"}
	g := newTestGateway(t, cfg, nil)

	tests := []struct {
		name      string
		configure func(*http.Request)
		wantLogin string
	}{
		{
			name: "owner login",
			configure: func(r *http.Request) {
				r.Header.Set(headerForwardedProto, "https")
				r.Header.Set(headerTailscaleLogin, testSubject)
			},
			wantLogin: testSubject,
		},
		{
			name: "additional allowed login",
			configure: func(r *http.Request) {
				r.Header.Set(headerForwardedProto, "https")
				r.Header.Set(headerTailscaleLogin, "bob@example.com")
			},
			wantLogin: "bob@example.com",
		},
		{
			name: "login outside the allow-list",
			configure: func(r *http.Request) {
				r.Header.Set(headerForwardedProto, "https")
				r.Header.Set(headerTailscaleLogin, "mallory@example.com")
			},
		},
		{
			name: "no forwarded proto",
			configure: func(r *http.Request) {
				r.Header.Set(headerTailscaleLogin, testSubject)
			},
		},
		{
			name: "plain HTTP forwarded proto",
			configure: func(r *http.Request) {
				r.Header.Set(headerForwardedProto, "http")
				r.Header.Set(headerTailscaleLogin, testSubject)
			},
		},
		{
			name:      "no login header",
			configure: func(r *http.Request) { r.Header.Set(headerForwardedProto, "https") },
		},
		{
			name: "trusted-proxy headers are ignored in tailscale mode",
			configure: func(r *http.Request) {
				r.Header.Set(headerForwardedProto, "https")
				r.Header.Set(headerProxyToken, testProxyToken)
				r.Header.Set(headerProxyIssuer, testIssuer)
				r.Header.Set(headerProxySubject, testSubject)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://desktop.example/api/me", nil)
			test.configure(request)
			id, err := g.authenticateHuman(request)
			if test.wantLogin == "" {
				if !errors.Is(err, errNotAuthenticated) {
					t.Fatalf("error = %v, want %v", err, errNotAuthenticated)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id.login != test.wantLogin || id.subject != test.wantLogin ||
				id.actor != actorHuman || id.authMode != authModeTailscale || id.issuer != testIssuer {
				t.Fatalf("identity = %#v", id)
			}
		})
	}
}

func TestTrustedProxyConsoleAuthRequiresTokenAndOwnerTuple(t *testing.T) {
	cfg := testConfig()
	cfg.ConsoleAuthMode = ConsoleAuthTrustedProxy
	g := newTestGateway(t, cfg, nil)

	request := httptest.NewRequest(http.MethodGet, "https://desktop.example/api/me", nil)
	request.Header.Set(headerProxyIssuer, testIssuer)
	request.Header.Set(headerProxySubject, testSubject)
	if _, err := g.authenticateHuman(request); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("uncredentialed trusted headers: error = %v, want %v", err, errNotAuthenticated)
	}

	request.Header.Set(headerProxyToken, testProxyToken)
	id, err := g.authenticateHuman(request)
	if err != nil {
		t.Fatal(err)
	}
	if id.actor != actorHuman || id.authMode != authModeTrustedProxy {
		t.Fatalf("identity = %#v", id)
	}

	request.Header.Set(headerProxySubject, "mallory@example.com")
	if _, err := g.authenticateHuman(request); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("non-owner subject: error = %v, want %v", err, errNotAuthenticated)
	}

	// A tailscale header must not authenticate anything in trusted-proxy mode.
	tailscaleOnly := httptest.NewRequest(http.MethodGet, "https://desktop.example/api/me", nil)
	tailscaleOnly.Header.Set(headerForwardedProto, "https")
	tailscaleOnly.Header.Set(headerTailscaleLogin, testSubject)
	if _, err := g.authenticateHuman(tailscaleOnly); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("tailscale header in trusted-proxy mode: error = %v, want %v", err, errNotAuthenticated)
	}
}

// Ticket #19: a forged Host header must never be sufficient. Locality comes
// from the bound listener, and the request Host is only an extra condition.
func TestDevConsoleAuthNeedsALoopbackListenerAndTheToken(t *testing.T) {
	cfg := testConfig()
	cfg.ConsoleAuthMode = ConsoleAuthDev
	g := newTestGateway(t, cfg, nil)

	forged := httptest.NewRequest(http.MethodGet, "http://localhost:8081/api/me?devToken="+testDevToken, nil)
	forged.Host = "localhost:8081"
	if _, err := g.authenticateHuman(forged); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("forged loopback Host without a loopback listener: error = %v, want %v", err, errNotAuthenticated)
	}

	// Even a forwarded-header claim of locality changes nothing.
	forged.Header.Set("X-Forwarded-For", "127.0.0.1")
	forged.Header.Set(headerForwardedProto, "http")
	if _, err := g.authenticateHuman(forged); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("forwarded headers granted dev access: error = %v", err)
	}

	g.devLoopbackListener = true
	id, err := g.authenticateHuman(forged)
	if err != nil {
		t.Fatal(err)
	}
	if id.actor != actorHuman || id.authMode != authModeDev || id.subject != testSubject {
		t.Fatalf("identity = %#v", id)
	}

	clusterHost := httptest.NewRequest(http.MethodGet, "http://gateway.desktops.svc/api/me?devToken="+testDevToken, nil)
	clusterHost.Host = "gateway.desktops.svc"
	if _, err := g.authenticateHuman(clusterHost); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("cluster Host on a loopback listener: error = %v, want %v", err, errNotAuthenticated)
	}

	wrongToken := httptest.NewRequest(http.MethodGet, "http://localhost:8081/api/me?devToken=wrong", nil)
	wrongToken.Host = "localhost:8081"
	if _, err := g.authenticateHuman(wrongToken); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("wrong dev token: error = %v, want %v", err, errNotAuthenticated)
	}

	noToken := httptest.NewRequest(http.MethodGet, "http://localhost:8081/api/me", nil)
	noToken.Host = "localhost:8081"
	if _, err := g.authenticateHuman(noToken); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("absent dev token: error = %v, want %v", err, errNotAuthenticated)
	}
}

func TestSameOriginAllowsHTTPOnlyForTheLoopbackDevListener(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://localhost:8081/api/vnc", nil)
	request.Header.Set("Origin", "http://localhost:8081")
	if sameOrigin(request, false) {
		t.Fatal("loopback HTTP origin was accepted outside dev mode")
	}
	if !sameOrigin(request, true) {
		t.Fatal("loopback HTTP origin was rejected in dev mode")
	}

	request = httptest.NewRequest(http.MethodGet, "http://192.0.2.10:8081/api/vnc", nil)
	request.Header.Set("Origin", "http://192.0.2.10:8081")
	if sameOrigin(request, true) {
		t.Fatal("non-loopback HTTP origin was accepted in dev mode")
	}
}

func TestMutationOriginPolicy(t *testing.T) {
	tailscale := newTestGateway(t, testConfig(), nil)
	devConfig := testConfig()
	devConfig.ConsoleAuthMode = ConsoleAuthDev
	dev := newTestGateway(t, devConfig, nil)
	dev.devLoopbackListener = true

	tests := []struct {
		name      string
		gateway   *Gateway
		target    string
		configure func(*http.Request)
		want      bool
	}{
		{
			name:    "console request without an origin",
			gateway: tailscale,
			target:  "https://desktop.example/api/power/start",
			configure: func(r *http.Request) {
				r.Header.Set(headerForwardedProto, "https")
				r.Header.Set(headerTailscaleLogin, testSubject)
			},
		},
		{
			name:    "console request from a hostile origin",
			gateway: tailscale,
			target:  "https://desktop.example/api/power/start",
			configure: func(r *http.Request) {
				r.Header.Set(headerForwardedProto, "https")
				r.Header.Set(headerTailscaleLogin, testSubject)
				r.Header.Set("Origin", "https://hostile.example")
			},
		},
		{
			name:    "console request from the same origin",
			gateway: tailscale,
			target:  "https://desktop.example/api/power/start",
			configure: func(r *http.Request) {
				r.Header.Set(headerForwardedProto, "https")
				r.Header.Set(headerTailscaleLogin, testSubject)
				r.Header.Set("Origin", "https://desktop.example")
			},
			want: true,
		},
		{
			name:    "console request over plain HTTP from the same authority",
			gateway: tailscale,
			target:  "https://desktop.example/api/power/start",
			configure: func(r *http.Request) {
				r.Header.Set(headerForwardedProto, "https")
				r.Header.Set(headerTailscaleLogin, testSubject)
				r.Header.Set("Origin", "http://desktop.example")
			},
		},
		{
			name:    "dev console request over loopback HTTP",
			gateway: dev,
			target:  "http://localhost:8081/api/power/start?devToken=" + testDevToken,
			configure: func(r *http.Request) {
				r.Host = "localhost:8081"
				r.Header.Set("Origin", "http://localhost:8081")
			},
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.target, nil)
			test.configure(request)
			id, err := test.gateway.authenticateHuman(request)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			if got := test.gateway.requireMutationOrigin(response, request, id); got != test.want {
				t.Fatalf("requireMutationOrigin() = %v, want %v", got, test.want)
			}
			if !test.want && response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", response.Code)
			}
		})
	}
}

// The plugin is not a browser: it has no ambient credential to be tricked into
// sending, so an absent Origin is fine and a foreign one is still refused.
func TestAgentMutationOriginPolicy(t *testing.T) {
	g := newTestGateway(t, testConfig(), nil)
	tests := []struct {
		origin string
		host   string
		want   bool
	}{
		{origin: "", want: true},
		{origin: "https://gateway.desktops.svc", host: "gateway.desktops.svc", want: true},
		{origin: "http://gateway.desktops.svc", host: "gateway.desktops.svc", want: true},
		{origin: "https://hostile.example", host: "gateway.desktops.svc"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodPost, "http://gateway.desktops.svc/api/control/acquire", nil)
		if test.host != "" {
			request.Host = test.host
		}
		if test.origin != "" {
			request.Header.Set("Origin", test.origin)
		}
		request.Header.Set("Authorization", "Bearer "+testAgentToken)
		id, err := g.authenticateAgent(request)
		if err != nil {
			t.Fatal(err)
		}
		if got := g.mutationOriginAllowed(request, id); got != test.want {
			t.Fatalf("origin %q: mutationOriginAllowed() = %v, want %v", test.origin, got, test.want)
		}
	}
}

func TestConstantTimeEqualRefusesEmptyExpectations(t *testing.T) {
	if constantTimeEqual("", "") {
		t.Fatal("an unset credential must never match")
	}
	if !constantTimeEqual(testAgentToken, testAgentToken) {
		t.Fatal("equal tokens did not match")
	}
	if constantTimeEqual(testAgentToken+"x", testAgentToken) {
		t.Fatal("a longer token matched")
	}
}

// The console must stay reachable for the owner whatever the operator renders
// into TAILSCALE_ALLOWED_LOGINS.
func TestTailscaleConsoleAuthAdmitsTheOwnerAbsentFromTheAllowedList(t *testing.T) {
	logins, err := parseAllowedLogins("bob@example.com", testSubject)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.AllowedLogins = logins
	g := newTestGateway(t, cfg, nil)

	request := httptest.NewRequest(http.MethodGet, "https://desktop.example/api/me", nil)
	request.Header.Set(headerForwardedProto, "https")
	request.Header.Set(headerTailscaleLogin, testSubject)
	id, err := g.authenticateHuman(request)
	if err != nil {
		t.Fatalf("owner login rejected by a list that omits it: %v", err)
	}
	if id.login != testSubject || id.actor != actorHuman {
		t.Fatalf("owner identity = %#v", id)
	}
}
