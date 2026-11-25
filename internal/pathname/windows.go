// Package pathname defines portable lexical checks for Windows names.
package pathname

import (
	"strings"
	"unicode"
)

func ValidWindowsName(name string) bool {
	return name != "" && !strings.HasSuffix(name, ".") && !strings.HasSuffix(name, " ") &&
		!strings.ContainsAny(name, "/\\<>:\"|?*") && !strings.ContainsFunc(name, unicode.IsControl)
}

func IsWindowsDeviceName(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.TrimRight(stem, " ")
	switch FoldWindowsASCII(stem) {
	case "con", "prn", "aux", "nul", "conin$", "conout$":
		return true
	}
	if len(stem) > 3 && (EqualWindowsASCII(stem[:3], "COM") || EqualWindowsASCII(stem[:3], "LPT")) {
		switch stem[3:] {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}
	return false
}

// EqualWindowsASCII applies the static comparison used for Windows roots.
func EqualWindowsASCII(left, right string) bool {
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

// FoldWindowsASCII folds ASCII letters and leaves all UTF-8 bytes unchanged.
func FoldWindowsASCII(name string) string {
	for i := 0; i < len(name); i++ {
		if lowerASCII(name[i]) != name[i] {
			result := []byte(name)
			for index := i; index < len(result); index++ {
				result[index] = lowerASCII(result[index])
			}
			return string(result)
		}
	}
	return name
}

func lowerASCII(letter byte) byte {
	if 'A' <= letter && letter <= 'Z' {
		return letter + ('a' - 'A')
	}
	return letter
}
