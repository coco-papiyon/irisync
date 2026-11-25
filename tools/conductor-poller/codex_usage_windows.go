package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	codexUsageInterval = time.Minute
	codexUsageTimeout  = 15 * time.Second
	codexLimitMarker   = "codex-usage-limit"
)

var errCodexLimited = errors.New("Codex usage limit reached")

type codexWindow struct {
	UsedPercent        *int
	WindowDurationMins int
	ResetsAt           int64
}

type codexLimits struct {
	Primary, Secondary   *codexWindow
	RateLimitReachedType string `json:"-"`
	Credits              *struct {
		HasCredits, Unlimited bool
	} `json:"-"`
}

func (limits codexLimits) reached() bool {
	if limits.RateLimitReachedType != "" {
		return true
	}
	if limits.Credits != nil && (limits.Credits.HasCredits || limits.Credits.Unlimited) {
		return false
	}
	return (limits.Primary != nil && limits.Primary.UsedPercent != nil && *limits.Primary.UsedPercent >= 100) ||
		(limits.Secondary != nil && limits.Secondary.UsedPercent != nil && *limits.Secondary.UsedPercent >= 100)
}

type codexUsageStatus struct {
	Limits        codexLimits
	CheckedAt     int64
	RefreshFailed bool
	Paused        bool
}

func (s *state) codexUsage(ctx context.Context) (codexUsageStatus, error) {
	var usage codexUsageStatus
	var windows string
	err := s.db.QueryRowContext(ctx, "SELECT windows, checked_at, refresh_failed, paused FROM codex_usage WHERE id = 1").
		Scan(&windows, &usage.CheckedAt, &usage.RefreshFailed, &usage.Paused)
	if err == nil {
		err = json.Unmarshal([]byte(windows), &usage.Limits)
	}
	return usage, err
}

func readCodexLimits(ctx context.Context, dir string) (codexLimits, error) {
	ctx, cancel := context.WithTimeout(ctx, codexUsageTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "app-server", "--listen", "stdio://")
	cmd.Dir = dir
	isolateCommand(cmd, dir)
	input, err := cmd.StdinPipe()
	if err != nil {
		return codexLimits{}, err
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return codexLimits{}, err
	}
	defer output.Close()
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return codexLimits{}, err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	return fetchCodexLimits(input, output)
}

func fetchCodexLimits(input io.Writer, output io.Reader) (codexLimits, error) {
	encoder := json.NewEncoder(input)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	reply := func(id int) (json.RawMessage, error) {
		for scanner.Scan() {
			var message struct {
				ID     int
				Result json.RawMessage
				Error  json.RawMessage
			}
			if json.Unmarshal(scanner.Bytes(), &message) == nil && message.ID == id {
				if len(message.Result) == 0 || len(message.Error) != 0 {
					return nil, errors.New("Codex app-server rejected the usage request")
				}
				return message.Result, nil
			}
		}
		return nil, errors.New("Codex app-server did not return a usage response")
	}
	if err := encoder.Encode(map[string]any{"method": "initialize", "id": 1, "params": map[string]any{
		"clientInfo": map[string]string{"name": "irisync_poller", "version": "1"},
	}}); err != nil {
		return codexLimits{}, err
	}
	if _, err := reply(1); err != nil {
		return codexLimits{}, err
	}
	if err := encoder.Encode(map[string]string{"method": "initialized"}); err != nil {
		return codexLimits{}, err
	}
	if err := encoder.Encode(map[string]any{"method": "account/rateLimits/read", "id": 2}); err != nil {
		return codexLimits{}, err
	}
	data, err := reply(2)
	if err != nil {
		return codexLimits{}, err
	}
	type rateLimits struct {
		Primary, Secondary   *codexWindow
		RateLimitReachedType string
		Credits              *struct{ HasCredits, Unlimited bool }
	}
	var result struct {
		RateLimits          rateLimits
		RateLimitsByLimitID map[string]rateLimits
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return codexLimits{}, errors.New("invalid Codex usage response")
	}
	limits, ok := result.RateLimitsByLimitID["codex"]
	if !ok {
		limits = result.RateLimits
	}
	if limits.RateLimitReachedType == "" && (limits.Primary == nil || limits.Primary.UsedPercent == nil) &&
		(limits.Secondary == nil || limits.Secondary.UsedPercent == nil) {
		return codexLimits{}, errors.New("Codex app-server provided no quota windows")
	}
	return codexLimits(limits), nil
}

func (p *poller) watchCodexUsage(ctx context.Context) {
	ticker := time.NewTicker(codexUsageInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refreshCodexUsage(ctx)
		}
	}
}

func (p *poller) refreshCodexUsage(ctx context.Context) {
	started := time.Now()
	limits, readErr := readCodexLimits(ctx, p.cfg.WorkDir)
	if ctx.Err() != nil {
		return
	}
	resumed, err := p.db.updateCodexUsage(ctx, started, limits, readErr)
	if err != nil {
		slog.Warn("could not save Codex usage status", "error", err)
		return
	}
	if resumed {
		slog.Info("Codex usage limit cleared; resuming queued workflows")
		p.requestPoll()
	}
}

func (s *state) updateCodexUsage(ctx context.Context, started time.Time, limits codexLimits, readErr error) (bool, error) {
	if readErr != nil {
		_, err := s.db.ExecContext(ctx, "UPDATE codex_usage SET refresh_failed = 1 WHERE id = 1")
		return false, err
	}
	windows, err := json.Marshal(limits)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var paused bool
	if err := tx.QueryRowContext(ctx, "SELECT paused FROM codex_usage WHERE id = 1").Scan(&paused); err != nil {
		return false, err
	}
	reached := limits.reached()
	var stillPaused bool
	// A reading that started before a worker hit its limit cannot resume it.
	err = tx.QueryRowContext(ctx, `
		UPDATE codex_usage SET windows = ?, checked_at = ?, refresh_failed = 0,
			paused = CASE WHEN detected_at >= ? THEN paused ELSE ? END
		WHERE id = 1 RETURNING paused`, string(windows), time.Now().Unix(), started.UnixNano(), reached).Scan(&stillPaused)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if !paused && stillPaused {
		slog.Warn("Codex quota exhausted; waiting for availability")
	}
	return paused && !stillPaused, nil
}

func (p *poller) requireCodex(ctx context.Context) error {
	var paused bool
	if err := p.db.db.QueryRowContext(ctx, "SELECT paused FROM codex_usage WHERE id = 1").Scan(&paused); err != nil {
		return err
	}
	if paused {
		return errCodexLimited
	}
	return nil
}

func (p *poller) deferCodexAttempt(ctx context.Context, phase, key string, attempt int) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := p.db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if attempt > 0 {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO codex_deferrals(phase, work_key, attempt) VALUES (?, ?, ?)`, phase, key, attempt)
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted == 0 {
			return tx.Commit()
		}
		if phase == "implementation attempt" {
			if _, err := tx.ExecContext(ctx, `UPDATE implementations SET attempt_limit = attempt_limit + 1
				WHERE issue_id || ':' || design_sha = ? AND attempts = ?`, key, attempt); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE codex_usage SET paused = 1, detected_at = ? WHERE id = 1", time.Now().UnixNano()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return errCodexLimited
}

// The CLI's stderr is distinct from model output. Keep only the recognized
// limit classification and bound the amount of partial text retained.
type codexErrorStream struct {
	line     []byte
	overflow bool
	limited  bool
}

func (d *codexErrorStream) Write(data []byte) (int, error) {
	for _, char := range data {
		if char == '\n' {
			d.limited = d.reached()
			d.line = d.line[:0]
			d.overflow = false
		} else if len(d.line) < 4096 {
			d.line = append(d.line, char)
		} else {
			d.overflow = true
		}
	}
	return len(data), nil
}

func (d *codexErrorStream) reached() bool {
	return d.limited || (!d.overflow && codexLimitMessage(strings.TrimSpace(string(d.line))))
}

func codexLimitMessage(message string) bool {
	message = strings.TrimPrefix(message, "ERROR: ")
	return strings.HasPrefix(message, "You’ve hit your usage limit.") ||
		strings.HasPrefix(message, "You've hit your usage limit.")
}

func codexEventLimited(data []byte) bool {
	var event struct {
		Type, Message string
		Error         struct{ Message string }
	}
	if json.Unmarshal(data, &event) != nil {
		return false
	}
	return (event.Type == "error" && codexLimitMessage(event.Message)) ||
		(event.Type == "turn.failed" && codexLimitMessage(event.Error.Message))
}

func codexExecError(dir, stage string, limited bool, err error) error {
	if limited {
		if err := os.WriteFile(filepath.Join(dir, codexLimitMarker), nil, 0600); err != nil {
			return fmt.Errorf("record Codex usage limit: %w", err)
		}
		return errCodexLimited
	}
	return fmt.Errorf("%s: %w", stage, err)
}

func codexAttemptLimited(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, codexLimitMarker))
	return err == nil
}
