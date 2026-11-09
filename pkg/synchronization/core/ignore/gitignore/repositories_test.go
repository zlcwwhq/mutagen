package gitignore

import (
	"os"
	"path/filepath"
	"testing"
)

// record evaluates the specified directories with a new ignorer and returns
// what it recorded about repositories.
func record(t *testing.T, root string, directories ...string) *Repositories {
	t.Helper()
	ignorer := NewIgnorer(root, nil)
	for _, directory := range directories {
		ignorer.EnsureDirectory(directory)
	}
	repositories := RecordedRepositories(ignorer)
	if repositories == nil {
		t.Fatal("no record for ignorer")
	}
	return repositories
}

// expectChanged checks the result of Changed.
func expectChanged(t *testing.T, repositories *Repositories, expected bool) {
	t.Helper()
	if changed := repositories.Changed(); changed != expected {
		t.Errorf("incorrect change detection: %t != %t", changed, expected)
	}
}

// TestRepositoriesOutsideRoot tests that changes to the exclude files of
// repositories whose Git directories are outside of the root are detected,
// both for the root and for a nested repository.
func TestRepositoriesOutsideRoot(t *testing.T) {
	// Create a root that's a linked worktree of a repository elsewhere, with a
	// nested repository whose Git directory is elsewhere too.
	base := t.TempDir()
	root := filepath.Join(base, "root")
	writeTree(t, base, withGitDirectories(map[string]string{
		"root/.git":                           "gitdir: ../repository/.git/worktrees/work\n",
		"root/sub/.git":                       "gitdir: ../../nested\n",
		"root/sub/x":                          "",
		"repository/.git/worktrees/work/HEAD": validHead,
		"repository/.git/worktrees/work/commondir": "../..\n",
	}, "repository/.git", "nested"))

	// Nothing has changed right after recording.
	repositories := record(t, root, "", "sub")
	expectChanged(t, repositories, false)

	// Changes to the exclude files are detected, including their creation, and
	// a new record matches them again.
	for _, exclude := range []string{"repository/.git/info/exclude", "nested/info/exclude"} {
		for _, contents := range []string{"x\n", "y\n"} {
			writeTree(t, base, map[string]string{exclude: contents})
			expectChanged(t, repositories, true)
			repositories = record(t, root, "", "sub")
			expectChanged(t, repositories, false)
		}
	}

	// Removing an exclude file is a change, but emptying a missing one isn't.
	if err := os.Remove(filepath.Join(base, "nested", "info", "exclude")); err != nil {
		t.Fatal(err)
	}
	expectChanged(t, repositories, true)
	repositories = record(t, root, "", "sub")
	writeTree(t, base, map[string]string{"nested/info/exclude": ""})
	expectChanged(t, repositories, false)
}

// TestRepositoriesTopLevel tests that changes to whether or not a directory is
// the top level of a repository's working tree are detected.
func TestRepositoriesTopLevel(t *testing.T) {
	// A root that isn't in a repository becomes one.
	root := t.TempDir()
	repositories := record(t, root, "")
	expectChanged(t, repositories, false)
	writeTree(t, root, withGitDirectories(nil, ".git"))
	expectChanged(t, repositories, true)

	// A nested repository's Git directory stops being one that Git accepts.
	writeTree(t, root, withGitDirectories(nil, "sub/.git", "other/.git"))
	repositories = record(t, root, "", "sub", "other")
	expectChanged(t, repositories, false)
	if err := os.Remove(filepath.Join(root, "sub", ".git", "HEAD")); err != nil {
		t.Fatal(err)
	}
	expectChanged(t, repositories, true)

	// A nested repository's directory is removed.
	repositories = record(t, root, "", "sub", "other")
	expectChanged(t, repositories, false)
	if err := os.RemoveAll(filepath.Join(root, "other")); err != nil {
		t.Fatal(err)
	}
	expectChanged(t, repositories, true)
}

// TestRepositoriesFailures tests that a failure to read an exclude file is
// recorded, so that it only counts as a change if it changes.
func TestRepositoriesFailures(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, withGitDirectories(map[string]string{"sub/.git/info/exclude": "x\n"}, "sub/.git"))
	exclude := filepath.Join(root, "sub", ".git", "info", "exclude")
	if err := os.Truncate(exclude, maximumRuleFileSize+1); err != nil {
		t.Fatal(err)
	}
	repositories := record(t, root, "", "sub")
	expectChanged(t, repositories, false)
	if err := os.Truncate(exclude, 0); err != nil {
		t.Fatal(err)
	}
	expectChanged(t, repositories, true)
}

// TestRepositoriesRecordedDirectories tests which directories are recorded:
// the root and the top levels of nested repositories that have been evaluated,
// but not other directories, so that only those are examined again.
func TestRepositoriesRecordedDirectories(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, withGitDirectories(map[string]string{"plain/x": "", "invalid/.git/x": ""}, "sub/.git", "unevaluated/.git"))
	repositories := record(t, root, "", "plain", "invalid", "sub")
	if len(repositories.states) != 2 {
		t.Errorf("incorrect number of recorded directories: %d", len(repositories.states))
	}
	for _, directory := range []string{"", "sub"} {
		if _, ok := repositories.states[directory]; !ok {
			t.Errorf("directory %q not recorded", directory)
		}
	}

	// Recording ends with the call that returns the record.
	ignorer := NewIgnorer(root, nil)
	ignorer.EnsureDirectory("")
	repositories = RecordedRepositories(ignorer)
	ignorer.EnsureDirectory("sub")
	if len(repositories.states) != 1 {
		t.Error("record changed after it was returned")
	}

	// Other ignorers don't have records.
	if RecordedRepositories(nil) != nil {
		t.Error("record returned for nil ignorer")
	}
}

// TestRepositoriesContains tests that a record only contains another if it
// has the same for every directory in the other.
func TestRepositoriesContains(t *testing.T) {
	// A record contains itself and records of fewer directories.
	root := t.TempDir()
	writeTree(t, root, withGitDirectories(map[string]string{".git/info/exclude": "x\n"}, ".git", "sub/.git"))
	repositories := record(t, root, "", "sub")
	partial := record(t, root, "")
	if !repositories.Contains(repositories) || !repositories.Contains(partial) {
		t.Error("record doesn't contain what it has")
	}

	// It doesn't contain a record of a directory that it doesn't have.
	if partial.Contains(repositories) {
		t.Error("record contains directory that it doesn't have")
	}

	// It doesn't contain a record of a directory that differs.
	writeTree(t, root, map[string]string{".git/info/exclude": "y\n"})
	if repositories.Contains(record(t, root, "")) {
		t.Error("record contains directory that differs")
	}

	// A nil record neither contains anything nor is contained, and it always
	// counts as changed.
	var missing *Repositories
	if missing.Contains(partial) || repositories.Contains(missing) {
		t.Error("nil record treated as known")
	}
	expectChanged(t, missing, true)
}
