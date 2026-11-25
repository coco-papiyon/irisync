// Package scan inventories regular files beneath an open synchronization root.
package scan

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type File struct {
	Path    string
	Size    int64
	ModTime time.Time
	SHA256  [sha256.Size]byte
}

type IssueKind string

const (
	Excluded      IssueKind = "excluded"
	InvalidName   IssueKind = "invalid_name"
	NameCollision IssueKind = "name_collision"
	Unreadable    IssueKind = "unreadable"
	Changed       IssueKind = "changed"
	Unscanned     IssueKind = "unscanned"
)

// Issue also covers descendants of Path. An empty Path denotes the whole root.
// Consumers must not use paths covered by an issue as evidence of deletion.
type Issue struct {
	Path string
	Kind IssueKind
	Err  error
}

type Result struct {
	Files  []File
	Issues []Issue
}

type scanner struct {
	ctx    context.Context
	root   *os.File
	result Result
}

// Scan walks a root without following links or reading special files. It
// returns results for safe ranges even if another range is incomplete.
// A non-nil error indicates that the root could not be scanned or the scan
// was canceled. Paths covered by issues, or by a root error, are unconfirmed.
func Scan(ctx context.Context, rootPath string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{Issues: []Issue{{Kind: Unscanned, Err: err}}}, err
	}
	if rootPath == "" {
		return Result{}, errors.New("scan root is required")
	}
	absolute, err := filepath.Abs(rootPath)
	if err != nil {
		return Result{}, fmt.Errorf("resolve scan root: %w", err)
	}
	root, err := openRoot(absolute)
	if err != nil {
		return Result{}, fmt.Errorf("open scan root: %w", err)
	}
	defer root.Close()
	s := scanner{ctx: ctx, root: root}
	openCurrentRoot := func() (*os.File, error) { return openRoot(absolute) }
	if err := s.walk(root, "", openCurrentRoot); err != nil {
		s.record("", Unscanned, err)
		return s.result, err
	}
	return s.result, nil
}

func (s *scanner) record(relative string, kind IssueKind, err error) {
	s.result.Issues = append(s.result.Issues, Issue{Path: relative, Kind: kind, Err: err})
}

func (s *scanner) walk(directory *os.File, relative string, openCurrent func() (*os.File, error)) error {
	before, err := directory.Stat()
	if err != nil {
		return err
	}
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return err
	}
	slices.Sort(names)
	fileStart := len(s.result.Files)

	// Identify all siblings in a collision before reading any of them,
	// including file/directory collisions and collisions in excluded entries.
	collisions, err := collidingNames(names)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		child := name
		if relative != "" {
			child = relative + "/" + name
		}
		if err := ValidateRelativePath(name); err != nil {
			s.record(child, InvalidName, err)
			continue
		}
		if collisions[name] {
			s.record(child, NameCollision, nil)
			continue
		}
		if err := s.visit(child); err != nil {
			return err
		}
	}

	if err := unchanged(directory, before, openCurrent); err != nil {
		s.result.Files = s.result.Files[:fileStart]
		if relative == "" {
			return err
		}
		s.record(relative, Changed, err)
	}
	return s.ctx.Err()
}

func (s *scanner) visit(relative string) error {
	openCurrent := func() (*os.File, error) { return openEntry(s.root, relative) }
	file, err := openCurrent()
	if err != nil {
		switch {
		case errors.Is(err, errExcluded):
			s.record(relative, Excluded, err)
		case errors.Is(err, fs.ErrNotExist):
			s.record(relative, Changed, err)
		default:
			s.record(relative, Unreadable, err)
		}
		return nil
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		s.record(relative, Unreadable, err)
		return nil
	}
	if before.IsDir() {
		if err := s.walk(file, relative, openCurrent); err != nil {
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			s.record(relative, Unscanned, err)
		}
		return nil
	}
	if !before.Mode().IsRegular() {
		s.record(relative, Excluded, errExcluded)
		return nil
	}

	digest := sha256.New()
	size, err := io.Copy(digest, contextReader{ctx: s.ctx, file: file})
	if err != nil {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		s.record(relative, Unreadable, err)
		return nil
	}
	if size != before.Size() {
		s.record(relative, Changed, errors.New("file size changed during scan"))
		return nil
	}
	if err := unchanged(file, before, openCurrent); err != nil {
		s.record(relative, Changed, err)
		return nil
	}
	var hash [sha256.Size]byte
	digest.Sum(hash[:0])
	s.result.Files = append(s.result.Files, File{
		Path: relative, Size: size, ModTime: before.ModTime(), SHA256: hash,
	})
	return nil
}

func unchanged(file *os.File, before fs.FileInfo, openCurrent func() (*os.File, error)) error {
	after, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := openCurrent()
	if err != nil {
		return err
	}
	defer current.Close()
	named, err := current.Stat()
	if err != nil {
		return err
	}
	if !sameSnapshot(before, after) || !sameSnapshot(before, named) {
		return errors.New("entry changed during scan")
	}
	return nil
}

func sameSnapshot(before, after fs.FileInfo) bool {
	return os.SameFile(before, after) && before.Mode() == after.Mode() &&
		before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

type contextReader struct {
	ctx  context.Context
	file *os.File
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.Read(buffer)
}
