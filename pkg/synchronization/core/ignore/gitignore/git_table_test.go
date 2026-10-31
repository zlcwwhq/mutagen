package gitignore

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestGitMatchTable checks the expectations in the table of matching tests
// against git, which guards against mistakes in the table itself. The table
// describes whether or not a rule file ignores a path based on the rules alone,
// whereas git also reports a path as ignored if any of its parents is, so
// tests with a parent that git ignores are not checked.
func TestGitMatchTable(t *testing.T) {
	// Some of the paths in the table aren't portable.
	if runtime.GOOS == "windows" {
		t.Skip("table paths are not portable to Windows")
	}

	// Create a harness and a repository.
	h := newGitHarness(t)
	root := t.TempDir()
	h.initialize(root)

	// Create a scenario for each test that can be represented on disk and
	// collect the paths to check, including those of every parent.
	var paths []string
	type check struct {
		test    matchTest
		path    string
		parents []string
	}
	var checks []check
	for i, test := range matchTests {
		// Skip paths that can't exist.
		components := strings.Split(test.path, "/")
		if contains(components, "") || contains(components, ".") || contains(components, "..") {
			continue
		}

		// Create the path.
		name := "t" + strconv.Itoa(i)
		directory := filepath.Join(root, name)
		target := filepath.Join(directory, filepath.FromSlash(test.path))
		parent := filepath.Dir(target)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatal("unable to create parent directories:", err)
		}
		if test.directory {
			err := os.Mkdir(target, 0o755)
			if err != nil {
				t.Fatal("unable to create directory:", err)
			}
		} else if err := os.WriteFile(target, nil, 0o644); err != nil {
			t.Fatal("unable to create file:", err)
		}
		if err := os.WriteFile(filepath.Join(directory, ".gitignore"), []byte(test.rules), 0o644); err != nil {
			t.Fatalf("unable to write rule file for %+v: %v", test, err)
		}

		// Record what to check.
		c := check{test: test, path: name + "/" + test.path}
		for j := 1; j < len(components); j++ {
			c.parents = append(c.parents, name+"/"+strings.Join(components[:j], "/"))
		}
		paths = append(paths, c.path)
		paths = append(paths, c.parents...)
		checks = append(checks, c)
	}

	// Ask git and check each expectation.
	ignored := h.checkIgnore(root, paths)
	for _, c := range checks {
		var parentIgnored bool
		for _, parent := range c.parents {
			parentIgnored = parentIgnored || ignored[parent]
		}
		if parentIgnored {
			continue
		}
		if ignored[c.path] != c.test.ignored {
			t.Errorf(
				"rules %q, path %q (directory: %v): git result did not match expected: %v != %v",
				c.test.rules, c.test.path, c.test.directory, ignored[c.path], c.test.ignored,
			)
		}
	}
}

// contains returns true if the slice contains the value.
func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
