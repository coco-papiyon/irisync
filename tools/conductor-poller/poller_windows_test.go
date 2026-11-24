package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == childFlag {
		os.Exit(runChild(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestConductorPrefersRepositoryRuntime(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, ".conductor", ".local", "conductor-runtime", "Scripts", "conductor.exe")
	if err := os.MkdirAll(filepath.Dir(local), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, nil, 0600); err != nil {
		t.Fatal(err)
	}
	path, err := conductorExecutable(root)
	if err != nil || path != local {
		t.Fatalf("Conductor path = %q, want %q (%v)", path, local, err)
	}
}

func TestPollingStartsImmediatelyAndWakesOnCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := poller{workflowDone: make(chan struct{}, 1)}
	polled := make(chan struct{}, 2)
	done := make(chan error, 1)
	go func() {
		done <- runPollingLoop(ctx, false, time.Hour, p.workflowDone, func(context.Context) error {
			polled <- struct{}{}
			return nil
		})
	}()

	expectPoll := func() {
		t.Helper()
		select {
		case <-polled:
		case <-time.After(2 * time.Second):
			t.Fatal("poll did not run immediately")
		}
	}
	expectPoll()
	p.requestPoll()
	expectPoll()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("poll loop did not stop: %v", err)
	}
}

func TestPollingOnceDoesNotDrainCompletions(t *testing.T) {
	p := poller{workflowDone: make(chan struct{}, 1)}
	count := 0
	err := runPollingLoop(context.Background(), true, time.Hour, p.workflowDone, func(context.Context) error {
		count++
		p.requestPoll()
		p.requestPoll()
		return nil
	})
	if err != nil || count != 1 || len(p.workflowDone) != 1 {
		t.Fatalf("once polled %d times with %d notifications (%v)", count, len(p.workflowDone), err)
	}
}

func TestFailedWorkflowWaitsBeforeRetryingTheSameIssue(t *testing.T) {
	p := poller{retryAfter: map[string]time.Time{"failed": time.Now().Add(time.Hour)}}
	if p.launchDesign(context.Background(), projectIssue{ID: "failed"}) {
		t.Fatal("a failed workflow was immediately restarted")
	}
}

func TestStateRecoversAnInterruptedRunWithoutRerunningSuccess(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, closeState, err := openState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	issue := projectIssue{ID: "I_1", ItemID: "P_1", Number: 1}
	runID, err := db.claim(ctx, issue, 2)
	if err != nil || runID == 0 {
		closeState()
		t.Fatalf("first claim: id=%d err=%v", runID, err)
	}
	if _, closeOther, err := openState(ctx, dir); err == nil {
		closeOther()
		closeState()
		t.Fatal("second poller acquired the same state")
	}
	if duplicate, err := db.claim(ctx, issue, 2); err != nil || duplicate != 0 {
		closeState()
		t.Fatalf("duplicate claim: id=%d err=%v", duplicate, err)
	}
	closeState()

	db, closeState, err = openState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeState()
	var outcome string
	if err := db.db.QueryRow("SELECT outcome FROM smoke_runs WHERE id = ?", runID).Scan(&outcome); err != nil || outcome != "interrupted" {
		t.Fatalf("expected interrupted: outcome=%q err=%v", outcome, err)
	}
	retryID, err := db.claim(ctx, issue, 2)
	if err != nil || retryID <= runID {
		t.Fatalf("retry claim: id=%d err=%v", retryID, err)
	}
	if err := db.finish(ctx, retryID, "run.log", runResult{outcome: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := db.claim(ctx, issue, 3); err != nil || duplicate != 0 {
		t.Fatalf("claim after success: id=%d err=%v", duplicate, err)
	}
}

func TestConductorJobExitAndCancellation(t *testing.T) {
	dir := t.TempDir()
	log, err := os.Create(filepath.Join(dir, "job.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	ctx := context.Background()
	result := classifyRun(ctx, runInJob(ctx, dir, log, "cmd.exe", "/d", "/c", "exit 7"))
	if result.outcome != "failed" || result.exitCode != 7 {
		t.Fatalf("expected exit code 7: %+v", result)
	}
	timeout, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err = runInJob(timeout, dir, log, "pwsh.exe", "-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Seconds 30")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected Job Object timeout, got %v", err)
	}
}

func TestAutoReadySelection(t *testing.T) {
	cfg := config{Repository: "owner/irisync", AutoReady: true, TestIssueNumbers: []int{6}}
	g := githubProject{cfg: cfg}
	item := projectIssue{Number: 29, Status: "Ready", Repository: cfg.Repository}
	if !g.runnable(item) || !cfg.designEnabled(item.Number) || !cfg.implementationEnabled(item.Number) {
		t.Fatal("Ready issue requires a per-issue entry in auto_ready mode")
	}
	if cfg.designEnabled(6) || cfg.implementationEnabled(6) {
		t.Fatal("smoke issue was also selected for production work")
	}
	item.Number = 6
	if !g.runnable(item) {
		t.Fatal("Ready smoke issue was not selected")
	}
	item.Number = 29
	item.Status = "Pending"
	if g.runnable(item) {
		t.Fatal("Pending issue was selected before promotion")
	}
	item.Status = "Ready"
	item.Closed = true
	if g.runnable(item) {
		t.Fatal("closed Ready issue was selected")
	}
	item.Closed = false
	item.Repository = "owner/another"
	if g.runnable(item) {
		t.Fatal("issue from another repository was selected")
	}
	item.Repository = cfg.Repository
	g.cfg.AutoReady = false
	if g.runnable(item) {
		t.Fatal("existing explicit mode selected an issue without an entry")
	}
}

func TestAutoReadyResumesOnlyRecordedWork(t *testing.T) {
	ctx := context.Background()
	state, closeState, err := openState(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeState()
	if _, err := state.db.Exec("INSERT INTO designs(issue_id) VALUES ('known')"); err != nil {
		t.Fatal(err)
	}
	if _, err := state.db.Exec("INSERT INTO implementations(issue_id, design_sha, base_sha, pr_number) VALUES ('published', 'design', 'base', 85)"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, status string
		want       bool
	}{
		{"unknown", "Design Review", false},
		{"known", "Design Review", true},
		{"known", "Implementing", true},
		{"known", "Blocked", false},
		{"published", "PR Review", true},
		{"published", "Blocked", true},
	} {
		got, err := state.tracksIssue(ctx, projectIssue{ID: tc.id, Status: tc.status})
		if err != nil || got != tc.want {
			t.Errorf("%s %s: tracked=%t, err=%v", tc.id, tc.status, got, err)
		}
	}
}
