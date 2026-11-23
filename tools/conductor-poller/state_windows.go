package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS smoke_runs (
	id INTEGER PRIMARY KEY,
	issue_id TEXT NOT NULL,
	item_id TEXT NOT NULL,
	issue_number INTEGER NOT NULL,
	outcome TEXT NOT NULL CHECK (outcome IN ('running','succeeded','failed','interrupted','timed_out')),
	started_at TEXT NOT NULL,
	finished_at TEXT,
	exit_code INTEGER,
	log_path TEXT,
	error TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS one_live_or_successful_smoke_run
	ON smoke_runs (issue_id)
	WHERE outcome IN ('running', 'succeeded');
CREATE INDEX IF NOT EXISTS smoke_run_history ON smoke_runs (issue_id);
CREATE TABLE IF NOT EXISTS designs (
    issue_id TEXT PRIMARY KEY,
    attempts INTEGER NOT NULL DEFAULT 0,
    generated_markdown TEXT NOT NULL DEFAULT '',
    usage_json TEXT NOT NULL DEFAULT '',
    revision TEXT NOT NULL DEFAULT '',
    revision_number INTEGER NOT NULL DEFAULT 0,
    approved_sha TEXT NOT NULL DEFAULT '',
    approval_comment_id INTEGER NOT NULL DEFAULT 0,
    approver_id INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS implementations (
    issue_id TEXT NOT NULL,
    design_sha TEXT NOT NULL,
    base_sha TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    commit_sha TEXT NOT NULL DEFAULT '',
    pr_number INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (issue_id, design_sha)
);
CREATE TABLE IF NOT EXISTS pr_reviews (
	pr_number INTEGER NOT NULL,
	head_sha TEXT NOT NULL,
	base_sha TEXT NOT NULL,
	approved INTEGER NOT NULL,
	feedback TEXT NOT NULL,
	PRIMARY KEY (pr_number, head_sha, base_sha)
);
CREATE TABLE IF NOT EXISTS pr_fixes (
	id INTEGER PRIMARY KEY,
	pr_number INTEGER NOT NULL,
	kind TEXT NOT NULL,
	head_sha TEXT NOT NULL,
	base_sha TEXT NOT NULL,
	review_id INTEGER NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	seed_sha TEXT NOT NULL DEFAULT '',
	commit_sha TEXT NOT NULL DEFAULT '',
	published INTEGER NOT NULL DEFAULT 0,
	discarded INTEGER NOT NULL DEFAULT 0,
	UNIQUE (pr_number, kind, head_sha, base_sha, review_id)
);
`

type state struct{ db *sql.DB }

func openState(ctx context.Context, dir string) (*state, func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, "poller.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("lock poller state (another instance may be running): %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "poller.sqlite"))
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	closeState := func() {
		db.Close()
		file.Close()
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		closeState()
		return nil, nil, err
	}
	// The previous instance no longer holds the lock. Only a fresh poll of
	// GitHub can make an interrupted smoke run eligible again.
	if _, err := db.ExecContext(ctx, `
		UPDATE smoke_runs SET outcome = 'interrupted', finished_at = ?,
			error = 'poller stopped before persisting the result'
		WHERE outcome = 'running'`, utcNow()); err != nil {
		closeState()
		return nil, nil, err
	}
	return &state{db}, closeState, nil
}

func (s *state) tracksIssue(ctx context.Context, item projectIssue) (bool, error) {
	query := "SELECT EXISTS (SELECT 1 FROM designs WHERE issue_id = ?)"
	if isReviewStatus(item.Status) {
		query = "SELECT EXISTS (SELECT 1 FROM implementations WHERE issue_id = ? AND pr_number != 0)"
	}
	var tracked bool
	err := s.db.QueryRowContext(ctx, query, item.ID).Scan(&tracked)
	return tracked, err
}

func (s *state) claim(ctx context.Context, issue projectIssue, maxAttempts int) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO smoke_runs (issue_id, item_id, issue_number, outcome, started_at)
		SELECT ?, ?, ?, 'running', ?
		WHERE (SELECT count(*) FROM smoke_runs WHERE issue_id = ?) < ?
		AND NOT EXISTS (SELECT 1 FROM smoke_runs WHERE issue_id = ? AND outcome IN ('running', 'succeeded'))
		RETURNING id`, issue.ID, issue.ItemID, issue.Number, utcNow(), issue.ID, maxAttempts, issue.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (s *state) finish(ctx context.Context, id int64, logPath string, result runResult) error {
	var failure string
	if result.err != nil {
		failure = result.err.Error()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE smoke_runs SET outcome = ?, finished_at = ?, exit_code = ?, log_path = ?, error = ?
		WHERE id = ? AND outcome = 'running'`, result.outcome, utcNow(), result.exitCode, logPath, failure, id)
	return err
}

func utcNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }
