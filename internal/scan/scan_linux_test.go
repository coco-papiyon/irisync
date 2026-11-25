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
	writeFile(t, root, "Ä/report", "must not descend")
	writeFile(t, root, "ä/report", "must not descend")
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
	requireIssue(t, got, "Ä", NameCollision)
	requireIssue(t, got, "ä", NameCollision)
	requireIssue(t, got, "NUL.txt", InvalidName)
	for _, name := range []string{"outside-link", "inside-link", "fifo"} {
		requireIssue(t, got, name, Excluded)
	}
}

func TestLinuxRootPathIsCheckedBeforeCleaning(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFile(t, root, "safe/marker", "must not scan a different directory")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := Scan(context.Background(), root+"/link/../safe")
	if err == nil || len(got.Files) != 0 {
		t.Fatalf("expected the original root spelling to be rejected, got %+v, error %v", got, err)
	}
}

func TestLinuxRootAllowsNativeNameWithTrailingPeriod(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "safe./marker", "valid POSIX root")
	got, err := Scan(context.Background(), filepath.Join(root, "safe."))
	if err != nil {
		t.Fatal(err)
	}
	requireOnlyFile(t, got, "marker")
}

func TestLinuxCaseComparisonPreservesDistinctNames(t *testing.T) {
	names := []string{"Ǆ", "ǅ", "ა", "Ა", "\U00010400", "\U00010428", "\xff", "\ufffd"}
	collisions, err := collidingNames(names)
	if err != nil || len(collisions) != 0 {
		t.Fatalf("unexpected collisions: %v, error %v", collisions, err)
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
