package scan

//go:generate go run windowscasegen/main.go

import (
	"fmt"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var compareStringOrdinal = windows.NewLazySystemDLL("kernel32.dll").NewProc("CompareStringOrdinal")

func collidingNames(names []string) (map[string]bool, error) {
	type entry struct {
		name string
		wide []uint16
	}
	entries := make([]entry, 0, len(names))
	for _, name := range names {
		wide, err := windows.UTF16FromString(name)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{name: name, wide: wide})
	}
	var compareErr error
	compare := func(left, right entry) int {
		result, _, err := compareStringOrdinal.Call(
			uintptr(unsafe.Pointer(&left.wide[0])), uintptr(len(left.wide)-1),
			uintptr(unsafe.Pointer(&right.wide[0])), uintptr(len(right.wide)-1), 1,
		)
		if result == 0 {
			compareErr = fmt.Errorf("compare Windows names: %w", err)
			return strings.Compare(left.name, right.name)
		}
		return int(result) - 2 // Windows returns 1, 2 or 3 for less, equal or greater.
	}
	slices.SortFunc(entries, compare)
	collisions := make(map[string]bool)
	for i := 1; i < len(entries); i++ {
		if compare(entries[i-1], entries[i]) == 0 {
			collisions[entries[i-1].name] = true
			collisions[entries[i].name] = true
		}
	}
	return collisions, compareErr
}
