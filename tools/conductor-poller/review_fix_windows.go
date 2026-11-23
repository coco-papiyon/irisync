package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type reviewFix struct {
	id, reviewID                   int64
	attempts                       int
	kind, head, base, seed, commit string
}

func reviewFixLimit(kind string) int {
	if kind == "conflict" {
		return 2
	}
	return 3
}

func (p *poller) fixReview(ctx context.Context, item projectIssue, pr pullDetails, design designRef, kind string, reviewID int64, feedback string) error {
	if _, err := p.db.db.ExecContext(ctx, `INSERT OR IGNORE INTO pr_fixes(pr_number, kind, head_sha, base_sha, review_id) VALUES (?, ?, ?, ?, ?)`,
		pr.Number, kind, pr.Head.SHA, pr.Base.SHA, reviewID); err != nil {
		return err
	}
	fix := reviewFix{kind: kind, reviewID: reviewID, head: pr.Head.SHA, base: pr.Base.SHA}
	err := p.db.db.QueryRowContext(ctx, `SELECT id, attempts, seed_sha FROM pr_fixes
		WHERE pr_number = ? AND kind = ? AND head_sha = ? AND base_sha = ? AND review_id = ?`,
		pr.Number, kind, pr.Head.SHA, pr.Base.SHA, reviewID).Scan(&fix.id, &fix.attempts, &fix.seed)
	if err != nil {
		return err
	}

	if item.Status != "PR Fixing" {
		if err := p.github.setDesignStatus(ctx, item, "PR Fixing"); err != nil {
			return err
		}
		item.Status = "PR Fixing"
	}
	job, workDir, err := p.prepareFix(ctx, pr, design, fix, feedback)
	if err != nil {
		return err
	}
	previous := filepath.Join(workDir, fmt.Sprintf("attempt-%d", fix.attempts))
	tree, err := os.ReadFile(filepath.Join(previous, "approved-tree"))
	if err != nil || job.checkTree(string(tree)) != nil {
		var total int
		if err := p.db.db.QueryRowContext(ctx, "SELECT coalesce(sum(attempts),0) FROM pr_fixes WHERE pr_number = ? AND kind = ?",
			pr.Number, kind).Scan(&total); err != nil {
			return err
		}
		if total >= reviewFixLimit(kind) {
			return p.github.setDesignStatus(ctx, item, "Blocked")
		}
		job.Feedback += "\n" + readFeedback(previous, "step-error.txt") + "\n" + readFeedback(previous, "review.json")
		dir := filepath.Join(workDir, fmt.Sprintf("attempt-%d", fix.attempts+1))
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if _, err := p.db.db.ExecContext(ctx, "UPDATE pr_fixes SET attempts = attempts + 1 WHERE id = ?", fix.id); err != nil {
			return err
		}
		limit := p.cfg.ImplementationTimeout
		if kind == "conflict" {
			limit = min(limit, 15*time.Minute)
		}
		if err := p.runReviewWorkflow(ctx, "implement.yaml", dir, limit, job); err != nil {
			return err
		}
		tree, err = os.ReadFile(filepath.Join(dir, "approved-tree"))
		if err != nil {
			if total+1 >= reviewFixLimit(kind) {
				return p.github.setDesignStatus(ctx, item, "Blocked")
			}
			return fmt.Errorf("PR #%d correction failed validation; see %s", pr.Number, dir)
		}
		if err := job.checkTree(string(tree)); err != nil {
			return err
		}
	}
	parents := []string{pr.Head.SHA}
	if kind == "conflict" {
		parents = append(parents, pr.Base.SHA)
	}
	fix.commit, err = p.createReviewCommit(ctx, string(tree), "Correct Irisync PR", parents...)
	if err != nil {
		return err
	}
	if _, err := p.db.db.ExecContext(ctx, "UPDATE pr_fixes SET commit_sha = ?, discarded = 0 WHERE id = ?", fix.commit, fix.id); err != nil {
		return err
	}
	return p.publishFix(ctx, item, pr, fix)
}

func (p *poller) prepareFix(ctx context.Context, pr pullDetails, design designRef, fix reviewFix, feedback string) (implementationManifest, string, error) {
	job := implementationManifest{Base: pr.Head.SHA, Design: design.Markdown,
		Validator: filepath.Join(p.cfg.WorkDir, ".conductor", "scripts", "validate-implementation.ps1")}
	for _, sha := range []string{pr.Head.SHA, pr.Base.SHA} {
		if err := p.fetchReviewCommit(ctx, sha); err != nil {
			return job, "", err
		}
	}
	if fix.kind == "conflict" {
		tree, paths, err := p.conflictTree(ctx, pr)
		if err != nil {
			return job, "", err
		}
		job.Base = fix.seed
		if job.Base == "" {
			job.Base, err = p.createReviewCommit(ctx, tree, "Seed local conflict resolution", pr.Head.SHA, pr.Base.SHA)
			if err != nil {
				return job, "", err
			}
			if _, err := p.db.db.ExecContext(ctx, "UPDATE pr_fixes SET seed_sha = ? WHERE id = ?", job.Base, fix.id); err != nil {
				return job, "", err
			}
		}
		job.ConflictPaths = paths
		bothSides, err := gitRawOutput(ctx, p.cfg.WorkDir, append([]string{"diff", "--no-ext-diff", "--no-textconv", pr.Head.SHA, pr.Base.SHA, "--"}, paths...)...)
		if err != nil {
			return job, "", err
		}
		feedback = fmt.Sprintf("Resolve the merge conflict in these paths: %s. Preserve the intent of both sides. Remove all conflict markers.\n\nDifferences between PR head and main:\n%s",
			strings.Join(paths, ", "), boundedReviewText(string(bothSides)))
	}
	dir, err := p.reviewDirectory(pr.Number, fmt.Sprintf("fix-%d", fix.id))
	if err != nil {
		return job, "", err
	}
	job.Worktree = filepath.Join(dir, "worktree")
	job.Branch = fmt.Sprintf("conductor/review/pr-%d-%d", pr.Number, fix.id)
	job.Feedback = feedback
	if err := p.prepareWorktree(ctx, job.Worktree, job.Branch, job.Base); err != nil {
		return job, "", err
	}
	return job, dir, nil
}

func (p *poller) conflictTree(ctx context.Context, pr pullDetails) (string, []string, error) {
	out, err := gitRawOutput(ctx, p.cfg.WorkDir, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", pr.Head.SHA, pr.Base.SHA)
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return "", nil, err
		}
	}
	parts := strings.FieldsFunc(string(out), func(ch rune) bool { return ch == 0 || ch == '\r' || ch == '\n' })
	if len(parts) < 2 || !validCommitSHA(parts[0]) {
		return "", nil, fmt.Errorf("Git did not return a conflicted tree and path list")
	}
	return parts[0], parts[1:], nil
}

func (m implementationManifest) checkConflictMarkers() error {
	if len(m.ConflictPaths) == 0 {
		return nil
	}
	args := []string{"--literal-pathspecs", "grep", "-I", "-q", "-E", "^(<<<<<<< |=======$|>>>>>>> |[|]{7} )", "--"}
	_, err := gitOutput(context.Background(), m.Worktree, append(args, m.ConflictPaths...)...)
	if err == nil {
		return fmt.Errorf("unresolved conflict markers remain in the candidate worktree")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil
	}
	return err
}

func (p *poller) createReviewCommit(ctx context.Context, tree, message string, parents ...string) (string, error) {
	if !validCommitSHA(tree) {
		return "", fmt.Errorf("invalid Git tree for the correction")
	}
	actor, err := p.github.automationIdentity(ctx)
	if err != nil {
		return "", err
	}
	email := fmt.Sprintf("%d+%s@users.noreply.github.com", actor.ID, actor.Login)
	args := []string{"-c", "user.name=" + actor.Login, "-c", "user.email=" + email, "commit-tree", tree, "-m", message}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	commit, err := gitOutput(ctx, p.cfg.WorkDir, args...)
	if err != nil {
		return "", err
	}
	if !validCommitSHA(string(commit)) {
		return "", fmt.Errorf("Git did not return a correction commit")
	}
	return string(commit), nil
}

func (p *poller) resumeFix(ctx context.Context, item projectIssue, pr pullDetails) (bool, error) {
	var fix reviewFix
	err := p.db.db.QueryRowContext(ctx, `SELECT id, kind, head_sha, base_sha, review_id, commit_sha
		FROM pr_fixes WHERE pr_number = ? AND published = 0 AND discarded = 0 AND commit_sha <> ''
		AND ((head_sha = ? AND base_sha = ?) OR commit_sha = ?) ORDER BY id DESC LIMIT 1`,
		pr.Number, pr.Head.SHA, pr.Base.SHA, pr.Head.SHA).
		Scan(&fix.id, &fix.kind, &fix.head, &fix.base, &fix.reviewID, &fix.commit)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, p.publishFix(ctx, item, pr, fix)
}

func (p *poller) publishFix(ctx context.Context, item projectIssue, pr pullDetails, fix reviewFix) error {
	if !validCommitSHA(fix.commit) {
		return fmt.Errorf("invalid correction commit")
	}
	fresh, err := p.github.pullDetails(ctx, pr.Number, pr.Head.Ref)
	if err != nil {
		return err
	}
	if fresh.State != "open" || fresh.Merged {
		return nil
	}
	if fresh.Head.SHA != fix.commit {
		if fresh.Head.SHA != fix.head || fresh.Base.SHA != fix.base {
			return nil
		}
		if fix.kind != "conflict" {
			request, err := p.github.humanRequest(ctx, fresh)
			if err != nil {
				return err
			}
			if request.ID != fix.reviewID {
				_, err := p.db.db.ExecContext(ctx, "UPDATE pr_fixes SET discarded = 1 WHERE id = ?", fix.id)
				return err
			}
		}
		var design string
		if err := p.db.db.QueryRowContext(ctx, "SELECT design_sha FROM implementations WHERE issue_id = ? AND pr_number = ?", item.ID, pr.Number).Scan(&design); err != nil {
			return err
		}
		approved, err := p.approvedImplementation(ctx, item)
		if err != nil {
			return err
		}
		if approved.Head != design {
			return p.github.setDesignStatus(ctx, item, "Blocked")
		}
		if _, err := p.github.automationIdentity(ctx); err != nil {
			return err
		}
		if err := p.github.setDesignStatus(ctx, item, "PR Fixing"); err != nil {
			return err
		}
		item.Status = "PR Fixing"
		if _, err := gitOutput(ctx, p.cfg.WorkDir, "merge-base", "--is-ancestor", fix.head, fix.commit); err != nil {
			return fmt.Errorf("correction is not a fast-forward update: %w", err)
		}
		if _, err := gitOutput(ctx, p.cfg.WorkDir, "push", p.github.repositoryURL(), fix.commit+":refs/heads/"+fresh.Head.Ref); err != nil {
			return err
		}
	}
	if err := p.github.setDesignStatus(ctx, item, "PR Review"); err != nil {
		return err
	}
	_, err = p.db.db.ExecContext(ctx, "UPDATE pr_fixes SET published = 1 WHERE id = ?", fix.id)
	return err
}
