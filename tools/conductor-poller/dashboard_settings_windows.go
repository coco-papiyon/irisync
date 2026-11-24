package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type dashboardSettings struct {
	Revision                   string
	DesignApproverIDs          string
	DashboardApproverLogin     string
	PollInterval               string
	AutoReady                  bool
	TestIssueNumbers           string
	DesignIssueNumbers         string
	ImplementationIssueNumbers string
}

var dashboardGitHubLogin = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,37}[a-zA-Z0-9])?$`)

func decodeDashboardRequest(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	if r.Header.Get("Origin") != "http://"+r.Host {
		dashboardError(w, http.StatusForbidden, "ダッシュボードから操作してください。")
		return false
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		dashboardError(w, http.StatusUnsupportedMediaType, "送信データの形式が正しくありません。")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(new(any)) != io.EOF {
		dashboardError(w, http.StatusBadRequest, "送信データを読み取れませんでした。")
		return false
	}
	return true
}

func settingsRevision(source []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(source))
}

func settingsNumberText[T int | int64](numbers []T) string {
	values := make([]string, len(numbers))
	for i, number := range numbers {
		values[i] = strconv.FormatInt(int64(number), 10)
	}
	return strings.Join(values, ", ")
}

func parseSettingsNumbers[T int | int64](field, text string) ([]T, error) {
	values := strings.Fields(strings.ReplaceAll(text, ",", " "))
	numbers := make([]T, 0, len(values))
	for _, value := range values {
		number, err := strconv.ParseInt(value, 10, 64)
		if err != nil || number < 1 || int64(T(number)) != number {
			return nil, fmt.Errorf("%s には正の整数を入力してください。", field)
		}
		numbers = append(numbers, T(number))
	}
	return numbers, nil
}

func (d dashboardSource) settings(w http.ResponseWriter, _ *http.Request) {
	source, err := os.ReadFile(d.configPath)
	if err != nil {
		dashboardError(w, http.StatusInternalServerError, "Poller の設定ファイルを読み取れませんでした。")
		return
	}
	cfg, err := decodeConfig(d.configPath, bytes.NewReader(source))
	if err != nil {
		dashboardError(w, http.StatusConflict, "設定ファイルの形式を確認してください。")
		return
	}
	dashboardJSON(w, struct {
		dashboardSettings
		FileName, DefaultApproverLogin string
	}{
		dashboardSettings: dashboardSettings{
			Revision: settingsRevision(source), DesignApproverIDs: settingsNumberText(cfg.DesignApproverIDs),
			DashboardApproverLogin: cfg.DashboardApproverLogin, PollInterval: cfg.PollInterval.String(),
			AutoReady: cfg.AutoReady, TestIssueNumbers: settingsNumberText(cfg.TestIssueNumbers),
			DesignIssueNumbers:         settingsNumberText(cfg.DesignIssueNumbers),
			ImplementationIssueNumbers: settingsNumberText(cfg.ImplementationIssueNumbers),
		},
		FileName: filepath.Base(d.configPath), DefaultApproverLogin: defaultDashboardApprover(cfg),
	})
}

func (d dashboardSource) saveSettings(w http.ResponseWriter, r *http.Request, mu *sync.Mutex) {
	var settings dashboardSettings
	if !decodeDashboardRequest(w, r, &settings, 32<<10) {
		return
	}

	mu.Lock()
	defer mu.Unlock()
	path, err := filepath.EvalSymlinks(d.configPath)
	if err != nil {
		dashboardError(w, http.StatusInternalServerError, "Poller の設定ファイルを開けませんでした。")
		return
	}
	current, err := os.ReadFile(path)
	if err != nil {
		dashboardError(w, http.StatusInternalServerError, "Poller の設定ファイルを読み取れませんでした。")
		return
	}
	if settings.Revision != settingsRevision(current) {
		dashboardError(w, http.StatusConflict, "設定が更新されています。設定画面を開き直してから保存してください。")
		return
	}
	previous, err := decodeConfig(d.configPath, bytes.NewReader(current))
	if err != nil {
		dashboardError(w, http.StatusConflict, "設定ファイルの形式を確認してください。")
		return
	}
	updated, err := applyDashboardSettings(current, previous, settings)
	if err != nil {
		dashboardError(w, http.StatusBadRequest, err.Error())
		return
	}
	if bytes.Equal(current, updated) {
		dashboardJSON(w, struct{ Message, Revision string }{"変更はありません。", settings.Revision})
		return
	}
	if _, err := decodeConfig(d.configPath, bytes.NewReader(updated)); err != nil {
		dashboardError(w, http.StatusBadRequest, "設定を保存できません。"+err.Error())
		return
	}
	if err := replaceDashboardConfig(path, updated); err != nil {
		dashboardError(w, http.StatusInternalServerError, "設定ファイルに保存できませんでした。書き込み権限を確認してください。")
		return
	}
	dashboardJSON(w, struct{ Message, Revision string }{
		"保存しました。ダッシュボードの承認設定に反映されました。稼働中の Poller に反映するには Poller を再起動してください。", settingsRevision(updated),
	})
}

func applyDashboardSettings(source []byte, previous config, settings dashboardSettings) ([]byte, error) {
	login := strings.TrimSpace(settings.DashboardApproverLogin)
	if login != "" && !dashboardGitHubLogin.MatchString(login) {
		return nil, fmt.Errorf("承認者の GitHub ログイン名を確認してください。")
	}
	ids, err := parseSettingsNumbers[int64]("design_approver_ids", settings.DesignApproverIDs)
	if err != nil {
		return nil, err
	}
	test, err := parseSettingsNumbers[int]("test_issue_numbers", settings.TestIssueNumbers)
	if err != nil {
		return nil, err
	}
	design, err := parseSettingsNumbers[int]("design_issue_numbers", settings.DesignIssueNumbers)
	if err != nil {
		return nil, err
	}
	implementation, err := parseSettingsNumbers[int]("implementation_issue_numbers", settings.ImplementationIssueNumbers)
	if err != nil {
		return nil, err
	}
	interval := strings.TrimSpace(settings.PollInterval)
	duration, err := time.ParseDuration(interval)
	if err != nil || duration <= 0 || duration > 24*time.Hour {
		return nil, fmt.Errorf("ポーリング間隔は 0 より長く 24h 以下にしてください。たとえば 5m と入力します。")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("設定ファイルの形式を確認してください。")
	}
	values := []struct {
		key     string
		value   any
		changed bool
	}{
		{"design_approver_ids", ids, settingsNumberText(ids) != settingsNumberText(previous.DesignApproverIDs)},
		{"dashboard_approver_login", login, login != previous.DashboardApproverLogin},
		{"poll_interval", interval, duration != previous.PollInterval},
		{"auto_ready", settings.AutoReady, settings.AutoReady != previous.AutoReady},
		{"test_issue_numbers", test, settingsNumberText(test) != settingsNumberText(previous.TestIssueNumbers)},
		{"design_issue_numbers", design, settingsNumberText(design) != settingsNumberText(previous.DesignIssueNumbers)},
		{"implementation_issue_numbers", implementation, settingsNumberText(implementation) != settingsNumberText(previous.ImplementationIssueNumbers)},
	}
	changed := false
	for _, entry := range values {
		if entry.changed {
			if err := updateDashboardSetting(document.Content[0], entry.key, entry.value); err != nil {
				return nil, err
			}
			changed = true
		}
	}
	if !changed {
		return source, nil
	}
	return yaml.Marshal(&document)
}

func updateDashboardSetting(mapping *yaml.Node, key string, value any) error {
	var replacement yaml.Node
	if err := replacement.Encode(value); err != nil {
		return err
	}
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		old := mapping.Content[i+1]
		replacement.HeadComment, replacement.LineComment, replacement.FootComment = old.HeadComment, old.LineComment, old.FootComment
		if old.Anchor != "" {
			return fmt.Errorf("%s は YAML アンカーで共有されているため、設定ファイルを直接編集してください。", key)
		}
		if old.Kind == replacement.Kind {
			replacement.Style = old.Style
		}
		mapping.Content[i+1] = &replacement
		return nil
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &replacement)
	return nil
}

func replaceDashboardConfig(path string, contents []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".poller-settings-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
