package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	v1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

const GitHubAPIBaseURL = "https://api.github.com"

var ErrUpstream = errors.New("credential consumer upstream request failed")

// HTTPDoer is the small seam used to test the fixed protocol operation without
// contacting GitHub.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// GitHubClient implements only github.createIssue. The URL and all protocol
// headers are fixed here; neither is accepted from model-controlled input.
type GitHubClient struct {
	Client  HTTPDoer
	BaseURL string
}

func (c GitHubClient) CreateIssue(ctx context.Context, token string, operation v1alpha1.GitHubCreateIssueSpec) (v1alpha1.GitHubIssueResult, error) {
	if err := ValidateGitHubCreateIssue(operation.Repository, operation.Title, operation.Body); err != nil {
		return v1alpha1.GitHubIssueResult{}, err
	}
	if strings.TrimSpace(token) == "" {
		return v1alpha1.GitHubIssueResult{}, errors.New("credential is empty")
	}
	base := c.BaseURL
	if base == "" {
		base = GitHubAPIBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != "api.github.com" || parsed.Path != "" {
		return v1alpha1.GitHubIssueResult{}, errors.New("fixed GitHub API endpoint is invalid")
	}
	parts := strings.Split(operation.Repository, "/")
	endpoint := strings.TrimRight(base, "/") + "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/issues"
	payload, err := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}{Title: operation.Title, Body: operation.Body})
	if err != nil {
		return v1alpha1.GitHubIssueResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return v1alpha1.GitHubIssueResult{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "typeclaw-credential-runner")
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	client := c.Client
	if client == nil {
		client = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return v1alpha1.GitHubIssueResult{}, ErrUpstream
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return v1alpha1.GitHubIssueResult{}, ErrUpstream
	}

	limited := io.LimitReader(response.Body, 1<<20)
	var result struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
		Title   string `json:"title"`
	}
	if err := json.NewDecoder(limited).Decode(&result); err != nil {
		return v1alpha1.GitHubIssueResult{}, errors.New("GitHub returned an invalid result")
	}
	if result.Number <= 0 || result.HTMLURL == "" || result.Title == "" {
		return v1alpha1.GitHubIssueResult{}, errors.New("GitHub returned an incomplete result")
	}
	resultURL, err := url.Parse(result.HTMLURL)
	if err != nil || resultURL.Scheme != "https" || resultURL.Hostname() != "github.com" || resultURL.User != nil {
		return v1alpha1.GitHubIssueResult{}, fmt.Errorf("%w: GitHub result URL", ErrUpstream)
	}
	return v1alpha1.GitHubIssueResult{
		Number: result.Number,
		URL:    result.HTMLURL,
		Title:  result.Title,
	}, nil
}
