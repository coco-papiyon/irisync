package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const linkedReviewPRsQuery = `query($issue:ID!, $after:String) {
  node(id:$issue) {
    ... on Issue {
      closedByPullRequestsReferences(first:100, after:$after) {
        pageInfo { endCursor hasNextPage }
        nodes { number headRefName baseRefName isCrossRepository repository { nameWithOwner } }
      }
    }
  }
}`

type linkedReviewPR struct {
	Number                   int
	HeadRefName, BaseRefName string
	IsCrossRepository        bool
	Repository               struct{ NameWithOwner string }
}

func (g githubProject) linkedReviewPR(ctx context.Context, item projectIssue) (linkedReviewPR, error) {
	var candidate linkedReviewPR
	baseArgs := []string{"graphql", "-f", "query=" + linkedReviewPRsQuery, "-f", "issue=" + item.ID}
	args := baseArgs
	for {
		var page struct {
			Data struct {
				Node *struct {
					Links struct {
						Nodes    []linkedReviewPR
						PageInfo struct {
							EndCursor   string
							HasNextPage bool
						}
					} `json:"closedByPullRequestsReferences"`
				}
			}
		}
		if err := g.getJSON(ctx, &page, args...); err != nil {
			return linkedReviewPR{}, err
		}
		if page.Data.Node == nil {
			return linkedReviewPR{}, fmt.Errorf("could not read linked PRs for Issue #%d", item.Number)
		}
		links := page.Data.Node.Links
		for _, pr := range links.Nodes {
			if pr.Number <= 0 || pr.HeadRefName == "" || pr.BaseRefName != "main" ||
				pr.IsCrossRepository || !strings.EqualFold(pr.Repository.NameWithOwner, g.cfg.Repository) {
				continue
			}
			if candidate.Number != 0 && pr.Number != candidate.Number {
				return linkedReviewPR{}, fmt.Errorf("Issue #%d has multiple eligible PRs; keep one closing PR linked", item.Number)
			}
			candidate = pr
		}
		if !links.PageInfo.HasNextPage {
			return candidate, nil
		}
		if links.PageInfo.EndCursor == "" {
			return linkedReviewPR{}, fmt.Errorf("linked PR pagination did not return a cursor")
		}
		args = append(baseArgs, "-f", "after="+links.PageInfo.EndCursor)
	}
}

func (p *poller) advanceManualPR(ctx context.Context, item projectIssue) error {
	var number int
	var branch string
	err := p.db.db.QueryRowContext(ctx, "SELECT pr_number, head_branch FROM manual_prs WHERE issue_id = ?", item.ID).Scan(&number, &branch)
	if errors.Is(err, sql.ErrNoRows) {
		if item.Closed || item.Status != "PR Review" {
			return nil
		}
		candidate, err := p.github.linkedReviewPR(ctx, item)
		if err != nil || candidate.Number == 0 {
			return err
		}
		pr, err := p.github.pullDetails(ctx, candidate.Number, candidate.HeadRefName)
		if err != nil || pr.State != "open" {
			return err
		}
		if _, err := p.db.db.ExecContext(ctx, "INSERT INTO manual_prs(issue_id, pr_number, head_branch) VALUES (?, ?, ?)",
			item.ID, pr.Number, pr.Head.Ref); err != nil {
			return err
		}
		return p.reviewManualPR(ctx, item, pr)
	}
	if err != nil {
		return err
	}
	pr, err := p.github.pullDetails(ctx, number, branch)
	if err != nil {
		return err
	}
	return p.reviewManualPR(ctx, item, pr)
}

func (p *poller) reviewManualPR(ctx context.Context, item projectIssue, pr pullDetails) error {
	if pr.Merged {
		merger := githubIdentity{ID: pr.MergedBy.ID, Login: pr.MergedBy.Login, Type: pr.MergedBy.Type}
		return p.applyMergeObservation(ctx, item, pr, mergeObservation{
			Merged: true, HumanMerger: registeredApprover(p.cfg, merger) && !merger.sameAccount(pr.User),
		})
	}
	if item.Closed || item.Status == "Blocked" {
		return nil
	}
	if pr.State != "open" {
		return p.github.setDesignStatus(ctx, item, "Blocked")
	}
	observation := mergeObservation{Open: true}
	if pr.Draft || pr.Mergeable == nil || !*pr.Mergeable {
		return p.applyMergeObservation(ctx, item, pr, observation)
	}
	observation.Mergeable = true
	verdict, err := p.reviewAndPublish(ctx, &item, pr, func() (string, error) { return p.manualReviewReference(ctx, item) })
	if err != nil {
		return err
	}
	requested, err := p.github.hasChangeRequest(ctx, pr)
	if err != nil {
		return err
	}
	observation.Reviewed = verdict.Approved && !requested
	if observation.Reviewed {
		observation.PassedCI, err = p.manualPRChecks(ctx, pr.Head.SHA)
		if err != nil {
			return err
		}
	}
	return p.applyMergeObservation(ctx, item, pr, observation)
}

func (p *poller) manualReviewReference(ctx context.Context, item projectIssue) (string, error) {
	designPath := fmt.Sprintf("docs/design/issues/issue-%d.md", item.Number)
	ref, err := p.github.readDesignRef(ctx, fmt.Sprintf("conductor/design/issue-%d", item.Number), designPath)
	if err != nil {
		return "", err
	}
	if validCommitSHA(ref.Head) && validDesign(ref.Markdown) {
		return dashboardSecrets.ReplaceAllString(ref.Markdown, "[redacted]"), nil
	}
	return "", nil
}

func (p *poller) manualPRChecks(ctx context.Context, head string) (bool, error) {
	checks, err := p.github.reviewChecks(ctx, head, "")
	if err != nil {
		return false, err
	}
	if err := p.fetchReviewCommit(ctx, head); err != nil {
		return false, err
	}
	_, err = gitOutput(ctx, p.cfg.WorkDir, "cat-file", "-e", head+":.github/workflows/irisync.yml")
	return successfulManualChecks(checks, err == nil), nil
}

func successfulManualChecks(checks []prCheck, irisyncCI bool) bool {
	required := []string{"Conductor smoke (Windows)"}
	if irisyncCI {
		required = append(required, "Irisync Go (windows-2025)", "Irisync Go (ubuntu-latest)")
	}
	for _, check := range checks {
		if check.App.ID != 15368 || check.App.Slug != "github-actions" {
			continue
		}
		if check.Status != "completed" || (check.Conclusion != "success" && check.Conclusion != "neutral" && check.Conclusion != "skipped") {
			return false
		}
		if check.Conclusion == "success" {
			required = slices.DeleteFunc(required, func(name string) bool { return name == check.Name })
		}
	}
	return len(required) == 0
}
