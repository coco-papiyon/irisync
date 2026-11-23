package main

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

type githubIdentity struct {
	Login string
	ID    int64
}

func (g githubProject) automationIdentity(ctx context.Context) (githubIdentity, error) {
	var actor githubIdentity
	if err := g.getJSON(ctx, &actor, "user"); err != nil {
		return actor, err
	}
	if actor.ID <= 0 || !strings.EqualFold(actor.Login, g.cfg.GitHubLogin) {
		return actor, fmt.Errorf("unexpected GitHub account before implementation operation")
	}
	return actor, nil
}

func (g githubProject) repositoryURL() string {
	return "https://github.com/" + g.cfg.Repository + ".git"
}

type implementationPR struct {
	Number     int
	State      string
	MergedAt   string `json:"merged_at"`
	Head, Base struct {
		Ref, SHA string
		Repo     struct {
			FullName string `json:"full_name"`
		}
	}
}

func existingImplementationPR(prs []implementationPR, repository, branch, commit string) (int, error) {
	for _, pr := range prs {
		if pr.Head.Ref != branch || pr.Base.Ref != "main" ||
			!strings.EqualFold(pr.Head.Repo.FullName, repository) || !strings.EqualFold(pr.Base.Repo.FullName, repository) {
			continue
		}
		if pr.Head.SHA != commit {
			return 0, fmt.Errorf("existing PR #%d does not point at the validated implementation commit", pr.Number)
		}
		if pr.State != "open" && pr.MergedAt == "" {
			return 0, fmt.Errorf("existing PR #%d was closed without merging", pr.Number)
		}
		return pr.Number, nil
	}
	return 0, nil
}

func (g githubProject) findImplementationPR(ctx context.Context, branch, commit string) (int, error) {
	owner, _, _ := strings.Cut(g.cfg.Repository, "/")
	query := url.Values{"state": {"all"}, "head": {owner + ":" + branch}, "base": {"main"}, "per_page": {"100"}}
	var pages [][]implementationPR
	if err := g.getJSON(ctx, &pages, "repos/"+g.cfg.Repository+"/pulls?"+query.Encode(), "--paginate", "--slurp"); err != nil {
		return 0, err
	}
	return existingImplementationPR(slices.Concat(pages...), g.cfg.Repository, branch, commit)
}

func (p *poller) publishImplementation(ctx context.Context, item projectIssue, design, branch, commit string) error {
	number, err := p.github.findImplementationPR(ctx, branch, commit)
	if err != nil {
		return err
	}
	if number == 0 {
		if err := p.pushImplementation(ctx, branch, commit); err != nil {
			return err
		}
		approved, err := p.approvedImplementation(ctx, item)
		if err != nil {
			return err
		}
		if approved.Head != design {
			return p.github.setDesignStatus(ctx, item, "Design Review")
		}
		if err := p.github.setDesignStatus(ctx, item, "Implementing"); err != nil {
			return err
		}
		issue := fmt.Sprintf("#%d", item.Number)
		body := fmt.Sprintf("対象Issue %s\n\n承認済みの設計コミット %s\n\n機械検証とCodexによる自己チェックを通過しました。\n", issue, design)
		if err := p.github.mutate(ctx, "POST", "repos/"+p.cfg.Repository+"/pulls", map[string]string{
			"title": "Implement Irisync issue " + issue, "body": body, "head": branch, "base": "main",
		}); err != nil {
			return err
		}
		number, err = p.github.findImplementationPR(ctx, branch, commit)
		if err != nil {
			return fmt.Errorf("could not confirm the implementation PR: %w", err)
		}
		if number == 0 {
			return fmt.Errorf("the implementation PR is not visible yet")
		}
	}
	if _, err := p.db.db.ExecContext(ctx, "UPDATE implementations SET pr_number = ? WHERE issue_id = ? AND design_sha = ?",
		number, item.ID, design); err != nil {
		return err
	}
	return p.github.setDesignStatus(ctx, item, "PR Review")
}

func (p *poller) pushImplementation(ctx context.Context, branch, commit string) error {
	if !validCommitSHA(commit) || !strings.HasPrefix(branch, "conductor/implement/issue-") {
		return fmt.Errorf("invalid implementation branch or commit")
	}
	ref, err := p.github.readDesignRef(ctx, branch, "README.md")
	if err != nil {
		return err
	}
	if ref.Head == commit {
		return nil
	}
	if ref.Head != "" {
		return fmt.Errorf("implementation branch already has a different commit")
	}
	if _, err := p.github.automationIdentity(ctx); err != nil {
		return err
	}
	_, err = gitOutput(ctx, p.cfg.WorkDir, "push", p.github.repositoryURL(), commit+":refs/heads/"+branch)
	return err
}
