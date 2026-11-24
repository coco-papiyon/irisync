package irisync_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCLI(t *testing.T) {
	controller := buildCLI(t, "controller")
	agent := buildCLI(t, "agent")

	for _, binary := range []string{controller, agent} {
		t.Run(filepath.Base(binary), func(t *testing.T) {
			checkCLI(t, binary, 0, "")
			checkCLI(t, binary, 0, "Usage of", "--help")
			checkCLI(t, binary, 2, "flag provided but not defined", "--unknown")
			checkCLI(t, binary, 2, "unexpected positional arguments", "extra")
		})
	}

	t.Run("missing configuration", func(t *testing.T) {
		checkCLI(t, controller, 1, "configuration file not found", "--config", filepath.Join(t.TempDir(), "missing.yaml"))
	})
	t.Run("unreadable configuration", func(t *testing.T) {
		checkCLI(t, controller, 1, "could not read configuration file", "--config", t.TempDir())
	})

	for _, test := range []struct {
		name       string
		yaml       string
		diagnostic string
	}{
		{"configuration", "pairs: []\n", ""},
		{"empty mapping", "{}\n", ""},
		{"empty document", "", "expected one mapping"},
		{"syntax error", "fixture-value: [\n", "syntax error"},
		{"non-mapping", "- fixture-value\n", "expected one mapping"},
		{"multiple documents", "pairs: []\n---\nfixture-value: []\n", "multiple documents"},
		{"duplicate root key", "fixture-value: 1\n\"fixture-value\": 2\n", "duplicate key"},
		{"duplicate nested key", "pairs:\n  - fixture-value: 1\n    fixture-value: 2\n", "duplicate key"},
		{"duplicate alias key", "? &k fixture-value\n: 1\n? *k\n: 2\n", "duplicate key"},
		{"duplicate nested alias key", "anchor: &k fixture-value\nnested:\n  ? [*k, 1]\n  : first\n  ? [fixture-value, 1]\n  : second\n", "duplicate key"},
		{"duplicate sequence key", "nested:\n  ? [fixture-value, 1]\n  : first\n  ? [fixture-value, 1]\n  : second\n", "duplicate key"},
		{"distinct sequence keys", "nested:\n  ? [fixture-value, 1]\n  : first\n  ? [fixture-value, \"1\"]\n  : second\n", ""},
		{"reordered mapping keys", "? {a: 1, b: 2}\n: fixture-value\n? {b: 2, a: 1}\n: fixture-value\n", "duplicate key"},
		{"nested reordered mapping keys", "? [{a: 1, b: 2}]\n: fixture-value\n? [{b: 2, a: 1}]\n: fixture-value\n", "duplicate key"},
		{"different key value pairs", "? {a: 1, b: 2}\n: fixture-value\n? {b: 1, a: 2}\n: fixture-value\n", ""},
		{"different nested sequence order", "? {a: [1, 2]}\n: fixture-value\n? {a: [2, 1]}\n: fixture-value\n", ""},
		{"duplicate repeated alias chains", repeatedAliasKeys(), "duplicate key"},
		{"duplicate recursive alias key", "? &cycle [*cycle]\n: fixture-value\n? *cycle\n: fixture-value\n", "duplicate key"},
		{"distinct recursive keys", "? &left [*left]\n: fixture-value\n? &right [*right]\n: fixture-value\n", ""},
		{"wide mapping", wideMapping(), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(test.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			code := 0
			if test.diagnostic != "" {
				code = 1
			}
			checkCLI(t, controller, code, test.diagnostic, "--config", path)
		})
	}
}

func wideMapping() string {
	const keyCount = 20000
	var document strings.Builder
	for index := 0; index < keyCount; index++ {
		fmt.Fprintf(&document, "key-%d: fixture-value\n", index)
	}
	return document.String()
}

func repeatedAliasKeys() string {
	const depth = 34
	var document strings.Builder
	document.WriteString("anchors:\n")
	for _, prefix := range []string{"left", "right"} {
		fmt.Fprintf(&document, "  - &%s0 fixture-value\n", prefix)
		for level := 1; level <= depth; level++ {
			fmt.Fprintf(&document, "  - &%s%d [*%s%d, *%s%d]\n", prefix, level, prefix, level-1, prefix, level-1)
		}
	}
	fmt.Fprintf(&document, "nested:\n  ? [fixture-value, *left%d]\n  : first\n  ? [fixture-value, *right%d]\n  : second\n", depth, depth)
	return document.String()
}

func buildCLI(t *testing.T, name string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", binary, "./cmd/"+name)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: build %s: %v\n%s", runtime.GOOS, name, err, output)
	}
	return binary
}

func checkCLI(t *testing.T, binary string, wantCode int, diagnostic string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = t.TempDir()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s/%s exceeded the execution timeout", runtime.GOOS, filepath.Base(binary))
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run %s: %v", binary, err)
		}
		code = exit.ExitCode()
	}
	if code != wantCode {
		t.Errorf("exit code = %d, want %d", code, wantCode)
	}
	if stdout.Len() != 0 {
		t.Error("unexpected standard output")
	}
	if diagnostic == "" && stderr.Len() != 0 || !strings.Contains(stderr.String(), diagnostic) {
		t.Errorf("unexpected diagnostic, want %q", diagnostic)
	}
	if strings.Contains(stderr.String(), "fixture-value") {
		t.Error("diagnostic contains configuration content")
	}
}
