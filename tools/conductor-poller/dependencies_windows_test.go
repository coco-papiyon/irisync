package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPendingScopeKeepsOtherProjectStatusesForPrerequisites(t *testing.T) {
	pending, statuses := pendingAndStatuses([]projectIssue{
		{ID: "candidate", Repository: "owner/irisync", Status: "Pending"},
		{ID: "backlog", Repository: "owner/irisync", Status: "Backlog"},
		{ID: "archived", Repository: "owner/irisync", Status: "Pending", Archived: true},
		{ID: "closed", Repository: "owner/irisync", Status: "Pending", Closed: true},
		{ID: "other", Repository: "owner/other", Status: "Pending"},
		{ID: "dependency", Repository: "owner/other", Status: "Done"},
	}, "owner/irisync")
	if len(pending) != 1 || pending[0].ID != "candidate" || statuses["dependency"] != "Done" {
		t.Fatalf("unexpected pending scope: %v and %v", pending, statuses)
	}
}

func TestDependenciesMustBeNonEmptyCompleteAndAllDone(t *testing.T) {
	tests := []struct {
		name, response string
		statuses       map[string]string
		want           bool
	}{
		{"all Done", `{"id":"target","issueDependenciesSummary":{"totalBlockedBy":2},"blockedBy":{"nodes":[{"id":"a"},{"id":"b"}]}}`, map[string]string{"a": "Done", "b": "Done"}, true},
		{"one prerequisite Pending", `{"id":"target","issueDependenciesSummary":{"totalBlockedBy":2},"blockedBy":{"nodes":[{"id":"a"},{"id":"b"}]}}`, map[string]string{"a": "Done", "b": "Pending"}, false},
		{"no explicit dependency", `{"id":"target","issueDependenciesSummary":{"totalBlockedBy":0},"blockedBy":{"nodes":[]}}`, nil, false},
		{"prerequisite with no project status", `{"id":"target","issueDependenciesSummary":{"totalBlockedBy":1},"blockedBy":{"nodes":[{"id":"absent"}]}}`, map[string]string{"absent": ""}, false},
		{"incomplete dependency response", `{"id":"target","issueDependenciesSummary":{"totalBlockedBy":2},"blockedBy":{"nodes":[{"id":"a"}]}}`, map[string]string{"a": "Done"}, false},
		{"inaccessible dependency", `{"id":"target","issueDependenciesSummary":{"totalBlockedBy":2},"blockedBy":{"nodes":[{"id":"a"},null]}}`, map[string]string{"a": "Done"}, false},
		{"self dependency", `{"id":"target","issueDependenciesSummary":{"totalBlockedBy":1},"blockedBy":{"nodes":[{"id":"target"}]}}`, map[string]string{"target": "Done"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var page dependencyPage
			if err := json.Unmarshal([]byte(test.response), &page); err != nil {
				t.Fatal(err)
			}
			got, err := (githubProject{}).dependenciesDone(context.Background(), page, test.statuses)
			if err != nil || got != test.want {
				t.Fatalf("wanted %v, got %v (%v)", test.want, got, err)
			}
		})
	}
}
