package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

type designComment struct {
	ID        int64
	Body      string
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		ID    int64
		Login string
		Type  string
	}
}

type designIssue struct {
	Title  string
	Body   string
	NodeID string `json:"node_id"`
	State  string
}

type designRef struct {
	Head          string
	DefaultHead   string
	DefaultBranch string
	Markdown      string
}

func (g githubProject) readDesignRef(ctx context.Context, branch, path string) (designRef, error) {
	query := `query($repo:String!,$owner:String!,$ref:String!,$path:String!) {
		viewer { login }
		repository(owner:$owner,name:$repo) {
			defaultBranchRef { name target { oid } }
			ref(qualifiedName:$ref) { target { oid ... on Commit { file(path:$path) { object { ... on Blob { text } } } } } }
		}
	}`
	owner, repo, _ := strings.Cut(g.cfg.Repository, "/")
	var response struct {
		Data struct {
			Viewer     struct{ Login string }
			Repository *struct {
				DefaultBranchRef struct {
					Name   string
					Target struct{ OID string }
				}
				Ref struct {
					Target struct {
						OID  string
						File struct{ Object struct{ Text string } }
					}
				}
			}
		}
	}
	err := g.getJSON(ctx, &response, "graphql", "-f", "query="+query, "-f", "repo="+repo,
		"-f", "owner="+owner, "-f", "ref=refs/heads/"+branch, "-f", "path="+path)
	if err != nil {
		return designRef{}, err
	}
	if !strings.EqualFold(response.Data.Viewer.Login, g.cfg.GitHubLogin) || response.Data.Repository == nil {
		return designRef{}, fmt.Errorf("unexpected GitHub account or repository while reading design ref")
	}
	data := response.Data.Repository
	return designRef{Head: data.Ref.Target.OID, DefaultHead: data.DefaultBranchRef.Target.OID, DefaultBranch: data.DefaultBranchRef.Name, Markdown: data.Ref.Target.File.Object.Text}, nil
}

func (g githubProject) getJSON(ctx context.Context, target any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.GitHubTimeout)
	defer cancel()
	data, err := ghOutput(ctx, append([]string{"api", "--hostname", "github.com"}, args...)...)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func (g githubProject) mutate(ctx context.Context, method, endpoint string, input any) error {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.GitHubTimeout)
	defer cancel()
	current, err := ghOutput(ctx, "api", "--hostname", "github.com", "user", "--jq", ".login")
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(string(current)), g.cfg.GitHubLogin) {
		return fmt.Errorf("GitHub account changed before %s", method)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "gh", "api", "--hostname", "github.com", endpoint, "--method", method, "--input", "-")
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		// The response can contain the input Markdown or Issue comment.
		return fmt.Errorf("GitHub %s request failed: %w", method, err)
	}
	if endpoint == "graphql" {
		var response struct{ Errors []json.RawMessage }
		if err := json.Unmarshal(out, &response); err != nil || len(response.Errors) > 0 {
			return fmt.Errorf("GitHub rejected the Projects mutation")
		}
	}
	return nil
}

func (g githubProject) publishDesign(ctx context.Context, branch, path, markdown string) (designRef, error) {
	endpoint := "repos/" + g.cfg.Repository
	ref, err := g.readDesignRef(ctx, branch, path)
	if err != nil {
		return ref, err
	}
	if ref.Head == "" {
		if ref.DefaultHead == "" {
			return ref, fmt.Errorf("default branch commit not found")
		}
		if err := g.mutate(ctx, "POST", endpoint+"/git/refs", map[string]string{
			"ref": "refs/heads/" + branch, "sha": ref.DefaultHead,
		}); err != nil {
			return ref, err
		}
	}
	if ref.Markdown == "" {
		if err := g.mutate(ctx, "PUT", endpoint+"/contents/"+path, map[string]string{
			"branch": branch, "message": "Add Conductor issue design",
			"content": base64.StdEncoding.EncodeToString([]byte(markdown)),
		}); err != nil {
			return ref, err
		}
		ref, err = g.readDesignRef(ctx, branch, path)
		if err != nil {
			return ref, err
		}
	}
	if ref.Markdown != markdown || !validCommitSHA(ref.Head) {
		return ref, fmt.Errorf("design on the issue branch differs from the generated draft")
	}
	return ref, nil
}

func (g githubProject) setDesignStatus(ctx context.Context, item projectIssue, target string) error {
	query := `query($project:ID!,$item:ID!){
		viewer{login}
		item:node(id:$item){...on ProjectV2Item{isArchived,fieldValueByName(name:"Status"){...on ProjectV2ItemFieldSingleSelectValue{name}},content{...on Issue{id closed}}}}
		project:node(id:$project){...on ProjectV2{field(name:"Status"){...on ProjectV2SingleSelectField{id options{id name}}}}}
	}`
	var response struct {
		Data struct {
			Viewer struct{ Login string }
			Item   struct {
				IsArchived       bool
				FieldValueByName struct{ Name string }
				Content          struct {
					ID     string
					Closed bool
				}
			}
			Project struct {
				Field struct {
					ID      string
					Options []struct{ ID, Name string }
				}
			}
		}
	}
	if err := g.getJSON(ctx, &response, "graphql", "-f", "query="+query, "-f", "project="+g.cfg.ProjectID, "-f", "item="+item.ItemID); err != nil {
		return err
	}
	data := response.Data
	if !strings.EqualFold(data.Viewer.Login, g.cfg.GitHubLogin) || data.Item.IsArchived ||
		data.Item.Content.ID != item.ID || data.Item.Content.Closed && target != "Done" {
		return fmt.Errorf("issue changed before updating Status")
	}
	if data.Item.FieldValueByName.Name == target {
		return nil
	}
	if data.Item.FieldValueByName.Name != item.Status || data.Project.Field.ID == "" {
		return fmt.Errorf("Status changed before updating the issue")
	}
	for _, option := range data.Project.Field.Options {
		if option.Name == target {
			return g.mutate(ctx, "POST", "graphql", map[string]any{
				"query": `mutation($input:UpdateProjectV2ItemFieldValueInput!){updateProjectV2ItemFieldValue(input:$input){clientMutationId}}`,
				"variables": map[string]any{"input": map[string]any{
					"projectId": g.cfg.ProjectID, "itemId": item.ItemID, "fieldId": data.Project.Field.ID,
					"value": map[string]string{"singleSelectOptionId": option.ID},
				}},
			})
		}
	}
	return fmt.Errorf("Status option %q is unavailable", target)
}

func (g githubProject) comments(ctx context.Context, number int) ([]designComment, error) {
	var pages [][]designComment
	if err := g.getJSON(ctx, &pages, fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", g.cfg.Repository, number), "--paginate", "--slurp"); err != nil {
		return nil, err
	}
	return slices.Concat(pages...), nil
}
