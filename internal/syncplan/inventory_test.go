package syncplan

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coco-papiyon/irisync/internal/config"
	"github.com/coco-papiyon/irisync/internal/scan"
	"github.com/coco-papiyon/irisync/internal/state"
)

func TestPlanWithSQLiteAndScannedRoots(t *testing.T) {
	ctx := context.Background()
	localRoot, remoteRoot := t.TempDir(), t.TempDir()
	db, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.sqlite3"))
	requireOK(t, err)
	defer db.Close()
	pair := config.Pair{ID: "pair", ConflictPriority: config.PreferLocal}
	now := time.Now()
	content := []byte("saved")
	baseline := state.File{Kind: "file", Size: int64(len(content)), SHA256: sha256.Sum256(content), ModTime: now}
	for _, key := range []string{"gone.txt", "swap", "deleted.txt", "tombstone.txt", "partial/child.txt"} {
		id, err := db.Enqueue(ctx, pair.ID, key)
		requireOK(t, err)
		attempt, err := db.Claim(ctx, id, now)
		requireOK(t, err)
		saved := baseline
		saved.Deleted = key == "tombstone.txt"
		requireOK(t, db.Apply(ctx, state.ConfirmedResult{QueueID: id, Attempt: attempt, Baseline: saved}))
	}
	requireOK(t, os.Mkdir(filepath.Join(localRoot, "swap"), 0700))
	requireOK(t, os.WriteFile(filepath.Join(localRoot, "only-local.txt"), []byte("new"), 0600))
	requireOK(t, os.WriteFile(filepath.Join(localRoot, "tombstone.txt"), content, 0600))
	requireOK(t, os.Mkdir(filepath.Join(remoteRoot, "partial"), 0700))
	for _, path := range []string{"swap", "deleted.txt", "partial/child.txt"} {
		requireOK(t, os.WriteFile(filepath.Join(remoteRoot, filepath.FromSlash(path)), content, 0600))
	}
	local, err := scan.ScanInventory(ctx, localRoot)
	requireOK(t, err)
	remote, err := scan.ScanInventory(ctx, remoteRoot)
	requireOK(t, err)
	local.Issues = append(local.Issues, scan.Issue{Path: "partial", Kind: scan.Unscanned})

	plan, err := Plan(ctx, db, pair, local, remote)
	requireOK(t, err)
	want := []struct {
		path   string
		action Action
		reason ConflictReason
		target Side
	}{
		{"deleted.txt", Delete, "", Remote},
		{"gone.txt", RecordDeletion, "", ""},
		{"only-local.txt", Copy, "", Remote},
		{"partial/child.txt", Reobserve, "", ""},
		{"swap", Conflict, KindMismatch, ""},
		{"tombstone.txt", Conflict, Reappearance, ""},
	}
	if len(plan) != len(want) {
		t.Fatalf("expected %d decisions, got %+v", len(want), plan)
	}
	for i, expected := range want {
		got := plan[i]
		if got.Path != expected.path || got.Action != expected.action || got.Reason != expected.reason || got.Target != expected.target {
			t.Fatalf("%s: unexpected decision %+v", expected.path, got)
		}
	}
	saved, found, err := db.Baseline(ctx, pair.ID, "gone.txt")
	requireOK(t, err)
	if !found || saved.Deleted {
		t.Fatal("planning modified the confirmed baseline")
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "deleted.txt")); err != nil {
		t.Fatalf("planning modified a file: %v", err)
	}
}

func TestPlanProtectsCrossSideNamingAndUnverifiedAbsence(t *testing.T) {
	ctx := context.Background()
	pair := config.Pair{ID: "pair"}
	local := scan.Inventory{Result: scan.Result{Files: []scan.File{
		{Path: "Folder/left.txt"}, {Path: "only-local.txt"},
	}}, Complete: true}
	remote := scan.Inventory{Result: scan.Result{Files: []scan.File{{Path: "folder/right.txt"}}}}
	plan, err := Plan(ctx, baselineFixture{}, pair, local, remote)
	requireOK(t, err)
	if len(plan) != 3 || plan[0].Action != Reobserve ||
		plan[1].Action != Conflict || plan[1].Reason != NameCollision || plan[2].Action != Reobserve {
		t.Fatalf("incomplete inventory did not require reobservation: %+v", plan)
	}

	remote.Complete = true
	plan, err = Plan(ctx, baselineFixture{}, pair, local, remote)
	requireOK(t, err)
	if len(plan) != 3 ||
		plan[0].Path != "Folder/left.txt" || plan[0].Action != Conflict || plan[0].Reason != NameCollision ||
		plan[1].Path != "folder/right.txt" || plan[1].Action != Conflict || plan[1].Reason != NameCollision ||
		plan[2].Path != "only-local.txt" || plan[2].Action != Copy || plan[2].Target != Remote {
		t.Fatalf("unexpected plan after complete observations: %+v", plan)
	}

	remote.Issues = []scan.Issue{{Kind: scan.Unscanned}}
	plan, err = Plan(ctx, baselineFixture{}, pair, local, remote)
	requireOK(t, err)
	for _, decision := range plan {
		if decision.Remote.Presence != Unknown || decision.Action != Reobserve {
			t.Fatalf("root issue left a decision based on stale observations: %+v", decision)
		}
	}
	readErr := errors.New("baseline read failed")
	if decisions, err := Plan(ctx, baselineFixture{err: readErr}, pair, local, remote); !errors.Is(err, readErr) || decisions != nil {
		t.Fatalf("failed baseline produced decisions: %+v, %v", decisions, err)
	}
}

type baselineFixture struct{ err error }

func (b baselineFixture) Baselines(context.Context, string) ([]state.BaselineEntry, error) {
	return nil, b.err
}

func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
