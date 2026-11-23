package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type config struct {
	ProjectID                  string        `yaml:"project_id"`
	Repository                 string        `yaml:"repository"`
	GitHubLogin                string        `yaml:"github_login"`
	PollInterval               time.Duration `yaml:"poll_interval"`
	GitHubTimeout              time.Duration `yaml:"github_timeout"`
	WorkflowTimeout            time.Duration `yaml:"workflow_timeout"`
	ParallelRuns               int           `yaml:"parallel_runs"`
	MaxAttempts                int           `yaml:"max_attempts"`
	DataDir                    string        `yaml:"data_dir"`
	WorkDir                    string        `yaml:"work_dir"`
	TestIssueNumbers           []int         `yaml:"test_issue_numbers"`
	DesignIssueNumbers         []int         `yaml:"design_issue_numbers"`
	DesignApproverIDs          []int64       `yaml:"design_approver_ids"`
	DesignTimeout              time.Duration `yaml:"design_timeout"`
	ImplementationIssueNumbers []int         `yaml:"implementation_issue_numbers"`
	ImplementationTimeout      time.Duration `yaml:"implementation_timeout"`
}

func loadConfig(path string) (config, error) {
	cfg := config{
		PollInterval: 5 * time.Minute, GitHubTimeout: 30 * time.Second,
		WorkflowTimeout: 2 * time.Minute, ParallelRuns: 1, MaxAttempts: 3,
		DataDir: ".local", WorkDir: "..", DesignTimeout: 15 * time.Minute,
		ImplementationTimeout: 30 * time.Minute,
	}
	file, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return cfg, fmt.Errorf("expected a single YAML document")
	}
	if cfg.ProjectID == "" || cfg.GitHubLogin == "" || !strings.Contains(cfg.Repository, "/") {
		return cfg, fmt.Errorf("project_id, repository (owner/name), and github_login are required")
	}
	if cfg.PollInterval <= 0 || cfg.PollInterval > 24*time.Hour ||
		cfg.GitHubTimeout <= 0 || cfg.WorkflowTimeout <= 0 || cfg.DesignTimeout <= 0 || cfg.ImplementationTimeout <= 0 || cfg.ParallelRuns < 1 || cfg.MaxAttempts < 1 {
		return cfg, fmt.Errorf("intervals, timeouts, parallel_runs, and max_attempts must be positive; poll_interval must be at most 24h")
	}
	for _, number := range cfg.TestIssueNumbers {
		if number < 1 {
			return cfg, fmt.Errorf("test_issue_numbers must contain positive issue numbers")
		}
	}
	if len(cfg.DesignIssueNumbers) > 0 && len(cfg.DesignApproverIDs) == 0 {
		return cfg, fmt.Errorf("design_approver_ids are required when design is enabled")
	}
	for _, id := range cfg.DesignApproverIDs {
		if id < 1 {
			return cfg, fmt.Errorf("design_approver_ids must contain positive GitHub user IDs")
		}
	}
	for _, number := range cfg.DesignIssueNumbers {
		if number < 1 || slices.Contains(cfg.TestIssueNumbers, number) {
			return cfg, fmt.Errorf("design_issue_numbers must be positive and distinct from test_issue_numbers")
		}
	}
	for _, number := range cfg.ImplementationIssueNumbers {
		if !slices.Contains(cfg.DesignIssueNumbers, number) {
			return cfg, fmt.Errorf("implementation_issue_numbers must be included in design_issue_numbers")
		}
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return cfg, err
	}
	cfg.DataDir = configPath(base, cfg.DataDir)
	cfg.WorkDir = configPath(base, cfg.WorkDir)
	return cfg, nil
}

func configPath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}
