package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

type designComment struct {
	ID        int64
	Body      string
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      githubIdentity
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
	data, err := g.ghOutput(ctx, append([]string{"api", "--hostname", "github.com"}, args...)...)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func (g githubProject) mutate(ctx context.Context, method, endpoint string, input any) error {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.GitHubTimeout)
	defer cancel()
	if _, err := g.automationIdentity(ctx); err != nil {
		return fmt.Errorf("identify GitHub account before %s %q: %w", method, endpoint, err)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	cmd, err := g.ghCommand(ctx, "api", "--hostname", "github.com", endpoint, "--method", method, "--input", "-", "--include")
	if err != nil {
		return err
	}
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return githubMutationResult(method, endpoint, out, err)
}

var githubRequestID = regexp.MustCompile(`^[0-9A-Fa-f]{1,16}(?::[0-9A-Fa-f]{1,16}){3,5}$`)

func githubMutationResult(method, endpoint string, output []byte, commandErr error) error {
	reader := bufio.NewReader(bytes.NewReader(output))
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		if commandErr != nil {
			return fmt.Errorf("GitHub %s %q: no HTTP response: %w", method, endpoint, commandErr)
		}
		return fmt.Errorf("GitHub %s %q: invalid HTTP response headers", method, endpoint)
	}
	// Log only the status code and the validated request ID. The response
	// body and other headers may echo credentials or submitted content.
	result := fmt.Sprintf("GitHub %s %q: HTTP %d", method, endpoint, response.StatusCode)
	if id := response.Header.Get("X-GitHub-Request-Id"); githubRequestID.MatchString(id) {
		result += " (request ID " + id + ")"
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s: request failed", result)
	}
	if endpoint == "graphql" {
		// gh writes a decoded body after the headers, even if the HTTP
		// headers describe transfer or content encodings.
		var message struct{ Errors []json.RawMessage }
		body, err := io.ReadAll(reader)
		if err != nil || json.Unmarshal(body, &message) != nil {
			return fmt.Errorf("%s: invalid GraphQL response", result)
		}
		if len(message.Errors) > 0 {
			return fmt.Errorf("%s: GraphQL mutation rejected", result)
		}
	}
	if commandErr != nil {
		return fmt.Errorf("%s: %w", result, commandErr)
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
	g = g.projectAPI()
	query := `query($project:ID!,$item:ID!){
		viewer{login}
		item:node(id:$item){...on ProjectV2Item{isArchived,project{id},fieldValueByName(name:"Status"){...on ProjectV2ItemFieldSingleSelectValue{name}},content{...on Issue{id closed}}}}
		project:node(id:$project){...on ProjectV2{field(name:"Status"){...on ProjectV2SingleSelectField{id options{id name}}}}}
	}`
	var response struct {
		Data struct {
			Viewer struct{ Login string }
			Item   struct {
				IsArchived       bool
				Project          struct{ ID string }
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
	if !strings.EqualFold(data.Viewer.Login, g.cfg.GitHubLogin) || data.Item.IsArchived || data.Item.Project.ID != g.cfg.ProjectID ||
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
			err := g.mutate(ctx, "POST", "graphql", map[string]any{
				"query": `mutation($input:UpdateProjectV2ItemFieldValueInput!){updateProjectV2ItemFieldValue(input:$input){clientMutationId}}`,
				"variables": map[string]any{"input": map[string]any{
					"projectId": g.cfg.ProjectID, "itemId": item.ItemID, "fieldId": data.Project.Field.ID,
					"value": map[string]string{"singleSelectOptionId": option.ID},
				}},
			})
			if err != nil {
				return fmt.Errorf("update Project Status %q to %q: %w", item.Status, target, err)
			}
			return nil
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
