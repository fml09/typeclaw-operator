// Command credential-runner executes one approved typed credential operation.
// It is intentionally not a shell or HTTP proxy: the only operation compiled
// into this binary is github.createIssue, and the only credential input is the
// one file projected by the Runner Job.
package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	v1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/credential"
)

const (
	operationEnv  = "TYPECLAW_CREDENTIAL_OPERATION"
	repositoryEnv = "TYPECLAW_GITHUB_REPOSITORY"
	titleEnv      = "TYPECLAW_GITHUB_TITLE"
	bodyEnv       = "TYPECLAW_GITHUB_BODY"
	credentialEnv = "TYPECLAW_CREDENTIAL_FILE"
)

func main() {
	if err := run(); err != nil {
		// Never print err: upstream messages and paths can contain credential
		// material. run has already emitted only a bounded result code.
		os.Exit(1)
	}
}

func run() error {
	if os.Getenv(operationEnv) != string(v1alpha1.CredentialOperationGitHubCreateIssue) {
		return writeResult(credential.RunnerResult{ErrorCode: credential.ErrorCodeRunnerFailed})
	}
	operation := v1alpha1.GitHubCreateIssueSpec{
		Repository: os.Getenv(repositoryEnv),
		Title:      os.Getenv(titleEnv),
		Body:       os.Getenv(bodyEnv),
	}
	if err := credential.ValidateGitHubCreateIssue(operation.Repository, operation.Title, operation.Body); err != nil {
		return writeResult(credential.RunnerResult{ErrorCode: credential.ErrorCodeResultInvalid})
	}
	path := os.Getenv(credentialEnv)
	if path == "" {
		path = credential.RunnerCredentialFile
	}
	token, err := os.ReadFile(path)
	if err != nil {
		return writeResult(credential.RunnerResult{ErrorCode: credential.ErrorCodeCredentialUnavailable})
	}
	defer zero(token)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := (credential.GitHubClient{}).CreateIssue(ctx, strings.TrimSpace(string(token)), operation)
	if err != nil {
		code := credential.ErrorCodeRunnerFailed
		if errors.Is(err, credential.ErrUpstream) {
			code = credential.ErrorCodeNetworkUnavailable
		}
		return writeResult(credential.RunnerResult{ErrorCode: code})
	}
	return writeResult(credential.RunnerResult{Result: &result})
}

func writeResult(result credential.RunnerResult) error {
	encoded, err := credential.EncodeRunnerResult(result)
	if err != nil {
		return err
	}
	return os.WriteFile(credential.RunnerResultPath, encoded, 0o600)
}

func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
