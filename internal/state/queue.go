package state

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/coco-papiyon/irisync/internal/scan"
)

type QueueStatus string

const (
	Waiting           QueueStatus = "waiting"
	InProgress        QueueStatus = "running"
	RetryWait         QueueStatus = "retry_wait"
	NeedsVerification QueueStatus = "needs_verification"
)

type QueueItem struct {
	ID       int64
	PairID   string
	Path     string
	Status   QueueStatus
	Attempts int64
	RetryAt  *time.Time
}

func (s *Store) Enqueue(ctx context.Context, pairID, path string) (int64, error) {
	if pairID == "" {
		return 0, errors.New("pair ID is empty")
	}
	if err := scan.ValidateRelativePath(path); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO sync_queue(pair_id, relative_path)
		VALUES (?, ?) RETURNING id`, pairID, path).Scan(&id)
	return id, err
}

func (s *Store) Queue(ctx context.Context, pairID string) ([]QueueItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, relative_path, status, attempts, retry_at
		FROM sync_queue WHERE pair_id = ? ORDER BY id`, pairID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []QueueItem
	for rows.Next() {
		item := QueueItem{PairID: pairID}
		var retry sql.NullString
		if err := rows.Scan(&item.ID, &item.Path, &item.Status, &item.Attempts, &retry); err != nil {
			return nil, err
		}
		if retry.Valid {
			at, err := time.Parse(timestampLayout, retry.String)
			if err != nil {
				return nil, err
			}
			item.RetryAt = &at
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) Claim(ctx context.Context, id int64, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sync_queue
		SET status = 'running', attempts = attempts + 1, retry_at = NULL
		WHERE id = ? AND (status = 'waiting' OR (status = 'retry_wait' AND retry_at <= ?))
			AND NOT EXISTS (SELECT 1 FROM sync_queue AS earlier
				WHERE earlier.pair_id = sync_queue.pair_id AND earlier.relative_path = sync_queue.relative_path
					AND earlier.id < sync_queue.id)`, id, timestamp(now))
	return changed(result, err, "queue item is not ready")
}

func (s *Store) ScheduleRetry(ctx context.Context, id int64, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sync_queue SET status = 'retry_wait', retry_at = ?
		WHERE id = ? AND status = 'running'`, timestamp(at), id)
	return changed(result, err, "queue item is not running")
}

// RecoverInterrupted is called at controller startup, before dispatching work.
func (s *Store) RecoverInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sync_queue SET status = 'needs_verification' WHERE status = 'running'")
	return err
}

// RequeueVerified lets the engine retry after it verifies the filesystem state.
func (s *Store) RequeueVerified(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, "UPDATE sync_queue SET status = 'waiting' WHERE id = ? AND status = 'needs_verification'", id)
	return changed(result, err, "queue item does not need verification")
}
