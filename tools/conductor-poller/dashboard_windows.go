package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

//go:embed dashboard/*
var dashboardFiles embed.FS

const dashboardDefaultAddr = "127.0.0.1:8765"
const dashboardLogLimit = 128 << 10

func runDashboardOnly(ctx context.Context, cfg config, github githubProject, configPath string) error {
	file := url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(filepath.Join(cfg.DataDir, "poller.sqlite"))}
	file.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite", file.String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("open existing Poller state for the dashboard: %w", err)
	}
	addr := cfg.DashboardAddr
	if addr == "" {
		addr = dashboardDefaultAddr
	}
	stop, err := startDashboard(ctx, addr, github, db, true, configPath)
	if err != nil {
		return err
	}
	defer stop()
	<-ctx.Done()
	return ctx.Err()
}

func startDashboard(ctx context.Context, addr string, github githubProject, db *sql.DB, viewingOnly bool, configPath string) (func(), error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return nil, fmt.Errorf("dashboard_addr must use a loopback IP and port, such as %s", dashboardDefaultAddr)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for Poller dashboard: %w", err)
	}
	address := listener.Addr().String()
	source := dashboardSource{github: github, db: db, viewingOnly: viewingOnly, configPath: configPath}
	server := &http.Server{
		Handler: source.handler(address), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Poller dashboard stopped", "error", err)
		}
	}()
	slog.Info("Poller dashboard available", "url", "http://"+address, "dashboard_only", viewingOnly)
	return func() {
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if server.Shutdown(stop) != nil {
			_ = server.Close()
		}
	}, nil
}

type dashboardSource struct {
	github      githubProject
	db          *sql.DB
	viewingOnly bool
	configPath  string
}

func (d dashboardSource) handler(host string) http.Handler {
	mux := http.NewServeMux()
	for route, file := range map[string]string{"/{$}": "index.html", "/assets/dashboard.css": "dashboard.css", "/assets/dashboard.js": "dashboard.js"} {
		content, err := dashboardFiles.ReadFile("dashboard/" + file)
		mux.HandleFunc("GET "+route, func(w http.ResponseWriter, r *http.Request) {
			if err != nil {
				http.NotFound(w, r)
				return
			}
			http.ServeContent(w, r, file, time.Time{}, bytes.NewReader(content))
		})
	}
	mux.HandleFunc("GET /api/snapshot", d.snapshot)
	mux.HandleFunc("GET /api/log", d.log)
	mux.HandleFunc("GET /api/item", d.item)
	mux.HandleFunc("GET /api/settings", d.settings)
	var writeMu sync.Mutex
	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		d.saveSettings(w, r, &writeMu)
	})
	mux.HandleFunc("POST /api/approve", func(w http.ResponseWriter, r *http.Request) {
		d.approve(w, r, &writeMu)
	})
	mux.HandleFunc("POST /api/retry-implementation", func(w http.ResponseWriter, r *http.Request) {
		d.retryImplementation(w, r, &writeMu)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		if r.Host != host {
			http.Error(w, "Invalid dashboard host", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type dashboardIssue struct {
	Number             int
	Title, Status, URL string
	UpdatedAt          time.Time
	Retry              *implementationRetry `json:",omitempty"`
}

type dashboardPR struct {
	Number                                    int
	Title, State, URL, ReviewDecision, Checks string
	UpdatedAt                                 time.Time
	IsDraft                                   bool
	Issues                                    []int
	revision                                  string
}

type dashboardResult struct {
	Phase, Outcome, Timestamp string
	LogPath                   string
	Feedback                  string `json:",omitempty"`
	Issue, PR, Attempts       int
	revision                  string
	runID                     int
}

type dashboardLog struct {
	Path, Group string
	Modified    time.Time
	Bytes       int64
}

func (d dashboardSource) snapshot(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Repository  string
		Updated     time.Time
		ViewingOnly bool
		Issues      []dashboardIssue
		PRs         []dashboardPR
		Results     []dashboardResult
		Errors      map[string]string
	}{
		Repository: d.github.cfg.Repository, ViewingOnly: d.viewingOnly, Updated: time.Now(),
		Errors: make(map[string]string),
	}
	projectNumbers := make(map[string]int)
	var retries map[string]implementationRetry
	if !d.viewingOnly {
		retries, _ = d.implementationRetries(r.Context())
	}
	issues, err := d.github.projectIssues(r.Context())
	if err != nil {
		data.Errors["issues"] = "Project の Issue を取得できませんでした。GitHub 認証と Project へのアクセスを確認してください。"
	}
	for _, issue := range issues {
		if issue.Archived || !strings.EqualFold(issue.Repository, data.Repository) {
			continue
		}
		projectNumbers[issue.ID] = issue.Number
		if issue.Closed {
			continue
		}
		entry := dashboardIssue{
			Number: issue.Number, Title: issue.Title, Status: issue.Status, UpdatedAt: issue.UpdatedAt,
			URL: fmt.Sprintf("https://github.com/%s/issues/%d", data.Repository, issue.Number),
		}
		if retry, ok := retries[issue.ID]; ok && retryableProjectIssue(d.github.cfg, issue) {
			entry.Retry = &retry
		}
		data.Issues = append(data.Issues, entry)
	}
	if data.PRs, err = d.pullRequests(r.Context()); err != nil {
		data.Errors["prs"] = "GitHub から PR と CI の状況を取得できませんでした。"
	}
	if data.Results, err = d.results(r.Context(), projectNumbers); err != nil {
		data.Errors["results"] = "ローカルの実行結果を読み取れませんでした。"
	}
	active, err := d.activeResults(r.Context())
	if err != nil {
		data.Errors["results"] = "実行中の処理を読み取れませんでした。"
	}
	logs, err := dashboardLogs(d.github.cfg.DataDir)
	if err != nil {
		data.Errors["logs"] = "一部の実行ログを確認できませんでした。"
	}
	data.Results = append(dashboardAttemptResults(d.github.cfg.DataDir, logs), data.Results...)
	data.Results = append(active, data.Results...)
	connectDashboardRecords(data.PRs, data.Results, logs)
	dashboardJSON(w, data)
}

func (d dashboardSource) pullRequests(ctx context.Context) ([]dashboardPR, error) {
	query := `query($owner:String!, $name:String!) {
		viewer { login }
		repository(owner:$owner,name:$name) {
			pullRequests(first:50,states:[OPEN],orderBy:{field:UPDATED_AT,direction:DESC}) {
				nodes {
					number title state isDraft updatedAt reviewDecision headRefName
					closingIssuesReferences(first:20) { nodes { number repository { nameWithOwner } } }
					commits(last:1) { nodes { commit { statusCheckRollup { state } } } }
				}
			}
		}
	}`
	var result struct {
		Data struct {
			Viewer     struct{ Login string }
			Repository *struct {
				PullRequests struct {
					Nodes []struct {
						dashboardPR
						HeadRefName             string
						ClosingIssuesReferences struct {
							Nodes []struct {
								Number     int
								Repository struct{ NameWithOwner string }
							}
						}
						Commits struct {
							Nodes []struct {
								Commit struct{ StatusCheckRollup struct{ State string } }
							}
						}
					}
				}
			}
		}
	}
	owner, repo, _ := strings.Cut(d.github.cfg.Repository, "/")
	err := d.github.getJSON(ctx, &result, "graphql", "-f", "query="+query, "-f", "owner="+owner, "-f", "name="+repo)
	if err != nil {
		return nil, err
	}
	if result.Data.Repository == nil || !strings.EqualFold(result.Data.Viewer.Login, d.github.cfg.GitHubLogin) {
		return nil, fmt.Errorf("unexpected GitHub account or repository")
	}
	var prs []dashboardPR
	for _, node := range result.Data.Repository.PullRequests.Nodes {
		pr := node.dashboardPR
		pr.URL = fmt.Sprintf("https://github.com/%s/pull/%d", d.github.cfg.Repository, pr.Number)
		if ref, ok := strings.CutPrefix(node.HeadRefName, "conductor/implement/issue-"); ok {
			number, revision, _ := strings.Cut(ref, "-")
			issue, _ := strconv.Atoi(number)
			if issue > 0 && validCommitSHA(revision) {
				pr.Issues = append(pr.Issues, issue)
				pr.revision = revision
			}
		}
		for _, linked := range node.ClosingIssuesReferences.Nodes {
			if strings.EqualFold(linked.Repository.NameWithOwner, d.github.cfg.Repository) && !slices.Contains(pr.Issues, linked.Number) {
				pr.Issues = append(pr.Issues, linked.Number)
			}
		}
		if commits := node.Commits.Nodes; len(commits) > 0 {
			pr.Checks = commits[0].Commit.StatusCheckRollup.State
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

func (d dashboardSource) results(ctx context.Context, numbers map[string]int) ([]dashboardResult, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT phase, issue_id, issue_number, pr_number, outcome, attempts, timestamp, revision, run_id, feedback FROM (
			SELECT 'smoke' AS phase, issue_id, issue_number, 0 AS pr_number, outcome,
				0 AS attempts, COALESCE(finished_at, started_at) AS timestamp, '' AS revision, id AS run_id, rowid AS record_order, '' AS feedback
				FROM smoke_runs WHERE outcome <> 'running' OR NOT EXISTS (
					SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'active_workflows'
				)
			UNION ALL
			SELECT 'design', issue_id, 0, 0,
				CASE WHEN approved_sha <> '' THEN 'approved' WHEN revision <> '' THEN 'awaiting approval' ELSE 'recorded' END,
				attempts, '', revision, 0, rowid, '' FROM designs
			UNION ALL
			SELECT 'implementation', issue_id, 0, pr_number,
				CASE WHEN pr_number <> 0 THEN 'published' WHEN commit_sha <> '' THEN 'validated' ELSE 'recorded' END,
				attempts, '', design_sha, 0, rowid, '' FROM implementations
			UNION ALL
			SELECT 'review', '', 0, pr_number,
				CASE WHEN approved THEN 'approved' ELSE 'changes requested' END, 0, '', head_sha || '-' || base_sha, 0, rowid, feedback
				FROM pr_reviews
		) ORDER BY timestamp DESC, pr_number DESC, issue_number DESC, record_order DESC LIMIT 250`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []dashboardResult
	for rows.Next() {
		var result dashboardResult
		var id string
		if err := rows.Scan(&result.Phase, &id, &result.Issue, &result.PR, &result.Outcome,
			&result.Attempts, &result.Timestamp, &result.revision, &result.runID, &result.Feedback); err != nil {
			return nil, err
		}
		if result.Issue == 0 {
			result.Issue = numbers[id]
		}
		result.Feedback = dashboardReviewText(result.Feedback)
		results = append(results, result)
	}
	return results, rows.Err()
}

func (d dashboardSource) activeResults(ctx context.Context) ([]dashboardResult, error) {
	// A dashboard-only process can open a database from before this table was added.
	var exists bool
	err := d.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'active_workflows')").Scan(&exists)
	if err != nil || !exists {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, "SELECT issue_number, pr_number, phase, attempt, log_path, started_at, process_id FROM active_workflows")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []dashboardResult
	for rows.Next() {
		result := dashboardResult{Outcome: "running"}
		var pid uint32
		if err := rows.Scan(&result.Issue, &result.PR, &result.Phase, &result.Attempts, &result.LogPath, &result.Timestamp, &pid); err != nil {
			return nil, err
		}
		started, err := time.Parse(time.RFC3339Nano, result.Timestamp)
		if err == nil && workflowProcessAlive(pid, started) {
			results = append(results, result)
		}
	}
	return results, rows.Err()
}

func workflowProcessAlive(pid uint32, started time.Time) bool {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(process)
	status, err := windows.WaitForSingleObject(process, 0)
	if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		return false
	}
	var created, exited, kernel, user windows.Filetime
	err = windows.GetProcessTimes(process, &created, &exited, &kernel, &user)
	// Do not show a stale result if Windows reused the recorded PID.
	return err == nil && !time.Unix(0, created.Nanoseconds()).After(started)
}

var dashboardLogPath = regexp.MustCompile(`^(?:(?:run-[0-9]+|design-[0-9]+-[0-9]+|poller(?:-[A-Za-z0-9-]+)?)(?:\.out)?\.log|implementation/[0-9]+-[0-9a-f]{40}/attempt-[0-9]+/(?:(?:edit|validation|review|workflow)\.log|step-error\.txt|review\.json)|review/[0-9]+/(?:fix-[0-9]+(?:/attempt-[0-9]+)?|review-[0-9a-f]{40}-[0-9a-f]{40}|merge-[0-9a-f]{40})/(?:(?:edit|validation|review|workflow)\.log|step-error\.txt|review\.json))$`)

var dashboardSecrets = regexp.MustCompile(`(?is)-----BEGIN (?:[A-Z]+ )*PRIVATE KEY-----.*?-----END (?:[A-Z]+ )*PRIVATE KEY-----|\b(?:gh[opusr]_[A-Za-z0-9_.-]{15,}|github_pat_[A-Za-z0-9_]{15,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})`)

func dashboardLogs(root string) ([]dashboardLog, error) {
	var logs []dashboardLog
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		parts := strings.Split(name, "/")
		if entry.IsDir() {
			if name != "." && (len(parts) > 4 || (parts[0] != "implementation" && parts[0] != "review")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !dashboardLogPath.MatchString(name) {
			return nil
		}
		stat, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		group := parts[0]
		if len(parts) == 1 {
			group, _, _ = strings.Cut(group, "-")
			group, _, _ = strings.Cut(group, ".")
		}
		logs = append(logs, dashboardLog{Path: name, Group: group, Modified: stat.ModTime(), Bytes: stat.Size()})
		return nil
	})
	slices.SortFunc(logs, func(a, b dashboardLog) int { return b.Modified.Compare(a.Modified) })
	return logs[:min(250, len(logs))], err
}

func dashboardAttemptResults(root string, logs []dashboardLog) []dashboardResult {
	available := make(map[string]bool, len(logs))
	for _, log := range logs {
		available[log.Path] = log.Bytes > 0
	}
	var results []dashboardResult
	for _, log := range logs {
		if !strings.HasSuffix(log.Path, "/workflow.log") || log.Bytes == 0 || log.Bytes > 16<<10 {
			continue
		}
		parts := strings.Split(log.Path, "/")
		var result dashboardResult
		switch {
		case log.Group == "implementation" && len(parts) == 4:
			number, revision, _ := strings.Cut(parts[1], "-")
			result.Issue, _ = strconv.Atoi(number)
			result.Attempts, _ = strconv.Atoi(strings.TrimPrefix(parts[2], "attempt-"))
			result.Phase, result.revision = "implementation attempt", revision
		case log.Group == "review" && len(parts) == 5 && strings.HasPrefix(parts[2], "fix-"):
			result.PR, _ = strconv.Atoi(parts[1])
			result.Attempts, _ = strconv.Atoi(strings.TrimPrefix(parts[3], "attempt-"))
			result.Phase = "pr fix"
		default:
			continue
		}
		file, err := openDashboardLog(root, log.Path)
		if err != nil {
			continue
		}
		var stages struct {
			Edit       json.RawMessage `json:"edit_exit"`
			Validation json.RawMessage `json:"validation_exit"`
			Review     json.RawMessage `json:"review_exit"`
		}
		err = json.NewDecoder(io.LimitReader(file, 16<<10)).Decode(&stages)
		file.Close()
		if err != nil {
			continue
		}
		// Conductor has emitted both JSON numbers and strings for exit codes.
		edit := strings.Trim(string(stages.Edit), `"`)
		validation := strings.Trim(string(stages.Validation), `"`)
		review := strings.Trim(string(stages.Review), `"`)
		result.Outcome = "failed"
		if edit == "0" && validation == "0" && review == "0" {
			result.Outcome = "succeeded"
		} else if edit == "" {
			continue
		}
		directory := strings.TrimSuffix(log.Path, "workflow.log")
		result.Timestamp = log.Modified.Format(time.RFC3339Nano)
		result.LogPath = log.Path
		if failed := directory + "step-error.txt"; result.Outcome == "failed" && available[failed] {
			result.LogPath = failed
		}
		if _, err := strconv.Atoi(review); err == nil {
			result.Feedback = dashboardReviewFeedback(root, directory+"review.json")
		}
		results = append(results, result)
	}
	return results
}

func dashboardReviewFeedback(root, path string) string {
	file, err := openDashboardLog(root, path)
	if err != nil {
		return ""
	}
	defer file.Close()
	var review reviewVerdict
	if err := json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&review); err != nil {
		return ""
	}
	return dashboardReviewText(review.Feedback)
}

func dashboardReviewText(feedback string) string {
	return strings.TrimSpace(boundedReviewText(dashboardSecrets.ReplaceAllString(feedback, "[redacted]")))
}

func connectDashboardRecords(prs []dashboardPR, results []dashboardResult, logs []dashboardLog) {
	issueByPR := make(map[int]int)
	prByRevision := make(map[string]int)
	available := make(map[string]dashboardLog, len(logs))
	for _, log := range logs {
		available[log.Path] = log
	}
	for _, result := range results {
		if result.PR == 0 || result.Phase != "implementation" {
			continue
		}
		if result.Issue > 0 {
			issueByPR[result.PR] = result.Issue
		}
		if result.revision != "" {
			prByRevision[result.revision] = result.PR
		}
	}
	for i := range prs {
		pr := &prs[i]
		if issue := issueByPR[pr.Number]; issue > 0 && !slices.Contains(pr.Issues, issue) {
			pr.Issues = append(pr.Issues, issue)
		}
		if issueByPR[pr.Number] == 0 && len(pr.Issues) == 1 {
			issueByPR[pr.Number] = pr.Issues[0]
		}
		if pr.revision != "" && prByRevision[pr.revision] == 0 {
			prByRevision[pr.revision] = pr.Number
		}
	}
	for i := range results {
		result := &results[i]
		if result.Outcome == "running" {
			if available[result.LogPath].Bytes == 0 {
				result.LogPath = ""
			}
			continue
		}
		if result.Phase == "implementation attempt" {
			result.PR = prByRevision[result.revision]
		}
		if result.Issue == 0 {
			result.Issue = issueByPR[result.PR]
		}
		var name string
		switch result.Phase {
		case "smoke":
			name = fmt.Sprintf("run-%d.log", result.runID)
		case "design":
			name = fmt.Sprintf("design-%d-%d.log", result.Issue, result.Attempts)
		case "review":
			name = fmt.Sprintf("review/%d/review-%s/workflow.log", result.PR, result.revision)
		}
		if _, exists := available[name]; exists {
			result.LogPath = name
		}
		log := available[result.LogPath]
		if result.Timestamp == "" && !log.Modified.IsZero() {
			result.Timestamp = log.Modified.Format(time.RFC3339Nano)
		}
		if log.Bytes == 0 {
			result.LogPath = ""
		}
	}
}

func openDashboardLog(directory, name string) (*os.File, error) {
	if !dashboardLogPath.MatchString(name) {
		return nil, fs.ErrNotExist
	}
	root, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.ToSlash(rel) != name {
		return nil, fs.ErrNotExist
	}
	return os.Open(path)
}

func (d dashboardSource) log(w http.ResponseWriter, r *http.Request) {
	file, err := openDashboardLog(d.github.cfg.DataDir, r.URL.Query().Get("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	start := max(0, stat.Size()-dashboardLogLimit)
	data, err := io.ReadAll(io.NewSectionReader(file, start, stat.Size()-start))
	if err != nil {
		http.Error(w, "Unable to read log", http.StatusInternalServerError)
		return
	}
	content := strings.ToValidUTF8(string(data), "�")
	if start > 0 {
		_, content, _ = strings.Cut(content, "\n")
	}
	dashboardJSON(w, struct {
		Text      string
		Truncated bool
	}{dashboardSecrets.ReplaceAllString(content, "[redacted]"), start > 0})
}

func dashboardJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
