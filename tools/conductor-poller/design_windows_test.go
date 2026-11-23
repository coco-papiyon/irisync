package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

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
	if result := findApproval([]designComment{request, earlier, wrongUser, oldRevision, edited, selfApproval, shortSHA}, cfg, request.Body, sha); result.ID != 0 {
		t.Fatalf("accepted invalid approval: %d", result.ID)
	}
	valid := comment(16, 1, "owner", "/approve-design")
	if result := findApproval([]designComment{valid, request}, cfg, request.Body, sha); result.ID != valid.ID {
		t.Fatalf("did not accept valid approval: %d", result.ID)
	}
	newRequest := comment(17, 2, "automation", p.requestBody(path, sha, 2))
	if result := findApproval([]designComment{request, valid, newRequest}, cfg, newRequest.Body, sha); result.ID != 0 {
		t.Fatalf("reused approval from prior review: %d", result.ID)
	}
	newRevision := p.requestBody(path, other, 2)
	if result := findApproval([]designComment{request, valid}, cfg, newRevision, other); result.ID != 0 {
		t.Fatalf("accepted approval for another design: %d", result.ID)
	}

	legacyRequest := request
	legacyRequest.Body += " " + sha
	legacyApproval := comment(18, 1, "owner", "/approve-design "+sha)
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
