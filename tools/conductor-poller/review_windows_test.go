package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEmbeddedSkillsHaveMetadata(t *testing.T) {
	for name, skill := range map[string]string{
		"irisync-design": irisyncDesignSkill, "irisync-implementation": irisyncImplementationSkill, "irisync-review": irisyncReviewSkill,
	} {
		t.Run(name, func(t *testing.T) {
			parts := strings.SplitN(strings.ReplaceAll(skill, "\r\n", "\n"), "---\n", 3)
			if len(parts) != 3 || parts[0] != "" {
				t.Fatal("embedded skill has no YAML frontmatter")
			}
			var metadata struct{ Name, Description string }
			if err := yaml.Unmarshal([]byte(parts[1]), &metadata); err != nil ||
				metadata.Name != name || strings.TrimSpace(metadata.Description) == "" || strings.TrimSpace(parts[2]) == "" {
				t.Fatalf("invalid embedded skill: %+v (%v)", metadata, err)
			}
		})
	}
}

func TestReviewCollectsAllFindingsAndDisplaysCriteria(t *testing.T) {
	report := reviewOutput{
		Criteria: []reviewCriterion{
			{Criterion: "入力検証", Status: "unmet", Evidence: "設定の境界値が未検証"},
			{Criterion: "CI の実行", Status: "post_publish", Evidence: "PR 公開後に確認"},
		},
		Findings: []reviewFinding{
			{Location: "config.go:23", Problem: "空の名前を受け入れる", Fix: "空文字を拒否する"},
			{Location: "config_test.go:40", Problem: "上限値のテストがない", Fix: "境界値のケースを追加する"},
		},
	}
	verdict, err := report.verdict()
	if err != nil || verdict.Approved || !strings.Contains(verdict.Feedback, "1. config.go:23") ||
		!strings.Contains(verdict.Feedback, "2. config_test.go:40") || !strings.Contains(verdict.Feedback, "[公開後に確認]") {
		t.Fatalf("unexpected consolidated review: %+v (%v)", verdict, err)
	}
	report.Findings = []reviewFinding{}
	if _, err := report.verdict(); err == nil {
		t.Fatal("an unmet criterion without a corresponding finding was accepted")
	}
	report.Criteria[0].Status = "met"
	if verdict, err := report.verdict(); err != nil || !verdict.Approved {
		t.Fatalf("publication-only checks blocked the review: %+v (%v)", verdict, err)
	}
}

func TestReviewPolicyMigrationPreservesExistingVerdict(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `CREATE TABLE pr_reviews(approved INTEGER, feedback TEXT);
		INSERT INTO pr_reviews VALUES (0, 'previous finding');`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := ensureReviewPolicyVersion(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var approved, version int
	var feedback string
	if err := db.QueryRowContext(ctx, "SELECT approved, feedback, policy_version FROM pr_reviews").Scan(&approved, &feedback, &version); err != nil ||
		approved != 0 || feedback != "previous finding" || version != 0 {
		t.Fatalf("unexpected migrated verdict: %d %q %d (%v)", approved, feedback, version, err)
	}
}

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

func TestManualPRChecksRequireBothIrisyncJobsWhenConfigured(t *testing.T) {
	check := prCheck{Status: "completed", Conclusion: "success"}
	check.App.ID, check.App.Slug = 15368, "github-actions"
	checks := []prCheck{check, check, check}
	checks[0].Name = "Conductor smoke (Windows)"
	checks[1].Name = "Irisync Go (windows-2025)"
	checks[2].Name = "Irisync Go (ubuntu-latest)"
	if !successfulManualChecks(checks[:1], false) || !successfulManualChecks(checks, true) ||
		successfulManualChecks(checks[:2], true) {
		t.Fatal("manual PR accepted the wrong set of checks")
	}
	checks[2].App.ID = 1
	if successfulManualChecks(checks, true) {
		t.Fatal("a different App was accepted for required Irisync CI")
	}
	checks[2].App.ID = 15368
	checks[2].Conclusion = "failure"
	if successfulManualChecks(checks, true) {
		t.Fatal("a failed Irisync check was accepted")
	}
}

func TestManualPRIsTrackedAfterRegistration(t *testing.T) {
	ctx := context.Background()
	state, closeState, err := openState(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeState()
	item := projectIssue{ID: "issue", Status: "PR Review"}
	if tracked, err := state.tracksIssue(ctx, item); err != nil || tracked {
		t.Fatalf("unregistered PR tracked=%v, error=%v", tracked, err)
	}
	if _, err := state.db.ExecContext(ctx, "INSERT INTO manual_prs(issue_id, pr_number, head_branch) VALUES (?, ?, ?)", item.ID, 51, "feat/a01"); err != nil {
		t.Fatal(err)
	}
	item.Status, item.Closed = "Merge Ready", true
	if tracked, err := state.tracksIssue(ctx, item); err != nil || !tracked {
		t.Fatalf("registered PR tracked=%v, error=%v", tracked, err)
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
