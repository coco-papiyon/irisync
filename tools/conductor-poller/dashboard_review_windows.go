package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type dashboardConversationEntry struct {
	Author, Body, State, URL string
	At                       time.Time
}

type dashboardApproval struct {
	Revision, URL, Author, Reason string
	requestAuthor                 githubIdentity
}

type dashboardDiscussion struct {
	Entries  []dashboardConversationEntry
	Approval dashboardApproval
}

type dashboardApprovalRequest struct {
	Kind     string `json:"kind"`
	Number   int    `json:"number"`
	Revision string `json:"revision"`
	Author   string `json:"author"`
}

func validDashboardItem(kind string, number int) bool {
	return number > 0 && (kind == "issue" || kind == "pr")
}

func dashboardError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	dashboardJSON(w, struct{ Message string }{message})
}

func (d dashboardSource) item(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	number, err := strconv.Atoi(r.URL.Query().Get("number"))
	if err != nil || !validDashboardItem(kind, number) {
		dashboardError(w, http.StatusBadRequest, "対象の Issue または PR を指定してください。")
		return
	}
	discussion, _, err := d.discussion(r.Context(), kind, number)
	if err != nil {
		dashboardError(w, http.StatusBadGateway, "GitHub のコメント・レビューを取得できませんでした。")
		return
	}
	dashboardJSON(w, discussion)
}

func (d dashboardSource) discussion(ctx context.Context, kind string, number int) (dashboardDiscussion, githubProject, error) {
	var detail dashboardDiscussion
	comments, err := d.github.comments(ctx, number)
	if err != nil {
		return detail, githubProject{}, err
	}
	githubURL := "https://github.com/" + d.github.cfg.Repository
	itemURL := fmt.Sprintf("%s/issues/%d", githubURL, number)
	if kind == "pr" {
		itemURL = fmt.Sprintf("%s/pull/%d", githubURL, number)
	}
	for _, comment := range comments {
		detail.Entries = append(detail.Entries, dashboardConversationEntry{
			Author: comment.User.Login, Body: dashboardSecrets.ReplaceAllString(comment.Body, "[REDACTED]"),
			URL: fmt.Sprintf("%s#issuecomment-%d", itemURL, comment.ID), At: comment.CreatedAt,
		})
	}

	settingsAvailable := true
	if d.configPath != "" {
		cfg, err := loadConfig(d.configPath)
		settingsAvailable = err == nil
		if settingsAvailable {
			d.github.cfg.DesignApproverIDs = cfg.DesignApproverIDs
			d.github.cfg.DashboardApproverLogin = cfg.DashboardApproverLogin
		}
	}
	var human githubProject
	if kind == "issue" && settingsAvailable {
		detail.Approval = d.designApproval(ctx, number, comments)
		if detail.Approval.Revision != "" && detail.Approval.Reason == "" {
			var actor githubIdentity
			human, actor, detail.Approval.Reason = d.approver(ctx)
			detail.Approval.Author = actor.Login
			if detail.Approval.Reason == "" && actor.sameAccount(detail.Approval.requestAuthor) {
				detail.Approval.Reason = "自分が出した設計承認依頼は承認できません。"
			}
		}
	} else if kind == "pr" {
		pull := fmt.Sprintf("repos/%s/pulls/%d", d.github.cfg.Repository, number)
		reviews, err := d.github.pullReviews(ctx, pull)
		if err != nil {
			return detail, human, err
		}
		for _, review := range reviews {
			if review.State == "PENDING" {
				continue
			}
			detail.Entries = append(detail.Entries, dashboardConversationEntry{
				Author: review.User.Login, Body: dashboardSecrets.ReplaceAllString(review.Body, "[REDACTED]"),
				State: review.State, URL: review.URL, At: review.SubmittedAt,
			})
		}
		if settingsAvailable {
			detail.Approval, human = d.pullApproval(ctx, pull, number, reviews)
		}
	}
	if !settingsAvailable {
		detail.Approval.Reason = "承認者の設定を読み取れませんでした。"
	}
	slices.SortStableFunc(detail.Entries, func(a, b dashboardConversationEntry) int { return a.At.Compare(b.At) })
	return detail, human, nil
}

func defaultDashboardApprover(cfg config) string {
	if cfg.GitHubAuthMode == githubAuthApp {
		return cfg.GitHubProjectLogin
	}
	return cfg.GitHubLogin
}

func (d dashboardSource) approver(ctx context.Context) (githubProject, githubIdentity, string) {
	g := d.github
	login := strings.TrimSpace(g.cfg.DashboardApproverLogin)
	if login == "" {
		login = defaultDashboardApprover(g.cfg)
	}
	human := g.projectAPI()
	if g.app == nil || !strings.EqualFold(login, human.cfg.GitHubLogin) {
		tokenCtx, cancel := context.WithTimeout(ctx, g.cfg.GitHubTimeout)
		defer cancel()
		token, err := storedGitHubToken(tokenCtx, login)
		if err != nil {
			return githubProject{}, githubIdentity{}, "承認者のアカウントで gh auth login を行ってください。"
		}
		human = githubProject{cfg: g.cfg, storedToken: token}
		human.cfg.GitHubLogin = login
	}
	actor, err := human.automationIdentity(ctx)
	if err != nil || !registeredApprover(g.cfg, actor) {
		return githubProject{}, githubIdentity{}, "承認者の gh 認証と design_approver_ids を確認してください。"
	}
	return human, actor, ""
}

func (d dashboardSource) designApproval(ctx context.Context, number int, comments []designComment) dashboardApproval {
	g := d.github
	items, err := g.projectIssues(ctx)
	if err != nil {
		return dashboardApproval{Reason: "Project の設計レビュー状態を確認できませんでした。"}
	}
	reviewing := slices.ContainsFunc(items, func(item projectIssue) bool {
		return item.Number == number && !item.Closed && !item.Archived &&
			strings.EqualFold(item.Repository, g.cfg.Repository) && item.Status == "Design Review"
	})
	if !reviewing {
		return dashboardApproval{Reason: "設計承認は Design Review の Issue で行えます。"}
	}
	path := fmt.Sprintf("docs/design/issues/issue-%d.md", number)
	ref, err := g.readDesignRef(ctx, fmt.Sprintf("conductor/design/issue-%d", number), path)
	if err != nil || !validCommitSHA(ref.Head) || !validDesign(ref.Markdown) {
		return dashboardApproval{Reason: "現在の設計版を確認できませんでした。"}
	}
	request := dashboardDesignRequest(g.cfg, comments, path, ref.Head)
	if request.ID == 0 {
		return dashboardApproval{Reason: "現在の設計版への承認依頼を待っています。"}
	}
	approval := dashboardApproval{
		Revision: ref.Head, URL: fmt.Sprintf("https://github.com/%s/blob/%s/%s", g.cfg.Repository, ref.Head, path),
		requestAuthor: request.User,
	}
	body := strings.TrimSuffix(request.Body, " "+ref.Head)
	if comment := findApproval(comments, g.cfg, body, ref.Head); comment.ID != 0 {
		approval.Reason = "この設計版は承認済みです。"
	}
	return approval
}

func dashboardDesignRequest(cfg config, comments []designComment, path, revision string) designComment {
	p := poller{cfg: cfg}
	var request designComment
	for _, comment := range comments {
		var version int
		if comment.ID <= request.ID || !strings.EqualFold(comment.User.Login, cfg.GitHubLogin) {
			continue
		}
		if _, err := fmt.Sscanf(comment.Body, "第%d版の設計承認をお願いします。", &version); err != nil || version < 1 {
			continue
		}
		body := p.requestBody(path, revision, version)
		if matchesDesignRequest(comment, cfg, body, revision) {
			request = comment
		}
	}
	return request
}

func (d dashboardSource) pullApproval(ctx context.Context, pull string, number int, reviews []humanReview) (dashboardApproval, githubProject) {
	var pr pullDetails
	if err := d.github.getJSON(ctx, &pr, pull); err != nil || pr.Number != number || pr.User.ID <= 0 || !validCommitSHA(pr.Head.SHA) {
		return dashboardApproval{Reason: "現在の PR コミットを確認できませんでした。"}, githubProject{}
	}
	if pr.State != "open" || pr.Draft {
		return dashboardApproval{Reason: "PR を承認するにはオープンでレビュー可能な状態が必要です。"}, githubProject{}
	}
	human, actor, reason := d.approver(ctx)
	approval := dashboardApproval{
		Revision: pr.Head.SHA, URL: fmt.Sprintf("https://github.com/%s/pull/%d/commits/%s", d.github.cfg.Repository, number, pr.Head.SHA),
		Author: actor.Login, Reason: reason,
	}
	if reason == "" {
		switch {
		case actor.sameAccount(pr.User):
			approval.Reason = "自分が作成した PR は承認できません。"
		case dashboardPullApproved(reviews, actor.ID, pr.Head.SHA):
			approval.Reason = "このコミットはあなたが承認済みです。"
		}
	}
	return approval, human
}

func dashboardPullApproved(reviews []humanReview, actorID int64, sha string) bool {
	var latest humanReview
	for _, review := range reviews {
		if review.User.ID != actorID || review.ID <= latest.ID {
			continue
		}
		switch review.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			latest = review
		}
	}
	return latest.State == "APPROVED" && latest.CommitID == sha
}

func (d dashboardSource) approve(w http.ResponseWriter, r *http.Request, mu *sync.Mutex) {
	var request dashboardApprovalRequest
	if !decodeDashboardRequest(w, r, &request, 1024) {
		return
	}
	if !validDashboardItem(request.Kind, request.Number) || !validCommitSHA(request.Revision) || !dashboardGitHubLogin.MatchString(request.Author) {
		dashboardError(w, http.StatusBadRequest, "承認対象、コミット、承認者を確認してください。")
		return
	}

	mu.Lock()
	defer mu.Unlock()
	detail, human, err := d.discussion(r.Context(), request.Kind, request.Number)
	if err != nil {
		dashboardError(w, http.StatusBadGateway, "GitHub で承認対象の状態を確認できませんでした。")
		return
	}
	if detail.Approval.Revision != request.Revision {
		dashboardError(w, http.StatusConflict, "承認対象が変わりました。最新の内容を確認してください。")
		return
	}
	if detail.Approval.Reason != "" {
		dashboardError(w, http.StatusConflict, detail.Approval.Reason)
		return
	}
	if !strings.EqualFold(detail.Approval.Author, request.Author) {
		dashboardError(w, http.StatusConflict, "承認者の設定が変更されました。現在の承認者を確認してください。")
		return
	}
	repo := d.github.cfg.Repository
	endpoint := fmt.Sprintf("repos/%s/issues/%d/comments", repo, request.Number)
	payload := map[string]string{"body": designApprovalCommand + " " + request.Revision}
	if request.Kind == "pr" {
		endpoint = fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, request.Number)
		payload = map[string]string{"event": "APPROVE", "commit_id": request.Revision}
	}
	if err := human.mutate(r.Context(), http.MethodPost, endpoint, payload); err != nil {
		dashboardError(w, http.StatusBadGateway, "承認を投稿できませんでした。GitHub の権限と承認状況を確認してください。")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
