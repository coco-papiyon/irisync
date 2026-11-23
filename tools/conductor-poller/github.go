package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

const projectQuery = `query($id:ID!, $after:String) {
  viewer { login }
  node(id:$id) {
    ... on ProjectV2 {
      items(first:100, after:$after) {
        pageInfo { endCursor hasNextPage }
        nodes {
          id isArchived
          fieldValueByName(name:"Status") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
          content {
            ... on Issue { id number closed repository { nameWithOwner } }
          }
        }
      }
    }
  }
}`

type githubProject struct{ cfg config }

func isReviewStatus(status string) bool {
	return status == "PR Review" || status == "PR Fixing" || status == "Merge Ready" || status == "Blocked"
}

type projectIssue struct {
	ID, ItemID, Repository, Status string
	Number                         int
	Closed, Archived               bool
}

type projectPage struct {
	Viewer struct{ Login string }
	Node   *struct {
		Items struct {
			PageInfo struct {
				EndCursor   string
				HasNextPage bool
			}
			Nodes []struct {
				ID               string
				IsArchived       bool
				FieldValueByName struct{ Name string }
				Content          struct {
					ID         string
					Number     int
					Closed     bool
					Repository struct{ NameWithOwner string }
				}
			}
		}
	}
}

func (g githubProject) projectIssues(ctx context.Context) ([]projectIssue, error) {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.GitHubTimeout)
	defer cancel()
	var issues []projectIssue
	after := ""
	for {
		args := []string{"api", "graphql", "--hostname", "github.com", "-f", "query=" + projectQuery, "-f", "id=" + g.cfg.ProjectID}
		if after != "" {
			args = append(args, "-f", "after="+after)
		}
		data, err := ghOutput(ctx, args...)
		if err != nil {
			return nil, err
		}
		var response struct{ Data projectPage }
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("decode Projects page: %w", err)
		}
		page := response.Data
		if !strings.EqualFold(page.Viewer.Login, g.cfg.GitHubLogin) {
			return nil, fmt.Errorf("GitHub account is %q; expected %q", page.Viewer.Login, g.cfg.GitHubLogin)
		}
		if page.Node == nil {
			return nil, fmt.Errorf("configured GitHub Project is missing or inaccessible")
		}
		items := page.Node.Items
		for _, item := range items.Nodes {
			issue := item.Content
			if issue.ID == "" {
				continue
			}
			issues = append(issues, projectIssue{
				ID: issue.ID, ItemID: item.ID, Number: issue.Number, Status: item.FieldValueByName.Name,
				Repository: issue.Repository.NameWithOwner, Closed: issue.Closed, Archived: item.IsArchived,
			})
		}
		if !items.PageInfo.HasNextPage {
			return issues, nil
		}
		if items.PageInfo.EndCursor == "" || items.PageInfo.EndCursor == after {
			return nil, fmt.Errorf("Projects pagination did not advance")
		}
		after = items.PageInfo.EndCursor
	}
}

func (g githubProject) runnable(issue projectIssue) bool {
	if issue.Archived || !strings.EqualFold(issue.Repository, g.cfg.Repository) {
		return false
	}
	status := issue.Status
	smoke := status == "Ready" && slices.Contains(g.cfg.TestIssueNumbers, issue.Number)
	design := slices.Contains(g.cfg.DesignIssueNumbers, issue.Number) &&
		(status == "Ready" || status == "Designing" || status == "Design Review")
	implement := status == "Implementing" && slices.Contains(g.cfg.ImplementationIssueNumbers, issue.Number)
	review := isReviewStatus(status) && slices.Contains(g.cfg.ImplementationIssueNumbers, issue.Number)
	return review || !issue.Closed && (smoke || design || implement)
}

func ghOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	data, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return nil, fmt.Errorf("GitHub CLI: %s", strings.TrimSpace(string(exit.Stderr)))
	}
	return data, err
}
