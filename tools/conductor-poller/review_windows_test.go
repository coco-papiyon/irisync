package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReviewUsesAuthorizedRequestForTheCurrentHead(t *testing.T) {
	cfg := config{GitHubLogin: "operator", DesignApproverIDs: []int64{7, 8}}
	pr := pullDetails{User: githubIdentity{ID: 8, Login: "worker"}}
	pr.Head.SHA = "current"
	request := humanReview{ID: 10, State: "CHANGES_REQUESTED", CommitID: "current"}
	request.User.ID, request.User.Login, request.User.Type = 7, "operator", "User"
	stale := request
	stale.ID, stale.CommitID = 11, "previous"
	worker := request
	worker.ID, worker.User.ID, worker.User.Login = 12, 8, "worker"
	commentOnly := request
	commentOnly.ID, commentOnly.State = 13, "COMMENTED"
	reviews := []humanReview{stale, worker, request, commentOnly}
	if got := requestedHumanReview(reviews, cfg, pr); got.ID != request.ID {
		t.Fatalf("unexpected review was selected: %d", got.ID)
	}
	approved := request
	approved.ID, approved.State = 14, "APPROVED"
	if got := requestedHumanReview(append(reviews, approved), cfg, pr); got.ID != 0 {
		t.Fatalf("obsolete change request was reused: %d", got.ID)
	}
}

func TestMergeStatusRequiresCurrentVerifiedChecksAndHumanMerger(t *testing.T) {
	check := prCheck{Name: "Conductor smoke (Windows)", Status: "completed", Conclusion: "success"}
	check.App.ID, check.App.Slug = 15368, "github-actions"
	ready := mergeObservation{Open: true, Mergeable: true, Reviewed: true, PassedCI: successfulWindowsCheck([]prCheck{check})}
	if ready.status() != "Merge Ready" {
		t.Fatal("verified open PR did not enter Merge Ready")
	}
	check.Conclusion = "skipped"
	ready.PassedCI = successfulWindowsCheck([]prCheck{check})
	if ready.status() == "Merge Ready" {
		t.Fatal("skipped CI was accepted as successful")
	}
	if (mergeObservation{Merged: true}).status() == "Done" {
		t.Fatal("an unverified merger was accepted")
	}
	if (mergeObservation{Merged: true, HumanMerger: true}).status() != "Done" {
		t.Fatal("verified human merge was not recognized")
	}
}

func TestConflictTreeAndMarkers(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := gitOutput(ctx, repo, args...)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	write := func(contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "conflict.txt"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	author := []string{"-c", "user.name=Test", "-c", "user.email=test@example.org"}
	commit := func(contents string) string {
		t.Helper()
		write(contents)
		git("add", "--", "conflict.txt")
		git(append(append([]string{}, author...), "commit", "-m", strings.TrimSpace(contents))...)
		return git("rev-parse", "HEAD")
	}
	git("init", "--initial-branch=main")
	commit("common\n")
	git("switch", "-c", "issue")
	head := commit("source\n")
	git("switch", "main")
	base := commit("main\n")
	p := poller{cfg: config{WorkDir: repo}}
	var pr pullDetails
	pr.Head.SHA, pr.Base.SHA = head, base
	tree, paths, err := p.conflictTree(ctx, pr)
	if err != nil || !slices.Equal(paths, []string{"conflict.txt"}) {
		t.Fatalf("unexpected conflict: paths=%q err=%v", paths, err)
	}
	m := implementationManifest{Worktree: repo, ConflictPaths: paths}
	write(git("show", tree+":conflict.txt") + "\n")
	if err := m.checkConflictMarkers(); err == nil {
		t.Fatal("conflict markers escaped machine validation")
	}
	write("resolved\n")
	if err := m.checkConflictMarkers(); err != nil {
		t.Fatal(err)
	}
	if current := git("rev-parse", "HEAD"); current != base {
		t.Fatalf("computing a conflict changed the checkout: %s", current)
	}
}
