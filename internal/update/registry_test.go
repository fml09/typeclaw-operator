package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// registryFake serves the two-step GHCR anonymous flow and records the
// requests it received so tests can assert token-exchange semantics.
type registryFake struct {
	server *httptest.Server

	tokenRequests  int
	tagRequests    int
	lastAuthHeader string
	lastTokenQuery string
}

func newRegistryFake(t *testing.T, tags []string) *registryFake {
	t.Helper()
	fake := &registryFake{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			fake.tokenRequests++
			fake.lastTokenQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token-123"})
		case r.URL.Path == "/v2/fml09/typeclaw-runtime/tags/list":
			fake.tagRequests++
			fake.lastAuthHeader = r.Header.Get("Authorization")
			if got := r.URL.Query().Get("n"); got != "1000" {
				t.Errorf("tag list limit n = %q, want 1000", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string][]string{"tags": tags})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func TestListVersionsExchangesTokenAndFiltersTags(t *testing.T) {
	fake := newRegistryFake(t, []string{
		"0.48.10", "latest", "0.48.9-rc1", "0.9.2", "sha-digest", "0.48.2",
	})
	client := &RegistryClient{BaseURL: fake.server.URL, TokenURL: fake.server.URL + "/token"}
	tags, err := client.ListVersions(context.Background())
	if err != nil {
		t.Fatalf("ListVersions() error: %v", err)
	}

	want := []string{"0.48.10", "0.48.2", "0.9.2"}
	if len(tags) != len(want) {
		t.Fatalf("filtered tags = %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Fatalf("sorted tags = %v, want %v", tags, want)
		}
	}

	if fake.tokenRequests != 1 || fake.tagRequests != 1 {
		t.Fatalf("request counts token=%d tags=%d, want 1/1", fake.tokenRequests, fake.tagRequests)
	}
	for _, param := range []string{"service=ghcr.io", "scope=repository%3Afml09%2Ftypeclaw-runtime%3Apull"} {
		if !strings.Contains(fake.lastTokenQuery, param) {
			t.Errorf("token query %q missing %s", fake.lastTokenQuery, param)
		}
	}
	if fake.lastAuthHeader != "Bearer test-token-123" {
		t.Errorf("tag list Authorization = %q, want bearer token from exchange", fake.lastAuthHeader)
	}
}

func TestListVersionsSurfacesRegistryErrors(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer broken.Close()

	if _, err := (&RegistryClient{BaseURL: broken.URL, TokenURL: broken.URL + "/token"}).ListVersions(context.Background()); err == nil {
		t.Fatal("ListVersions() must fail when token issuance is rejected")
	}
}

func TestPickVersionTrackSelection(t *testing.T) {
	tags := []string{"0.47.3", "0.48.2", "0.48.10", "1.2.0"}

	cases := []struct {
		name  string
		track string
		want  string
		ok    bool
	}{
		{"latest picks global max", "latest", "1.2.0", true},
		{"minor track stays in stream", "0.48", "0.48.10", true},
		{"older stream still served", "0.47", "0.47.3", true},
		{"empty track is not a valid stream", "", "", false},
		{"unknown track finds nothing", "9.9", "", false},
		{"prefix must end at dot boundary handled by caller format", "0.4", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PickVersion(tags, tc.track)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("PickVersion(%q) = (%q, %v), want (%q, %v)", tc.track, got, ok, tc.want, tc.ok)
			}
		})
	}

	if _, ok := PickVersion([]string{"latest", "beta"}, "latest"); ok {
		t.Error("non-semver-only tag lists must yield no candidate")
	}
}
