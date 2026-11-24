package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDesignSkillReceivesIssueAndReferenceDocuments(t *testing.T) {
	secret := "ghs_" + strings.Repeat("x", 24)
	item := projectIssue{Number: 30, Repository: "irisync/repo"}
	issue := designIssue{Title: "設定を検証する", Body: "受け入れ基準: 空のペア ID を拒否する。 " + secret}
	data, err := designSkillInput(item, issue, map[string]string{"docs/design/basic-design.md": "重複を拒否する"})
	if err != nil {
		t.Fatal(err)
	}
	prompt, input, ok := strings.Cut(string(data), "\n\n入力データ（JSON）\n")
	if !ok || !strings.HasPrefix(prompt, irisyncDesignSkill) {
		t.Fatal("design skill was not supplied to the adapter")
	}
	var payload struct {
		Issue struct {
			Number           int
			URL, Title, Body string
		}
		Documents map[string]string `json:"repository_documents"`
	}
	if err := json.Unmarshal([]byte(input), &payload); err != nil || payload.Issue.Number != 30 ||
		payload.Issue.URL != "https://github.com/irisync/repo/issues/30" || payload.Issue.Title != issue.Title ||
		!strings.Contains(payload.Issue.Body, "空のペア ID") || strings.Contains(payload.Issue.Body, secret) ||
		payload.Documents["docs/design/basic-design.md"] != "重複を拒否する" {
		t.Fatalf("unexpected design input: %+v (%v)", payload, err)
	}
}

func TestDesignApprovalRequiresCurrentRequestAndAuthorizedUser(t *testing.T) {
	now := time.Now().UTC()
	comment := func(id, user int64, login, body string) designComment {
		c := designComment{ID: id, Body: body, CreatedAt: now, UpdatedAt: now}
		c.User.ID, c.User.Login, c.User.Type = user, login, "User"
		return c
	}
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	cfg := config{GitHubLogin: "automation", DesignApproverIDs: []int64{1, 2}}
	p := poller{cfg: cfg}
	path := "docs/design/issues/issue-29.md"
	request := comment(10, 2, "automation", p.requestBody(path, sha, 1))
	earlier := comment(9, 1, "owner", "/approve-design")
	wrongUser := comment(11, 3, "outsider", "/approve-design")
	oldRevision := comment(12, 1, "owner", "/approve-design "+other)
	edited := comment(13, 1, "owner", "/approve-design")
	edited.UpdatedAt = now.Add(time.Second)
	selfApproval := comment(14, 2, "automation", "/approve-design")
	shortSHA := comment(15, 1, "owner", "/approve-design "+sha[:7])
	renamedAuthor := comment(16, 2, "renamed-automation", "/approve-design")
	if result := findApproval([]designComment{request, earlier, wrongUser, oldRevision, edited, selfApproval, shortSHA, renamedAuthor}, cfg, request.Body, sha); result.ID != 0 {
		t.Fatalf("accepted invalid approval: %d", result.ID)
	}
	valid := comment(17, 1, "owner", "/approve-design")
	if result := findApproval([]designComment{valid, request}, cfg, request.Body, sha); result.ID != valid.ID {
		t.Fatalf("did not accept valid approval: %d", result.ID)
	}
	newRequest := comment(18, 2, "automation", p.requestBody(path, sha, 2))
	if result := findApproval([]designComment{request, valid, newRequest}, cfg, newRequest.Body, sha); result.ID != 0 {
		t.Fatalf("reused approval from prior review: %d", result.ID)
	}
	newRevision := p.requestBody(path, other, 2)
	if result := findApproval([]designComment{request, valid}, cfg, newRevision, other); result.ID != 0 {
		t.Fatalf("accepted approval for another design: %d", result.ID)
	}

	legacyRequest := request
	legacyRequest.Body += " " + sha
	legacyApproval := comment(19, 1, "owner", "/approve-design "+sha)
	for _, approval := range []designComment{valid, legacyApproval} {
		if result := findApproval([]designComment{legacyRequest, approval}, cfg, request.Body, sha); result.ID != approval.ID {
			t.Fatalf("did not accept approval %d for existing request: got %d", approval.ID, result.ID)
		}
	}
	bot := valid
	bot.User.Type = "Bot"
	if result := findApproval([]designComment{request, bot}, cfg, request.Body, sha); result.ID != 0 {
		t.Fatalf("a bot was treated as a human approver: %d", result.ID)
	}
}

func TestDesignRevisionClearsSavedApproval(t *testing.T) {
	ctx := context.Background()
	db, closeDB, err := openState(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	p := poller{db: db}
	if _, err := db.db.Exec("INSERT INTO designs(issue_id) VALUES ('issue')"); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	if version, err := p.storeRevision(ctx, "issue", sha); err != nil || version != 1 {
		t.Fatalf("initial revision: version=%d err=%v", version, err)
	}
	if err := p.saveApproval(ctx, "issue", sha, designComment{ID: 7}); err != nil {
		t.Fatal(err)
	}
	if version, err := p.storeRevision(ctx, "issue", strings.Repeat("b", 40)); err != nil || version != 2 {
		t.Fatalf("changed revision: version=%d err=%v", version, err)
	}
	var approvedSHA string
	var approvalID int64
	if err := db.db.QueryRow("SELECT approved_sha, approval_comment_id FROM designs WHERE issue_id = 'issue'").Scan(&approvedSHA, &approvalID); err != nil || approvedSHA != "" || approvalID != 0 {
		t.Fatalf("stale approval remains: sha=%q id=%d err=%v", approvedSHA, approvalID, err)
	}
}
