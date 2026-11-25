package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type designRecord struct {
	attempts, deferred int
	generated          string
}

func (p *poller) launchDesign(ctx context.Context, item projectIssue) bool {
	p.designMu.Lock()
	if p.activeDesigns[item.ID] || time.Now().Before(p.retryAfter[item.ID]) {
		p.designMu.Unlock()
		return false
	}
	delete(p.retryAfter, item.ID)
	p.activeDesigns[item.ID] = true
	p.designMu.Unlock()
	p.workers.Add(1)
	go func() {
		completion := workflowCompletion{}
		workerCtx := context.WithValue(ctx, workflowCompletionKey{}, &completion)
		var workerError error
		defer p.workers.Done()
		defer func() {
			p.designMu.Lock()
			delete(p.activeDesigns, item.ID)
			// A review without a retry budget must not immediately re-run on failure.
			if completion.ran && workerError != nil && !errors.Is(workerError, errCodexLimited) && !completion.boundedAttempt {
				p.retryAfter[item.ID] = time.Now().Add(p.cfg.PollInterval)
			}
			p.designMu.Unlock()
			<-p.slots
			if completion.ran {
				p.requestPoll()
			}
		}()
		advance := p.advanceDesign
		if isReviewStatus(item.Status) {
			advance = p.advancePRReview
		} else if p.cfg.implementationEnabled(item.Number) &&
			(item.Status == "Design Review" || item.Status == "Implementing") {
			advance = p.advanceImplementation
		}
		workerError = advance(workerCtx, item)
		if workerError != nil && !errors.Is(workerError, errCodexLimited) {
			slog.Warn("issue workflow did not advance", "issue", item.Number, "error", workerError)
		}
	}()
	return true
}

func (p *poller) advanceDesign(ctx context.Context, item projectIssue) error {
	if item.Status == "Ready" {
		if _, err := p.db.db.ExecContext(ctx, "INSERT OR IGNORE INTO designs(issue_id) VALUES (?)", item.ID); err != nil {
			return err
		}
		if err := p.github.setDesignStatus(ctx, item, "Designing"); err != nil {
			return err
		}
		item.Status = "Designing"
	}
	var record designRecord
	err := p.db.db.QueryRowContext(ctx, `
		SELECT attempts, generated_markdown,
			(SELECT count(*) FROM codex_deferrals WHERE phase = 'design' AND work_key = designs.issue_id)
		FROM designs WHERE issue_id = ?`, item.ID).Scan(&record.attempts, &record.generated, &record.deferred)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	branch := fmt.Sprintf("conductor/design/issue-%d", item.Number)
	path := fmt.Sprintf("docs/design/issues/issue-%d.md", item.Number)
	if item.Status == "Design Review" {
		return p.reviewDesign(ctx, item, branch, path)
	}
	if record.generated == "" {
		if record.attempts > 0 {
			if err := p.recoverDesignDeferral(ctx, item, record.attempts); err != nil {
				return err
			}
		}
		if record.attempts-record.deferred >= p.cfg.MaxAttempts {
			return p.github.setDesignStatus(ctx, item, "Blocked")
		}
		if err := p.generateDesign(ctx, item, record.attempts+1); err != nil {
			return err
		}
		if err := p.db.db.QueryRowContext(ctx, "SELECT generated_markdown FROM designs WHERE issue_id = ?", item.ID).Scan(&record.generated); err != nil {
			return err
		}
	}
	if err := p.github.setDesignStatus(ctx, item, "Designing"); err != nil {
		return err
	}
	ref, err := p.github.publishDesign(ctx, branch, path, record.generated)
	if err != nil {
		return err
	}
	version, err := p.storeRevision(ctx, item.ID, ref.Head)
	if err != nil {
		return err
	}
	if _, err := p.reviewRequest(ctx, item, p.requestBody(path, ref.Head, version), ref.Head); err != nil {
		return err
	}
	return p.github.setDesignStatus(ctx, item, "Design Review")
}

func (p *poller) recoverDesignDeferral(ctx context.Context, item projectIssue, attempt int) error {
	entries, err := os.ReadDir(p.cfg.DataDir)
	if err != nil {
		return err
	}
	prefix := fmt.Sprintf("design-%d-%d-", item.Number, attempt)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		dir := filepath.Join(p.cfg.DataDir, entry.Name())
		if !codexAttemptLimited(dir) {
			continue
		}
		err := p.deferCodexAttempt(ctx, "design", item.ID, attempt)
		if err == nil || errors.Is(err, errCodexLimited) {
			_ = os.RemoveAll(dir)
		}
		return err
	}
	return nil
}

func (p *poller) generateDesign(ctx context.Context, item projectIssue, attempt int) (runErr error) {
	if err := p.requireCodex(ctx); err != nil {
		return err
	}
	var issue designIssue
	if err := p.github.getJSON(ctx, &issue, fmt.Sprintf("repos/%s/issues/%d", p.cfg.Repository, item.Number)); err != nil {
		return err
	}
	if issue.NodeID != item.ID || issue.State != "open" {
		return fmt.Errorf("design issue changed before generation")
	}
	contextData := make(map[string]string)
	for _, path := range []string{"AGENTS.md", "docs/design/basic-design.md", "docs/design/implementation-plan.md", "docs/design/conductor-feasibility-study.md"} {
		data, err := os.ReadFile(filepath.Join(p.cfg.WorkDir, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		contextData[path] = string(data)
	}
	payload, err := designSkillInput(item, issue, contextData)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(p.cfg.DataDir, fmt.Sprintf("design-%d-%d-", item.Number, attempt))
	if err != nil {
		return err
	}
	defer func() {
		if !codexAttemptLimited(dir) || errors.Is(runErr, errCodexLimited) {
			_ = os.RemoveAll(dir)
		}
	}()
	input := filepath.Join(dir, "input.txt")
	output := filepath.Join(dir, "design.json")
	if err := os.WriteFile(input, payload, 0600); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(p.cfg.DataDir, fmt.Sprintf("design-%d-%d.log", item.Number, attempt))
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	if _, err := p.db.db.ExecContext(ctx, "UPDATE designs SET attempts = ? WHERE issue_id = ?", attempt, item.ID); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, p.cfg.DesignTimeout)
	defer cancel()
	workflow := filepath.Join(p.cfg.WorkDir, ".conductor", "workflows", "design.yaml")
	finishActivity := p.trackWorkflow(runCtx, workflowActivity{Issue: item.Number, Attempt: attempt, Phase: "design"}, log.Name())
	defer func() { finishActivity(classifyRun(runCtx, runErr)) }()
	err = runInJob(runCtx, p.cfg.WorkDir, log, p.conductor.executable, "--silent", "run", workflow, "--no-interactive",
		"--input", "adapter="+self, "--input", "input_path="+input, "--input", "output_path="+output)
	if codexAttemptLimited(dir) {
		return p.deferCodexAttempt(ctx, "design", item.ID, attempt)
	}
	if err != nil {
		return err
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return fmt.Errorf("design adapter did not return output: %w", err)
	}
	var result designOutput
	if err := json.Unmarshal(data, &result); err != nil || !validDesign(result.Markdown) || !utf8.ValidString(result.Markdown) {
		return fmt.Errorf("invalid design output")
	}
	usage, err := json.Marshal(result.Usage)
	if err != nil {
		return err
	}
	markdown := strings.TrimSpace(strings.ReplaceAll(result.Markdown, "\r\n", "\n")) + "\n"
	_, err = p.db.db.ExecContext(ctx, "UPDATE designs SET generated_markdown = ?, usage_json = ? WHERE issue_id = ?", markdown, string(usage), item.ID)
	return err
}

func designSkillInput(item projectIssue, issue designIssue, documents map[string]string) ([]byte, error) {
	payload, err := json.Marshal(map[string]any{
		"issue": map[string]any{
			"number": item.Number,
			"url":    fmt.Sprintf("https://github.com/%s/issues/%d", item.Repository, item.Number),
			"title":  dashboardSecrets.ReplaceAllString(issue.Title, "[redacted]"),
			"body":   dashboardSecrets.ReplaceAllString(issue.Body, "[redacted]"),
		},
		"repository_documents": documents,
	})
	if err != nil {
		return nil, err
	}
	return skillInput(irisyncDesignSkill, "取得済みの Issue 情報を要件に分解し、Irisync の設計書を作成する。", payload), nil
}

func (p *poller) reviewDesign(ctx context.Context, item projectIssue, branch, path string) error {
	ref, err := p.github.readDesignRef(ctx, branch, path)
	if err != nil {
		return err
	}
	if !validCommitSHA(ref.Head) || !validDesign(ref.Markdown) {
		_, err := p.storeRevision(ctx, item.ID, "")
		return err
	}
	version, err := p.storeRevision(ctx, item.ID, ref.Head)
	if err != nil {
		return err
	}
	request := p.requestBody(path, ref.Head, version)
	comments, err := p.reviewRequest(ctx, item, request, ref.Head)
	if err != nil {
		return err
	}
	approval := findApproval(comments, p.cfg, request, ref.Head)
	return p.saveApproval(ctx, item.ID, ref.Head, approval)
}

func (p *poller) storeRevision(ctx context.Context, issueID, sha string) (int, error) {
	_, err := p.db.db.ExecContext(ctx, "UPDATE designs SET revision = ?, revision_number = revision_number + 1, approved_sha = '', approval_comment_id = 0, approver_id = 0 WHERE issue_id = ? AND revision <> ?", sha, issueID, sha)
	if err != nil {
		return 0, err
	}
	var version int
	err = p.db.db.QueryRowContext(ctx, "SELECT revision_number FROM designs WHERE issue_id = ?", issueID).Scan(&version)
	return version, err
}

func (p *poller) saveApproval(ctx context.Context, issueID, sha string, comment designComment) error {
	approved := ""
	if comment.ID != 0 {
		approved = sha
	}
	_, err := p.db.db.ExecContext(ctx, "UPDATE designs SET approved_sha = ?, approval_comment_id = ?, approver_id = ? WHERE issue_id = ? AND revision = ?",
		approved, comment.ID, comment.User.ID, issueID, sha)
	return err
}

const designApprovalCommand = "/approve-design"

func (p *poller) requestBody(path, sha string, version int) string {
	return fmt.Sprintf("第%d版の設計承認をお願いします。\n\n[設計書](https://github.com/%s/blob/%s/%s)\n\n承認する場合、Issueに次の1行をコメントしてください。\n\n%s", version, p.cfg.Repository, sha, path, designApprovalCommand)
}

func (p *poller) reviewRequest(ctx context.Context, item projectIssue, body, sha string) ([]designComment, error) {
	comments, err := p.github.comments(ctx, item.Number)
	if err != nil {
		return nil, err
	}
	for _, comment := range comments {
		if matchesDesignRequest(comment, p.cfg, body, sha) {
			return comments, nil
		}
	}
	err = p.github.mutate(ctx, "POST", fmt.Sprintf("repos/%s/issues/%d/comments", p.cfg.Repository, item.Number), map[string]string{"body": body})
	return comments, err
}

func matchesDesignRequest(comment designComment, cfg config, request, sha string) bool {
	// Preserve requests posted before the SHA was removed from the command.
	return strings.EqualFold(comment.User.Login, cfg.GitHubLogin) &&
		(comment.Body == request || comment.Body == request+" "+sha)
}

func findApproval(comments []designComment, cfg config, request, sha string) designComment {
	if !validCommitSHA(sha) {
		return designComment{}
	}
	var requestComment designComment
	for _, c := range comments {
		if c.ID > requestComment.ID && matchesDesignRequest(c, cfg, request, sha) {
			requestComment = c
		}
	}
	var result designComment
	if requestComment.ID == 0 {
		return result
	}
	for _, c := range comments {
		body := strings.TrimSpace(c.Body)
		if c.ID > requestComment.ID && c.ID > result.ID && c.CreatedAt.Equal(c.UpdatedAt) &&
			registeredApprover(cfg, c.User) && !c.User.sameAccount(requestComment.User) &&
			(body == designApprovalCommand || body == designApprovalCommand+" "+sha) {
			result = c
		}
	}
	return result
}

func validCommitSHA(sha string) bool {
	return len(sha) == 40 && strings.IndexFunc(sha, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) == -1
}
