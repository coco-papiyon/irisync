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

type githubProject struct {
	cfg          config
	app          *githubAppCredentials
	storedToken  string
	projectToken string
}

func (g githubProject) projectAPI() githubProject {
	if g.app == nil {
		return g
	}
	cfg := g.cfg
	cfg.GitHubLogin = cfg.GitHubProjectLogin
	return githubProject{cfg: cfg, storedToken: g.projectToken}
}

func (g *githubProject) resolveProjectID(ctx context.Context) error {
	if g.cfg.ProjectID != "" {
		return nil
	}
	query := `query($owner:String!, $number:Int!) {
		viewer { login }
		repositoryOwner(login:$owner) {
			... on User { projectV2(number:$number) { id } }
			... on Organization { projectV2(number:$number) { id } }
		}
	}`
	var response struct {
		Data struct {
			Viewer          struct{ Login string }
			RepositoryOwner struct{ ProjectV2 struct{ ID string } }
		}
	}
	project := g.projectAPI()
	owner, number := g.cfg.ProjectOwner, g.cfg.ProjectNumber
	if err := project.getJSON(ctx, &response, "graphql", "-f", "query="+query, "-f", "owner="+owner, "-F", fmt.Sprintf("number=%d", number)); err != nil {
		return fmt.Errorf("resolve Project %s #%d: %w", owner, number, err)
	}
	if !strings.EqualFold(response.Data.Viewer.Login, project.cfg.GitHubLogin) {
		return fmt.Errorf("unexpected GitHub account while resolving Project %s #%d", owner, number)
	}
	id := response.Data.RepositoryOwner.ProjectV2.ID
	if id == "" {
		return fmt.Errorf("Project %s #%d was not found or cannot be accessed", owner, number)
	}
	g.cfg.ProjectID = id
	return nil
}

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
	g = g.projectAPI()
	ctx, cancel := context.WithTimeout(ctx, g.cfg.GitHubTimeout)
	defer cancel()
	var issues []projectIssue
	after := ""
	for {
		args := []string{"api", "graphql", "--hostname", "github.com", "-f", "query=" + projectQuery, "-f", "id=" + g.cfg.ProjectID}
		if after != "" {
			args = append(args, "-f", "after="+after)
		}
		data, err := g.ghOutput(ctx, args...)
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
	design := g.cfg.designEnabled(issue.Number) &&
		(status == "Ready" || status == "Designing" || status == "Design Review")
	implement := status == "Implementing" && g.cfg.implementationEnabled(issue.Number)
	review := isReviewStatus(status) && g.cfg.implementationEnabled(issue.Number)
	return review || !issue.Closed && (smoke || design || implement)
}

func (g githubProject) ghCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	var token string
	if g.app != nil {
		installation, err := g.app.token(ctx)
		if err != nil {
			return nil, err
		}
		token = installation.value
	} else {
		token = g.storedToken
	}
	if token != "" {
		cmd.Env = append(cmd.Env, "GH_TOKEN="+token, "GH_DEBUG=")
	}
	return cmd, nil
}

func (g githubProject) ghOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd, err := g.ghCommand(ctx, args...)
	if err != nil {
		return nil, err
	}
	data, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if exit, ok := err.(*exec.ExitError); ok {
		detail := strings.TrimSpace(string(exit.Stderr))
		if g.app != nil || g.storedToken != "" {
			for _, env := range cmd.Environ() {
				key, value, _ := strings.Cut(env, "=")
				if strings.EqualFold(key, "GH_TOKEN") && value != "" {
					detail = strings.ReplaceAll(detail, value, "[redacted]")
				}
			}
		}
		return nil, fmt.Errorf("GitHub CLI: %s", detail)
	}
	return data, err
}

type githubIdentity struct {
	Login string
	ID    int64 `json:"id"`
}

func (g githubProject) automationIdentity(ctx context.Context) (githubIdentity, error) {
	var actor githubIdentity
	if g.app == nil {
		if err := g.getJSON(ctx, &actor, "user"); err != nil {
			return actor, err
		}
	} else {
		var response struct {
			Data struct{ Viewer githubIdentity }
		}
		if err := g.getJSON(ctx, &response, "graphql", "-f", "query=query { viewer { login id: databaseId } }"); err != nil {
			return actor, err
		}
		actor = response.Data.Viewer
	}
	if actor.ID <= 0 || !strings.EqualFold(actor.Login, g.cfg.GitHubLogin) {
		return actor, fmt.Errorf("unexpected GitHub account before implementation operation")
	}
	return actor, nil
}
