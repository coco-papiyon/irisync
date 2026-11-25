package config

import (
	"path"
	"strings"

	"github.com/coco-papiyon/irisync/internal/pathname"
)

type rootPath struct {
	windows bool
	volume  string
	parts   []string
}

// Windows drive paths and UNC prefixes containing a backslash identify
// Windows syntax without confusing paths that start with POSIX double slashes.
func hasWindowsRootSyntax(value string) bool {
	return hasDrive(value) || strings.HasPrefix(value, "\\\\") ||
		strings.HasPrefix(value, "\\/") || strings.HasPrefix(value, "/\\")
}

func hasDrive(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	letter := value[0]
	return 'A' <= letter && letter <= 'Z' || 'a' <= letter && letter <= 'z'
}

// parseRoot compares paths lexically without accessing either filesystem.
// Windows drive and UNC rules apply independently of the controller OS.
func parseRoot(value string, windows bool) (rootPath, bool) {
	if strings.ContainsRune(value, 0) {
		return rootPath{}, false
	}
	root := rootPath{windows: windows}
	if windows {
		value = strings.ReplaceAll(value, "\\", "/")
		switch {
		case hasDrive(value):
			root.volume, value = value[:2], value[2:]
		case strings.HasPrefix(value, "//"):
			server, tail, _ := strings.Cut(value[2:], "/")
			share, rest, _ := strings.Cut(tail, "/")
			if !pathname.ValidWindowsName(server) || !pathname.ValidWindowsName(share) {
				return rootPath{}, false
			}
			root.volume, value = "//"+server+"/"+share, "/"+rest
		default:
			return rootPath{}, false
		}
		for _, component := range strings.Split(value, "/") {
			switch component {
			case "", ".", "..":
				continue
			}
			if !pathname.ValidWindowsName(component) || pathname.IsWindowsDeviceName(component) {
				return rootPath{}, false
			}
		}
	}
	if !strings.HasPrefix(value, "/") {
		return rootPath{}, false
	}
	if cleaned := path.Clean(value); cleaned != "/" {
		root.parts = strings.Split(cleaned[1:], "/")
	}
	return root, true
}

func (root rootPath) overlaps(other rootPath) bool {
	if root.windows != other.windows || !pathname.EqualWindowsASCII(root.volume, other.volume) {
		return false
	}
	for i := 0; i < min(len(root.parts), len(other.parts)); i++ {
		left, right := root.parts[i], other.parts[i]
		if left != right && (!root.windows || !pathname.EqualWindowsASCII(left, right)) {
			return false
		}
	}
	return true
}
