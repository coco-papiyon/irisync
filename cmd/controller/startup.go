package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/coco-papiyon/irisync/internal/config"
	"github.com/coco-papiyon/irisync/internal/state"
)

type startupPair struct {
	ID          string
	Requested   state.Desired
	Initial     state.Pair
	Progress    state.Progress
	HasProgress bool
}

// Restored state supplies startup requests. The engine will establish actual
// runtime and SSH connection state when it handles those requests.
func restorePairs(ctx context.Context, store *state.Store, pairs []config.Pair) ([]startupPair, error) {
	restored := make([]startupPair, 0, len(pairs))
	for _, pair := range pairs {
		saved, err := store.Pair(ctx, pair.ID)
		if err != nil {
			return nil, err
		}
		progress, hasProgress, err := store.Progress(ctx, pair.ID)
		if err != nil {
			return nil, err
		}
		request := state.Stopped
		if pair.Startup == config.Start || (pair.Startup == config.Restore && saved.Desired == state.Running) {
			request = state.Running
		}
		restored = append(restored, startupPair{
			ID: pair.ID, Requested: request, Initial: saved, Progress: progress, HasProgress: hasProgress,
		})
	}
	if err := store.RecoverInterrupted(ctx); err != nil {
		return nil, err
	}
	return restored, nil
}

func databasePath(path string, pairs []config.Pair) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("invalid state database path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", errors.New("could not resolve the state database directory")
	}
	for _, pair := range pairs {
		for _, directory := range []string{filepath.Dir(absolute), parent} {
			rel, err := filepath.Rel(pair.LocalPath, directory)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", errors.New("state database must be outside the local synchronization roots")
			}
		}
		root, err := os.Stat(pair.LocalPath)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", errors.New("could not inspect the local synchronization root")
		}
		for dir := parent; ; dir = filepath.Dir(dir) {
			info, err := os.Stat(dir)
			if err != nil {
				return "", errors.New("could not inspect the state database directory")
			}
			if os.SameFile(root, info) {
				return "", errors.New("state database must be outside the local synchronization roots")
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func withoutPath(err error) error {
	var pathError *fs.PathError
	if errors.As(err, &pathError) {
		return pathError.Err
	}
	return err
}
