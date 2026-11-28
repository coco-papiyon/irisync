package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type File struct {
	Kind    string
	Size    int64
	SHA256  [sha256.Size]byte
	ModTime time.Time
	Deleted bool
}

func (s *Store) Baseline(ctx context.Context, pairID, path string) (File, bool, error) {
	var file File
	var hash []byte
	var modified string
	err := s.db.QueryRowContext(ctx, `SELECT kind, size, sha256, mtime, deleted FROM sync_files
		WHERE pair_id = ? AND relative_path = ?`, pairID, path).
		Scan(&file.Kind, &file.Size, &hash, &modified, &file.Deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, err
	}
	if len(hash) != len(file.SHA256) {
		return File{}, false, errors.New("stored SHA-256 has an invalid length")
	}
	copy(file.SHA256[:], hash)
	file.ModTime, err = time.Parse(timestampLayout, modified)
	return file, err == nil, err
}

type ConfirmedResult struct {
	QueueID                int64
	Attempt                int64 `json:"-"` // Claim token, excluded from the durable result fingerprint.
	Baseline               File
	ProgressGeneration     int64 // Zero leaves progress unchanged.
	InitialSyncCompletedAt *time.Time
}

// Apply records the result of a verified filesystem operation and completes
// exactly its queue item. The receipt permits a safe retry after an unknown commit.
func (s *Store) Apply(ctx context.Context, result ConfirmedResult) error {
	if result.QueueID <= 0 || result.Attempt <= 0 || result.Baseline.Kind == "" || result.Baseline.Size < 0 ||
		result.ProgressGeneration < 0 || (result.InitialSyncCompletedAt != nil && result.ProgressGeneration == 0) {
		return errors.New("invalid confirmed sync result")
	}
	result.Baseline.ModTime = result.Baseline.ModTime.UTC()
	if result.InitialSyncCompletedAt != nil {
		at := result.InitialSyncCompletedAt.UTC()
		result.InitialSyncCompletedAt = &at
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	fingerprint := sha256.Sum256(payload)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	recorded, err := checkReceipt(ctx, tx, result.QueueID, fingerprint[:])
	if err != nil || recorded {
		return err
	}
	if err := applyResult(ctx, tx, result, fingerprint[:]); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		// A failed SQLite COMMIT can leave its transaction open. Discard
		// this connection before checking what a fresh connection sees.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), busyTimeout)
		defer cancel()
		recorded, checkErr := checkReceipt(checkCtx, s.db, result.QueueID, fingerprint[:])
		if recorded && checkErr == nil {
			return nil
		}
		return errors.Join(fmt.Errorf("commit sync result: %w", err), checkErr)
	}
	return nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func checkReceipt(ctx context.Context, db rowQuerier, id int64, fingerprint []byte) (bool, error) {
	var previous []byte
	err := db.QueryRowContext(ctx, "SELECT fingerprint FROM sync_receipts WHERE queue_id = ?", id).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(previous, fingerprint) {
		return false, errors.New("queue item already has a different confirmed result")
	}
	return true, nil
}

func applyResult(ctx context.Context, tx *sql.Tx, result ConfirmedResult, fingerprint []byte) error {
	var pair, path string
	err := tx.QueryRowContext(ctx, `SELECT pair_id, relative_path FROM sync_queue
		WHERE id = ? AND attempts = ? AND status IN ('running','needs_verification')`,
		result.QueueID, result.Attempt).Scan(&pair, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("queue attempt has changed or is not running or awaiting verification")
	}
	if err != nil {
		return err
	}
	file := result.Baseline
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sync_files(pair_id, relative_path, kind, size, sha256, mtime, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(pair_id, relative_path) DO UPDATE SET
			kind = excluded.kind, size = excluded.size, sha256 = excluded.sha256,
			mtime = excluded.mtime, deleted = excluded.deleted`,
		pair, path, file.Kind, file.Size, file.SHA256[:], timestamp(file.ModTime), file.Deleted); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sync_queue WHERE id = ?", result.QueueID); err != nil {
		return err
	}
	if result.ProgressGeneration != 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE pair_progress SET processed = processed + 1,
				process_total = CASE WHEN process_total > processed THEN process_total ELSE NULL END
			WHERE pair_id = ? AND generation = ?`, pair, result.ProgressGeneration); err != nil {
			return err
		}
	}
	if result.InitialSyncCompletedAt != nil {
		if _, err := completeInitialSync(ctx, tx, pair, result.ProgressGeneration, *result.InitialSyncCompletedAt); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO sync_receipts(queue_id, fingerprint) VALUES (?, ?)", result.QueueID, fingerprint)
	return err
}
