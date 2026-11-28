// Package syncplan classifies synchronization work without modifying either side.
package syncplan

import (
	"errors"

	"github.com/coco-papiyon/irisync/internal/config"
	"github.com/coco-papiyon/irisync/internal/scan"
	"github.com/coco-papiyon/irisync/internal/state"
)

type Presence uint8

const (
	Unknown Presence = iota
	Absent
	RegularFile
	Other
)

type Observation struct {
	Presence Presence
	File     scan.File
}

type Action string

const (
	Reobserve       Action = "reobserve"
	NoChange        Action = "no_change"
	Copy            Action = "copy"
	Delete          Action = "delete"
	Conflict        Action = "conflict"
	RefreshBaseline Action = "refresh_baseline"
	RecordDeletion  Action = "record_deletion"
)

type Side string

const (
	Local  Side = "local"
	Remote Side = "remote"
)

type ConflictReason string

const (
	InitialDifference ConflictReason = "initial_difference"
	BothUpdated       ConflictReason = "both_updated"
	DeleteAndUpdate   ConflictReason = "delete_and_update"
	Reappearance      ConflictReason = "reappearance"
	KindMismatch      ConflictReason = "kind_mismatch"
	NameCollision     ConflictReason = "name_collision"
)

type Decision struct {
	Path         string
	Action       Action
	Source       Side // Copy only.
	Target       Side // Copy or Delete.
	BackupTarget bool // The executor preserves existing target content before application.
	Reason       ConflictReason
	Preferred    Side // Content conflicts only. The executor still verifies and preserves data.
	Local        Observation
	Remote       Observation
}

// Decide classifies a single verified path. A nil baseline means no history
// and a baseline with Deleted set is an existing tombstone.
func Decide(path string, baseline *state.File, local, remote Observation, priority config.ConflictPriority) (Decision, error) {
	if err := scan.ValidateRelativePath(path); err != nil {
		return Decision{}, err
	}
	preferred, err := preferredSide(priority)
	if err != nil {
		return Decision{}, err
	}
	if local.Presence > Other || remote.Presence > Other ||
		(local.Presence == RegularFile && (local.File.Size < 0 || local.File.Path != path)) ||
		(remote.Presence == RegularFile && (remote.File.Size < 0 || remote.File.Path != path)) {
		return Decision{}, errors.New("invalid sync observation")
	}
	return decide(path, baseline, local, remote, preferred), nil
}

func preferredSide(priority config.ConflictPriority) (Side, error) {
	switch priority {
	case "", config.PreferLocal:
		return Local, nil
	case config.PreferRemote:
		return Remote, nil
	default:
		return "", errors.New("invalid sync conflict priority")
	}
}

func decide(path string, baseline *state.File, local, remote Observation, preferred Side) Decision {
	d := Decision{Path: path, Local: local, Remote: remote}
	if local.Presence == Unknown || remote.Presence == Unknown {
		d.Action = Reobserve
		return d
	}
	if local.Presence == Other || remote.Presence == Other ||
		(baseline != nil && !baseline.Deleted && baseline.Kind != "file") {
		d.Action, d.Reason = Conflict, KindMismatch
		return d
	}
	if baseline == nil {
		switch {
		case local.Presence == Absent && remote.Presence == Absent:
			d.Action = NoChange
		case local.Presence == Absent:
			d.Action, d.Source, d.Target = Copy, Remote, Local
		case remote.Presence == Absent:
			d.Action, d.Source, d.Target = Copy, Local, Remote
		case sameContent(local.File, remote.File):
			d.Action = RefreshBaseline
		default:
			d.Action, d.Reason, d.Preferred = Conflict, InitialDifference, preferred
		}
		return d
	}
	if baseline.Deleted {
		d.Action = NoChange
		if local.Presence != Absent || remote.Presence != Absent {
			d.Action, d.Reason = Conflict, Reappearance
		}
		return d
	}

	old := scan.File{Size: baseline.Size, SHA256: baseline.SHA256}
	switch {
	case local.Presence == Absent && remote.Presence == Absent:
		d.Action = RecordDeletion
	case local.Presence == Absent:
		if sameContent(remote.File, old) {
			d.Action, d.Target, d.BackupTarget = Delete, Remote, true
		} else {
			d.Action, d.Reason, d.Preferred = Conflict, DeleteAndUpdate, preferred
		}
	case remote.Presence == Absent:
		if sameContent(local.File, old) {
			d.Action, d.Target, d.BackupTarget = Delete, Local, true
		} else {
			d.Action, d.Reason, d.Preferred = Conflict, DeleteAndUpdate, preferred
		}
	case sameContent(local.File, remote.File):
		d.Action = RefreshBaseline
		if sameContent(local.File, old) {
			d.Action = NoChange
		}
	case sameContent(local.File, old):
		d.Action, d.Source, d.Target, d.BackupTarget = Copy, Remote, Local, true
	case sameContent(remote.File, old):
		d.Action, d.Source, d.Target, d.BackupTarget = Copy, Local, Remote, true
	default:
		d.Action, d.Reason, d.Preferred = Conflict, BothUpdated, preferred
	}
	return d
}

func sameContent(a, b scan.File) bool { return a.Size == b.Size && a.SHA256 == b.SHA256 }
