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
	cfg := config{GitHubLogin: "automation", DesignApproverIDs: []int64{1}}
	request := comment(10, 2, "automation", "current request")
	earlier := comment(9, 1, "owner", "/approve-design "+sha)
	wrongUser := comment(11, 3, "outsider", "/approve-design "+sha)
	oldRevision := comment(12, 1, "owner", "/approve-design "+other)
	edited := comment(13, 1, "owner", "/approve-design "+sha)
	edited.UpdatedAt = now.Add(time.Second)
	if result := findApproval([]designComment{request, earlier, wrongUser, oldRevision, edited}, cfg, request.Body, sha); result.ID != 0 {
		t.Fatalf("accepted invalid approval: %d", result.ID)
	}
	valid := comment(14, 1, "owner", "/approve-design "+sha)
	if result := findApproval([]designComment{valid, request}, cfg, request.Body, sha); result.ID != valid.ID {
		t.Fatalf("did not accept valid approval: %d", result.ID)
	}
	newRequest := comment(15, 2, "automation", "new request for same SHA after branch reset")
	if result := findApproval([]designComment{request, valid, newRequest}, cfg, newRequest.Body, sha); result.ID != 0 {
		t.Fatalf("reused approval from prior review: %d", result.ID)
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
