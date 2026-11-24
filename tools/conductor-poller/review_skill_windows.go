package main

import (
	"context"
	"fmt"
	"strings"
)

const implementationReviewSchema = `{
  "type":"object",
  "properties":{
    "criteria":{"type":"array","items":{"type":"object","properties":{
      "criterion":{"type":"string"},"status":{"type":"string","enum":["met","unmet","unverified","post_publish"]},"evidence":{"type":"string"}
    },"required":["criterion","status","evidence"],"additionalProperties":false}},
    "findings":{"type":"array","items":{"type":"object","properties":{
      "location":{"type":"string"},"problem":{"type":"string"},"fix":{"type":"string"}
    },"required":["location","problem","fix"],"additionalProperties":false}}
  },
  "required":["criteria","findings"],
  "additionalProperties":false
}`

type reviewCriterion struct {
	Criterion string `json:"criterion"`
	Status    string `json:"status"`
	Evidence  string `json:"evidence"`
}

type reviewFinding struct {
	Location string `json:"location"`
	Problem  string `json:"problem"`
	Fix      string `json:"fix"`
}

type reviewOutput struct {
	Criteria []reviewCriterion `json:"criteria"`
	Findings []reviewFinding   `json:"findings"`
}

func (r reviewOutput) verdict() (reviewVerdict, error) {
	if r.Criteria == nil || r.Findings == nil {
		return reviewVerdict{}, fmt.Errorf("review did not return criteria and findings")
	}
	var text strings.Builder
	for i, finding := range r.Findings {
		if strings.TrimSpace(finding.Location) == "" || strings.TrimSpace(finding.Problem) == "" || strings.TrimSpace(finding.Fix) == "" {
			return reviewVerdict{}, fmt.Errorf("review finding %d lacks a location, problem, or fix", i+1)
		}
		fmt.Fprintf(&text, "%d. %s — %s\n   修正方法 %s\n", i+1, finding.Location, finding.Problem, finding.Fix)
	}
	if len(r.Findings) == 0 {
		text.WriteString("修正必須の指摘はありません。\n")
	}
	if len(r.Criteria) != 0 {
		text.WriteString("\n受け入れ基準の確認\n")
	}
	for _, criterion := range r.Criteria {
		status, ok := reviewCriterionLabels[criterion.Status]
		if !ok || strings.TrimSpace(criterion.Criterion) == "" || strings.TrimSpace(criterion.Evidence) == "" {
			return reviewVerdict{}, fmt.Errorf("review criterion lacks valid status, description, or evidence")
		}
		if criterion.Status == "unmet" && len(r.Findings) == 0 {
			return reviewVerdict{}, fmt.Errorf("review reported an unmet criterion without a finding")
		}
		fmt.Fprintf(&text, "- [%s] %s — %s\n", status, criterion.Criterion, criterion.Evidence)
	}
	return reviewVerdict{
		Approved: len(r.Findings) == 0,
		Feedback: strings.TrimSpace(dashboardSecrets.ReplaceAllString(text.String(), "[redacted]")),
	}, nil
}

var reviewCriterionLabels = map[string]string{
	"met": "満たす", "unmet": "未達", "unverified": "確認材料不足", "post_publish": "公開後に確認",
}

func (g githubProject) reviewIssueRequirements(ctx context.Context, item projectIssue) (string, error) {
	var issue designIssue
	path := fmt.Sprintf("repos/%s/issues/%d", g.cfg.Repository, item.Number)
	if err := g.getJSON(ctx, &issue, path); err != nil {
		return "", err
	}
	if issue.NodeID != item.ID || strings.TrimSpace(issue.Title) == "" {
		return "", fmt.Errorf("could not verify the Issue for code review")
	}
	return dashboardSecrets.ReplaceAllString(issue.Title+"\n\n"+issue.Body, "[redacted]"), nil
}
