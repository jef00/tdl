package up

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pruneEmptyDirs removes empty directories below each root, deepest first.
// The roots themselves are kept. Non-empty directories are left alone.
func pruneEmptyDirs(roots []string) []string {
	var removed []string
	seen := make(map[string]struct{})

	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}

		var dirs []string
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && path != root {
				if _, dup := seen[path]; !dup {
					seen[path] = struct{}{}
					dirs = append(dirs, path)
				}
			}
			return nil
		})

		sort.Slice(dirs, func(i, j int) bool {
			return strings.Count(dirs[i], string(filepath.Separator)) >
				strings.Count(dirs[j], string(filepath.Separator))
		})

		for _, dir := range dirs {
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) > 0 {
				continue
			}
			if os.Remove(dir) == nil {
				removed = append(removed, dir)
			}
		}
	}

	return removed
}
