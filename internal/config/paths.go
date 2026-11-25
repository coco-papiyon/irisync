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

func parseRemoteRoots(value string) []rootPath {
	windows := hasDrive(value) || strings.HasPrefix(value, "\\\\")
	root, ok := parseRoot(value, windows)
	if !ok {
		return nil
	}
	roots := []rootPath{root}
	// A leading // can be a POSIX path or a Windows UNC path. Check both
	// interpretations without losing POSIX overlaps such as /data and //data.
	if strings.HasPrefix(value, "//") {
		if unc, ok := parseRoot(value, true); ok {
			roots = append(roots, unc)
		}
	}
	return roots
}

func remoteRootsOverlap(roots, others []rootPath) bool {
	for _, root := range roots {
		for _, other := range others {
			if root.overlaps(other) {
				return true
			}
		}
	}
	return false
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
			if !validWindowsName(component) {
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

func (root rootPath) overlaps(other rootPath) bool {
	if root.windows != other.windows || !strings.EqualFold(root.volume, other.volume) {
		return false
	}
	for i := 0; i < min(len(root.parts), len(other.parts)); i++ {
		left, right := root.parts[i], other.parts[i]
		if left != right && (!root.windows || !strings.EqualFold(left, right)) {
			return false
		}
	}
	return true
}
