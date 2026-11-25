package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxScanExclusionsAndCollisions(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFile(t, root, "safe", "safe text")
	writeFile(t, root, "Tree", "collides with a directory")
	writeFile(t, root, "tree/child", "must not descend")
	writeFile(t, root, "NUL.txt", "invalid name")
	writeFile(t, outside, "secret", "not a scan candidate")

	for name, target := range map[string]string{
		"outside-link": outside,
		"inside-link":  "safe",
	} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	requireOnlyFile(t, got, "safe")
	requireIssue(t, got, "Tree", NameCollision)
	requireIssue(t, got, "tree", NameCollision)
	requireIssue(t, got, "NUL.txt", InvalidName)
	for _, name := range []string{"outside-link", "inside-link", "fifo"} {
		requireIssue(t, got, name, Excluded)
	}
}

func TestLinuxDirectoryReplacement(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFile(t, root, "sub/entry", "inside")
	writeFile(t, outside, "entry", "outside")
	dir, err := openRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"sub", "sub/entry"} {
		file, err := openEntry(dir, path)
		if file != nil {
			file.Close()
			t.Fatalf("opened a replacement symlink path %q", path)
		}
		if !errors.Is(err, errExcluded) {
			t.Errorf("open %q returned %v", path, err)
		}
	}
	if _, err := Scan(context.Background(), filepath.Join(root, "sub")); !errors.Is(err, errExcluded) {
		t.Fatalf("expected an excluded root error, got %v", err)
	}
}

func TestLinuxUnreadableSubtree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can list directories without read permission")
	}
	root := t.TempDir()
	writeFile(t, root, "safe", "retained")
	writeFile(t, root, "hidden/entry", "not read")
	hidden := filepath.Join(root, "hidden")
	if err := os.Chmod(hidden, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(hidden, 0700) })

	got, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	requireOnlyFile(t, got, "safe")
	requireIssue(t, got, "hidden", Unreadable)
}
