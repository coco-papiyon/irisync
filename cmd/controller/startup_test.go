package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/coco-papiyon/irisync/internal/config"
	"github.com/coco-papiyon/irisync/internal/state"
)

func TestRestoreStartupRequests(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetDesired(ctx, "active", state.Running); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartScan(ctx, "progress-only"); err != nil {
		t.Fatal(err)
	}
	pairs := []config.Pair{
		{ID: "active", Startup: config.Restore},
		{ID: "missing", Startup: config.Restore},
		{ID: "progress-only", Startup: config.Restore},
		{ID: "active", Startup: config.Stop},
		{ID: "missing", Startup: config.Start},
	}
	want := []state.Desired{state.Running, state.Stopped, state.Stopped, state.Stopped, state.Running}
	restored, err := restorePairs(ctx, store, pairs)
	if err != nil {
		t.Fatal(err)
	}
	for i, pair := range restored {
		if pair.Requested != want[i] {
			t.Fatalf("pair %d startup = %q, want %q", i, pair.Requested, want[i])
		}
	}
	if !restored[2].HasProgress || restored[2].Initial.Desired != "" {
		t.Fatal("progress-only state was not restored independently")
	}
	store.Close()
	if _, err := restorePairs(ctx, store, pairs); err == nil {
		t.Fatal("database read failure was treated as an empty history")
	}
}

func TestRejectDatabaseUnderLocalRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := databasePath(filepath.Join(root, "state.sqlite"), []config.Pair{{LocalPath: root}}); err == nil {
		t.Fatal("accepted a state database in a synchronization root")
	}
}
