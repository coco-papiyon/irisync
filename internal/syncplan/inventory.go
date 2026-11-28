package syncplan

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/coco-papiyon/irisync/internal/config"
	"github.com/coco-papiyon/irisync/internal/scan"
	"github.com/coco-papiyon/irisync/internal/state"
)

type BaselineReader interface {
	Baselines(context.Context, string) ([]state.BaselineEntry, error)
}

// Plan builds a stable, path-ordered plan from read-only state and inventories.
// An inventory without verified directory coverage may supply positive file
// observations, but omitted paths stay unknown.
func Plan(ctx context.Context, db BaselineReader, pair config.Pair, local, remote scan.Inventory) ([]Decision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	preferred, err := preferredSide(pair.ConflictPriority)
	if err != nil {
		return nil, err
	}
	baselines, err := db.Baselines(ctx, pair.ID)
	if err != nil {
		return nil, err
	}
	localIndex, err := indexInventory(local)
	if err != nil {
		return nil, err
	}
	remoteIndex, err := indexInventory(remote)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]*state.File, len(baselines)+len(local.Files)+len(remote.Files))
	for _, baseline := range baselines {
		if err := scan.ValidateRelativePath(baseline.Path); err != nil {
			return nil, err
		}
		if _, duplicate := paths[baseline.Path]; duplicate {
			return nil, errors.New("duplicate synchronization baseline")
		}
		file := baseline.File
		paths[baseline.Path] = &file
	}
	for _, index := range []inventoryIndex{localIndex, remoteIndex} {
		for path := range index.files {
			if _, exists := paths[path]; !exists {
				paths[path] = nil
			}
		}
	}
	candidates := make([]string, 0, len(paths))
	for path := range paths {
		candidates = append(candidates, path)
	}
	slices.Sort(candidates)
	names := slices.Clone(candidates)
	for _, index := range []inventoryIndex{localIndex, remoteIndex} {
		for path := range index.directories {
			names = append(names, path)
		}
		for path := range index.issues {
			if scan.ValidateRelativePath(path) == nil {
				names = append(names, path)
			}
		}
	}
	collisions, err := scan.ConflictingPaths(names)
	if err != nil {
		return nil, err
	}
	decisions := make([]Decision, 0, len(candidates))
	for _, path := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a, b := localIndex.observe(path), remoteIndex.observe(path)
		decision := decide(path, paths[path], a, b, preferred)
		if decision.Action != Reobserve && covered(path, collisions) {
			decision = Decision{Path: path, Action: Conflict, Reason: NameCollision, Local: a, Remote: b}
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

type inventoryIndex struct {
	files       map[string]scan.File
	directories map[string]bool
	issues      map[string]bool
	complete    bool
}

func indexInventory(inventory scan.Inventory) (inventoryIndex, error) {
	index := inventoryIndex{
		files:       make(map[string]scan.File, len(inventory.Files)),
		directories: make(map[string]bool, len(inventory.Directories)),
		issues:      make(map[string]bool, len(inventory.Issues)),
		complete:    inventory.Complete,
	}
	for _, path := range inventory.Directories {
		if err := scan.ValidateRelativePath(path); err != nil {
			return inventoryIndex{}, err
		}
		index.addDirectory(path)
	}
	for _, file := range inventory.Files {
		if err := scan.ValidateRelativePath(file.Path); err != nil {
			return inventoryIndex{}, err
		}
		if _, duplicate := index.files[file.Path]; duplicate || file.Size < 0 {
			return inventoryIndex{}, errors.New("invalid or duplicate file observation")
		}
		index.files[file.Path] = file
		index.addDirectory(parentPath(file.Path))
	}
	for path := range index.files {
		if index.directories[path] {
			return inventoryIndex{}, errors.New("path was observed as both a file and a directory")
		}
	}
	for _, issue := range inventory.Issues {
		index.issues[issue.Path] = true
	}
	return index, nil
}

func (index inventoryIndex) addDirectory(path string) {
	for path != "" && !index.directories[path] {
		index.directories[path] = true
		path = parentPath(path)
	}
}

func (index inventoryIndex) observe(path string) Observation {
	if covered(path, index.issues) {
		return Observation{Presence: Unknown}
	}
	if file, ok := index.files[path]; ok {
		return Observation{Presence: RegularFile, File: file}
	}
	if index.directories[path] {
		return Observation{Presence: Other}
	}
	for parent := parentPath(path); parent != ""; parent = parentPath(parent) {
		if _, blocked := index.files[parent]; blocked {
			return Observation{Presence: Other}
		}
	}
	if index.complete {
		return Observation{Presence: Absent}
	}
	return Observation{Presence: Unknown}
}

func covered(path string, blocked map[string]bool) bool {
	for {
		if blocked[path] {
			return true
		}
		if path == "" {
			return false
		}
		path = parentPath(path)
	}
}

func parentPath(path string) string {
	if split := strings.LastIndexByte(path, '/'); split >= 0 {
		return path[:split]
	}
	return ""
}
