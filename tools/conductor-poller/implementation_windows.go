package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const implementationAttempts = 4

var errImplementationAttempts = errors.New("implementation correction limit reached")

type implementationRecord struct {
	base, commit        string
	attempts, limit, pr int
}

func (p *poller) advanceImplementation(ctx context.Context, item projectIssue) error {
	ref, err := p.approvedImplementation(ctx, item)
	if err != nil {
		return err
	}
	if ref.Head == "" {
		if item.Status == "Implementing" {
			return p.github.setDesignStatus(ctx, item, "Design Review")
		}
		return nil
	}
	if !validCommitSHA(ref.DefaultHead) || ref.DefaultBranch != "main" {
		return fmt.Errorf("implementation requires the protected main branch")
	}

	if _, err := p.db.db.ExecContext(ctx, `INSERT OR IGNORE INTO implementations(issue_id, design_sha, base_sha)
		VALUES (?, ?, ?)`, item.ID, ref.Head, ref.DefaultHead); err != nil {
		return err
	}
	var record implementationRecord
	err = p.db.db.QueryRowContext(ctx, `SELECT base_sha, attempts, attempt_limit, commit_sha, pr_number FROM implementations
		WHERE issue_id = ? AND design_sha = ?`, item.ID, ref.Head).Scan(&record.base, &record.attempts, &record.limit, &record.commit, &record.pr)
	if err != nil {
		return err
	}
	if item.Status == "Design Review" {
		if err := p.github.setDesignStatus(ctx, item, "Implementing"); err != nil {
			return err
		}
		item.Status = "Implementing"
	}
	branch := fmt.Sprintf("conductor/implement/issue-%d-%s", item.Number, ref.Head)
	if record.pr != 0 {
		number, err := p.github.findImplementationPR(ctx, branch, record.commit)
		if err != nil {
			return err
		}
		if number != record.pr {
			return fmt.Errorf("recorded PR #%d could not be reconciled", record.pr)
		}
		return p.github.setDesignStatus(ctx, item, "PR Review")
	}
	if record.commit == "" {
		commit, err := p.runImplementation(ctx, item, ref, branch, record)
		if errors.Is(err, errImplementationAttempts) {
			return p.github.setDesignStatus(ctx, item, "Blocked")
		}
		if err != nil {
			return err
		}
		record.commit = commit
	}

	current, err := p.approvedImplementation(ctx, item)
	if err != nil {
		return err
	}
	if current.Head != ref.Head {
		return p.github.setDesignStatus(ctx, item, "Design Review")
	}
	if err := p.github.setDesignStatus(ctx, item, "Implementing"); err != nil {
		return err
	}
	return p.publishImplementation(ctx, item, ref.Head, branch, record.commit)
}

func (p *poller) approvedImplementation(ctx context.Context, item projectIssue) (designRef, error) {
	var present int
	err := p.db.db.QueryRowContext(ctx, "SELECT 1 FROM designs WHERE issue_id = ?", item.ID).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return designRef{}, nil
	}
	if err != nil {
		return designRef{}, err
	}
	branch := fmt.Sprintf("conductor/design/issue-%d", item.Number)
	path := fmt.Sprintf("docs/design/issues/issue-%d.md", item.Number)
	// Refresh the approval using the live comment and current design revision.
	if err := p.reviewDesign(ctx, item, branch, path); err != nil {
		return designRef{}, err
	}
	var approved string
	if err := p.db.db.QueryRowContext(ctx, "SELECT approved_sha FROM designs WHERE issue_id = ?", item.ID).Scan(&approved); err != nil {
		return designRef{}, err
	}
	if !validCommitSHA(approved) {
		return designRef{}, nil
	}
	ref, err := p.github.readDesignRef(ctx, branch, path)
	if err != nil {
		return designRef{}, err
	}
	if ref.Head != approved || !validDesign(ref.Markdown) {
		return designRef{}, nil
	}
	return ref, nil
}

func (p *poller) runImplementation(ctx context.Context, item projectIssue, ref designRef, branch string, record implementationRecord) (_ string, runErr error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ImplementationTimeout)
	defer cancel()
	workRoot := filepath.Join(p.cfg.DataDir, "implementation", fmt.Sprintf("%d-%s", item.Number, ref.Head))
	if err := os.MkdirAll(workRoot, 0700); err != nil {
		return "", err
	}
	worktree := filepath.Join(workRoot, "worktree")
	if err := p.prepareWorktree(ctx, worktree, branch, record.base); err != nil {
		return "", err
	}
	previous := filepath.Join(workRoot, fmt.Sprintf("attempt-%d", record.attempts))
	job := implementationManifest{
		Worktree: worktree, Branch: branch, Base: record.base, Design: ref.Markdown,
		ConductorExecutable: p.conductor.executable,
		Validator:           filepath.Join(p.cfg.WorkDir, ".conductor", "scripts", "validate-implementation.ps1"),
		Feedback:            strings.TrimSpace(readFeedback(previous, "step-error.txt") + "\n" + readPreviousReview(previous)),
	}
	if tree, err := os.ReadFile(filepath.Join(previous, "approved-tree")); err == nil && job.checkTree(string(tree)) == nil {
		return p.commitImplementation(ctx, item, ref.Head, record.base, string(tree))
	}
	if record.attempts >= record.limit {
		return "", errImplementationAttempts
	}
	requirements, err := p.github.reviewIssueRequirements(ctx, item)
	if err != nil {
		return "", err
	}
	job.IssueRequirements = requirements
	attempt := record.attempts + 1
	if record.attempts >= implementationAttempts && record.limit-record.attempts == implementationAttempts {
		job.reuseStagedImplementation(ctx)
	}
	dir := filepath.Join(workRoot, fmt.Sprintf("attempt-%d", attempt))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	manifest, err := json.Marshal(job)
	if err != nil {
		return "", err
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	log, err := os.OpenFile(filepath.Join(dir, "workflow.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer log.Close()
	if _, err := p.db.db.ExecContext(ctx, "UPDATE implementations SET attempts = ? WHERE issue_id = ? AND design_sha = ?", attempt, item.ID, ref.Head); err != nil {
		return "", err
	}
	workflow := filepath.Join(p.cfg.WorkDir, ".conductor", "workflows", "implement.yaml")
	finishActivity := p.trackWorkflow(ctx, workflowActivity{Issue: item.Number, Attempt: attempt, Phase: "implementation attempt"}, log.Name())
	defer func() { finishActivity(classifyRun(ctx, runErr)) }()
	err = runInJob(ctx, p.cfg.WorkDir, log, p.conductor.executable, "--silent", "run", workflow, "--no-interactive",
		"--input", "adapter="+self, "--input", "manifest="+manifestPath)
	if err != nil {
		if ctx.Err() == nil && attempt == record.limit {
			return "", errImplementationAttempts
		}
		return "", err
	}
	tree, err := os.ReadFile(filepath.Join(dir, "approved-tree"))
	if err != nil {
		if ctx.Err() == nil && attempt == record.limit {
			return "", errImplementationAttempts
		}
		return "", fmt.Errorf("implementation attempt %d did not pass all checks; see %s", attempt, log.Name())
	}
	if err := job.checkTree(string(tree)); err != nil {
		return "", err
	}
	return p.commitImplementation(ctx, item, ref.Head, record.base, string(tree))
}

func (p *poller) commitImplementation(ctx context.Context, item projectIssue, design, base, tree string) (string, error) {
	actor, err := p.github.automationIdentity(ctx)
	if err != nil {
		return "", err
	}
	email := fmt.Sprintf("%d+%s@users.noreply.github.com", actor.ID, actor.Login)
	commit, err := gitOutput(ctx, p.cfg.WorkDir, "-c", "user.name="+actor.Login, "-c", "user.email="+email,
		"commit-tree", tree, "-p", base, "-m", fmt.Sprintf("Implement Irisync issue #%d", item.Number),
		"-m", "Approved design: "+design)
	if err != nil {
		return "", fmt.Errorf("could not commit the validated implementation tree: %w", err)
	}
	if !validCommitSHA(string(commit)) {
		return "", fmt.Errorf("Git did not return an implementation commit")
	}
	_, err = p.db.db.ExecContext(ctx, "UPDATE implementations SET commit_sha = ? WHERE issue_id = ? AND design_sha = ?", string(commit), item.ID, design)
	return string(commit), err
}

func (p *poller) prepareWorktree(ctx context.Context, path, branch, base string) error {
	if !validCommitSHA(base) {
		return fmt.Errorf("invalid implementation base")
	}
	job := implementationManifest{Worktree: path, Branch: branch, Base: base}
	if _, err := os.Stat(path); err == nil {
		return job.checkWorktree()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := gitOutput(ctx, p.cfg.WorkDir, "cat-file", "-e", base+"^{commit}"); err != nil {
		if _, err := p.github.automationIdentity(ctx); err != nil {
			return err
		}
		if err := p.github.remoteGit(ctx, p.cfg.WorkDir, "fetch", "--no-tags", p.github.repositoryURL(), base); err != nil {
			return err
		}
	}
	if _, err := gitOutput(ctx, p.cfg.WorkDir, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
		if _, err := gitOutput(ctx, p.cfg.WorkDir, "worktree", "add", path, branch); err != nil {
			return err
		}
	} else if _, err := gitOutput(ctx, p.cfg.WorkDir, "worktree", "add", "--no-track", "-b", branch, path, base); err != nil {
		return err
	}
	return job.checkWorktree()
}

func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	output, err := gitRawOutput(ctx, dir, args...)
	return []byte(strings.TrimSpace(string(output))), err
}

func gitRawOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return runGit(ctx, dir, "", args...)
}

func (g githubProject) remoteGit(ctx context.Context, dir string, args ...string) error {
	var token string
	if g.app != nil {
		ctx, cancel := context.WithTimeout(ctx, g.cfg.GitHubTimeout)
		defer cancel()
		installation, err := g.app.token(ctx)
		if err != nil {
			return err
		}
		token = installation.value
	}
	_, err := runGit(ctx, dir, token, args...)
	return err
}

func runGit(ctx context.Context, dir, token string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	config := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
		"-c", "commit.gpgsign=false", "-c", "push.followTags=false"}
	cmd := exec.CommandContext(ctx, "git", append(config, args...)...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	if token != "" {
		cmd.Env = append(cmd.Env, "GH_TOKEN="+token, "GH_PROMPT_DISABLED=1", "GH_DEBUG=")
	}
	output, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && len(exit.Stderr) != 0 {
			detail := strings.TrimSpace(string(exit.Stderr))
			if token != "" {
				detail = strings.ReplaceAll(detail, token, "[redacted]")
			}
			return output, fmt.Errorf("Git command failed: %s: %w", detail, err)
		}
		return output, err
	}
	return output, nil
}
