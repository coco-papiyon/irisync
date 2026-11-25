package scan

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/coco-papiyon/irisync/internal/pathname"
)

// ValidateRelativePath validates an unmodified, slash-separated path that can
// be represented on both supported platforms.
func ValidateRelativePath(relative string) error {
	for i, name := range strings.Split(relative, "/") {
		if !utf8.ValidString(name) || name == "." || name == ".." ||
			!pathname.ValidWindowsName(name) || pathname.IsWindowsDeviceName(name) {
			return fmt.Errorf("invalid component at index %d", i)
		}
	}
	return nil
}

var errExcluded = errors.New("symbolic link, reparse point, mount boundary or special file")

func openEntry(root *os.File, relative string) (*os.File, error) {
	if err := ValidateRelativePath(relative); err != nil {
		return nil, err
	}
	return openRelative(root, relative)
}
