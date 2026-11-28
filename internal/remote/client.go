package remote

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coco-papiyon/irisync/internal/config"
	"github.com/coco-papiyon/irisync/internal/scan"
)

const negotiationTimeout = 30 * time.Second
const scanTimeout = 5 * time.Minute
const shutdownTimeout = 2 * time.Second

// Scan connects with OpenSSH and returns results only after the agent sends
// the completion frame. The caller can classify ErrDisconnected separately.
func Scan(ctx context.Context, connection config.SSH, root string) (scan.Result, error) {
	if root == "" || !utf8.ValidString(root) || strings.ContainsRune(root, 0) {
		return scan.Result{}, ErrScan
	}
	return scanCommand(ctx, root, "ssh", sshArgs(connection)...)
}

func sshArgs(connection config.SSH) []string {
	args := []string{"-T",
		"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no",
		"-o", "ControlPath=none", "-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no",
		"-o", "PermitLocalCommand=no", "-o", "RemoteCommand=none", "-o", "SessionType=default",
		"-o", "ForkAfterAuthentication=no", "-o", "StdinNull=no", "-o", "ConnectTimeout=15",
	}
	if !connection.UseSSHConfig {
		port := connection.Port
		if port == 0 {
			port = 22
		}
		args = append(args, "-F", "none", "-l", connection.User, "-p", strconv.Itoa(port))
		if connection.IdentityFile != "" {
			args = append(args, "-o", "IdentitiesOnly=yes", "-i", connection.IdentityFile)
		}
	}
	return append(args, "--", connection.Host, "irisync-agent --stdio")
}

func scanCommand(ctx context.Context, root, executable string, args ...string) (_ scan.Result, runErr error) {
	session, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(session, executable, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return scan.Result{}, ErrConnection
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return scan.Result{}, ErrConnection
	}
	defer out.Close()
	// OpenSSH may print user, host and key details. Stderr is separate
	// from the protocol and is discarded without a copying goroutine.
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return scan.Result{}, ctx.Err()
		}
		return scan.Result{}, ErrConnection
	}
	stopClosing := context.AfterFunc(session, func() {
		in.Close()
		out.Close()
	})
	defer stopClosing()
	defer func() {
		_ = in.Close()
		if runErr != nil {
			cancel()
		}
		timer := time.AfterFunc(shutdownTimeout, cancel)
		_ = cmd.Wait()
		timer.Stop()
	}()
	err = phase(session, cancel, negotiationTimeout, func() error {
		if err := writeMessage(in, hello()); err != nil {
			return ErrConnection
		}
		reply, _, err := readMessage(out)
		switch {
		case errors.Is(err, ErrProtocol):
			return err
		case err != nil:
			return ErrConnection
		case reply.Type == "failure" && reply.Code == "incompatible":
			return ErrIncompatible
		case reply.Type != "hello":
			return ErrProtocol
		case !compatible(reply):
			return ErrIncompatible
		default:
			return nil
		}
	})
	if err != nil {
		return scan.Result{}, fmt.Errorf("negotiate with agent: %w", err)
	}
	var result scan.Result
	err = phase(session, cancel, scanTimeout, func() error {
		if err := writeMessage(in, message{Type: "scan", Root: root}); err != nil {
			if errors.Is(err, ErrProtocol) {
				return err
			}
			return ErrDisconnected
		}
		var err error
		result, err = receiveScan(out)
		return err
	})
	if err != nil {
		return scan.Result{}, fmt.Errorf("scan remote root: %w", err)
	}
	return result, nil
}

func phase(ctx context.Context, stopSession context.CancelFunc, timeout time.Duration, work func() error) error {
	limited, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(limited, stopSession)
	err := work()
	stop()
	if limited.Err() != nil {
		return limited.Err()
	}
	return err
}

func receiveScan(in io.Reader) (scan.Result, error) {
	result := scan.Result{Files: []scan.File{}, Issues: []scan.Issue{}}
	seen := make(map[string]bool)
	var responseBytes int
	for {
		msg, size, err := readMessage(in)
		if errors.Is(err, ErrProtocol) {
			return scan.Result{}, err
		}
		if err != nil {
			return scan.Result{}, ErrDisconnected
		}
		responseBytes += size
		if responseBytes > maxScanBytes {
			return scan.Result{}, ErrScanTooLarge
		}
		switch msg.Type {
		case "file":
			file := msg.File
			if file == nil || file.Size == nil || *file.Size < 0 || file.ModTime == nil ||
				len(file.SHA256) != sha256.Size || scan.ValidateRelativePath(file.Path) != nil || seen[file.Path] {
				return scan.Result{}, ErrProtocol
			}
			seen[file.Path] = true
			result.Files = append(result.Files, scan.File{
				Path: file.Path, Size: *file.Size, ModTime: *file.ModTime, SHA256: [sha256.Size]byte(file.SHA256),
			})
		case "issue":
			if msg.Issue == nil || !validIssue(*msg.Issue) {
				return scan.Result{}, ErrProtocol
			}
			result.Issues = append(result.Issues, scan.Issue{Path: string(msg.Issue.Path), Kind: msg.Issue.Kind})
		case "complete":
			return result, nil
		case "failure":
			if msg.Code == "scan_failed" {
				return scan.Result{}, ErrScan
			}
			return scan.Result{}, ErrProtocol
		default:
			return scan.Result{}, ErrProtocol
		}
	}
}

func validIssue(issue scanIssue) bool {
	switch issue.Kind {
	case scan.InvalidName, scan.NameCollision, scan.Excluded, scan.Unreadable, scan.Changed, scan.Unscanned:
	default:
		return false
	}
	if len(issue.Path) == 0 {
		return issue.Kind == scan.Unscanned
	}
	for _, part := range strings.Split(string(issue.Path), "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return false
		}
	}
	return true
}
