package scan

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRelativePath(t *testing.T) {
	for _, name := range []string{"report.txt", "資料（共有）/報告 [Q1] & 集計", "dir/a+b", ".config"} {
		if err := ValidateRelativePath(name); err != nil {
			t.Errorf("rejects %q: %v", name, err)
		}
	}
	for _, name := range []string{
		"", ".", "..", "dir/../outside", "dir/./file", "/absolute", "dir//file", "dir/",
		"C:/absolute", "C:relative", "\\\\host\\share", "dir\\file", "dir/file:stream",
		"NUL.txt", "dir/cOm¹.txt", "dir/end.", "dir/end ", "dir/?.txt", "dir/\x00", "dir/\xff",
	} {
		if err := ValidateRelativePath(name); err == nil {
			t.Errorf("accepts invalid path %q", name)
		}
	}
}

func TestScanMetadata(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "資料/[Q1] 報告 & 集計.txt", "sample content")
	writeFile(t, root, "empty", "")
	stamp := time.Unix(1_700_000_000, 123_456_700)
	physical := filepath.Join(root, "資料", "[Q1] 報告 & 集計.txt")
	if err := os.Chtimes(physical, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	got, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Issues) != 0 || len(got.Files) != 2 {
		t.Fatalf("unexpected scan result: %+v", got)
	}
	want := map[string]string{"資料/[Q1] 報告 & 集計.txt": "sample content", "empty": ""}
	for _, file := range got.Files {
		content, exists := want[file.Path]
		if !exists || file.Size != int64(len(content)) || file.SHA256 != sha256.Sum256([]byte(content)) {
			t.Errorf("invalid metadata for %q: %+v", file.Path, file)
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if !file.ModTime.Equal(info.ModTime()) {
			t.Errorf("mod time for %q = %v, want %v", file.Path, file.ModTime, info.ModTime())
		}
	}
}

func TestMissingRootAndCanceledScan(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"", filepath.Join(root, "missing")} {
		if got, err := Scan(context.Background(), path); err == nil || len(got.Files) != 0 {
			t.Fatalf("invalid root %q: got %+v, error %v", path, got, err)
		}
	}
	parentComponent := root + "/child/.."
	if got, err := Scan(context.Background(), parentComponent); err == nil || len(got.Files) != 0 {
		t.Fatalf("parent component was accepted: got %+v, error %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := Scan(ctx, root)
	if !errors.Is(err, context.Canceled) || len(got.Files) != 0 ||
		len(got.Issues) != 1 || got.Issues[0].Path != "" || got.Issues[0].Kind != Unscanned {
		t.Fatalf("canceled scan: got %+v, error %v", got, err)
	}
}

func TestMetadataChangeInvalidatesSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "entry", "first")
	directory, err := openRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	opener := func() (*os.File, error) { return openEntry(directory, "entry") }
	file, err := opener()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	before, err := captureSnapshot(file)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "entry", "changed size")
	if err := unchanged(file, before, opener); err == nil {
		t.Fatal("changed file passed its metadata check")
	}
}

func TestSameSizeUpdateWithPreservedModTimeInvalidatesSnapshot(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "entry")
	writeFile(t, root, "entry", "old")
	stamp := time.Unix(1_700_000_000, 123_456_700)
	if err := os.Chtimes(filename, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	directory, err := openRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	opener := func() (*os.File, error) { return openEntry(directory, "entry") }
	file, err := opener()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	before, err := captureSnapshot(file)
	if err != nil {
		t.Fatal(err)
	}
	if content, err := io.ReadAll(file); err != nil || string(content) != "old" {
		t.Fatalf("initial read: %q, error %v", content, err)
	}
	// Leave enough room for filesystems whose timestamp clock ticks in
	// milliseconds to distinguish the two metadata states.
	time.Sleep(20 * time.Millisecond)
	writeFile(t, root, "entry", "new")
	if err := os.Chtimes(filename, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before.FileInfo, after) || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) {
		t.Fatal("the fixture must preserve identity, size and modification time")
	}
	if err := unchanged(file, before, opener); !errors.Is(err, errEntryChanged) {
		t.Fatalf("want a changed entry, got %v", err)
	}
}

func TestCollisionComparison(t *testing.T) {
	collisions, err := collidingNames([]string{
		"Child", "CHILD", "child", "Childish", "Ä", "ä", "É", "é", "K", "K",
		"が", "か\u3099",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Child", "CHILD", "child", "Ä", "ä", "É", "é"} {
		if !collisions[name] {
			t.Errorf("missing collision for %q", name)
		}
	}
	for _, name := range []string{"Childish", "K", "K", "が", "か\u3099"} {
		if collisions[name] {
			t.Errorf("unexpected collision for %q", name)
		}
	}
}

func writeFile(t *testing.T, root, relative, content string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func requireIssue(t *testing.T, result Result, relative string, kind IssueKind) {
	t.Helper()
	for _, item := range result.Issues {
		if item.Path == relative && item.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %q issue for %q: %+v", kind, relative, result.Issues)
}

func requireOnlyFile(t *testing.T, result Result, name string) {
	t.Helper()
	if len(result.Files) != 1 || result.Files[0].Path != name {
		var paths []string
		for _, file := range result.Files {
			paths = append(paths, file.Path)
		}
		t.Fatalf("want only %q, got %s", name, strings.Join(paths, ", "))
	}
}
