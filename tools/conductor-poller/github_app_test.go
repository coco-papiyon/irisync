package main

import (
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubAuthConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poller.yaml")
	for _, tc := range []struct {
		name, settings, mode string
		wantErr              bool
	}{
		{name: "existing config", settings: "github_login: user\n", mode: githubAuthGH},
		{name: "app", settings: "github_auth_mode: app\ngithub_app_id: 123\ngithub_app_key_path: keys/app.pem\ngithub_project_login: owner\n", mode: githubAuthApp},
		{name: "missing key", settings: "github_auth_mode: app\ngithub_app_id: 123\ngithub_project_login: owner\n", wantErr: true},
		{name: "missing Project login", settings: "github_auth_mode: app\ngithub_app_id: 123\ngithub_app_key_path: keys/app.pem\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := os.WriteFile(path, []byte("project_id: project\nrepository: owner/irisync\n"+tc.settings), 0600)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected configuration error")
				}
				return
			}
			if err != nil || cfg.GitHubAuthMode != tc.mode {
				t.Fatalf("loadConfig returned mode %q and error %v", cfg.GitHubAuthMode, err)
			}
			if tc.mode == githubAuthApp && (cfg.GitHubAppID != "123" || cfg.GitHubAppKeyPath != filepath.Join(dir, "keys", "app.pem")) {
				t.Fatal("numeric App ID or relative key path was not loaded")
			}
		})
	}
}

func TestGitHubAppInstallationToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	signingKey, err := parseGitHubAppKey(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	var mints atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Error("expected three JWT segments")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		digest := sha256.Sum256([]byte(strings.Join(parts[:2], ".")))
		signature, signatureErr := base64.RawURLEncoding.DecodeString(parts[2])
		payload, payloadErr := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Iss string
			Iat int64
			Exp int64
		}
		now := time.Now()
		if signatureErr != nil || payloadErr != nil ||
			rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature) != nil ||
			json.Unmarshal(payload, &claims) != nil || claims.Iss != "client-id" ||
			claims.Iat >= now.Unix() || claims.Exp <= now.Unix() || claims.Exp > now.Add(10*time.Minute).Unix() {
			t.Error("invalid JWT signature or claims")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/irisync/installation":
			fmt.Fprint(w, `{"id":42,"app_slug":"irisync-app"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			var scope struct{ Repositories []string }
			if err := json.NewDecoder(r.Body).Decode(&scope); err != nil || len(scope.Repositories) != 1 || scope.Repositories[0] != "irisync" {
				t.Error("installation token request is not scoped to irisync")
				http.Error(w, "invalid scope", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": fmt.Sprintf("test-token-%d", mints.Add(1)), "expires_at": now.Add(time.Hour),
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := &githubAppCredentials{
		appID: "client-id", repository: "owner/irisync", apiURL: server.URL, key: signingKey, client: server.Client(),
	}
	first, err := app.token(context.Background())
	if err != nil || first.login != "irisync-app[bot]" || first.value != "test-token-1" {
		t.Fatalf("unexpected initial installation authentication: %v", err)
	}
	cached, err := app.token(context.Background())
	if err != nil || cached != first || mints.Load() != 1 {
		t.Fatalf("installation token was not cached: %v", err)
	}
	app.cached.expiresAt = time.Now().Add(time.Minute)
	refreshed, err := app.token(context.Background())
	if err != nil || refreshed.value != "test-token-2" || refreshed.login != first.login {
		t.Fatalf("installation token was not refreshed: %v", err)
	}
}
