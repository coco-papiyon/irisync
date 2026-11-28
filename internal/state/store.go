// Package state persists the controller's confirmed synchronization state.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const applicationID = 0x49525359
const busyTimeout = 5 * time.Second

const schemaV1 = `
CREATE TABLE pair_runtime_state (
    pair_id TEXT PRIMARY KEY NOT NULL,
    desired_state TEXT CHECK (desired_state IN ('running','stopped')),
    initial_sync_completed_at TEXT
);
CREATE TABLE pair_progress (
    pair_id TEXT PRIMARY KEY NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    scanned INTEGER NOT NULL DEFAULT 0 CHECK (scanned >= 0),
    processed INTEGER NOT NULL DEFAULT 0 CHECK (processed >= 0),
    scan_total INTEGER CHECK (scan_total >= scanned),
    process_total INTEGER CHECK (process_total >= processed)
);
CREATE TABLE sync_files (
    pair_id TEXT NOT NULL,
    relative_path TEXT NOT NULL,
    kind TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    sha256 BLOB NOT NULL CHECK (length(sha256) = 32),
    mtime TEXT NOT NULL,
    deleted INTEGER NOT NULL CHECK (deleted IN (0,1)),
    PRIMARY KEY (pair_id, relative_path)
) WITHOUT ROWID;
CREATE TABLE sync_queue (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    pair_id TEXT NOT NULL,
    relative_path TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting','running','retry_wait','needs_verification')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    retry_at TEXT,
    CHECK ((status = 'retry_wait') = (retry_at IS NOT NULL))
);
CREATE INDEX sync_queue_pair_id ON sync_queue(pair_id, id);
CREATE INDEX sync_queue_pair_path ON sync_queue(pair_id, relative_path, id);
CREATE TABLE sync_receipts (
    queue_id INTEGER PRIMARY KEY,
    fingerprint BLOB NOT NULL CHECK (length(fingerprint) = 32)
);
`

type Store struct {
	db   *sql.DB
	lock *os.File
}

// Open prepares a local SQLite file and applies pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("state database path is empty")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	for _, suffix := range []string{"", "-journal", "-wal", "-shm", ".lock"} {
		info, err := os.Lstat(path + suffix)
		if err == nil && !info.Mode().IsRegular() {
			return nil, errors.New("state database and its sidecars must be regular files")
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	keepLock := false
	defer func() {
		if !keepLock {
			lock.Close()
		}
	}()
	if err := lockStateFile(lock); err != nil {
		return nil, fmt.Errorf("lock state database: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		if err := file.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}

	name := filepath.ToSlash(path)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	dsn := url.URL{Scheme: "file", Path: name}
	values := url.Values{"mode": {"rw"}, "_txlock": {"immediate"}, "_pragma": {
		"busy_timeout(" + fmt.Sprint(busyTimeout.Milliseconds()) + ")", "synchronous(FULL)",
	}}
	dsn.RawQuery = values.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate state database: %w", err)
	}
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journal); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable state database WAL: %w", err)
	}
	if journal != "wal" {
		db.Close()
		return nil, errors.New("state database did not enable WAL")
	}
	keepLock = true
	return &Store{db: db, lock: lock}, nil
}

func (s *Store) Close() error { return errors.Join(s.db.Close(), s.lock.Close()) }

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id, version int
	if err := tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&id); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	switch {
	case id == applicationID && version == 1:
		return tx.Commit()
	case id == applicationID && version > 1:
		return fmt.Errorf("unsupported state database version %d", version)
	case id != 0 || version != 0:
		return errors.New("file is not an Irisync state database")
	}
	var objects int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&objects); err != nil {
		return err
	}
	if objects != 0 {
		return errors.New("database already contains an unrelated schema")
	}
	if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d; PRAGMA user_version = 1", applicationID)); err != nil {
		return err
	}
	return tx.Commit()
}

// timestamp uses fixed-width nanoseconds so SQLite can order retry times as text.
const timestampLayout = "2006-01-02T15:04:05.000000000Z"

func timestamp(value time.Time) string { return value.UTC().Format(timestampLayout) }
