package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

type pullDetails struct {
	implementationPR
	Merged    bool
	Mergeable *bool
	Draft     bool
	MergedBy  struct {
		ID          int64
		Login, Type string
	} `json:"merged_by"`
}

type humanReview struct {
	ID       int64
	State    string
	Body     string
	CommitID string `json:"commit_id"`
	User     struct {
		ID          int64
		Login, Type string
	}
}

func (g githubProject) pullDetails(ctx context.Context, number int, branch string) (pullDetails, error) {
	var pr pullDetails
	err := g.getJSON(ctx, &pr, fmt.Sprintf("repos/%s/pulls/%d", g.cfg.Repository, number))
	if err != nil {
		return pr, err
	}
	if pr.Number != number || pr.Head.Ref != branch || pr.Base.Ref != "main" ||
		!strings.EqualFold(pr.Head.Repo.FullName, g.cfg.Repository) || !strings.EqualFold(pr.Base.Repo.FullName, g.cfg.Repository) ||
		!validCommitSHA(pr.Head.SHA) || !validCommitSHA(pr.Base.SHA) {
		return pr, fmt.Errorf("PR #%d does not match the recorded Irisync branch and base", number)
	}
	return pr, nil
}

func authorizedHuman(cfg config, id int64, login, userType string) bool {
	return userType == "User" && login != "" && !strings.EqualFold(login, cfg.GitHubLogin) && slices.Contains(cfg.DesignApproverIDs, id)
}

func requestedHumanReview(reviews []humanReview, cfg config, head string) humanReview {
	latest := make(map[int64]humanReview)
	for _, review := range reviews {
		if review.CommitID != head || !authorizedHuman(cfg, review.User.ID, review.User.Login, review.User.Type) {
			continue
		}
		switch review.State {
		case "CHANGES_REQUESTED", "APPROVED", "DISMISSED":
			if review.ID > latest[review.User.ID].ID {
				latest[review.User.ID] = review
			}
		}
	}
	var request humanReview
	for _, review := range latest {
		if review.State == "CHANGES_REQUESTED" && review.ID > request.ID {
			request = review
		}
	}
	return request
}

func (g githubProject) humanRequest(ctx context.Context, pr pullDetails) (humanReview, error) {
	var pages [][]humanReview
	pull := fmt.Sprintf("repos/%s/pulls/%d", g.cfg.Repository, pr.Number)
	if err := g.getJSON(ctx, &pages, pull+"/reviews?per_page=100", "--paginate", "--slurp"); err != nil {
		return humanReview{}, err
	}
	request := requestedHumanReview(slices.Concat(pages...), g.cfg, pr.Head.SHA)
	if request.ID == 0 {
		return request, nil
	}
	var inlinePages [][]struct {
		Body, Path string
		User       struct{ ID int64 }
	}
	if err := g.getJSON(ctx, &inlinePages, fmt.Sprintf("%s/reviews/%d/comments?per_page=100", pull, request.ID), "--paginate", "--slurp"); err != nil {
		return humanReview{}, err
	}
	var body strings.Builder
	body.WriteString(boundedReviewText(request.Body))
	for _, comment := range slices.Concat(inlinePages...) {
		if comment.User.ID == request.User.ID && body.Len() < 12<<10 {
			fmt.Fprintf(&body, "\n\n%s\n%s", comment.Path, boundedReviewText(comment.Body))
		}
	}
	request.Body = strings.TrimSpace(body.String())
	return request, nil
}

type prCheck struct {
	Name, Status, Conclusion string
	App                      struct {
		ID   int64
		Slug string
	}
}

func successfulWindowsCheck(checks []prCheck) bool {
	for _, check := range checks {
		if check.Name == "Conductor smoke (Windows)" && check.App.ID == 15368 && check.App.Slug == "github-actions" {
			return check.Status == "completed" && check.Conclusion == "success"
		}
	}
	return false
}

func (g githubProject) windowsCheck(ctx context.Context, head string) (bool, error) {
	var pages []struct {
		CheckRuns []prCheck `json:"check_runs"`
	}
	path := fmt.Sprintf("repos/%s/commits/%s/check-runs?filter=latest&check_name=Conductor+smoke+%%28Windows%%29&per_page=100", g.cfg.Repository, head)
	if err := g.getJSON(ctx, &pages, path, "--paginate", "--slurp"); err != nil {
		return false, err
	}
	var checks []prCheck
	for _, page := range pages {
		checks = append(checks, page.CheckRuns...)
	}
	return successfulWindowsCheck(checks), nil
}

func (g githubProject) publishReview(ctx context.Context, pr pullDetails, verdict reviewVerdict) error {
	marker := "<!-- irisync-review:" + pr.Head.SHA + ":" + pr.Base.SHA + " -->"
	comments, err := g.comments(ctx, pr.Number)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if strings.EqualFold(comment.User.Login, g.cfg.GitHubLogin) && strings.HasPrefix(comment.Body, marker+"\n") {
			return nil
		}
	}
	body := "独立したCodexレビューで修正が必要な指摘はありませんでした。"
	if !verdict.Approved {
		text := strings.ReplaceAll(strings.TrimSpace(boundedReviewText(verdict.Feedback)), "@", "＠")
		body = "独立したCodexレビューで修正が必要な指摘がありました。\n\n    " + strings.ReplaceAll(text, "\n", "\n    ")
	}
	return g.mutate(ctx, "POST", fmt.Sprintf("repos/%s/issues/%d/comments", g.cfg.Repository, pr.Number), map[string]string{
		"body": marker + "\n" + body + "\n\n対象コミット " + pr.Head.SHA,
	})
}

func boundedReviewText(input string) string {
	const limit = 6 << 10
	if len(input) > limit {
		input = input[:limit]
	}
	return strings.ToValidUTF8(input, "")
}
