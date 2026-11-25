package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchCodexLimits(t *testing.T) {
	const replies = `{"id":1,"result":{}}
{"method":"account/updated","params":{"ignored":"account data"}}
{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":2}},"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":0,"windowDurationMins":300},"secondary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":1780000000}}}}}
`
	var sent bytes.Buffer
	limits, err := fetchCodexLimits(&sent, strings.NewReader(replies))
	if err != nil {
		t.Fatal(err)
	}
	if limits.Primary == nil || limits.Primary.UsedPercent == nil || *limits.Primary.UsedPercent != 0 ||
		limits.Primary.WindowDurationMins != 300 || limits.Secondary.ResetsAt != 1780000000 || !limits.reached() {
		t.Fatalf("unexpected limits: %+v", limits)
	}
	decoder := json.NewDecoder(&sent)
	for _, want := range []string{"initialize", "initialized", "account/rateLimits/read"} {
		var request struct{ Method string }
		if err := decoder.Decode(&request); err != nil || request.Method != want {
			t.Fatalf("request method = %q, err = %v, want %q", request.Method, err, want)
		}
	}
}

func TestCodexFailureClassification(t *testing.T) {
	var stderr codexErrorStream
	stderr.Write([]byte("warning: something else\r\nERROR: You"))
	stderr.Write([]byte("’ve hit your usage limit. Try again later.\r\n"))
	if !stderr.reached() {
		t.Fatal("did not detect the split stderr message")
	}
	if !codexEventLimited([]byte(`{"type":"turn.failed","error":{"message":"You've hit your usage limit. Try again later."}}`)) {
		t.Fatal("did not detect the structured error")
	}
	if codexEventLimited([]byte(`{"type":"item.completed","message":"You've hit your usage limit."}`)) {
		t.Fatal("classified a model item as a CLI error")
	}
	var unrelated codexErrorStream
	unrelated.Write([]byte("ERROR: unexpected status 429 Too Many Requests\n"))
	if unrelated.reached() {
		t.Fatal("classified a generic 429 as a ChatGPT usage limit")
	}
	dir := t.TempDir()
	err := codexExecError(dir, "edit", stderr.reached(), errors.New("unpublished detail"))
	if err != errCodexLimited || !codexAttemptLimited(dir) {
		t.Fatalf("classification did not reach parent process: %v", err)
	}
}

func TestCodexPauseSurvivesRestartAndResumesOnNewReading(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, closeState, err := openState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	const issue, sha = "issue", "revision"
	p := poller{db: db}
	_, err = db.db.ExecContext(ctx, `INSERT INTO implementations(issue_id, design_sha, base_sha, attempts, attempt_limit)
		VALUES (?, ?, 'base', 4, 4)`, issue, sha)
	if err != nil {
		closeState()
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Minute)
	if err := p.deferCodexAttempt(ctx, "implementation attempt", issue+":"+sha, 4); err != errCodexLimited {
		closeState()
		t.Fatalf("limit classification = %v", err)
	}
	closeState()
	db, closeState, err = openState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeState()
	p.db = db
	if err := p.requireCodex(ctx); err != errCodexLimited {
		t.Fatalf("quota state after reopening = %v", err)
	}
	if err := p.deferCodexAttempt(ctx, "implementation attempt", issue+":"+sha, 4); err != nil {
		t.Fatal(err)
	}
	var attempts, limit int
	if err := db.db.QueryRowContext(ctx, "SELECT attempts, attempt_limit FROM implementations").Scan(&attempts, &limit); err != nil {
		t.Fatal(err)
	}
	if attempts != 4 || limit != 5 {
		t.Fatalf("attempts = %d, limit = %d, want 4 and 5", attempts, limit)
	}
	used := 3
	available := codexLimits{Primary: &codexWindow{UsedPercent: &used, WindowDurationMins: 300}}
	if resumed, err := db.updateCodexUsage(ctx, stale, available, nil); err != nil || resumed {
		t.Fatalf("stale reading resumed = %t, err = %v", resumed, err)
	}
	if resumed, err := db.updateCodexUsage(ctx, time.Now(), codexLimits{}, errors.New("unavailable")); err != nil || resumed {
		t.Fatalf("failed reading resumed = %t, err = %v", resumed, err)
	}
	if status, err := db.codexUsage(ctx); err != nil || !status.Paused || !status.RefreshFailed {
		t.Fatalf("quota after failed reading = %+v, err = %v", status, err)
	}
	if resumed, err := db.updateCodexUsage(ctx, time.Now(), available, nil); err != nil || !resumed {
		t.Fatalf("fresh reading resumed = %t, err = %v", resumed, err)
	}
	if err := p.requireCodex(ctx); err != nil {
		t.Fatalf("quota gate after recovery: %v", err)
	}
}

func TestRecoverInterruptedDesignDeferral(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, closeState, err := openState(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer closeState()
	p := poller{cfg: config{DataDir: root}, db: db}
	work, err := os.MkdirTemp(root, "design-33-1-")
	if err != nil {
		t.Fatal(err)
	}
	if err := codexExecError(work, "design", true, errors.New("usage error")); err != errCodexLimited {
		t.Fatal(err)
	}
	issue := projectIssue{ID: "issue-33", Number: 33}
	if err := p.recoverDesignDeferral(ctx, issue, 1); err != errCodexLimited {
		t.Fatalf("recovery error = %v", err)
	}
	var recorded int
	if err := db.db.QueryRowContext(ctx, `
		SELECT count(*) FROM codex_deferrals WHERE phase = 'design' AND work_key = ? AND attempt = 1`, issue.ID).
		Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("recorded deferrals = %d, err = %v", recorded, err)
	}
	if _, err := os.Stat(filepath.Join(work, codexLimitMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deferral marker not cleared: %v", err)
	}
}
