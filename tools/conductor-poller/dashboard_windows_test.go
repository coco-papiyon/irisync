package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDashboardLogAndHostBoundaries(t *testing.T) {
	dir := t.TempDir()
	secret := "ghs_" + strings.Repeat("x", 24)
	if err := os.WriteFile(filepath.Join(dir, "run-1.log"), []byte("workflow result "+secret), 0600); err != nil {
		t.Fatal(err)
	}
	source := dashboardSource{github: githubProject{cfg: config{DataDir: dir}}}
	handler := source.handler("127.0.0.1:8765")
	for _, tc := range []struct {
		method, path, host string
		status             int
	}{
		{"GET", "/", "127.0.0.1:8765", http.StatusOK},
		{"GET", "/assets/dashboard.js", "127.0.0.1:8765", http.StatusOK},
		{"GET", "/api/log?path=run-1.log", "127.0.0.1:8765", http.StatusOK},
		{"GET", "/api/log?path=" + url.QueryEscape("../run-1.log"), "127.0.0.1:8765", http.StatusNotFound},
		{"GET", "/api/log?path=poller.sqlite", "127.0.0.1:8765", http.StatusNotFound},
		{"POST", "/api/snapshot", "127.0.0.1:8765", http.StatusMethodNotAllowed},
		{"GET", "/api/log?path=run-1.log", "another.example:8765", http.StatusForbidden},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Host = tc.host
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Errorf("%s %s host=%s: status=%d, expected %d", tc.method, tc.path, tc.host, response.Code, tc.status)
		}
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("dashboard exposed a token")
		}
		if tc.host == "127.0.0.1:8765" && tc.path == "/api/log?path=run-1.log" {
			var log struct{ Text string }
			if err := json.Unmarshal(response.Body.Bytes(), &log); err != nil || log.Text != "workflow result [redacted]" {
				t.Fatalf("log response: %q, %v", log.Text, err)
			}
		}
	}
	if stop, err := startDashboard(context.Background(), "0.0.0.0:0", source.github, nil, true, ""); err == nil {
		stop()
		t.Fatal("dashboard accepted a non-loopback listener")
	}
}

func TestDashboardRejectsInvalidApprovalRequests(t *testing.T) {
	const host = "127.0.0.1:8765"
	handler := (dashboardSource{}).handler(host)
	for _, tc := range []struct {
		method, origin, contentType, body string
		status                            int
	}{
		{"GET", "http://" + host, "", "", http.StatusMethodNotAllowed},
		{"POST", "", "application/json", `{}`, http.StatusForbidden},
		{"POST", "https://other.example", "application/json", `{}`, http.StatusForbidden},
		{"POST", "http://" + host, "text/plain", `{}`, http.StatusUnsupportedMediaType},
		{"POST", "http://" + host, "application/json", `{"kind":"issue","number":29,"revision":"abcdef0"}`, http.StatusBadRequest},
		{"POST", "http://" + host, "application/json", `{"kind":"issue","number":29,"revision":"` + strings.Repeat("a", 40) + `"} {}`, http.StatusBadRequest},
	} {
		req := httptest.NewRequest(tc.method, "http://"+host+"/api/approve", strings.NewReader(tc.body))
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Content-Type", tc.contentType)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Errorf("%s origin=%q: status=%d, expected %d", tc.method, tc.origin, response.Code, tc.status)
		}
	}
}

func TestDashboardSettingsUpdateAndConflict(t *testing.T) {
	const host = "127.0.0.1:8765"
	path := filepath.Join(t.TempDir(), "poller.yaml")
	initial := `# Local settings
repository: irisync/repo
project_number: 4
github_login: operator
github_app_key_path: private-marker.pem
design_approver_ids: [7] # Human reviewers
poll_interval: 5m
`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	handler := (dashboardSource{configPath: path}).handler(host)
	call := func(method, origin string, payload []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://"+host+"/api/settings", strings.NewReader(string(payload)))
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	initialResponse := call(http.MethodGet, "", nil)
	var edit dashboardSettings
	if err := json.Unmarshal(initialResponse.Body.Bytes(), &edit); err != nil || initialResponse.Code != http.StatusOK ||
		edit.DesignApproverIDs != "7" || strings.Contains(initialResponse.Body.String(), "private-marker") {
		t.Fatalf("unexpected settings response: %d %s", initialResponse.Code, initialResponse.Body.String())
	}
	save := func(expect int, origin string) {
		t.Helper()
		payload, err := json.Marshal(edit)
		if err != nil {
			t.Fatal(err)
		}
		response := call(http.MethodPost, origin, payload)
		if response.Code != expect {
			t.Fatalf("settings save: got %d, want %d: %s", response.Code, expect, response.Body.String())
		}
	}
	edit.DesignApproverIDs = "10320836, 9007199254740993"
	edit.DashboardApproverLogin = "reviewer"
	edit.PollInterval = "30s"
	edit.ImplementationIssueNumbers = "29"
	save(http.StatusForbidden, "https://other.example")
	save(http.StatusBadRequest, "http://"+host)
	edit.DesignIssueNumbers = "29"
	save(http.StatusOK, "http://"+host)
	save(http.StatusConflict, "http://"+host)
	cfg, err := loadConfig(path)
	if err != nil || cfg.PollInterval.String() != "30s" || cfg.DashboardApproverLogin != "reviewer" ||
		len(cfg.DesignApproverIDs) != 2 || cfg.DesignApproverIDs[1] != 9007199254740993 || !cfg.implementationEnabled(29) {
		t.Fatalf("unexpected saved settings: %+v (%v)", cfg, err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(contents), "private-marker.pem") || !strings.Contains(string(contents), "# Human reviewers") {
		t.Fatalf("unrelated YAML values or comments were lost: %v", err)
	}
}

func TestDashboardDesignRequestTargetsCurrentPublishedRevision(t *testing.T) {
	cfg := config{Repository: "irisync/repo", GitHubLogin: "worker[bot]"}
	p := poller{cfg: cfg}
	path := "docs/design/issues/issue-29.md"
	old, current := strings.Repeat("a", 40), strings.Repeat("b", 40)
	oldBody := p.requestBody(path, old, 1)
	currentBody := p.requestBody(path, current, 2)
	comments := []designComment{{ID: 1, Body: oldBody}, {ID: 2, Body: currentBody}}
	comments[0].User.Login = cfg.GitHubLogin
	comments[1].User.Login = "another-user"
	if got := dashboardDesignRequest(cfg, comments, path, current); got.ID != 0 {
		t.Fatalf("another user's request matched: %q", got)
	}
	comments[1].User.Login = cfg.GitHubLogin
	comments[1].Body += " " + current
	if got := dashboardDesignRequest(cfg, comments, path, current); got != comments[1] {
		t.Fatalf("expected current request, got %+v", got)
	}
}

func TestDashboardChecksCurrentHumanPRApproval(t *testing.T) {
	head := strings.Repeat("a", 40)
	reviews := []humanReview{
		{ID: 1, State: "APPROVED", CommitID: head},
		{ID: 2, State: "COMMENTED", CommitID: head},
		{ID: 3, State: "CHANGES_REQUESTED", CommitID: head},
	}
	for i := range reviews {
		reviews[i].User.ID = 1
	}
	if !dashboardPullApproved(reviews[:2], 1, head) || dashboardPullApproved(reviews[:2], 2, head) ||
		dashboardPullApproved(reviews[:2], 1, strings.Repeat("b", 40)) || dashboardPullApproved(reviews, 1, head) {
		t.Fatal("unexpected approval for actor or commit")
	}
}

func TestDashboardReadsImplementationOutcome(t *testing.T) {
	dir := t.TempDir()
	folder := "implementation/29-" + strings.Repeat("a", 40) + "/attempt-3/"
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(folder)), 0700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"workflow.log":   `{"edit_exit":0,"validation_exit":"0","review_exit":1}`,
		"step-error.txt": "review failed",
		"manifest.json":  "implementation input",
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(folder+name)), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := dashboardLogs(dir)
	if err != nil || len(logs) != 2 {
		t.Fatalf("expected the two viewer logs only, got %d (%v)", len(logs), err)
	}
	results := dashboardAttemptResults(dir, logs)
	if len(results) != 1 || results[0].Outcome != "failed" || results[0].Issue != 29 ||
		results[0].Attempts != 3 || results[0].LogPath != folder+"step-error.txt" {
		t.Fatalf("unexpected dashboard results: %+v", results)
	}
}

func TestDashboardConnectsRecordsToAnIssueAndPR(t *testing.T) {
	revision := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	prs := []dashboardPR{{Number: 51}}
	results := []dashboardResult{
		{Phase: "smoke", Issue: 29, runID: 2},
		{Phase: "design", Issue: 29, Attempts: 1},
		{Phase: "implementation", Issue: 29, PR: 51, revision: revision},
		{Phase: "implementation attempt", Issue: 29, revision: revision},
		{Phase: "implementation attempt", Issue: 29, revision: other},
		{Phase: "review", PR: 51, revision: revision + "-" + other},
		{Phase: "review", PR: 51, revision: other + "-" + revision},
	}
	logs := []dashboardLog{
		{Path: "run-2.log", Group: "run"},
		{Path: "design-29-1.log", Group: "design"},
		{Path: "implementation/29-" + revision + "/attempt-1/validation.log", Group: "implementation"},
		{Path: "implementation/29-" + other + "/attempt-1/validation.log", Group: "implementation"},
		{Path: "review/51/review-" + revision + "-" + other + "/workflow.log", Group: "review"},
		{Path: "review/52/review-" + other + "-" + revision + "/workflow.log", Group: "review"},
	}
	connectDashboardRecords(prs, results, logs)
	if len(prs[0].Issues) != 1 || prs[0].Issues[0] != 29 || results[3].PR != 51 || results[4].PR != 0 || results[5].Issue != 29 {
		t.Fatalf("unexpected PR or result relations: %+v %+v", prs, results)
	}
	if results[0].LogPath != logs[0].Path || results[1].LogPath != logs[1].Path ||
		results[5].LogPath != logs[4].Path || results[6].LogPath != "" {
		t.Fatalf("unexpected result log paths: %+v", results)
	}
}
