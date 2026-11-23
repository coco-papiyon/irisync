package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

type githubInstallationToken struct {
	value, login string
	expiresAt    time.Time
}

type githubAppCredentials struct {
	appID, repository, apiURL string
	key                       *rsa.PrivateKey
	client                    *http.Client
	mu                        sync.Mutex
	cached                    githubInstallationToken
}

func newGitHubProject(ctx context.Context, cfg config) (githubProject, error) {
	if cfg.GitHubAuthMode != githubAuthApp {
		return githubProject{cfg: cfg}, nil
	}
	keyPEM, err := os.ReadFile(cfg.GitHubAppKeyPath)
	if err != nil {
		return githubProject{}, fmt.Errorf("read GitHub App private key: %w", err)
	}
	key, err := parseGitHubAppKey(keyPEM)
	if err != nil {
		return githubProject{}, err
	}
	app := &githubAppCredentials{
		appID: cfg.GitHubAppID, repository: cfg.Repository, apiURL: "https://api.github.com", key: key,
		client: &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.GitHubTimeout)
	defer cancel()
	token, err := app.token(ctx)
	if err != nil {
		return githubProject{}, err
	}
	cfg.GitHubLogin = token.login
	projectToken, err := storedGitHubToken(ctx, cfg.GitHubProjectLogin)
	if err != nil {
		return githubProject{}, err
	}
	return githubProject{cfg: cfg, app: app, projectToken: projectToken}, nil
}

func storedGitHubToken(ctx context.Context, login string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com", "--user", login)
	cmd.Env = slices.DeleteFunc(cmd.Environ(), func(env string) bool {
		key, _, _ := strings.Cut(env, "=")
		switch strings.ToUpper(key) {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_DEBUG":
			return true
		default:
			return false
		}
	})
	output, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(output)) == "" {
		return "", fmt.Errorf("no stored gh authentication for github_project_login %q", login)
	}
	return strings.TrimSpace(string(output)), nil
}
func parseGitHubAppKey(keyPEM []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block != nil {
		switch block.Type {
		case "RSA PRIVATE KEY":
			if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
				return key, nil
			}
		case "PRIVATE KEY":
			if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
				if rsaKey, ok := key.(*rsa.PrivateKey); ok {
					return rsaKey, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("GitHub App private key must be an unencrypted RSA PEM (PKCS#1 or PKCS#8)")
}

func (a *githubAppCredentials) jwt(now time.Time) (string, error) {
	claims, err := json.Marshal(map[string]any{
		"iss": a.appID,
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	encoding := base64.RawURLEncoding
	message := encoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return message + "." + encoding.EncodeToString(signature), nil
}

func (a *githubAppCredentials) token(ctx context.Context) (githubInstallationToken, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return githubInstallationToken{}, err
	}
	now := time.Now()
	if a.cached.value != "" && now.Add(5*time.Minute).Before(a.cached.expiresAt) {
		return a.cached, nil
	}
	jwt, err := a.jwt(now)
	if err != nil {
		return githubInstallationToken{}, err
	}
	owner, repo, _ := strings.Cut(a.repository, "/")
	var installation struct {
		ID      int64
		AppSlug string `json:"app_slug"`
	}
	endpoint := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	if err := a.request(ctx, http.MethodGet, endpoint, jwt, nil, &installation); err != nil {
		return githubInstallationToken{}, fmt.Errorf("find GitHub App installation: %w", err)
	}
	if installation.ID <= 0 || installation.AppSlug == "" {
		return githubInstallationToken{}, fmt.Errorf("GitHub App installation response is missing its ID or app slug")
	}
	login := installation.AppSlug + "[bot]"
	if a.cached.login != "" && a.cached.login != login {
		return githubInstallationToken{}, fmt.Errorf("GitHub App bot login changed; restart the Poller")
	}
	var response struct {
		Token     string
		ExpiresAt time.Time `json:"expires_at"`
	}
	payload, err := json.Marshal(map[string][]string{"repositories": {repo}})
	if err != nil {
		return githubInstallationToken{}, err
	}
	endpoint = fmt.Sprintf("/app/installations/%d/access_tokens", installation.ID)
	if err := a.request(ctx, http.MethodPost, endpoint, jwt, payload, &response); err != nil {
		return githubInstallationToken{}, fmt.Errorf("create GitHub App installation token: %w", err)
	}
	if response.Token == "" || !response.ExpiresAt.After(time.Now()) {
		return githubInstallationToken{}, fmt.Errorf("GitHub App returned an empty or expired installation token")
	}
	a.cached = githubInstallationToken{value: response.Token, expiresAt: response.ExpiresAt, login: login}
	return a.cached, nil
}

func (a *githubAppCredentials) request(ctx context.Context, method, endpoint, jwt string, body []byte, target any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.apiURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub API HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
		return fmt.Errorf("invalid GitHub App API response")
	}
	return nil
}
