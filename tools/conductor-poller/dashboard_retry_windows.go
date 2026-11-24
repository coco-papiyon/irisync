package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
)

type implementationRetry struct {
	Revision string
	Attempts int
}

func retryableProjectIssue(cfg config, issue projectIssue) bool {
	return issue.Status == "Blocked" && !issue.Closed && !issue.Archived &&
		strings.EqualFold(issue.Repository, cfg.Repository) && cfg.implementationEnabled(issue.Number)
}

func (d dashboardSource) implementationRetries(ctx context.Context) (map[string]implementationRetry, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT i.issue_id, i.design_sha, i.attempts
		FROM implementations i JOIN designs d ON d.issue_id = i.issue_id AND d.approved_sha = i.design_sha AND d.revision = i.design_sha
		WHERE i.attempts >= ?1 AND (i.attempt_limit <= i.attempts OR i.attempt_limit = i.attempts + ?1)
			AND i.pr_number = 0 AND i.commit_sha = ''
			AND NOT EXISTS (SELECT 1 FROM manual_prs m WHERE m.issue_id = i.issue_id)
			AND NOT EXISTS (SELECT 1 FROM implementations a WHERE a.issue_id = i.issue_id AND a.pr_number != 0)`, implementationAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	retries := make(map[string]implementationRetry)
	for rows.Next() {
		var id string
		var retry implementationRetry
		if err := rows.Scan(&id, &retry.Revision, &retry.Attempts); err != nil {
			return nil, err
		}
		retries[id] = retry
	}
	return retries, rows.Err()
}

func grantImplementationRetry(ctx context.Context, db *sql.DB, issueID string, retry implementationRetry) (bool, error) {
	result, err := db.ExecContext(ctx, `
		UPDATE implementations SET attempt_limit = attempts + ?1
		WHERE issue_id = ?2 AND design_sha = ?3 AND attempts = ?4 AND attempts >= ?1
			AND (attempt_limit <= attempts OR attempt_limit = attempts + ?1)
			AND pr_number = 0 AND commit_sha = ''
			AND EXISTS (SELECT 1 FROM designs d WHERE d.issue_id = ?2 AND d.approved_sha = ?3 AND d.revision = ?3)
			AND NOT EXISTS (SELECT 1 FROM manual_prs m WHERE m.issue_id = ?2)
			AND NOT EXISTS (SELECT 1 FROM implementations a WHERE a.issue_id = ?2 AND a.pr_number != 0)`,
		implementationAttempts, issueID, retry.Revision, retry.Attempts)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (d dashboardSource) retryImplementation(w http.ResponseWriter, r *http.Request, mu *sync.Mutex) {
	var request struct {
		Number int
		implementationRetry
	}
	if !decodeDashboardRequest(w, r, &request, 1024) {
		return
	}
	if d.viewingOnly {
		dashboardError(w, http.StatusForbidden, "実装の再開には通常起動の Poller が必要です。")
		return
	}
	if request.Number < 1 || request.Attempts < implementationAttempts || !validCommitSHA(request.Revision) {
		dashboardError(w, http.StatusBadRequest, "再開する実装の情報を確認してください。")
		return
	}
	mu.Lock()
	defer mu.Unlock()
	ctx := r.Context()
	items, err := d.github.projectIssues(ctx)
	if err != nil {
		dashboardError(w, http.StatusBadGateway, "Project の現在の状態を確認できませんでした。")
		return
	}
	index := slices.IndexFunc(items, func(item projectIssue) bool {
		return item.Number == request.Number && retryableProjectIssue(d.github.cfg, item)
	})
	if index < 0 {
		dashboardError(w, http.StatusConflict, "Issue の状態が変わりました。一覧を更新してください。")
		return
	}
	item := items[index]
	retries, err := d.implementationRetries(ctx)
	if err != nil {
		dashboardError(w, http.StatusInternalServerError, "再開できる実装の記録を読み取れませんでした。")
		return
	}
	if retries[item.ID] != request.implementationRetry {
		dashboardError(w, http.StatusConflict, "再開対象の設計または試行回数が変わりました。一覧を更新してください。")
		return
	}
	branch := fmt.Sprintf("conductor/design/issue-%d", item.Number)
	path := fmt.Sprintf("docs/design/issues/issue-%d.md", item.Number)
	ref, err := d.github.readDesignRef(ctx, branch, path)
	if err != nil {
		dashboardError(w, http.StatusBadGateway, "現在の設計版を確認できませんでした。")
		return
	}
	if ref.Head != request.Revision || !validDesign(ref.Markdown) || ref.DefaultBranch != "main" {
		dashboardError(w, http.StatusConflict, "設計版が変わりました。最新の承認状況を確認してください。")
		return
	}
	comments, err := d.github.comments(ctx, item.Number)
	if err != nil {
		dashboardError(w, http.StatusBadGateway, "設計への承認コメントを確認できませんでした。")
		return
	}
	approvalRequest := dashboardDesignRequest(d.github.cfg, comments, path, ref.Head)
	body := strings.TrimSuffix(approvalRequest.Body, " "+ref.Head)
	if approvalRequest.ID == 0 || findApproval(comments, d.github.cfg, body, ref.Head).ID == 0 {
		dashboardError(w, http.StatusConflict, "現在の設計版への承認を確認できませんでした。")
		return
	}
	pr, err := d.github.linkedReviewPR(ctx, item)
	if err != nil || pr.Number != 0 {
		dashboardError(w, http.StatusConflict, "紐づく PR を確認してください。オープンな実装 PR がある場合は PR Review から引き継げます。")
		return
	}
	granted, err := grantImplementationRetry(ctx, d.db, item.ID, request.implementationRetry)
	if err != nil || !granted {
		dashboardError(w, http.StatusConflict, "実装の試行枠を追加できませんでした。現在の実行状況を確認してください。")
		return
	}
	if err := d.github.setDesignStatus(ctx, item, "Implementing"); err != nil {
		dashboardError(w, http.StatusBadGateway, "Project のステータスを変更できませんでした。Blocked の場合は再度実装を再開できます。")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
