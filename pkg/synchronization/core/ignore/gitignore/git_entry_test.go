package gitignore

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// gitEmptyBlob is the object name of the empty blob, which update-index can
// add to the index without the object existing.
const gitEmptyBlob = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"

// TestGitEntryDifferential compares IsGitEntry with the names that git refuses
// to add to its index as path components when it protects ".git" on NTFS and
// HFS+, which it does for exactly the names that those filesystems can treat as
// ".git".
func TestGitEntryDifferential(t *testing.T) {
	// Windows doesn't allow all of the names in file paths, and git there
	// refuses backslashes in paths for other reasons.
	if runtime.GOOS == "windows" {
		t.Skip("names are not portable to Windows")
	}

	// Create a harness and a repository. Git has only guarded against NTFS
	// streams since 2.24.1, so older versions can't be compared.
	h := newGitHarness(t)
	output, _ := h.run("", nil, "version")
	if major, minor, ok := parseGitVersion(string(output)); !ok || major < 2 || (major == 2 && minor < 25) {
		t.Skip("git predates NTFS stream protection")
	}
	root := t.TempDir()
	h.initialize(root)

	// Ask git to add a file beneath each name, and to place the name at the
	// end of a path, which is checked the same way.
	var compared int
	for _, test := range gitEntryTests {
		// Skip names that git refuses for other reasons.
		if test.name == "" || test.name == "." || strings.HasPrefix(test.name, "\\") {
			continue
		}
		for _, path := range []string{test.name + "/x", "a/" + test.name} {
			command := exec.Command(
				h.path,
				"-c", "core.protectNTFS=true", "-c", "core.protectHFS=true",
				"update-index", "--add", "--cacheinfo", "100644,"+gitEmptyBlob+","+path,
			)
			command.Dir = root
			command.Env = h.environment
			output, err := command.CombinedOutput()
			if _, ok := err.(*exec.ExitError); err != nil && !ok {
				t.Fatal("unable to run git:", err)
			} else if refused := err != nil; refused != test.expected {
				t.Errorf("path %q: git refuses: %v, we classify as Git entry: %v\n%s", path, refused, test.expected, output)
			} else if refused && !strings.Contains(string(output), "Invalid path") {
				t.Errorf("path %q: git refused it for another reason: %s", path, output)
			}
			compared++
		}
	}
	if compared == 0 {
		t.Error("nothing compared")
	}
}
