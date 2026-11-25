package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const implementationPatchSchema = `{"type":"object","properties":{"status":{"enum":["changed","already_satisfied","blocked"]},"reason":{"type":"string"},"patch":{"type":"string"}},"required":["status","reason","patch"],"additionalProperties":false}`

const editBlockedPrefix = "edit: correction stopped: "

type implementationEdit struct {
	Status, Reason, Patch string
}

func (e implementationEdit) validate(hasFeedback bool) error {
	reason := dashboardReviewText(e.Reason)
	switch {
	case e.Status == "changed" && strings.TrimSpace(e.Patch) != "":
		return nil
	case e.Status == "already_satisfied" && e.Patch == "" && reason != "" && !hasFeedback:
		return nil
	case e.Status == "blocked" && e.Patch == "" && reason != "":
		return fmt.Errorf("correction stopped: %s", reason)
	case strings.TrimSpace(e.Patch) == "" && hasFeedback:
		return fmt.Errorf("correction stopped: no patch for the pending feedback. %s", reason)
	default:
		return fmt.Errorf("correction stopped: inconsistent edit response. %s", reason)
	}
}

func implementationEditBlocked(dir string) bool {
	return strings.HasPrefix(readFeedback(dir, "step-error.txt"), editBlockedPrefix)
}

type implementationManifest struct {
	Worktree, Base, Branch, Design, Feedback, Validator string
	ConductorExecutable                                 string
	ResumeTree                                          string
	PreviousReviewTree                                  string
	IssueRequirements                                   string
	ConflictPaths                                       []string
}

type reviewVerdict struct {
	Approved bool   `json:"approved"`
	Feedback string `json:"feedback"`
}

func runImplementationStep(step, manifestPath string) (stepErr error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var job implementationManifest
	if err := json.Unmarshal(data, &job); err != nil {
		return err
	}
	dir := filepath.Dir(manifestPath)
	defer func() {
		if stepErr != nil {
			_ = os.WriteFile(filepath.Join(dir, "step-error.txt"), []byte(step+": "+stepErr.Error()), 0600)
		}
	}()
	if err := job.checkWorktree(); err != nil {
		return err
	}
	switch step {
	case "edit":
		if job.ResumeTree != "" {
			return job.checkTree(job.ResumeTree)
		}
		return job.edit(dir)
	case "validate":
		return job.validate(dir)
	case "review":
		return job.review(dir)
	default:
		return fmt.Errorf("unknown implementation step %q", step)
	}
}

func (m *implementationManifest) reuseStagedImplementation(ctx context.Context) {
	tree, err := gitOutput(ctx, m.Worktree, "write-tree")
	if err != nil {
		return
	}
	base, err := gitOutput(ctx, m.Worktree, "rev-parse", m.Base+"^{tree}")
	if err == nil && !bytes.Equal(base, tree) && m.checkTree(string(tree)) == nil {
		m.ResumeTree = string(tree)
	}
}

func (m implementationManifest) checkWorktree() error {
	root, err := gitOutput(context.Background(), m.Worktree, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("identify implementation worktree: %w", err)
	}
	want, err := os.Stat(m.Worktree)
	if err != nil {
		return err
	}
	got, err := os.Stat(string(root))
	if err != nil {
		return fmt.Errorf("read Git worktree root: %w", err)
	}
	if !want.IsDir() || !os.SameFile(want, got) {
		return fmt.Errorf("implementation worktree root differs from Git root")
	}
	branch, err := gitOutput(context.Background(), m.Worktree, "symbolic-ref", "--short", "HEAD")
	if err != nil || string(branch) != m.Branch {
		return fmt.Errorf("implementation branch is missing or changed")
	}
	head, err := gitOutput(context.Background(), m.Worktree, "rev-parse", "HEAD")
	if err != nil || string(head) != m.Base {
		return fmt.Errorf("implementation worktree HEAD is no longer the pinned base")
	}
	return nil
}

func (m implementationManifest) edit(dir string) error {
	payload, err := m.editPayload()
	if err != nil {
		return err
	}
	schemaPath, response := filepath.Join(dir, "patch-schema.json"), filepath.Join(dir, "patch.json")
	if err := os.WriteFile(schemaPath, []byte(implementationPatchSchema), 0600); err != nil {
		return err
	}
	cmd := exec.Command("codex", "-a", "never", "exec", "--sandbox", "read-only",
		"--ignore-user-config", "--ephemeral", "-c", `web_search="disabled"`,
		"--output-schema", schemaPath, "--output-last-message", response, "-")
	cmd.Dir = m.Worktree
	cmd.Stdin = bytes.NewReader(skillInput(irisyncImplementationSkill, "承認済み設計と現在の worktree を照合し、必要な実装または修正の差分を返す。", payload))
	log, err := os.Create(filepath.Join(dir, "edit.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	isolateCommand(cmd, dir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Codex edit: %w", err)
	}
	data, err := os.ReadFile(response)
	if err != nil {
		return err
	}
	var proposed implementationEdit
	if err := json.Unmarshal(data, &proposed); err != nil {
		return err
	}
	if err := proposed.validate(strings.TrimSpace(m.Feedback) != ""); err != nil {
		return err
	}
	return m.applyPatch(dir, proposed.Patch)
}

func (m implementationManifest) editPayload() ([]byte, error) {
	// Include the staged candidate even when the read-only agent cannot run Git.
	// Expanded context supplies the current text of changed files on ordinary retries.
	diff, err := gitRawOutput(context.Background(), m.Worktree, "diff", "--cached", "--no-ext-diff",
		"--no-textconv", "--no-color", "--unified=1000000", m.Base)
	if err != nil {
		return nil, fmt.Errorf("read staged candidate for editing: %w", err)
	}
	if len(diff) > 256<<10 {
		diff, err = gitRawOutput(context.Background(), m.Worktree, "diff", "--cached", "--no-ext-diff",
			"--no-textconv", "--no-color", m.Base)
		if err != nil {
			return nil, fmt.Errorf("read staged candidate for editing: %w", err)
		}
		if len(diff) > 256<<10 {
			return nil, fmt.Errorf("correction stopped: staged source diff exceeds the edit input limit")
		}
	}
	return json.Marshal(map[string]string{
		"approved_design": m.Design, "issue_requirements": m.IssueRequirements,
		"previous_feedback":     dashboardSecrets.ReplaceAllString(m.Feedback, "[redacted]"),
		"staged_diff_from_base": dashboardSecrets.ReplaceAllString(string(diff), "[redacted]"),
	})
}

func (m implementationManifest) applyPatch(dir, patch string) error {
	if err := m.checkWorktree(); err != nil {
		return err
	}
	patch = strings.ReplaceAll(patch, "\r\n", "\n")
	if patch == "" {
		return nil
	}
	if !strings.HasPrefix(patch, "diff --git ") || len(patch) > 256<<10 {
		return fmt.Errorf("Codex did not return a bounded Git patch")
	}
	for _, line := range strings.Split(patch, "\n") {
		field, mode, found := strings.Cut(line, " mode ")
		if found && (field == "new file" || field == "new") && mode != "100644" && mode != "100755" {
			return fmt.Errorf("the generated patch proposes an unsupported file mode")
		}
	}
	patchPath := filepath.Join(dir, "implementation.patch")
	if err := os.WriteFile(patchPath, []byte(strings.TrimRight(patch, "\n")+"\n"), 0600); err != nil {
		return err
	}
	ctx := context.Background()
	args := []string{"apply", "--index", "--whitespace=nowarn"}
	if _, err := gitOutput(ctx, m.Worktree, append(append([]string{}, args...), "--check", "--", patchPath)...); err != nil {
		return err
	}
	_, err := gitOutput(ctx, m.Worktree, append(args, "--", patchPath)...)
	return err
}

func (m implementationManifest) validate(dir string) error {
	ctx := context.Background()
	if err := m.checkConflictMarkers(); err != nil {
		return err
	}
	if _, err := gitOutput(ctx, m.Worktree, "add", "--all", "--", "."); err != nil {
		return err
	}
	if _, err := gitOutput(ctx, m.Worktree, "diff", "--cached", "--check", "--no-ext-diff"); err != nil {
		return err
	}
	tree, err := gitOutput(ctx, m.Worktree, "write-tree")
	if err != nil {
		return err
	}
	baseTree, err := gitOutput(ctx, m.Worktree, "rev-parse", m.Base+"^{tree}")
	if err != nil {
		return err
	}
	if bytes.Equal(tree, baseTree) {
		return fmt.Errorf("the implementation did not change tracked files")
	}
	files, err := gitOutput(ctx, m.Worktree, "diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z")
	if err != nil {
		return err
	}
	for _, file := range strings.Split(string(files), "\x00") {
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		cmd := exec.Command("gofmt", "-l", filepath.Join(m.Worktree, filepath.FromSlash(file)))
		if output, err := cmd.Output(); err != nil || len(output) > 0 {
			return fmt.Errorf("gofmt check failed for %s", file)
		}
	}
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		return err
	}
	log, err := os.Create(filepath.Join(dir, "validation.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	// Keep Git's temporary repositories close to the primary checkout root.
	localDir := filepath.Join(filepath.Dir(filepath.Dir(m.Validator)), ".local")
	localDir, err = filepath.EvalSymlinks(localDir)
	if err != nil {
		return fmt.Errorf("resolve validation temp root: %w", err)
	}
	scratch, err := os.MkdirTemp(localDir, "v-")
	if err != nil {
		return fmt.Errorf("create validation temp directory: %w", err)
	}
	defer os.RemoveAll(scratch)
	writableRoots, err := json.Marshal([]string{scratch})
	if err != nil {
		return err
	}
	cmd := exec.Command("codex", "-c", `sandbox_mode="workspace-write"`,
		"-c", "sandbox_workspace_write.writable_roots="+string(writableRoots),
		"-c", `sandbox_workspace_write.network_access=false`, "sandbox", "--", pwsh,
		"-NoProfile", "-NonInteractive", "-File", m.Validator, "-ScratchDirectory", scratch)
	cmd.Dir = m.Worktree
	cmd.Stdout, cmd.Stderr = log, log
	isolateCommand(cmd, dir)
	if m.ConductorExecutable != "" {
		if !filepath.IsAbs(m.ConductorExecutable) {
			return fmt.Errorf("Conductor executable path must be absolute")
		}
		// This also supports worktrees whose smoke scripts resolve Conductor through PATH.
		cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(m.ConductorExecutable)+string(os.PathListSeparator)+os.Getenv("PATH"))
		fmt.Fprintln(log, "Conductor executable:", m.ConductorExecutable)
	}
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			stage := map[int]string{21: "go vet", 22: "go test", 23: "go build", 24: "Conductor smoke test"}[exit.ExitCode()]
			if stage != "" {
				return fmt.Errorf("%s failed in the Windows sandbox", stage)
			}
		}
		return fmt.Errorf("sandboxed validation: %w", err)
	}
	if err := m.checkTree(string(tree)); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "validated-tree"), tree, 0600)
}

func (m implementationManifest) review(dir string) error {
	tree, err := os.ReadFile(filepath.Join(dir, "validated-tree"))
	if err != nil {
		return err
	}
	if err := m.checkTree(string(tree)); err != nil {
		return err
	}
	diff, err := gitRawOutput(context.Background(), m.Worktree, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--no-color", "--binary")
	if err != nil {
		return err
	}
	if len(diff) > 256<<10 {
		return fmt.Errorf("implementation diff exceeds the self-review input limit")
	}
	input := map[string]string{"issue_requirements": m.IssueRequirements, "approved_design": m.Design,
		"diff": string(diff), "previous_feedback": dashboardSecrets.ReplaceAllString(m.Feedback, "[redacted]")}
	if validCommitSHA(m.PreviousReviewTree) {
		delta, err := gitRawOutput(context.Background(), m.Worktree, "diff", "--no-ext-diff",
			"--no-textconv", "--no-color", m.PreviousReviewTree, string(tree))
		if err == nil && len(delta) <= 64<<10 {
			input["diff_since_previous_review"] = dashboardSecrets.ReplaceAllString(string(delta), "[redacted]")
		}
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	prompt := "実装後の自己レビューを行う。前回の指摘があれば先に解消を確認し、承認済み設計の同じ基準に沿って全体を判定する。ステージ済みの差分はローカル検証を通過している。PR 公開前のため GitHub 上の CI 結果やレビュー記録は確認対象外とする。"
	verdict, err := codexReview(dir, prompt, payload)
	if err != nil {
		return err
	}
	if !verdict.Approved {
		return fmt.Errorf("Codex self-review requested a correction")
	}
	if err := m.checkTree(string(tree)); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "approved-tree"), tree, 0600)
}

func codexReview(dir, prompt string, payload []byte) (reviewVerdict, error) {
	var verdict reviewVerdict
	schemaPath, response := filepath.Join(dir, "review-schema.json"), filepath.Join(dir, "review.json")
	if err := os.WriteFile(schemaPath, []byte(implementationReviewSchema), 0600); err != nil {
		return verdict, err
	}
	cmd := exec.Command("codex", "-a", "never", "exec", "--sandbox", "read-only",
		"--skip-git-repo-check", "--ignore-user-config", "--ephemeral", "--disable", "shell_tool", "--disable", "unified_exec",
		"-c", `web_search="disabled"`, "--output-schema", schemaPath, "--output-last-message", response, "-")
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(skillInput(irisyncReviewSkill, prompt, payload))
	log, err := os.Create(filepath.Join(dir, "review.log"))
	if err != nil {
		return verdict, err
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	isolateCommand(cmd, dir)
	if err := cmd.Run(); err != nil {
		return verdict, fmt.Errorf("Codex review: %w", err)
	}
	data, err := os.ReadFile(response)
	if err != nil {
		return verdict, err
	}
	var output reviewOutput
	if err := json.Unmarshal(data, &output); err != nil {
		return verdict, err
	}
	verdict, err = output.verdict()
	if err != nil {
		return verdict, err
	}
	data, err = json.Marshal(verdict)
	if err != nil {
		return verdict, err
	}
	return verdict, os.WriteFile(response, data, 0600)
}

func (m implementationManifest) checkTree(want string) error {
	if !validCommitSHA(want) {
		return fmt.Errorf("missing validated Git tree")
	}
	if err := m.checkWorktree(); err != nil {
		return err
	}
	ctx := context.Background()
	if _, err := gitOutput(ctx, m.Worktree, "diff", "--quiet", "--no-ext-diff"); err != nil {
		return fmt.Errorf("worktree changed after staging")
	}
	untracked, err := gitOutput(ctx, m.Worktree, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil || len(untracked) != 0 {
		return fmt.Errorf("untracked files appeared after staging")
	}
	tree, err := gitOutput(ctx, m.Worktree, "write-tree")
	if err != nil || string(tree) != want {
		return fmt.Errorf("staged tree changed after validation")
	}
	return nil
}

func isolateCommand(cmd *exec.Cmd, dir string) {
	for _, env := range cmd.Environ() {
		key, _, _ := strings.Cut(env, "=")
		switch strings.ToUpper(key) {
		case "PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "USERPROFILE", "APPDATA",
			"LOCALAPPDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432", "PROGRAMDATA", "HOMEDRIVE", "HOMEPATH",
			"USERDOMAIN", "USERNAME", "SYSTEMDRIVE", "PSMODULEPATH", "CODEX_HOME", "CONDUCTOR_HOME", "CONDUCTOR_NO_UPDATE_CHECK",
			"PYTHONUTF8", "GOPATH", "GOMODCACHE":
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "GH_CONFIG_DIR="+filepath.Join(dir, "github"), "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GCM_INTERACTIVE=never")
}

func readFeedback(dir, name string) string {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	defer f.Close()
	const limit = 12 << 10
	if info, err := f.Stat(); err == nil && info.Size() > limit {
		f.Seek(-limit, io.SeekEnd)
	}
	data, _ := io.ReadAll(io.LimitReader(f, limit))
	return strings.ToValidUTF8(string(data), "")
}

func previousValidationFailure(workRoot string, attempts int) string {
	for n := attempts; n > 0; n-- {
		dir := filepath.Join(workRoot, fmt.Sprintf("attempt-%d", n))
		stageError := strings.TrimSpace(readFeedback(dir, "step-error.txt"))
		if strings.HasPrefix(stageError, "validate:") {
			output := strings.TrimSpace(readFeedback(dir, "validation.log"))
			return strings.TrimSpace(stageError + "\n" + output)
		}
		if _, err := os.Stat(filepath.Join(dir, "validated-tree")); err == nil {
			break
		}
	}
	return ""
}

func readPreviousReview(dir string) string {
	file, err := os.Open(filepath.Join(dir, "review.json"))
	if err != nil {
		return ""
	}
	defer file.Close()
	var verdict reviewVerdict
	if err := json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&verdict); err != nil {
		return ""
	}
	return boundedReviewText(verdict.Feedback)
}
