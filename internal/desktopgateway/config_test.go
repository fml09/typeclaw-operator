package desktopgateway

import (
	"net"
	"strings"
	"testing"
	"time"
)

func baseEnv() map[string]string {
	return map[string]string{
		"DESKTOP_NAMESPACE": testNamespace,
		"DESKTOP_NAME":      testName,
		"OWNER_ISSUER":      testIssuer,
		"OWNER_SUBJECT":     testSubject,
		"AGENT_TOKEN":       testAgentToken,
		"GUEST_AGENT_TOKEN": testGuestToken,
	}
}

func lookupFrom(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func TestLoadConfigDefaultsMatchTheContract(t *testing.T) {
	cfg, err := LoadConfig(lookupFrom(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentListenAddress != ":8080" || cfg.ConsoleListenAddress != ":8081" {
		t.Fatalf("listen addresses = %q and %q", cfg.AgentListenAddress, cfg.ConsoleListenAddress)
	}
	if cfg.ConsoleAuthMode != ConsoleAuthTailscale {
		t.Fatalf("console auth mode = %q, want tailscale", cfg.ConsoleAuthMode)
	}
	if len(cfg.AllowedLogins) != 1 || cfg.AllowedLogins[0] != testSubject {
		t.Fatalf("allowed logins = %v, want the owner alone", cfg.AllowedLogins)
	}
	want := "http://" + testName + "-agent." + testNamespace + ".svc:9876"
	if cfg.GuestAgentAddress != want {
		t.Fatalf("guest agent address = %q, want %q", cfg.GuestAgentAddress, want)
	}
	if cfg.OS != "linux" || cfg.NoVNCDir != DefaultNoVNCDir {
		t.Fatalf("os = %q, noVNC dir = %q", cfg.OS, cfg.NoVNCDir)
	}
}

func TestLoadConfigRejectsUnusableEnvironments(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"missing namespace", func(env map[string]string) { delete(env, "DESKTOP_NAMESPACE") }, "DESKTOP_NAMESPACE"},
		{"namespace is a path", func(env map[string]string) { env["DESKTOP_NAMESPACE"] = "a/../b" }, "DESKTOP_NAMESPACE"},
		{"name is a path", func(env map[string]string) { env["DESKTOP_NAME"] = "inst/../../secrets" }, "DESKTOP_NAME"},
		{"missing owner subject", func(env map[string]string) { delete(env, "OWNER_SUBJECT") }, "OWNER_SUBJECT"},
		{"owner subject with a line break", func(env map[string]string) { env["OWNER_SUBJECT"] = "a\nb" }, "OWNER_SUBJECT"},
		{"short agent token", func(env map[string]string) { env["AGENT_TOKEN"] = "short" }, "AGENT_TOKEN"},
		{"short guest token", func(env map[string]string) { env["GUEST_AGENT_TOKEN"] = "short" }, "GUEST_AGENT_TOKEN"},
		{"unknown console auth mode", func(env map[string]string) { env["CONSOLE_AUTH_MODE"] = "oidc" }, "CONSOLE_AUTH_MODE"},
		{"unknown os", func(env map[string]string) { env["DESKTOP_OS"] = "plan9" }, "DESKTOP_OS"},
		{
			"trusted proxy without a token",
			func(env map[string]string) { env["CONSOLE_AUTH_MODE"] = "trusted-proxy" },
			"AUTH_PROXY_TOKEN",
		},
		{
			"dev mode without a token",
			func(env map[string]string) { env["CONSOLE_AUTH_MODE"] = "dev" },
			"DEV_ACCESS_TOKEN",
		},
		{
			"relative noVNC directory",
			func(env map[string]string) { env["NOVNC_DIR"] = "novnc" },
			"NOVNC_DIR",
		},
		{
			"negative duration",
			func(env map[string]string) { env["AGENT_LEASE_TTL"] = "-1s" },
			"AGENT_LEASE_TTL",
		},
		{
			"screenshot concurrency out of range",
			func(env map[string]string) { env["SCREENSHOT_CONCURRENCY"] = "0" },
			"SCREENSHOT_CONCURRENCY",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := baseEnv()
			test.mutate(env)
			_, err := LoadConfig(lookupFrom(env))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadConfig() error = %v, want one naming %s", err, test.want)
			}
		})
	}
}

// Ticket #19: dev mode is only startable when the console listener is bound to
// loopback, so a configuration that would expose it must fail before binding.
func TestDevConsoleAuthRequiresALoopbackListenAddress(t *testing.T) {
	tests := []struct {
		address string
		wantErr bool
	}{
		{address: "127.0.0.1:8081"},
		{address: "localhost:8081"},
		{address: "[::1]:8081"},
		{address: ":8081", wantErr: true},
		{address: "0.0.0.0:8081", wantErr: true},
		{address: "10.4.5.6:8081", wantErr: true},
		{address: "8081", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			env := baseEnv()
			env["CONSOLE_AUTH_MODE"] = "dev"
			env["DEV_ACCESS_TOKEN"] = testDevToken
			env["CONSOLE_LISTEN_ADDRESS"] = test.address
			_, err := LoadConfig(lookupFrom(env))
			if test.wantErr != (err != nil) {
				t.Fatalf("LoadConfig(%q) error = %v, wantErr %v", test.address, err, test.wantErr)
			}
		})
	}
}

func TestRequireLoopbackConsoleListenerReadsTheBoundAddress(t *testing.T) {
	loopback, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer loopback.Close()
	if err := RequireLoopbackConsoleListener(loopback.Addr()); err != nil {
		t.Fatalf("loopback listener rejected: %v", err)
	}

	wildcard, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer wildcard.Close()
	if err := RequireLoopbackConsoleListener(wildcard.Addr()); err == nil {
		t.Fatal("a listener bound to every interface was accepted for dev mode")
	}

	// A Host header can claim loopback; a Unix socket address cannot be
	// mistaken for one, and neither may pass.
	if err := RequireLoopbackConsoleListener(&net.UnixAddr{Name: "/tmp/console.sock", Net: "unix"}); err == nil {
		t.Fatal("a non-TCP listener was accepted for dev mode")
	}
}

func TestLoadConfigParsesOptionalOverrides(t *testing.T) {
	env := baseEnv()
	env["LISTEN_ADDRESS"] = "127.0.0.1:9090"
	env["CONSOLE_LISTEN_ADDRESS"] = "127.0.0.1:9091"
	env["DESKTOP_OS"] = "Windows"
	env["CONSOLE_AUTH_MODE"] = "trusted-proxy"
	env["AUTH_PROXY_TOKEN"] = testProxyToken
	env["TAILSCALE_ALLOWED_LOGINS"] = "alice@example.com, bob@example.com ,"
	env["GUEST_AGENT_ADDRESS"] = "http://127.0.0.1:9876/"
	env["GUEST_AGENT_TIMEOUT"] = "10s"
	env["AGENT_LEASE_TTL"] = "30s"
	env["STATUS_TIMEOUT"] = "2s"
	env["SCREENSHOT_TIMEOUT"] = "3s"
	env["POWER_TIMEOUT"] = "4s"
	env["SCREENSHOT_CONCURRENCY"] = "7"
	env["CONSOLE_URL"] = "https://desk.tailnet.ts.net"

	cfg, err := LoadConfig(lookupFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OS != "windows" {
		t.Fatalf("os = %q, want the lowercased value", cfg.OS)
	}
	if len(cfg.AllowedLogins) != 2 || cfg.AllowedLogins[1] != "bob@example.com" {
		t.Fatalf("allowed logins = %v", cfg.AllowedLogins)
	}
	if cfg.GuestAgentTimeout != 10*time.Second || cfg.AgentLeaseTTL != 30*time.Second ||
		cfg.StatusTimeout != 2*time.Second || cfg.ScreenshotTimeout != 3*time.Second ||
		cfg.PowerTimeout != 4*time.Second {
		t.Fatalf("durations = %#v", cfg)
	}
	if cfg.ScreenshotConcurrency != 7 || cfg.ConsoleURL != "https://desk.tailnet.ts.net" {
		t.Fatalf("overrides = %#v", cfg)
	}
}

// The allowed-login list widens console access beyond the owner, so a list
// rendered from an Instance that names only other people must not lock the
// owner out of their own desktop.
func TestAllowedLoginsAlwaysIncludeTheOwner(t *testing.T) {
	env := baseEnv()
	env["TAILSCALE_ALLOWED_LOGINS"] = "bob@example.com,carol@example.com"
	cfg, err := LoadConfig(lookupFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if !allowedLogin(cfg.AllowedLogins, testSubject) {
		t.Fatalf("allowed logins = %v, want the owner to remain admitted", cfg.AllowedLogins)
	}
	for _, login := range []string{"bob@example.com", "carol@example.com"} {
		if !allowedLogin(cfg.AllowedLogins, login) {
			t.Fatalf("allowed logins = %v, want %q admitted", cfg.AllowedLogins, login)
		}
	}

	env["TAILSCALE_ALLOWED_LOGINS"] = testSubject + ", bob@example.com"
	cfg, err = LoadConfig(lookupFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedLogins) != 2 {
		t.Fatalf("allowed logins = %v, want the owner listed once", cfg.AllowedLogins)
	}
}
