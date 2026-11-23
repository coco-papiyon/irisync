package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// Only dependency IDs are requested. GitHub's Issue closed state does not
// determine completion, which comes from the configured Project's Status.
const dependenciesQuery = `query($ids:[ID!]!, $after:String) {
  viewer { login }
  nodes(ids:$ids) {
    ... on Issue {
      id
      issueDependenciesSummary { totalBlockedBy }
      blockedBy(first:100, after:$after) {
        pageInfo { endCursor hasNextPage }
        nodes { id }
      }
    }
  }
}`

type dependencyPage struct {
	ID                       string
	IssueDependenciesSummary *struct{ TotalBlockedBy int }
	BlockedBy                *struct {
		PageInfo struct {
			EndCursor   string
			HasNextPage bool
		}
		Nodes []struct{ ID string }
	}
}

func (g githubProject) promotePending(ctx context.Context, issues []projectIssue) (int, error) {
	pending, statuses := pendingAndStatuses(issues, g.cfg.Repository)
	promoted := 0
	var promotionErr error
	const batchSize = 25
	for offset := 0; offset < len(pending); offset += batchSize {
		batch := pending[offset:min(offset+batchSize, len(pending))]
		ids := make([]string, 0, len(batch))
		for _, issue := range batch {
			ids = append(ids, issue.ID)
		}
		pages, err := g.dependencyPages(ctx, ids, "")
		if err != nil {
			return promoted, errors.Join(promotionErr, err)
		}
		for _, issue := range batch {
			page, found := pages[issue.ID]
			if !found {
				continue
			}
			satisfied, err := g.dependenciesDone(ctx, page, statuses)
			if err != nil {
				promotionErr = errors.Join(promotionErr, fmt.Errorf("issue #%d prerequisites: %w", issue.Number, err))
				continue
			}
			if !satisfied {
				continue
			}
			if err := g.setDesignStatus(ctx, issue, "Ready"); err != nil {
				promotionErr = errors.Join(promotionErr, fmt.Errorf("promote issue #%d: %w", issue.Number, err))
				continue
			}
			promoted++
			slog.Info("issue prerequisites are Done", "issue", issue.Number)
		}
	}
	return promoted, promotionErr
}

func pendingAndStatuses(issues []projectIssue, repository string) ([]projectIssue, map[string]string) {
	var pending []projectIssue
	statuses := make(map[string]string)
	for _, issue := range issues {
		statuses[issue.ID] = issue.Status
		if issue.Status == "Pending" && !issue.Closed && !issue.Archived && strings.EqualFold(issue.Repository, repository) {
			pending = append(pending, issue)
		}
	}
	return pending, statuses
}

func (g githubProject) dependencyPages(ctx context.Context, ids []string, after string) (map[string]dependencyPage, error) {
	args := []string{"graphql", "-f", "query=" + dependenciesQuery}
	for _, id := range ids {
		args = append(args, "-f", "ids[]="+id)
	}
	if after != "" {
		args = append(args, "-f", "after="+after)
	}
	var response struct {
		Data struct {
			Viewer struct{ Login string }
			Nodes  []dependencyPage
		}
	}
	if err := g.getJSON(ctx, &response, args...); err != nil {
		return nil, err
	}
	if !strings.EqualFold(response.Data.Viewer.Login, g.cfg.GitHubLogin) {
		return nil, fmt.Errorf("GitHub account changed while reading issue dependencies")
	}
	pages := make(map[string]dependencyPage, len(response.Data.Nodes))
	for _, page := range response.Data.Nodes {
		if page.ID != "" {
			pages[page.ID] = page
		}
	}
	return pages, nil
}

func (g githubProject) dependenciesDone(ctx context.Context, page dependencyPage, statuses map[string]string) (bool, error) {
	if page.IssueDependenciesSummary == nil || page.IssueDependenciesSummary.TotalBlockedBy == 0 {
		return false, nil
	}
	total := page.IssueDependenciesSummary.TotalBlockedBy
	issueID := page.ID
	seen := make(map[string]bool)
	after := ""
	for {
		if page.BlockedBy == nil || page.IssueDependenciesSummary == nil || page.IssueDependenciesSummary.TotalBlockedBy != total {
			return false, nil
		}
		countBefore := len(seen)
		for _, prerequisite := range page.BlockedBy.Nodes {
			status, known := statuses[prerequisite.ID]
			if prerequisite.ID == "" || prerequisite.ID == issueID || known && status != "Done" {
				return false, nil
			}
			seen[prerequisite.ID] = true
		}
		next := page.BlockedBy.PageInfo
		if !next.HasNextPage {
			// Missing or hidden prerequisites must never promote an issue.
			break
		}
		if next.EndCursor == "" || next.EndCursor == after || len(seen) == countBefore || len(seen) >= total {
			return false, fmt.Errorf("issue dependency pagination did not advance")
		}
		after = next.EndCursor
		pages, err := g.dependencyPages(ctx, []string{issueID}, after)
		if err != nil {
			return false, err
		}
		page = pages[issueID]
	}
	if len(seen) != total {
		return false, nil
	}
	for id := range seen {
		status, known := statuses[id]
		if !known {
			var err error
			status, err = g.projectStatusForIssue(ctx, id)
			if err != nil {
				return false, err
			}
			statuses[id] = status
		}
		if status != "Done" {
			return false, nil
		}
	}
	return true, nil
}

// The main project query lists active items. Look up an absent prerequisite
// directly to account for a Done item that has since been archived.
func (g githubProject) projectStatusForIssue(ctx context.Context, id string) (string, error) {
	g = g.projectAPI()
	query := `query($id:ID!, $after:String) {
	  viewer { login }
	  node(id:$id) {
	    ... on Issue {
	      projectItems(first:100, includeArchived:true, after:$after) {
	        pageInfo { endCursor hasNextPage }
	        nodes {
	          project { id }
	          fieldValueByName(name:"Status") {
	            ... on ProjectV2ItemFieldSingleSelectValue { name }
	          }
	        }
	      }
	    }
	  }
	}`
	after := ""
	for {
		args := []string{"graphql", "-f", "query=" + query, "-f", "id=" + id}
		if after != "" {
			args = append(args, "-f", "after="+after)
		}
		var response struct {
			Data struct {
				Viewer struct{ Login string }
				Node   *struct {
					ProjectItems struct {
						PageInfo struct {
							EndCursor   string
							HasNextPage bool
						}
						Nodes []struct {
							Project          struct{ ID string }
							FieldValueByName struct{ Name string }
						}
					}
				}
			}
		}
		if err := g.getJSON(ctx, &response, args...); err != nil {
			return "", err
		}
		page := response.Data
		if !strings.EqualFold(page.Viewer.Login, g.cfg.GitHubLogin) {
			return "", fmt.Errorf("GitHub account changed while checking a prerequisite")
		}
		if page.Node == nil {
			return "", nil
		}
		items := page.Node.ProjectItems
		for _, item := range items.Nodes {
			if item.Project.ID == g.cfg.ProjectID {
				return item.FieldValueByName.Name, nil
			}
		}
		if !items.PageInfo.HasNextPage {
			return "", nil
		}
		if items.PageInfo.EndCursor == "" || items.PageInfo.EndCursor == after {
			return "", fmt.Errorf("prerequisite project pagination did not advance")
		}
		after = items.PageInfo.EndCursor
	}
}
