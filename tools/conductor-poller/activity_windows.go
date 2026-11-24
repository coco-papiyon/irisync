package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

type workflowActivity struct {
	Issue, PR, Attempt int
	RunID              int64
	Phase              string
}

type workflowCompletionKey struct{}

type workflowCompletion struct {
	ran, boundedAttempt bool
}

func (p *poller) trackWorkflow(ctx context.Context, activity workflowActivity, logPath string) func(runResult) {
	logName, err := filepath.Rel(p.cfg.DataDir, logPath)
	if err != nil {
		logName = ""
	}
	logName = filepath.ToSlash(logName)
	logger := slog.With("issue", activity.Issue, "phase", activity.Phase, "log", logName)
	if activity.PR > 0 {
		logger = logger.With("pr", activity.PR)
	}
	if activity.Attempt > 0 {
		logger = logger.With("attempt", activity.Attempt)
	}
	if activity.RunID > 0 {
		logger = logger.With("run", activity.RunID)
	}
	startedAt := time.Now()
	started := startedAt.UTC().Format(time.RFC3339Nano)
	logger.Info("workflow started")
	_, err = p.db.db.ExecContext(ctx, `
		INSERT INTO active_workflows(issue_number, pr_number, phase, attempt, log_path, started_at, process_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(issue_number) DO UPDATE SET pr_number=excluded.pr_number, phase=excluded.phase,
			attempt=excluded.attempt, log_path=excluded.log_path, started_at=excluded.started_at, process_id=excluded.process_id`,
		activity.Issue, activity.PR, activity.Phase, activity.Attempt, logName, started, os.Getpid())
	recorded := err == nil
	if err != nil {
		slog.Warn("could not display active workflow", "issue", activity.Issue, "error", err)
	}
	return func(result runResult) {
		if completion, ok := ctx.Value(workflowCompletionKey{}).(*workflowCompletion); ok {
			completion.ran = true
			completion.boundedAttempt = completion.boundedAttempt || activity.Attempt > 0
		}
		logger.Info("workflow finished", "outcome", result.outcome, "exit_code", result.exitCode,
			"duration", time.Since(startedAt).Round(time.Millisecond))
		if !recorded {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := p.db.db.ExecContext(cleanup, "DELETE FROM active_workflows WHERE issue_number = ? AND started_at = ?", activity.Issue, started); err != nil {
			slog.Warn("could not clear active workflow", "issue", activity.Issue, "error", err)
		}
	}
}
