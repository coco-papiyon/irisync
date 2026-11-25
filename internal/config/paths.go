package config

import (
	"path"
	"strings"
	"unicode"
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
			if !validWindowsName(server) || !validWindowsName(share) {
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
			if !validWindowsName(component) || isWindowsDeviceName(component) {
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

func validWindowsName(name string) bool {
	return name != "" && !strings.HasSuffix(name, ".") && !strings.HasSuffix(name, " ") &&
		!strings.ContainsAny(name, "<>:\"|?*") && !strings.ContainsFunc(name, unicode.IsControl)
}

func isWindowsDeviceName(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.TrimRight(stem, " ")
	for _, device := range [...]string{"CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$"} {
		if equalWindowsPathPart(stem, device) {
			return true
		}
	}
	if len(stem) > 3 && (equalWindowsPathPart(stem[:3], "COM") || equalWindowsPathPart(stem[:3], "LPT")) {
		switch stem[3:] {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}
	return false
}

func (root rootPath) overlaps(other rootPath) bool {
	if root.windows != other.windows || !equalWindowsPathPart(root.volume, other.volume) {
		return false
	}
	for i := 0; i < min(len(root.parts), len(other.parts)); i++ {
		left, right := root.parts[i], other.parts[i]
		if left != right && (!root.windows || !equalWindowsPathPart(left, right)) {
			return false
		}
	}
	return true
}

// Static Windows path comparison folds ASCII letters only. Non-ASCII bytes
// must match exactly. Filesystem-specific equivalences are checked at startup.
func equalWindowsPathPart(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := 0; i < len(left); i++ {
		if lowerASCII(left[i]) != lowerASCII(right[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(letter byte) byte {
	if 'A' <= letter && letter <= 'Z' {
		return letter + ('a' - 'A')
	}
	return letter
}
