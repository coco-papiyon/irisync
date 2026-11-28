package state

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func openTestStore(t *testing.T, ctx context.Context, path string) *Store {
	t.Helper()
	store, err := Open(ctx, path)
	must(t, err)
	t.Cleanup(func() {
		// The restart tests also close their first instance explicitly.
		_ = store.Close()
	})
	return store
}

func claimTestItem(t *testing.T, ctx context.Context, store *Store, id int64, now time.Time) int64 {
	t.Helper()
	attempt, err := store.Claim(ctx, id, now)
	must(t, err)
	return attempt
}

func TestStateSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "状態 #1.sqlite3")
	store := openTestStore(t, ctx, path)
	if other, err := Open(ctx, path); err == nil {
		other.Close()
		t.Fatal("two stores acquired the same state")
	}
	now := time.Date(2026, 11, 28, 3, 1, 2, 123, time.UTC)
	must(t, store.SetDesired(ctx, "a", Running))
	generation, err := store.StartScan(ctx, "a")
	must(t, err)
	scanTotal, processTotal := int64(2), int64(1)
	must(t, store.SaveScanProgress(ctx, "a", generation, 2, &scanTotal, &processTotal))
	for _, pair := range []string{"a", "b"} {
		id, err := store.Enqueue(ctx, pair, "空 白/A.txt")
		must(t, err)
		attempt := claimTestItem(t, ctx, store, id, now)
		result := ConfirmedResult{QueueID: id, Attempt: attempt, Baseline: File{
			Kind: "file", Size: 1, SHA256: sha256.Sum256([]byte(pair)), ModTime: now, Deleted: pair == "b",
		}}
		if pair == "a" {
			result.ProgressGeneration = generation
			result.InitialSyncCompletedAt = &now
		}
		must(t, store.Apply(ctx, result))
		must(t, store.Apply(ctx, result))
	}
	bGeneration, err := store.StartScan(ctx, "b")
	must(t, err)
	zero := int64(0)
	must(t, store.SaveScanProgress(ctx, "b", bGeneration, 0, nil, &zero))
	interrupted, err := store.Enqueue(ctx, "a", "work")
	must(t, err)
	interruptedAttempt := claimTestItem(t, ctx, store, interrupted, now)
	retry, err := store.Enqueue(ctx, "a", "retry")
	must(t, err)
	retryAttempt := claimTestItem(t, ctx, store, retry, now)
	must(t, store.ScheduleRetry(ctx, retry, retryAttempt, now.Add(time.Hour)))
	must(t, store.Close())

	store = openTestStore(t, ctx, path)
	must(t, store.RecoverInterrupted(ctx))
	pair, err := store.Pair(ctx, "a")
	must(t, err)
	if pair.Desired != Running || pair.InitialSyncCompletedAt == nil || !pair.InitialSyncCompletedAt.Equal(now) {
		t.Fatalf("pair a after reopen: %+v", pair)
	}
	pair, err = store.Pair(ctx, "b")
	must(t, err)
	if pair.Desired != "" {
		t.Fatal("progress created a desired-state record")
	}
	progress, found, err := store.Progress(ctx, "a")
	if err != nil || !found || progress.Scanned != 2 || progress.Processed != 1 ||
		progress.ScanTotal == nil || *progress.ScanTotal != 2 || progress.ProcessTotal == nil || *progress.ProcessTotal != 1 {
		t.Fatalf("pair a progress after reopen: %+v, found=%t, err=%v", progress, found, err)
	}
	progress, found, err = store.Progress(ctx, "b")
	if err != nil || !found || progress.ScanTotal != nil || progress.ProcessTotal == nil || *progress.ProcessTotal != 0 {
		t.Fatalf("pair b progress after reopen: %+v, found=%t, err=%v", progress, found, err)
	}
	for _, id := range []string{"a", "b"} {
		file, found, err := store.Baseline(ctx, id, "空 白/A.txt")
		if err != nil || !found || file.Deleted != (id == "b") ||
			file.SHA256 != sha256.Sum256([]byte(id)) || !file.ModTime.Equal(now) {
			t.Fatalf("pair %s baseline after reopen: %+v, found=%t, err=%v", id, file, found, err)
		}
	}
	if _, found, err := store.Baseline(ctx, "b", "空 白/a.txt"); err != nil || found {
		t.Fatalf("different case key: found=%t, err=%v", found, err)
	}
	items, err := store.Queue(ctx, "a")
	if err != nil || len(items) != 2 || items[0].ID != interrupted || items[0].Status != NeedsVerification ||
		items[1].ID != retry || items[1].Status != RetryWait || items[1].RetryAt == nil ||
		!items[1].RetryAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("queue after reopen: %+v, err=%v", items, err)
	}
	if _, err := store.Claim(ctx, interrupted, now); err == nil {
		t.Fatal("an interrupted item was eligible before revalidation")
	}
	must(t, store.RequeueVerified(ctx, interrupted, interruptedAttempt))
	claimTestItem(t, ctx, store, interrupted, now)
}

func TestApplyIsAtomicAndKeepsNewerQueueItems(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx, filepath.Join(t.TempDir(), "state.sqlite"))
	now := time.Now().UTC()
	old, err := store.Enqueue(ctx, "a", "file")
	must(t, err)
	oldAttempt := claimTestItem(t, ctx, store, old, now)
	must(t, store.Apply(ctx, ConfirmedResult{QueueID: old, Attempt: oldAttempt, Baseline: File{Kind: "file", Size: 7, ModTime: now}}))
	generation, err := store.StartScan(ctx, "a")
	must(t, err)
	current, err := store.Enqueue(ctx, "a", "file")
	must(t, err)
	next, err := store.Enqueue(ctx, "a", "file")
	must(t, err)
	if _, err := store.Claim(ctx, next, now); err == nil {
		t.Fatal("a newer result could overtake the earlier item for its path")
	}
	currentAttempt := claimTestItem(t, ctx, store, current, now)
	_, err = store.db.ExecContext(ctx, `
		CREATE TRIGGER fail_receipt BEFORE INSERT ON sync_receipts
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`)
	must(t, err)
	result := ConfirmedResult{QueueID: current, Attempt: currentAttempt, ProgressGeneration: generation,
		Baseline: File{Kind: "file", ModTime: now, Deleted: true}}
	if err := store.Apply(ctx, result); err == nil {
		t.Fatal("injected failure was ignored")
	}
	file, found, err := store.Baseline(ctx, "a", "file")
	if err != nil || !found || file.Deleted || file.Size != 7 {
		t.Fatalf("baseline after failed transaction: %+v, found=%t, err=%v", file, found, err)
	}
	progress, _, err := store.Progress(ctx, "a")
	if err != nil || progress.Processed != 0 {
		t.Fatalf("progress after failed transaction: %+v, err=%v", progress, err)
	}
	items, err := store.Queue(ctx, "a")
	if err != nil || len(items) != 2 || items[0].ID != current || items[0].Status != InProgress || items[1].ID != next {
		t.Fatalf("queue after failed transaction: %+v, err=%v", items, err)
	}
	_, err = store.db.ExecContext(ctx, "DROP TRIGGER fail_receipt")
	must(t, err)
	must(t, store.Apply(ctx, result))
	must(t, store.Apply(ctx, result))
	file, found, err = store.Baseline(ctx, "a", "file")
	if err != nil || !found || !file.Deleted {
		t.Fatalf("baseline after successful replay: %+v, found=%t, err=%v", file, found, err)
	}
	progress, _, err = store.Progress(ctx, "a")
	if err != nil || progress.Processed != 1 {
		t.Fatalf("progress after successful replay: %+v, err=%v", progress, err)
	}
	items, err = store.Queue(ctx, "a")
	if err != nil || len(items) != 1 || items[0].ID != next {
		t.Fatalf("newer queued item was lost: %+v, err=%v", items, err)
	}
	if err := store.CompleteInitialSync(ctx, "a", generation, now); err == nil {
		t.Fatal("marked initial sync complete with pending work")
	}
	newGeneration, err := store.StartScan(ctx, "a")
	must(t, err)
	result.Attempt = claimTestItem(t, ctx, store, next, now)
	result.QueueID = next
	must(t, store.Apply(ctx, result))
	progress, _, err = store.Progress(ctx, "a")
	if err != nil || progress.Generation != newGeneration || progress.Processed != 0 {
		t.Fatalf("old result counted in new generation: %+v, err=%v", progress, err)
	}
}

func TestRejectsFutureSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store := openTestStore(t, ctx, path)
	_, err := store.db.ExecContext(ctx, "PRAGMA user_version = 2")
	must(t, err)
	must(t, store.Close())
	if store, err := Open(ctx, path); err == nil {
		store.Close()
		t.Fatal("opened a future schema")
	}
}

func TestCommitFailureRechecksOnFreshConnection(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx, filepath.Join(t.TempDir(), "state.sqlite"))
	now := time.Now().UTC()
	id, err := store.Enqueue(ctx, "a", "file")
	must(t, err)
	attempt := claimTestItem(t, ctx, store, id, now)
	_, err = store.db.ExecContext(ctx, `
		PRAGMA foreign_keys = ON;
		CREATE TABLE parent_test(id INTEGER PRIMARY KEY);
		CREATE TABLE child_test(parent_id INTEGER REFERENCES parent_test(id) DEFERRABLE INITIALLY DEFERRED);
		CREATE TRIGGER fail_commit AFTER INSERT ON sync_receipts BEGIN INSERT INTO child_test VALUES (1); END;
	`)
	must(t, err)
	result := ConfirmedResult{QueueID: id, Attempt: attempt, Baseline: File{Kind: "file", ModTime: now}}
	if err := store.Apply(ctx, result); err == nil {
		t.Fatal("deferred commit failure was ignored")
	}
	if _, found, err := store.Baseline(ctx, "a", "file"); err != nil || found {
		t.Fatalf("checked connection saw a failed result: found=%t, err=%v", found, err)
	}
	_, err = store.db.ExecContext(ctx, "DROP TRIGGER fail_commit")
	must(t, err)
	must(t, store.Apply(ctx, result))
}

func TestIncompleteScanCannotBeMarkedComplete(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx, filepath.Join(t.TempDir(), "state.sqlite"))
	generation, err := store.StartScan(ctx, "a")
	must(t, err)
	total := int64(3)
	must(t, store.SaveScanProgress(ctx, "a", generation, 1, &total, nil))
	if err := store.CompleteInitialSync(ctx, "a", generation, time.Now()); err == nil {
		t.Fatal("marked initial sync complete while the stored scan was incomplete")
	}
	must(t, store.SaveScanProgress(ctx, "a", generation, total, &total, nil))
	must(t, store.CompleteInitialSync(ctx, "a", generation, time.Now()))
}

func TestEarlierAttemptCannotCompleteRetriedQueueItem(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx, filepath.Join(t.TempDir(), "state.sqlite"))
	now := time.Now()
	id, err := store.Enqueue(ctx, "a", "file")
	must(t, err)
	oldAttempt := claimTestItem(t, ctx, store, id, now)
	old := ConfirmedResult{QueueID: id, Attempt: oldAttempt, Baseline: File{Kind: "file", ModTime: now}}
	must(t, store.ScheduleRetry(ctx, id, oldAttempt, now))
	currentAttempt := claimTestItem(t, ctx, store, id, now)
	if err := store.Apply(ctx, old); err == nil {
		t.Fatal("accepted a result from an earlier attempt")
	}
	if err := store.ScheduleRetry(ctx, id, oldAttempt, now); err == nil {
		t.Fatal("an earlier attempt could reschedule newer work")
	}
	current := old
	current.Attempt = currentAttempt
	must(t, store.Apply(ctx, current))
}
