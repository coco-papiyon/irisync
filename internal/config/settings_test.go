package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func localFixture(tail string) string {
	if runtime.GOOS == "windows" {
		return "C:/data/" + tail
	}
	return "/data/" + tail
}

func pairFixture(id, local, remote, ssh, extra string) string {
	return fmt.Sprintf("  - id: %s\n    local: {path: %q}\n    remote:\n      path: %q\n      ssh: %s\n%s", id, local, remote, ssh, extra)
}

func decodeFixture(t *testing.T, source string, environment map[string]string) (*Config, error) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filename, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := readMapping(filename)
	if err != nil {
		return nil, err
	}
	return decodeConfig(root, func(key string) (string, bool) {
		value, ok := environment[key]
		return value, ok
	})
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := decodeFixture(t, "{}", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantSync := Sync{
		MinDelay: 60 * time.Second, IdleDelay: 5 * time.Second, MaxDelay: 120 * time.Second,
		MaxFileSize: 1 << 30, FullScanInterval: 30 * time.Minute, RetryInterval: 5 * time.Second,
	}
	if cfg.Sync != wantSync || cfg.HTTPListen != "127.0.0.1:8080" ||
		cfg.Performance != (Performance{MaxConcurrentPairs: 5, MaxTransfersPerPair: 2}) || len(cfg.Pairs) != 0 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestConfigPrecedence(t *testing.T) {
	source := "server: {http: {listen: '127.0.0.1:8070'}}\nsync: {max_file_size: 2GiB}\n" +
		"performance: {max_concurrent_pairs: 3, max_transfers_per_pair: 4}\npairs:\n" +
		pairFixture("alpha", localFixture("a"), "/data/shared", "{host: alias-a}", "    startup: start\n    exclude: ['*.tmp', '.git/**']\n") +
		pairFixture("beta", localFixture("b"), "/data/shared", "{host: example.test, use_ssh_config: false, user: sync}", "    startup: stop\n    sync: {max_file_size: 500MiB}\n    conflict: {priority: remote}\n") +
		pairFixture("gamma", localFixture("c"), "D:/shared", "{host: alias-b}", "    startup: restore\n    sync: {max_file_size: 0}\n") +
		pairFixture("delta", localFixture("d"), "/data/other", "{host: alias-c}", "")
	env := map[string]string{
		"IRISYNC_HTTP_LISTEN":          "[::1]:8081",
		"IRISYNC_MAX_FILE_SIZE":        "3GiB",
		"IRISYNC_FULL_SCAN_INTERVAL":   "15m",
		"IRISYNC_MAX_CONCURRENT_PAIRS": "6",
	}
	cfg, err := decodeFixture(t, source, env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sync.MaxFileSize != 3<<30 || cfg.Sync.FullScanInterval != 15*time.Minute ||
		cfg.HTTPListen != "[::1]:8081" || cfg.Performance != (Performance{6, 4}) {
		t.Fatalf("unexpected effective global settings: %+v", cfg)
	}
	for i, want := range []struct {
		startup Startup
		size    int64
	}{{Start, 3 << 30}, {Stop, 500 << 20}, {Restore, 0}, {Restore, 3 << 30}} {
		if pair := cfg.Pairs[i]; pair.Startup != want.startup || pair.MaxFileSize != want.size {
			t.Errorf("pair %d: startup %q, max file size %d", i, pair.Startup, pair.MaxFileSize)
		}
	}
	alpha, beta := cfg.Pairs[0], cfg.Pairs[1]
	if !alpha.SSH.UseSSHConfig || alpha.SSH.Port != 0 || alpha.ConflictPriority != PreferLocal ||
		strings.Join(alpha.Exclude, ",") != "*.tmp,.git/**" || beta.SSH.Port != 22 || beta.SSH.User != "sync" ||
		beta.SSH.UseSSHConfig || beta.ConflictPriority != PreferRemote {
		t.Fatalf("unexpected pair settings: %+v %+v", alpha, beta)
	}

	cfg, err = decodeFixture(t, source, nil)
	if err != nil || cfg.Pairs[0].MaxFileSize != 2<<30 || cfg.Performance.MaxConcurrentPairs != 3 {
		t.Fatalf("YAML alone: cfg = %+v, error = %v", cfg, err)
	}
	cfg, err = decodeFixture(t, source, map[string]string{"IRISYNC_MAX_FILE_SIZE": "0"})
	if err != nil || cfg.Pairs[0].MaxFileSize != 0 || cfg.Pairs[1].MaxFileSize != 500<<20 {
		t.Fatalf("unlimited global setting: cfg = %+v, error = %v", cfg, err)
	}
}

func TestInvalidConfigReturnsNoPartialSettings(t *testing.T) {
	first := "pairs:\n" + pairFixture("first", localFixture("a"), "/remote/a", "{host: host-one}", "")
	second := pairFixture("second", localFixture("b"), "/remote/b", "{host: host-two}", "")
	for _, tc := range []struct {
		name, source, field string
		env                 map[string]string
	}{
		{name: "document type", source: "[]", field: "expected one mapping"},
		{name: "invalid YAML", source: "pairs: [", field: "syntax error"},
		{name: "null pairs", source: "pairs: null", field: "pairs"},
		{name: "invalid known YAML key type", source: "!!int pairs: [null]", field: "pairs: expected a string key"},
		{name: "null pair", source: first + "  - null\n", field: "pairs[1]"},
		{name: "type", source: first + strings.Replace(second, "second", "123", 1), field: "pairs[1].id"},
		{name: "required", source: first + "  - {}\n", field: "pairs[1].id"},
		{name: "missing local path", source: first + strings.Replace(second, fmt.Sprintf("{path: %q}", localFixture("b")), "{}", 1), field: "pairs[1].local.path"},
		{name: "unknown startup", source: first + second + "    startup: fixture-value\n", field: "pairs[1].startup"},
		{name: "empty startup", source: first + second + "    startup: \"\"\n", field: "pairs[1].startup"},
		{name: "null startup", source: first + second + "    startup: null\n", field: "pairs[1].startup"},
		{name: "invalid glob", source: first + second + "    exclude: ['fixture-value[']\n", field: "pairs[1].exclude[0]"},
		{name: "non-string glob", source: first + second + "    exclude: [true]\n", field: "pairs[1].exclude[0]"},
		{name: "relative local", source: first + pairFixture("second", "relative/a", "/remote/a", "{host: host-two}", ""), field: "pairs[1].local.path"},
		{name: "relative remote", source: first + pairFixture("second", localFixture("b"), "D:relative", "{host: host-two}", ""), field: "pairs[1].remote.path"},
		{name: "invalid SSH host", source: first + strings.Replace(second, "host-two", "-fixture-value", 1), field: "pairs[1].remote.ssh.host"},
		{name: "invalid direct SSH host label", source: first + strings.Replace(second, "host-two", "fixture-value..example.test, use_ssh_config: false, user: sync", 1), field: "pairs[1].remote.ssh.host"},
		{name: "invalid HTTP host", source: "server: {http: {listen: 'fixture-value/host:8080'}}", field: "server.http.listen"},
		{name: "invalid HTTP host in env", source: first, field: "IRISYNC_HTTP_LISTEN", env: map[string]string{"IRISYNC_HTTP_LISTEN": "fixture-value/host:8080"}},
		{name: "alias with overrides", source: first + strings.Replace(second, "host-two", "host-two, user: fixture-value", 1), field: "pairs[1].remote.ssh.user"},
		{name: "direct requires user", source: first + strings.Replace(second, "host-two", "host-two, use_ssh_config: false", 1), field: "pairs[1].remote.ssh.user"},
		{name: "port range", source: first + strings.Replace(second, "host-two", "host-two, use_ssh_config: false, user: sync, port: 65536", 1), field: "pairs[1].remote.ssh.port"},
		{name: "strict boolean", source: first + strings.Replace(second, "host-two", "host-two, use_ssh_config: 'true'", 1), field: "pairs[1].remote.ssh.use_ssh_config"},
		{name: "invalid explicit boolean", source: first + strings.Replace(second, "host-two", "host-two, use_ssh_config: !!bool fixture-value", 1), field: "pairs[1].remote.ssh.use_ssh_config"},
		{name: "duplicate ID", source: first + strings.Replace(second, "second", "first", 1), field: "pairs[1].id duplicates pairs[0].id"},
		{name: "unknown key redaction", source: first + second + "    fixture-value: data\n", field: "pairs[1]: unknown key"},
		{name: "scalar alias type", source: "anchor: &value 123\n" + first + second + "    startup: *value\n", field: "pairs[1].startup"},
		{name: "invalid size", source: "sync: {max_file_size: -1}\n", field: "sync.max_file_size"},
		{name: "size overflow", source: "sync: {max_file_size: 8388608TiB}\n", field: "sync.max_file_size"},
		{name: "null YAML with env", source: "sync: {max_file_size: null}\n", field: "sync.max_file_size", env: map[string]string{"IRISYNC_MAX_FILE_SIZE": "0"}},
		{name: "invalid interval", source: "sync: {full_scan_interval: 30}\n", field: "sync.full_scan_interval"},
		{name: "delay range", source: "sync: {min_delay: 3m}\n", field: "sync.min_delay"},
		{name: "explicit empty env", source: first, field: "IRISYNC_MAX_FILE_SIZE", env: map[string]string{"IRISYNC_MAX_FILE_SIZE": ""}},
		{name: "invalid env", source: first, field: "IRISYNC_MAX_CONCURRENT_PAIRS", env: map[string]string{"IRISYNC_MAX_CONCURRENT_PAIRS": "fixture-value"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := decodeFixture(t, tc.source, tc.env)
			if cfg != nil || err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("want no configuration and diagnostic for %q, got cfg = %v, error = %v", tc.field, cfg, err)
			}
			if strings.Contains(err.Error(), "fixture-value") {
				t.Fatal("diagnostic contains configuration content")
			}
		})
	}
}

func TestPairRootOverlaps(t *testing.T) {
	const sameHost = "{host: alias-a}"
	const otherHost = "{host: alias-b}"
	const directA = "{host: '2001:db8::1', use_ssh_config: false, user: sync}"
	const directB = "{host: '2001:0DB8::1', use_ssh_config: false, user: other, port: 22}"
	const directOtherPort = "{host: '2001:0DB8::1', use_ssh_config: false, user: sync, port: 2222}"
	for _, tc := range []struct {
		name, localA, localB, remoteA, remoteB, sshA, sshB, field string
	}{
		{"same local across destinations", "a", "a", "/x", "/y", sameHost, otherHost, "local.path"},
		{"normalized local", "a", "a//./", "/x", "/y", sameHost, sameHost, "local.path"},
		{"nested local", "a", "a/b", "/x", "/y", sameHost, sameHost, "local.path"},
		{"component boundary", "a", "ab", "/data/a", "/data/ab", sameHost, sameHost, ""},
		{"local Kelvin sign is distinct", "a/K", "a/K", "/x", "/y", sameHost, sameHost, ""},
		{"same POSIX root", "a", "b", "/data/a", "/data/a", sameHost, sameHost, "remote.path"},
		{"nested POSIX root", "a", "b", "/data/a", "/data//a/./b", sameHost, "{host: ALIAS-A}", "remote.path"},
		{"POSIX double leading slash", "a", "b", "/data/a", "//data/a/b", sameHost, sameHost, "remote.path"},
		{"POSIX double leading slash with case difference", "a", "b", "//srv/Data", "//srv/data", sameHost, sameHost, ""},
		{"POSIX case", "a", "b", "/data/A", "/data/a", sameHost, sameHost, ""},
		{"different destinations", "a", "b", "/data/a", "/data/a", sameHost, otherHost, ""},
		{"same direct destination", "a", "b", "/data/a", "/data/a", directA, directB, "remote.path"},
		{"different direct ports", "a", "b", "/data/a", "/data/a", directA, directOtherPort, ""},
		{"Windows drive", "a", "b", "D:/DATA/a", "d:\\data\\a\\b\\", sameHost, sameHost, "remote.path"},
		{"Windows Unicode case pair", "a", "b", "D:/Ä/data", "d:/ä/data/child", sameHost, sameHost, "remote.path"},
		{"Windows Kelvin sign is distinct", "a", "b", "D:/data/K", "d:/data/K", sameHost, sameHost, ""},
		{"Windows long s is distinct", "a", "b", "D:/data/S", "d:/data/ſ", sameHost, sameHost, ""},
		{"Windows supplementary case pair is distinct", "a", "b", "D:/data/\U00010400", "d:/data/\U00010428", sameHost, sameHost, ""},
		{"different Windows drives", "a", "b", "D:/data/a", "E:/data/a", sameHost, sameHost, ""},
		{"Windows UNC", "a", "b", "\\\\server\\share\\data", "\\\\SERVER\\Share/data/a", sameHost, sameHost, "remote.path"},
		{"Windows UNC Unicode shares are distinct", "a", "b", "\\\\server\\share-K\\data", "\\\\SERVER\\share-K/data", sameHost, sameHost, ""},
		{"Windows UNC with forward slashes", "a", "b", "\\\\server\\share\\data", "//SERVER/Share/data/a", sameHost, sameHost, "remote.path"},
		{"Windows UNC starts with slash and backslash", "a", "b", "\\\\server\\share\\data", "/\\server/share/data/child", sameHost, sameHost, "remote.path"},
		{"Windows UNC starts with backslash and slash", "a", "b", "\\\\server\\share\\data", "\\/server/share/data/child", sameHost, sameHost, "remote.path"},
		{"Windows UNC at distinct destinations", "a", "b", "\\\\server\\share\\data", "//SERVER/Share/data/a", sameHost, otherHost, ""},
		{"different UNC shares", "a", "b", "\\\\server\\share\\data", "\\\\SERVER\\other/data", sameHost, sameHost, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := pairFixture("first", localFixture(tc.localA), tc.remoteA, tc.sshA, "")
			b := pairFixture("second", localFixture(tc.localB), tc.remoteB, tc.sshB, "")
			for _, source := range []string{"pairs:\n" + a + b, "pairs:\n" + b + a} {
				cfg, err := decodeFixture(t, source, nil)
				if tc.field == "" {
					if err != nil || len(cfg.Pairs) != 2 {
						t.Fatalf("want two valid pairs, got %v, error = %v", cfg, err)
					}
				} else if cfg != nil || err == nil || !strings.Contains(err.Error(), "pairs[1]."+tc.field+" overlaps pairs[0]."+tc.field) {
					t.Fatalf("want overlap for %s, got %v, error = %v", tc.field, cfg, err)
				}
			}
		})
	}
}

func TestForwardSlashRemoteRootsUseWindowsSyntaxForTheirDestination(t *testing.T) {
	ambiguous := pairFixture("first", localFixture("a"), "//srv/Data", "{host: shared}", "") +
		pairFixture("second", localFixture("b"), "//srv/data", "{host: shared}", "")
	for _, tc := range []struct {
		host        string
		wantOverlap bool
	}{
		{"shared", true},
		{"other", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			hint := pairFixture("third", localFixture("c"), "D:/other", "{host: "+tc.host+"}", "")
			cfg, err := decodeFixture(t, "pairs:\n"+ambiguous+hint, nil)
			if tc.wantOverlap {
				if cfg != nil || err == nil || !strings.Contains(err.Error(), "pairs[1].remote.path overlaps pairs[0].remote.path") {
					t.Fatalf("want UNC overlap, got cfg = %v, error = %v", cfg, err)
				}
			} else if err != nil || len(cfg.Pairs) != 3 {
				t.Fatalf("want distinct POSIX roots, got cfg = %v, error = %v", cfg, err)
			}
		})
	}
}

func TestWindowsDestinationRequiresFullyQualifiedRoots(t *testing.T) {
	for _, windowsRoot := range []string{"C:/fixture-sync", "\\\\server\\share\\fixture-sync"} {
		t.Run(windowsRoot, func(t *testing.T) {
			windowsPair := pairFixture("first", localFixture("a"), windowsRoot, "{host: same-host}", "")
			driveRelativePair := pairFixture("second", localFixture("b"), "/fixture-sync/child", "{host: SAME-HOST}", "")
			for _, tc := range []struct {
				pairs string
				index int
			}{
				{windowsPair + driveRelativePair, 1},
				{driveRelativePair + windowsPair, 0},
			} {
				cfg, err := decodeFixture(t, "pairs:\n"+tc.pairs, nil)
				want := fmt.Sprintf("pairs[%d].remote.path: expected an absolute Windows path with a drive or UNC share", tc.index)
				if cfg != nil || err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("want %q, got cfg = %v, error = %v", want, cfg, err)
				}
			}
		})
	}
}

func TestRemoteRootDeviceNames(t *testing.T) {
	for _, tc := range []struct {
		path   string
		reject bool
	}{
		{"D:/fixture/NUL", true},
		{"D:/fixture/nUl.txt/child", true},
		{"D:/fixture/AUX .txt", true},
		{"D:/fixture/COM¹", true},
		{"\\\\server\\share\\fixture\\LPT9", true},
		{"D:/fixture/COM10", false},
		{"D:/fixture/console", false},
		{"/fixture/NUL", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			source := "pairs:\n" + pairFixture("first", localFixture("a"), tc.path, "{host: backup}", "")
			cfg, err := decodeFixture(t, source, nil)
			if tc.reject {
				if cfg != nil || err == nil || !strings.Contains(err.Error(), "pairs[0].remote.path") {
					t.Fatalf("want a path error, got cfg = %v, error = %v", cfg, err)
				}
			} else if err != nil || len(cfg.Pairs) != 1 {
				t.Fatalf("want a valid pair, got cfg = %v, error = %v", cfg, err)
			}
		})
	}
}

func TestLocalRootUsesControllerCaseRules(t *testing.T) {
	source := "pairs:\n" +
		pairFixture("first", localFixture("DATA"), "/shared", "{host: host-a}", "") +
		pairFixture("second", localFixture("data/child"), "/shared", "{host: host-b}", "")
	cfg, err := decodeFixture(t, source, nil)
	if runtime.GOOS == "windows" {
		if cfg != nil || err == nil || !strings.Contains(err.Error(), "local.path overlaps") {
			t.Fatalf("Windows case-insensitive comparison: cfg = %v, error = %v", cfg, err)
		}
	} else if err != nil || len(cfg.Pairs) != 2 {
		t.Fatalf("POSIX case-sensitive comparison: cfg = %v, error = %v", cfg, err)
	}
}

func TestLoadUsesEnvironment(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filename, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"IRISYNC_HTTP_LISTEN": "127.0.0.1:8080", "IRISYNC_MAX_FILE_SIZE": "0",
		"IRISYNC_FULL_SCAN_INTERVAL": "30m", "IRISYNC_MAX_CONCURRENT_PAIRS": "5",
	} {
		t.Setenv(key, value)
	}
	cfg, err := Load(filename)
	if err != nil || cfg.Sync.MaxFileSize != 0 {
		t.Fatalf("Load: cfg = %+v, error = %v", cfg, err)
	}
}

func TestYAMLAliases(t *testing.T) {
	cfg, err := decodeFixture(t, "startup: &policy start\npairs:\n"+
		pairFixture("alpha", localFixture("a"), "/shared", "{host: alias-a}", "    startup: *policy\n"), nil)
	if err != nil || cfg.Pairs[0].Startup != Start {
		t.Fatalf("aliased startup: cfg = %+v, error = %v", cfg, err)
	}
}
