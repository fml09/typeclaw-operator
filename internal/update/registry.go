// Package update implements registry-driven version selection for managed
// runtime rollouts (ADR 0004 capability parity): an anonymous GHCR client,
// semver-ish tag handling, and the effective-image rule that lets an active
// promotion override spec-derived resolution.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

const (
	// DefaultBaseURL serves the OCI v2 registry API for the managed runtime
	// repository.
	DefaultBaseURL = "https://ghcr.io"
	// DefaultTokenURL issues anonymous pull tokens for GHCR.
	DefaultTokenURL = "https://ghcr.io/token"

	defaultRepository = "fml09/typeclaw-runtime"
	tagListLimit      = 1000
)

// plainSemver matches release tags only: pre-releases and floating tags are
// never rollout candidates.
var plainSemver = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// RegistryClient lists managed-runtime versions from a container registry.
// The zero value targets the public GHCR endpoints anonymously; tests inject
// BaseURL, TokenURL, and HTTPClient against an httptest server.
type RegistryClient struct {
	// BaseURL is the OCI v2 API root (no /v2 suffix).
	BaseURL string
	// TokenURL is the anonymous-token issuer endpoint.
	TokenURL string
	// HTTPClient performs both requests; nil uses http.DefaultClient.
	HTTPClient *http.Client
}

type tokenResponse struct {
	Token string `json:"token"`
}

type tagListResponse struct {
	Tags []string `json:"tags"`
}

// ListVersions returns every plain semver release tag of the managed runtime
// repository, sorted newest first. It performs the standard two-step
// anonymous flow: exchange the pull scope for a bearer token, then fetch the
// tag list with it.
func (c *RegistryClient) ListVersions(ctx context.Context) ([]string, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	tokenURL := c.TokenURL
	if tokenURL == "" {
		tokenURL = DefaultTokenURL
	}
	httpc := c.HTTPClient
	if httpc == nil {
		httpc = http.DefaultClient
	}

	token, err := c.fetchToken(ctx, httpc, tokenURL)
	if err != nil {
		return nil, fmt.Errorf("registry token exchange: %w", err)
	}
	tags, err := c.fetchTags(ctx, httpc, base, token)
	if err != nil {
		return nil, fmt.Errorf("registry tag list: %w", err)
	}
	return tags, nil
}

func (c *RegistryClient) fetchToken(ctx context.Context, httpc *http.Client, tokenURL string) (string, error) {
	endpoint, err := url.Parse(tokenURL)
	if err != nil {
		return "", fmt.Errorf("parse token URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("service", "ghcr.io")
	query.Set("scope", "repository:"+defaultRepository+":pull")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var body tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if body.Token == "" {
		return "", fmt.Errorf("token response carried no token")
	}
	return body.Token, nil
}

func (c *RegistryClient) fetchTags(ctx context.Context, httpc *http.Client, baseURL, token string) ([]string, error) {

	endpoint := strings.TrimSuffix(baseURL, "/") +
		"/v2/" + defaultRepository + "/tags/list?n=" + strconv.Itoa(tagListLimit)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var body tagListResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode tag list: %w", err)
	}

	tags := make([]string, 0, len(body.Tags))
	for _, tag := range body.Tags {
		if plainSemver.MatchString(tag) {
			tags = append(tags, tag)
		}
	}
	sort.Slice(tags, func(i, j int) bool {
		return compareVersion(tags[i], tags[j]) > 0
	})
	return tags, nil
}

// Newer reports whether release tag a is strictly newer than b. Tags that
// fail semver parsing (including "") never count as newer, so a rollout
// candidate must beat the current release outright.
func Newer(a, b string) bool { return compareVersion(a, b) > 0 }

// PickVersion selects the rollout candidate for a track: "latest" picks the
// highest overall release; any other track ("0.48") picks the highest release
// prefixed with "<track>.". Returns false when no release tag qualifies.
func PickVersion(tags []string, track string) (string, bool) {
	best := ""
	for _, tag := range tags {
		if !plainSemver.MatchString(tag) {
			continue
		}
		if track != "latest" && !strings.HasPrefix(tag, track+".") {
			continue
		}
		if best == "" || compareVersion(tag, best) > 0 {
			best = tag
		}
	}
	return best, best != ""
}

// compareVersion orders two plain "M.m.p" tags; positive when a is newer.
// Tags reaching this path already matched plainSemver, so parsing failures
// order as oldest rather than panicking on malformed input.
func compareVersion(a, b string) int {
	as, aok := parseVersion(a)
	bs, bok := parseVersion(b)
	switch {
	case !aok && !bok:
		return 0
	case !aok:
		return -1
	case !bok:
		return 1
	}
	for i := range as {
		if as[i] != bs[i] {
			if as[i] > bs[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

func parseVersion(tag string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(tag, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Phase constants alias the canonical API values so controller code can read
// update.PhaseX without a second import.
const (
	PhaseIdle           = typeclawv1alpha1.UpdatePhaseIdle
	PhaseAwaitingBackup = typeclawv1alpha1.UpdatePhaseAwaitingBackup
	PhaseUpdating       = typeclawv1alpha1.UpdatePhaseUpdating
	PhaseConfirming     = typeclawv1alpha1.UpdatePhaseConfirming
	PhaseReady          = typeclawv1alpha1.UpdatePhaseReady
	PhaseRolledBack     = typeclawv1alpha1.UpdatePhaseRolledBack
)
