package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Desired string

const (
	Running Desired = "running"
	Stopped Desired = "stopped"
)

type Pair struct {
	Desired                Desired // Empty until a start or stop request was saved.
	InitialSyncCompletedAt *time.Time
}

type Progress struct {
	Generation   int64
	Scanned      int64
	Processed    int64
	ScanTotal    *int64
	ProcessTotal *int64
}

func (s *Store) Pair(ctx context.Context, id string) (Pair, error) {
	var desired, completed sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT desired_state, initial_sync_completed_at
		FROM pair_runtime_state WHERE pair_id = ?`, id).Scan(&desired, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return Pair{}, nil
	}
	if err != nil {
		return Pair{}, err
	}
	pair := Pair{Desired: Desired(desired.String)}
	if completed.Valid {
		at, err := time.Parse(timestampLayout, completed.String)
		if err != nil {
			return Pair{}, err
		}
		pair.InitialSyncCompletedAt = &at
	}
	return pair, nil
}

func (s *Store) SetDesired(ctx context.Context, id string, desired Desired) error {
	if id == "" || (desired != Running && desired != Stopped) {
		return errors.New("invalid pair or desired state")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pair_runtime_state(pair_id, desired_state) VALUES (?, ?)
		ON CONFLICT(pair_id) DO UPDATE SET desired_state = excluded.desired_state`, id, desired)
	return err
}

func (s *Store) Progress(ctx context.Context, id string) (Progress, bool, error) {
	var progress Progress
	err := s.db.QueryRowContext(ctx, `SELECT generation, scanned, processed, scan_total, process_total
		FROM pair_progress WHERE pair_id = ?`, id).
		Scan(&progress.Generation, &progress.Scanned, &progress.Processed, &progress.ScanTotal, &progress.ProcessTotal)
	if errors.Is(err, sql.ErrNoRows) {
		return Progress{}, false, nil
	}
	return progress, err == nil, err
}

// StartScan resets only progress and returns a new generation for checkpoints.
func (s *Store) StartScan(ctx context.Context, id string) (int64, error) {
	if id == "" {
		return 0, errors.New("pair ID is empty")
	}
	var generation int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO pair_progress(pair_id, generation) VALUES (?, 1)
		ON CONFLICT(pair_id) DO UPDATE SET generation = generation + 1,
			scanned = 0, processed = 0, scan_total = NULL, process_total = NULL
		RETURNING generation`, id).Scan(&generation)
	return generation, err
}

// SaveScanProgress records an absolute checkpoint, leaving transfer progress to Apply.
func (s *Store) SaveScanProgress(ctx context.Context, id string, generation, scanned int64, scanTotal, processTotal *int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE pair_progress
		SET scanned = ?, scan_total = ?, process_total = ?
		WHERE pair_id = ? AND generation = ? AND scanned <= ?`,
		scanned, scanTotal, processTotal, id, generation, scanned)
	return changed(result, err, "scan checkpoint is stale or its generation is missing")
}

// CompleteInitialSync records completion when the caller has verified the
// scan and transfers for this generation. Pending work still prevents completion.
func (s *Store) CompleteInitialSync(ctx context.Context, id string, generation int64, at time.Time) error {
	result, err := completeInitialSync(ctx, s.db, id, generation, at)
	return changed(result, err, "initial sync has a stale generation or incomplete work")
}

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func completeInitialSync(ctx context.Context, db executor, id string, generation int64, at time.Time) (sql.Result, error) {
	return db.ExecContext(ctx, `
		INSERT INTO pair_runtime_state(pair_id, initial_sync_completed_at)
		SELECT ?, ? WHERE EXISTS (
			SELECT 1 FROM pair_progress WHERE pair_id = ? AND generation = ? AND scanned = scan_total
				AND (process_total IS NULL OR processed = process_total))
			AND NOT EXISTS (SELECT 1 FROM sync_queue WHERE pair_id = ?)
		ON CONFLICT(pair_id) DO UPDATE SET initial_sync_completed_at =
			coalesce(pair_runtime_state.initial_sync_completed_at, excluded.initial_sync_completed_at)`,
		id, timestamp(at), id, generation, id)
}

func changed(result sql.Result, err error, message string) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return errors.New(message)
	}
	return err
}
