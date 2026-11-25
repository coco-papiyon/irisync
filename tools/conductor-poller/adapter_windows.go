package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const designOutputSchema = `{"type":"object","properties":{"markdown":{"type":"string"}},"required":["markdown"],"additionalProperties":false}`

type designOutput struct {
	Markdown string       `json:"markdown"`
	Usage    *designUsage `json:"usage,omitempty"`
}

type designUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func runDesignAdapter(inputPath, outputPath string) error {
	prompt, err := os.ReadFile(inputPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(inputPath)
	workspace, err := os.MkdirTemp("", "irisync-codex-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	schemaPath := filepath.Join(dir, "schema.json")
	rawPath := filepath.Join(dir, "response.json")
	if err := os.WriteFile(schemaPath, []byte(designOutputSchema), 0600); err != nil {
		return err
	}
	// The CLI starts outside the repository and receives the supplied material
	// on stdin. Design generation does not require shell tools.
	cmd := exec.Command("codex", "-a", "never", "exec", "--sandbox", "read-only",
		"--skip-git-repo-check", "--ignore-user-config", "--ephemeral",
		"--disable", "shell_tool", "--disable", "unified_exec",
		"-c", `web_search="disabled"`, "--json", "--output-schema", schemaPath,
		"--output-last-message", rawPath, "-")
	cmd.Dir = workspace
	cmd.Stdin = bytes.NewReader(prompt)
	for _, env := range cmd.Environ() {
		key, _, _ := strings.Cut(env, "=")
		switch strings.ToUpper(key) {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_CONFIG_DIR", "OPENAI_API_KEY":
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	cmd.Env = append(cmd.Env, "GH_CONFIG_DIR="+filepath.Join(dir, "github"))
	var stderr codexErrorStream
	cmd.Stderr = &stderr
	events, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var usage *designUsage
	var limited bool
	scanner := bufio.NewScanner(events)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		limited = limited || codexEventLimited(scanner.Bytes())
		var event struct {
			Type  string
			Usage *designUsage
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "turn.completed" {
			usage = event.Usage
		}
	}
	if err := scanner.Err(); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("read Codex events: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return codexExecError(dir, "codex exec", limited || stderr.reached(), err)
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		return err
	}
	var result designOutput
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("decode Codex design: %w", err)
	}
	if !validDesign(result.Markdown) {
		return fmt.Errorf("Codex did not produce a valid Markdown design")
	}
	result.Usage = usage
	out, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(outputPath, out, 0600)
}

func validDesign(markdown string) bool {
	return len(markdown) <= 128<<10 && strings.HasPrefix(strings.TrimSpace(markdown), "# ")
}
