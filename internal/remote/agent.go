package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/coco-papiyon/irisync/internal/scan"
)

// Serve runs the agent protocol until the controller closes its input.
func Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser, diagnostics io.Writer) error {
	return serve(ctx, in, out, diagnostics, scan.Scan)
}

func serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser, diagnostics io.Writer,
	walk func(context.Context, string) (scan.Result, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	stopClosing := context.AfterFunc(ctx, func() {
		in.Close()
		out.Close()
	})
	defer stopClosing()
	requests := make(chan message, 1)
	readerDone := make(chan struct{})
	var inputErr error
	go func() {
		defer close(readerDone)
		defer close(requests)
		defer cancel()
		for {
			msg, _, err := readMessage(in)
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					inputErr = ErrProtocol
				}
				return
			}
			select {
			case requests <- msg:
			case <-ctx.Done():
				return
			default:
				inputErr = ErrProtocol
				return
			}
		}
	}()
	defer func() {
		cancel()
		in.Close()
		out.Close()
		<-readerDone
	}()
	initial, ok := <-requests
	if !ok {
		<-readerDone
		return inputErr
	}
	if initial.Type != "hello" {
		return ErrProtocol
	}
	if !compatible(initial) {
		return writeMessage(out, message{Type: "failure", Code: "incompatible"})
	}
	if err := writeMessage(out, hello()); err != nil {
		return err
	}
	for request := range requests {
		if ctx.Err() != nil {
			break
		}
		if request.Type != "scan" {
			return ErrProtocol
		}
		var result scan.Result
		err := ErrScan
		if filepath.IsAbs(request.Root) {
			result, err = walk(ctx, request.Root)
		}
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			fmt.Fprintln(diagnostics, "agent: scan failed")
			if err := writeMessage(out, message{Type: "failure", Code: "scan_failed"}); err != nil {
				return err
			}
			continue
		}
		if err := sendScan(ctx, out, result); err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
	}
	<-readerDone
	return inputErr
}

func sendScan(ctx context.Context, out io.Writer, result scan.Result) error {
	for _, file := range result.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		wire := scanFile{Path: file.Path, Size: &file.Size, ModTime: &file.ModTime, SHA256: file.SHA256[:]}
		if err := writeMessage(out, message{Type: "file", File: &wire}); err != nil {
			return err
		}
	}
	for _, issue := range result.Issues {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeMessage(out, message{Type: "issue", Issue: &scanIssue{Path: []byte(issue.Path), Kind: issue.Kind}}); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeMessage(out, message{Type: "complete"})
}
