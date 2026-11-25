package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImplementationReusesOnlyMatchingPR(t *testing.T) {
	repository := "example/irisync"
	branch := "conductor/implement/issue-1"
	commit := strings.Repeat("a", 40)
	pr := implementationPR{Number: 42, State: "open"}
	pr.Head.Ref, pr.Head.SHA, pr.Head.Repo.FullName = branch, commit, repository
	pr.Base.Ref, pr.Base.Repo.FullName = "main", repository
	if got, err := existingImplementationPR([]implementationPR{pr}, repository, branch, commit); err != nil || got != 42 {
		t.Fatalf("expected existing PR from a previous run: PR=%d err=%v", got, err)
	}
	if _, err := existingImplementationPR([]implementationPR{pr}, repository, branch, strings.Repeat("b", 40)); err == nil {
		t.Fatal("existing PR has a different head")
	}
	pr.State = "closed"
	if _, err := existingImplementationPR([]implementationPR{pr}, repository, branch, commit); err == nil {
		t.Fatal("closed PR could be recreated")
	}
}

func TestImplementationTreeRejectsEditsAfterValidation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitOutput(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	git(repo, "init", "--initial-branch=main")
	git(repo, "-c", "user.name=Test", "-c", "user.email=test@example.org", "commit", "--allow-empty", "-m", "Base")
	base := git(repo, "rev-parse", "HEAD")
	tree := filepath.Join(root, "candidate")
	branch := "conductor/implement/issue-7"
	p := poller{cfg: config{WorkDir: repo}}
	if err := p.prepareWorktree(ctx, tree, branch, base); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tree, "README.md")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := implementationManifest{Worktree: tree, Base: base, Branch: branch}
	if err := m.applyPatch(root, "diff --git a/README.md b/README.md\nnew file mode 100644\n--- /dev/null\n+++ b/README.md\n@@ -0,0 +1 @@\n+# Candidate\n"); err != nil {
		t.Fatal(err)
	}
	validated := git(tree, "write-tree")
	m.Feedback = "Fix the candidate"
	m.VerifiedPrerequisites = &verifiedPrerequisites{
		ProjectNumber: 4,
		BlockedBy:     []verifiedPrerequisite{{Repository: "owner/example", Number: 30, Status: "Done"}},
	}
	data, err := m.editPayload()
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Feedback      string                 `json:"previous_feedback"`
		StagedDiff    string                 `json:"staged_diff_from_base"`
		Prerequisites *verifiedPrerequisites `json:"verified_prerequisites"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Feedback != m.Feedback || !strings.Contains(payload.StagedDiff, "+# Candidate\n") {
		t.Fatal("edit request omitted the previous code or review feedback")
	}
	if payload.Prerequisites == nil || payload.Prerequisites.ProjectNumber != 4 ||
		len(payload.Prerequisites.BlockedBy) != 1 || payload.Prerequisites.BlockedBy[0] != m.VerifiedPrerequisites.BlockedBy[0] {
		t.Fatal("edit request omitted the verified prerequisite")
	}
	diff, err := gitRawOutput(ctx, tree, "diff", "--cached")
	if err != nil || !strings.HasSuffix(string(diff), "+# Candidate\n") {
		t.Fatalf("review diff lost its final newline: %q, %v", diff, err)
	}
	if err := m.checkTree(validated); err != nil {
		t.Fatal(err)
	}
	write("# Changed\n")
	if err := m.checkTree(validated); err == nil {
		t.Fatal("accepted an unstaged change")
	}
	git(tree, "add", "--", "README.md")
	if err := m.checkTree(validated); err == nil {
		t.Fatal("accepted a newly staged tree")
	}
	if got := git(repo, "rev-parse", "HEAD"); got != base {
		t.Fatalf("primary checkout changed: %s", got)
	}
}

func TestImplementationEditStopsWhenSourceIsUnavailable(t *testing.T) {
	for _, response := range []string{
		`{"patch":""}`,
		`{"status":"blocked","reason":"Cannot read the current source","patch":""}`,
		`{"status":"already_satisfied","reason":"No change needed","patch":""}`,
	} {
		var edit implementationEdit
		if err := json.Unmarshal([]byte(response), &edit); err != nil {
			t.Fatal(err)
		}
		err := edit.validate(true)
		if err == nil || !strings.HasPrefix("edit: "+err.Error(), editBlockedPrefix) {
			t.Fatalf("expected the edit to stop before validation and review: %s (%v)", response, err)
		}
		if edit.Status == "blocked" && !strings.Contains(err.Error(), edit.Reason) {
			t.Fatalf("stop reason was lost: %v", err)
		}
	}
}

func TestPreviousValidationFailureSurvivesAnInterruptedEdit(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "attempt-1")
	second := filepath.Join(root, "attempt-2")
	write := func(dir, name, contents string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(first, "step-error.txt", "validate: go test failed")
	write(first, "validation.log", "--- FAIL: TestDashboardLog\nAccess is denied.\n")
	write(second, "step-error.txt", "edit: correction stopped: validation details are missing")
	feedback := previousValidationFailure(root, 2)
	if !strings.Contains(feedback, "TestDashboardLog") || !strings.Contains(feedback, "Access is denied.") {
		t.Fatalf("missing validation details: %q", feedback)
	}
	write(second, "validated-tree", strings.Repeat("a", 40))
	if feedback := previousValidationFailure(root, 2); feedback != "" {
		t.Fatalf("passed validation carried an older failure forward: %q", feedback)
	}
}
