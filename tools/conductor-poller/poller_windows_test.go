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
