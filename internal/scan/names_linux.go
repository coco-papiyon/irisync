package scan

import "github.com/coco-papiyon/irisync/internal/pathname"

// A Linux scan can detect portable ASCII case collisions without knowing the
// eventual Windows target's filesystem. Non-ASCII rules depend on that target.
func collidingNames(names []string) (map[string]bool, error) {
	first := make(map[string]string, len(names))
	collisions := make(map[string]bool)
	for _, name := range names {
		key := pathname.FoldWindowsASCII(name)
		if previous, found := first[key]; found {
			collisions[previous] = true
			collisions[name] = true
		} else {
			first[key] = name
		}
	}
	return collisions, nil
}
