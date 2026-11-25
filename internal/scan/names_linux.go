package scan

import (
	"slices"
	"strings"
	"unicode/utf8"
)

func collidingNames(names []string) (map[string]bool, error) {
	first := make(map[string]string, len(names))
	collisions := make(map[string]bool)
	for _, name := range names {
		if !utf8.ValidString(name) {
			continue
		}
		key := strings.Map(windowsUppercase, name)
		if previous, found := first[key]; found {
			collisions[previous] = true
			collisions[name] = true
		} else {
			first[key] = name
		}
	}
	return collisions, nil
}

type windowsUpperRange struct {
	first, last, stride, offset rune
}

// Match Windows ordinal case comparison, which uppercases each UTF-16 code
// unit separately. Supplementary characters are unchanged.
func windowsUppercase(value rune) rune {
	if value < 0x80 {
		if 'a' <= value && value <= 'z' {
			return value - ('a' - 'A')
		}
		return value
	}
	if value > 0xffff {
		return value
	}
	index, found := slices.BinarySearchFunc(windowsUpperRanges[:], value, func(item windowsUpperRange, code rune) int {
		if item.last < code {
			return -1
		}
		if item.first > code {
			return 1
		}
		return 0
	})
	if found {
		item := windowsUpperRanges[index]
		if (value-item.first)%item.stride == 0 {
			return value + item.offset
		}
	}
	return value
}
