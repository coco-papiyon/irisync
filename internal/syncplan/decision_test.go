package syncplan

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/coco-papiyon/irisync/internal/config"
	"github.com/coco-papiyon/irisync/internal/scan"
	"github.com/coco-papiyon/irisync/internal/state"
)

func TestDecide(t *testing.T) {
	const path = "日本語.txt"
	file := func(content string, at time.Time) Observation {
		return Observation{Presence: RegularFile, File: scan.File{
			Path: path, Size: int64(len(content)), SHA256: sha256.Sum256([]byte(content)), ModTime: at,
		}}
	}
	now := time.Now()
	unchanged, changed, another := file("a", now), file("b", now), file("c", now)
	mtimeOnly := file("a", now.Add(time.Hour))
	base := &state.File{Kind: "file", Size: 1, SHA256: unchanged.File.SHA256, ModTime: now}
	tombstone := &state.File{Kind: "file", Size: 1, SHA256: base.SHA256, Deleted: true}
	absent, unknown, directory := Observation{Presence: Absent}, Observation{}, Observation{Presence: Other}
	cases := []struct {
		name           string
		baseline       *state.File
		local, remote  Observation
		want           Action
		source, target Side
		reason         ConflictReason
		preferred      Side
		backup         bool
	}{
		{"initial local", nil, unchanged, absent, Copy, Local, Remote, "", "", false},
		{"initial remote", nil, absent, unchanged, Copy, Remote, Local, "", "", false},
		{"initial match", nil, unchanged, mtimeOnly, RefreshBaseline, "", "", "", "", false},
		{"initial difference", nil, unchanged, changed, Conflict, "", "", InitialDifference, Remote, false},
		{"initial empty", nil, absent, absent, NoChange, "", "", "", "", false},
		{"timestamp only", base, mtimeOnly, unchanged, NoChange, "", "", "", "", false},
		{"local update", base, changed, unchanged, Copy, Local, Remote, "", "", true},
		{"remote update", base, unchanged, changed, Copy, Remote, Local, "", "", true},
		{"same update", base, changed, changed, RefreshBaseline, "", "", "", "", false},
		{"divergent update", base, changed, another, Conflict, "", "", BothUpdated, Remote, false},
		{"local deleted", base, absent, mtimeOnly, Delete, "", Remote, "", "", true},
		{"remote deleted", base, mtimeOnly, absent, Delete, "", Local, "", "", true},
		{"local deleted remote updated", base, absent, changed, Conflict, "", "", DeleteAndUpdate, Remote, false},
		{"local updated remote deleted", base, changed, absent, Conflict, "", "", DeleteAndUpdate, Remote, false},
		{"both deleted", base, absent, absent, RecordDeletion, "", "", "", "", false},
		{"still deleted", tombstone, absent, absent, NoChange, "", "", "", "", false},
		{"reappeared", tombstone, unchanged, absent, Conflict, "", "", Reappearance, "", false},
		{"directory replaced file", base, directory, unchanged, Conflict, "", "", KindMismatch, "", false},
		{"one side unconfirmed", base, unknown, unchanged, Reobserve, "", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decide(path, tc.baseline, tc.local, tc.remote, config.PreferRemote)
			if err != nil {
				t.Fatal(err)
			}
			if got.Action != tc.want || got.Source != tc.source || got.Target != tc.target ||
				got.Reason != tc.reason || got.Preferred != tc.preferred || got.BackupTarget != tc.backup {
				t.Fatalf("unexpected decision: %+v, want %s %s", got, tc.want, tc.reason)
			}
			if got.Path != path || got.Local != tc.local || got.Remote != tc.remote {
				t.Fatal("decision lost the original observations")
			}
		})
	}
}
