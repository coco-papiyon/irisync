package scan

import "strings"

// ConflictingPaths identifies components that would collide under the same
// Windows comparison used by Scan. Callers also protect their descendants.
func ConflictingPaths(paths []string) (map[string]bool, error) {
	siblings := make(map[string]map[string]struct{})
	for _, path := range paths {
		for path != "" {
			parent, name := "", path
			if split := strings.LastIndexByte(path, '/'); split >= 0 {
				parent, name = path[:split], path[split+1:]
			}
			if siblings[parent] == nil {
				siblings[parent] = make(map[string]struct{})
			}
			if _, seen := siblings[parent][name]; seen {
				break
			}
			siblings[parent][name] = struct{}{}
			path = parent
		}
	}
	collisions := make(map[string]bool)
	for parent, group := range siblings {
		if len(group) <= 1 {
			continue
		}
		names := make([]string, 0, len(group))
		for name := range group {
			names = append(names, name)
		}
		conflicting, err := collidingNames(names)
		if err != nil {
			return nil, err
		}
		for name := range conflicting {
			path := name
			if parent != "" {
				path = parent + "/" + name
			}
			collisions[path] = true
		}
	}
	return collisions, nil
}
