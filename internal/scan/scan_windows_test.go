package scan

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsRejectsDriveRelativeRoot(t *testing.T) {
	got, err := Scan(context.Background(), "C:..")
	if err == nil || len(got.Files) != 0 {
		t.Fatalf("drive-relative root was accepted: got %+v, error %v", got, err)
	}
}

func TestWindowsRootSpellingIsCheckedBeforeFullPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "safe/marker", "valid root")
	for _, tail := range []string{"/safe.", "/safe. ", "/safe ", "/safe/.. "} {
		got, err := Scan(context.Background(), root+tail)
		if err == nil || len(got.Files) != 0 {
			t.Errorf("normalized invalid root %q: got %+v, error %v", tail, got, err)
		}
	}
	got, err := Scan(context.Background(), root+"/safe/.")
	if err != nil {
		t.Fatal(err)
	}
	requireOnlyFile(t, got, "marker")
	if _, err := absoluteRoot("\\\\server\\share. \\path"); err == nil {
		t.Fatal("accepted a UNC share with a trailing space")
	}
}

func TestWindowsJunctions(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFile(t, root, "safe", "inside")
	writeFile(t, root, "ordinary/sub/entry", "inside")
	writeFile(t, outside, "entry", "outside")
	for name, target := range map[string]string{
		"junction-outside": outside,
		"junction-inside":  filepath.Join(root, "ordinary"),
	} {
		createJunction(t, filepath.Join(root, name), target)
	}

	got, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 2 || got.Files[0].Path != "ordinary/sub/entry" || got.Files[1].Path != "safe" {
		t.Fatalf("unexpected files: %+v", got.Files)
	}
	for _, name := range []string{"junction-outside", "junction-inside"} {
		requireIssue(t, got, name, Excluded)
	}

	dir, err := openRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	for _, relative := range []string{"junction-outside/entry", "junction-inside/sub/entry"} {
		file, err := openEntry(dir, relative)
		if file != nil {
			file.Close()
			t.Fatalf("opened a path through a junction: %q", relative)
		}
		if !errors.Is(err, errExcluded) {
			t.Errorf("open %q returned %v", relative, err)
		}
	}
	for _, name := range []string{"junction-outside", "junction-inside/sub"} {
		if _, err := Scan(context.Background(), filepath.Join(root, filepath.FromSlash(name))); !errors.Is(err, errExcluded) {
			t.Errorf("expected an excluded root error for %q, got %v", name, err)
		}
	}
}

func TestWindowsFileSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFile(t, root, "safe", "inside")
	writeFile(t, outside, "entry", "outside")
	err := os.Symlink(filepath.Join(outside, "entry"), filepath.Join(root, "link"))
	if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Skipf("this Windows runner cannot create a symbolic link: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	requireOnlyFile(t, got, "safe")
	requireIssue(t, got, "link", Excluded)
}

func createJunction(t *testing.T, name, target string) {
	t.Helper()
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path $env:IRISYNC_TEST_LINK -Target $env:IRISYNC_TEST_TARGET | Out-Null")
	cmd.Env = append(os.Environ(), "IRISYNC_TEST_LINK="+name, "IRISYNC_TEST_TARGET="+target)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(name); err != nil {
			t.Errorf("remove junction: %v", err)
		}
	})
}
