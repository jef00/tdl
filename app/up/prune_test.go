package up

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneEmptyDirs(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) string {
		p = filepath.Join(root, p)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("a/b/c")       // fully empty chain
	keep := mk("d/e") // has a file
	mk("tdl_stage")   // empty, removed too
	if err := os.WriteFile(filepath.Join(keep, "x.nfo"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	pruneEmptyDirs([]string{root})

	for _, p := range []string{"a", "a/b", "a/b/c", "tdl_stage"} {
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed", p)
		}
	}
	for _, p := range []string{"d", "d/e"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("%s should be kept: %v", p, err)
		}
	}
	if _, err := os.Stat(root); err != nil {
		t.Error("root must be kept")
	}
}
