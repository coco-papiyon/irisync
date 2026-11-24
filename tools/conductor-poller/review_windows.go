package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type mergeObservation struct {
	Merged, HumanMerger, Open, Mergeable, Reviewed, PassedCI bool
}

const reviewPolicyVersion = 2

func (m mergeObservation) status() string {
	if m.Merged {
		if m.HumanMerger {
			return "Done"
		}
		return ""
	}
	if m.Open && m.Mergeable && m.Reviewed && m.PassedCI {
		return "Merge Ready"
	}
	return "PR Review"
}

func runReviewAdapter(mode, manifest string) error {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	dir := filepath.Dir(manifest)
	if mode == "--merge-check-adapter" {
		var observation mergeObservation
		if err := json.Unmarshal(data, &observation); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "merge-result.txt"), []byte(observation.status()), 0600)
	}
	prompt := "PR の独立レビューを行う。ci_checks は head_sha の GitHub Actions の記録である。CI の状態だけを理由にコードの修正を求めず、差分に具体的な問題がある場合は報告する。"
	_, err = codexReview(dir, prompt, data)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "review-complete"), []byte("ok"), 0600)
}

func (p *poller) advancePRReview(ctx context.Context, item projectIssue) error {
	var design string
	var number int
	err := p.db.db.QueryRowContext(ctx, `SELECT design_sha, pr_number FROM implementations
		WHERE issue_id = ? AND pr_number != 0 ORDER BY pr_number DESC LIMIT 1`, item.ID).Scan(&design, &number)
	if errors.Is(err, sql.ErrNoRows) {
		return p.advanceManualPR(ctx, item)
	}
	if err != nil {
		return err
	}
	branch := fmt.Sprintf("conductor/implement/issue-%d-%s", item.Number, design)
	pr, err := p.github.pullDetails(ctx, number, branch)
	if err != nil {
		return err
	}
	if pr.Merged {
		observation := mergeObservation{Merged: true, HumanMerger: authorizedHuman(p.cfg, pr.MergedBy.ID, pr.MergedBy.Login, pr.MergedBy.Type)}
		return p.applyMergeObservation(ctx, item, pr, observation)
	}
	if item.Status == "Blocked" || item.Closed {
		return nil
	}
	if pr.State != "open" {
		return p.github.setDesignStatus(ctx, item, "Blocked")
	}
	if pr.Draft || pr.Mergeable == nil {
		if item.Status == "Merge Ready" {
			return p.github.setDesignStatus(ctx, item, "PR Review")
		}
		return nil
	}
	approved, err := p.approvedImplementation(ctx, item)
	if err != nil {
		return err
	}
	if approved.Head != design {
		return p.github.setDesignStatus(ctx, item, "Blocked")
	}
	resumed, err := p.resumeFix(ctx, item, pr)
	if resumed || err != nil {
		return err
	}
	if !*pr.Mergeable {
		return p.fixReview(ctx, item, pr, approved, "conflict", 0, "")
	}
	request, err := p.github.humanRequest(ctx, pr)
	if err != nil {
		return err
	}
	if request.ID != 0 {
		if request.Body == "" {
			if item.Status == "Merge Ready" {
				return p.github.setDesignStatus(ctx, item, "PR Review")
			}
			return fmt.Errorf("human review %d has no repair instructions", request.ID)
		}
		return p.fixReview(ctx, item, pr, approved, "human", request.ID, request.Body)
	}
	verdict, err := p.reviewAndPublish(ctx, &item, pr, func() (string, error) { return approved.Markdown, nil })
	if err != nil {
		return err
	}
	if !verdict.Approved {
		return p.fixReview(ctx, item, pr, approved, "ai", 0, verdict.Feedback)
	}
	passed, err := p.github.windowsCheck(ctx, pr.Head.SHA)
	if err != nil {
		return err
	}
	return p.applyMergeObservation(ctx, item, pr, mergeObservation{Open: true, Mergeable: true, Reviewed: true, PassedCI: passed})
}

func (p *poller) reviewAndPublish(ctx context.Context, item *projectIssue, pr pullDetails, reference func() (string, error)) (reviewVerdict, error) {
	var verdict reviewVerdict
	err := p.db.db.QueryRowContext(ctx, `
		SELECT approved, feedback FROM pr_reviews
		WHERE pr_number = ? AND head_sha = ? AND base_sha = ?
			AND (approved = 1 OR policy_version = ?)`,
		pr.Number, pr.Head.SHA, pr.Base.SHA, reviewPolicyVersion).Scan(&verdict.Approved, &verdict.Feedback)
	if errors.Is(err, sql.ErrNoRows) {
		if item.Status != "PR Review" {
			if err := p.github.setDesignStatus(ctx, *item, "PR Review"); err != nil {
				return verdict, err
			}
			item.Status = "PR Review"
		}
		text, err := reference()
		if err != nil {
			return verdict, err
		}
		verdict, err = p.independentReview(ctx, *item, pr, text)
		if err != nil {
			return verdict, err
		}
	} else if err != nil {
		return verdict, err
	}
	if err := p.assertCurrentPR(ctx, pr); err != nil {
		return verdict, err
	}
	return verdict, p.github.publishReview(ctx, pr, verdict)
}

func (p *poller) independentReview(ctx context.Context, item projectIssue, pr pullDetails, design string) (reviewVerdict, error) {
	var verdict reviewVerdict
	if err := p.fetchReviewCommit(ctx, pr.Head.SHA); err != nil {
		return verdict, err
	}
	if err := p.fetchReviewCommit(ctx, pr.Base.SHA); err != nil {
		return verdict, err
	}
	diff, err := gitRawOutput(ctx, p.cfg.WorkDir, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", pr.Base.SHA+"..."+pr.Head.SHA)
	if err != nil {
		return verdict, err
	}
	if len(diff) == 0 || len(diff) > 256<<10 {
		return verdict, fmt.Errorf("PR diff is empty or exceeds the review limit")
	}
	dir, err := p.reviewDirectory(pr.Number, "review-"+pr.Head.SHA+"-"+pr.Base.SHA)
	if err != nil {
		return verdict, err
	}
	checks, err := p.github.reviewChecks(ctx, pr.Head.SHA, "")
	if err != nil {
		return verdict, err
	}
	requirements, err := p.github.reviewIssueRequirements(ctx, item)
	if err != nil {
		return verdict, err
	}
	input := map[string]any{"issue_requirements": requirements, "review_reference": design, "base_sha": pr.Base.SHA, "head_sha": pr.Head.SHA,
		"diff": string(diff), "ci_checks": reviewCIEvidence(checks)}
	activity := workflowActivity{Issue: item.Number, PR: pr.Number, Phase: "review"}
	if err := p.runReviewWorkflow(ctx, "pr-review.yaml", dir, p.cfg.ImplementationTimeout, input, activity); err != nil {
		return verdict, err
	}
	if _, err := os.Stat(filepath.Join(dir, "review-complete")); err != nil {
		return verdict, fmt.Errorf("independent review returned no verdict")
	}
	data, err := os.ReadFile(filepath.Join(dir, "review.json"))
	if err != nil {
		return verdict, err
	}
	if err := json.Unmarshal(data, &verdict); err != nil {
		return verdict, err
	}
	_, err = p.db.db.ExecContext(ctx, `
		INSERT INTO pr_reviews(pr_number, head_sha, base_sha, approved, feedback, policy_version)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(pr_number, head_sha, base_sha) DO UPDATE SET
			approved = excluded.approved, feedback = excluded.feedback, policy_version = excluded.policy_version`,
		pr.Number, pr.Head.SHA, pr.Base.SHA, verdict.Approved, boundedReviewText(verdict.Feedback), reviewPolicyVersion)
	return verdict, err
}

func (p *poller) applyMergeObservation(ctx context.Context, item projectIssue, pr pullDetails, observation mergeObservation) error {
	if item.Status == observation.status() {
		return nil
	}
	dir, err := p.reviewDirectory(pr.Number, "merge-"+pr.Head.SHA)
	if err != nil {
		return err
	}
	activity := workflowActivity{Issue: item.Number, PR: pr.Number, Phase: "merge check"}
	if err := p.runReviewWorkflow(ctx, "merge-check.yaml", dir, p.cfg.WorkflowTimeout, observation, activity); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, "merge-result.txt"))
	if err != nil {
		return err
	}
	status := string(data)
	if status != observation.status() || status == "" {
		return fmt.Errorf("PR #%d does not satisfy the human merge check", pr.Number)
	}
	if status == "Merge Ready" {
		fresh, err := p.github.pullDetails(ctx, pr.Number, pr.Head.Ref)
		if err != nil {
			return err
		}
		if !sameOpenRevision(pr, fresh) || fresh.Draft || fresh.Mergeable == nil || !*fresh.Mergeable {
			return nil
		}
		requested, err := p.github.hasChangeRequest(ctx, fresh)
		if err != nil || requested {
			return err
		}
	}
	return p.github.setDesignStatus(ctx, item, status)
}

func sameOpenRevision(expected, current pullDetails) bool {
	return current.State == "open" && !current.Merged && current.Head.SHA == expected.Head.SHA && current.Base.SHA == expected.Base.SHA
}

func (p *poller) assertCurrentPR(ctx context.Context, pr pullDetails) error {
	fresh, err := p.github.pullDetails(ctx, pr.Number, pr.Head.Ref)
	if err != nil {
		return err
	}
	if !sameOpenRevision(pr, fresh) {
		return fmt.Errorf("PR #%d changed during the operation", pr.Number)
	}
	return nil
}

func (p *poller) fetchReviewCommit(ctx context.Context, sha string) error {
	if !validCommitSHA(sha) {
		return fmt.Errorf("expected a complete Git commit SHA")
	}
	if _, err := gitOutput(ctx, p.cfg.WorkDir, "cat-file", "-e", sha+"^{commit}"); err == nil {
		return nil
	}
	if _, err := p.github.automationIdentity(ctx); err != nil {
		return err
	}
	return p.github.remoteGit(ctx, p.cfg.WorkDir, "fetch", "--no-tags", p.github.repositoryURL(), sha)
}

func (p *poller) reviewDirectory(number int, step string) (string, error) {
	dir := filepath.Join(p.cfg.DataDir, "review", fmt.Sprint(number), step)
	return dir, os.MkdirAll(dir, 0700)
}

func (p *poller) runReviewWorkflow(ctx context.Context, workflow, dir string, limit time.Duration, input any, activity workflowActivity) error {
	// Remove receipts from an interrupted earlier invocation before starting.
	for _, name := range []string{"review-complete", "merge-result.txt", "approved-tree", "validated-tree"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	manifest := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, "workflow.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	finishActivity := p.trackWorkflow(ctx, activity, log.Name())
	err = runInJob(ctx, p.cfg.WorkDir, log, p.conductor.executable, "--silent", "run",
		filepath.Join(p.cfg.WorkDir, ".conductor", "workflows", workflow), "--no-interactive",
		"--input", "adapter="+self, "--input", "manifest="+manifest)
	finishActivity(classifyRun(ctx, err))
	return err
}
