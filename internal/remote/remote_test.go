package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/coco-papiyon/irisync/internal/config"
	"github.com/coco-papiyon/irisync/internal/scan"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--remote-fixture" {
		if fixture(os.Args[2]) != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fixture(mode string) error {
	if mode == "agent" {
		_, _ = os.Stderr.WriteString("fixture: separated diagnostic\n")
		return Serve(context.Background(), os.Stdin, os.Stdout, os.Stderr)
	}
	if mode == "startup-error" {
		_, _ = os.Stderr.WriteString("sensitive fixture stderr\n")
		return ErrConnection
	}
	if mode == "silent" {
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	}
	if _, _, err := readMessage(os.Stdin); err != nil {
		return err
	}
	reply := hello()
	switch mode {
	case "version":
		reply.Version++
	case "feature":
		reply.Features = nil
	}
	if err := writeMessage(os.Stdout, reply); err != nil {
		return err
	}
	if mode != "partial" {
		return nil
	}
	request, _, err := readMessage(os.Stdin)
	if err != nil {
		return err
	}
	if request.Type != "scan" {
		return ErrProtocol
	}
	size := int64(0)
	now := time.Now()
	return writeMessage(os.Stdout, message{Type: "file", File: &scanFile{
		Path: "partial.txt", Size: &size, ModTime: &now, SHA256: make([]byte, sha256.Size),
	}})
}

func TestAgentSubprocessScansPortablePaths(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "日本語 空白", "a+b.txt")
	if err := os.Mkdir(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("first scan")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result, err := scanCommand(context.Background(), root, exe, "--remote-fixture", "agent")
	if err != nil || len(result.Files) != 1 || len(result.Issues) != 0 {
		t.Fatalf("first scan failed: result=%+v err=%v", result, err)
	}
	got := result.Files[0]
	if got.Path != "日本語 空白/a+b.txt" || got.Size != int64(len(data)) || got.SHA256 != sha256.Sum256(data) {
		t.Fatalf("unexpected file: %+v", got)
	}
	empty, err := scanCommand(context.Background(), t.TempDir(), exe, "--remote-fixture", "agent")
	if err != nil || len(empty.Files) != 0 || len(empty.Issues) != 0 {
		t.Fatalf("empty scan: result=%+v err=%v", empty, err)
	}
	if _, err := scanCommand(context.Background(), filepath.Join(root, "missing"), exe, "--remote-fixture", "agent"); !errors.Is(err, ErrScan) {
		t.Fatalf("missing root error: %v", err)
	}
}

func TestScanDiscardsResultsOnProtocolFailure(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode string
		want error
	}{
		{"startup-error", ErrConnection}, {"version", ErrIncompatible},
		{"feature", ErrIncompatible}, {"partial", ErrDisconnected},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			result, err := scanCommand(context.Background(), t.TempDir(), exe, "--remote-fixture", tc.mode)
			if !errors.Is(err, tc.want) || len(result.Files) != 0 || len(result.Issues) != 0 {
				t.Fatalf("result=%+v err=%v, want empty result and %v", result, err, tc.want)
			}
			if tc.mode == "startup-error" && err.Error() != "negotiate with agent: "+ErrConnection.Error() {
				t.Fatalf("unexpected startup diagnostic: %v", err)
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := scanCommand(ctx, t.TempDir(), exe, "--remote-fixture", "silent"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("negotiation did not honor cancellation: %v", err)
	}
}

func TestAgentCancelsScanOnInputEOF(t *testing.T) {
	in, request := io.Pipe()
	response, out := io.Pipe()
	defer request.Close()
	defer response.Close()
	started := make(chan struct{})
	agentDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		agentDone <- serve(ctx, in, out, io.Discard, func(scanCtx context.Context, _ string) (scan.Result, error) {
			close(started)
			<-scanCtx.Done()
			return scan.Result{}, scanCtx.Err()
		})
	}()
	if err := writeMessage(request, hello()); err != nil {
		t.Fatal(err)
	}
	if reply, _, err := readMessage(response); err != nil || reply.Type != "hello" {
		t.Fatalf("hello failed: %+v %v", reply, err)
	}
	if err := writeMessage(request, message{Type: "scan", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("agent did not start scanning")
	}
	request.Close()
	select {
	case err := <-agentDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("agent did not exit after stdin closed")
	}
}

func TestSSHArgumentsRequireTrustAndUseFixedCommand(t *testing.T) {
	for _, cfg := range []config.SSH{
		{Host: "backup", UseSSHConfig: true},
		{Host: "::1", User: "sync", Port: 2222, IdentityFile: "key with spaces"},
	} {
		args := sshArgs(cfg)
		if !slices.Equal(args[len(args)-3:], []string{"--", cfg.Host, "irisync-agent --stdio"}) {
			t.Fatalf("unexpected remote command: %q", args)
		}
		for _, option := range []string{"StrictHostKeyChecking=yes", "BatchMode=yes", "ClearAllForwardings=yes", "ControlPath=none"} {
			if !slices.Contains(args, option) {
				t.Fatalf("missing SSH policy %q", option)
			}
		}
		if cfg.UseSSHConfig {
			if slices.Contains(args, "-F") || slices.Contains(args, "-p") {
				t.Fatalf("overrode alias configuration: %q", args)
			}
		} else if !slices.Contains(args, "none") || !slices.Contains(args, "2222") ||
			!slices.Contains(args, "sync") || !slices.Contains(args, cfg.IdentityFile) {
			t.Fatalf("lost direct connection options: %q", args)
		}
	}
}

func TestOversizedFrameAndInvalidFileAreRejected(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], maxFrame+1)
	if _, _, err := readMessage(bytes.NewReader(header[:])); !errors.Is(err, ErrProtocol) {
		t.Fatalf("oversized frame error: %v", err)
	}
	var wire bytes.Buffer
	size, now := int64(0), time.Now()
	if err := writeMessage(&wire, message{Type: "file", File: &scanFile{
		Path: "../file", Size: &size, ModTime: &now, SHA256: make([]byte, sha256.Size),
	}}); err != nil {
		t.Fatal(err)
	}
	if result, err := receiveScan(&wire); !errors.Is(err, ErrProtocol) || len(result.Files) != 0 {
		t.Fatalf("path outside root: result=%+v err=%v", result, err)
	}
}
