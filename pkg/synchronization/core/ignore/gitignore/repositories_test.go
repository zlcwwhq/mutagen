package gitignore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
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

// TestRepositoriesUnacceptedGitEntries tests that changes to directories whose
// ".git" entries Git doesn't accept are detected, even though those directories
// aren't the top levels of repositories' working trees: such an entry becoming
// one that Git accepts, whether because the Git directory that a ".git" file
// points to is created, which happens outside of the root, or because a Git
// directory is repaired, a ".git" file being pointed elsewhere, and the removal
// of such an entry.
func TestRepositoriesUnacceptedGitEntries(t *testing.T) {
	// Create a root with a ".git" file that points to a Git directory that
	// doesn't exist yet, and an empty ".git" directory.
	base := t.TempDir()
	root := filepath.Join(base, "root")
	writeTree(t, root, map[string]string{"dangling/.git": "gitdir: ../../elsewhere\n", "empty/.git/x": ""})
	directories := []string{"", "dangling", "empty"}
	repositories := record(t, root, directories...)
	expectChanged(t, repositories, false)

	// The Git directory that the ".git" file points to is created.
	writeTree(t, base, withGitDirectories(nil, "elsewhere"))
	expectChanged(t, repositories, true)

	// The empty Git directory is completed.
	repositories = record(t, root, directories...)
	expectChanged(t, repositories, false)
	writeTree(t, root, withGitDirectories(nil, "empty/.git"))
	expectChanged(t, repositories, true)

	// The ".git" file is pointed to another Git directory that doesn't exist.
	writeTree(t, root, map[string]string{"dangling/.git": "gitdir: ../../nowhere\n"})
	repositories = record(t, root, directories...)
	expectChanged(t, repositories, false)
	writeTree(t, root, map[string]string{"dangling/.git": "gitdir: ../../other\n"})
	expectChanged(t, repositories, true)

	// The ".git" file is removed.
	repositories = record(t, root, directories...)
	expectChanged(t, repositories, false)
	if err := os.Remove(filepath.Join(root, "dangling", ".git")); err != nil {
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
// the root and the directories with ".git" entries that have been evaluated,
// whether or not Git accepts those entries, but not other directories, so that
// only those are examined again.
func TestRepositoriesRecordedDirectories(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, withGitDirectories(map[string]string{
		"plain/x":        "",
		"invalid/.git/x": "",
		"dangling/.git":  "gitdir: nowhere\n",
	}, "sub/.git", "unevaluated/.git"))
	repositories := record(t, root, "", "plain", "invalid", "dangling", "sub")
	if len(repositories.states) != 4 {
		t.Errorf("incorrect number of recorded directories: %d", len(repositories.states))
	}
	for _, directory := range []string{"", "invalid", "dangling", "sub"} {
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

// TestRepositoriesInclude tests that including another record only adds the
// directories that a record doesn't have.
func TestRepositoriesInclude(t *testing.T) {
	// Create a record of the root alone, then change the root's exclude file
	// and record a nested repository along with it.
	root := t.TempDir()
	writeTree(t, root, withGitDirectories(map[string]string{".git/info/exclude": "x\n"}, ".git", "sub/.git"))
	repositories := record(t, root, "")
	writeTree(t, root, map[string]string{".git/info/exclude": "y\n"})
	other := record(t, root, "", "sub")

	// The root still compares with the original record, while the nested
	// repository is checked too.
	repositories.Include(other)
	if len(repositories.states) != 2 {
		t.Fatal("nested repository not included")
	}
	expectChanged(t, repositories, true)
	writeTree(t, root, map[string]string{".git/info/exclude": "x\n"})
	expectChanged(t, repositories, false)
	writeTree(t, root, map[string]string{"sub/.git/info/exclude": "x\n"})
	expectChanged(t, repositories, true)

	// Nil records are tolerated, and a nil record always counts as changed.
	repositories.Include(nil)
	var missing *Repositories
	missing.Include(other)
	expectChanged(t, missing, true)
}

// TestRepositoriesChangedAt tests that changes are detected for individual
// directories, whether or not they're recorded, with directories that aren't
// recorded taken to have no ".git" entry.
func TestRepositoriesChangedAt(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, withGitDirectories(map[string]string{"plain/x": ""}, "sub/.git"))
	repositories := record(t, root, "", "sub", "plain")
	for _, directory := range []string{"", "sub", "plain", "unevaluated", "missing"} {
		if repositories.ChangedAt(directory) {
			t.Errorf("unchanged directory %q reported as changed", directory)
		}
	}

	// A directory that wasn't recorded gets a ".git" entry, which Git doesn't
	// even have to accept.
	writeTree(t, root, map[string]string{"plain/.git": "gitdir: nowhere\n"})
	if !repositories.ChangedAt("plain") {
		t.Error("new Git entry not detected")
	} else if repositories.ChangedAt("sub") {
		t.Error("other directory reported as changed")
	}

	// A recorded directory's Git entry is removed.
	if err := os.RemoveAll(filepath.Join(root, "sub", ".git")); err != nil {
		t.Fatal(err)
	}
	if !repositories.ChangedAt("sub") {
		t.Error("removed Git entry not detected")
	}

	// A nil record always counts as changed.
	var missing *Repositories
	if !missing.ChangedAt("") {
		t.Error("nil record not reported as changed")
	}
}

// evaluate evaluates the specified directories with a new ignorer and returns
// the ignorer.
func evaluate(root string, directories ...string) ignore.DirectoryIgnorer {
	ignorer := NewIgnorer(root, nil)
	for _, directory := range directories {
		ignorer.EnsureDirectory(directory)
	}
	return ignorer
}

// TestRepositoriesAgrees tests that an ignorer agrees with a record if it found
// the same for the recorded directories that it evaluated, and if the other
// directories in which it found ".git" entries aren't ones beneath which
// decisions were reused.
func TestRepositoriesAgrees(t *testing.T) {
	// Create a record, and ignorers that evaluate the same directories, or only
	// some of them.
	root := t.TempDir()
	writeTree(t, root, withGitDirectories(map[string]string{"plain/x": "", "other/x": ""}, "sub/.git"))
	repositories := record(t, root, "", "sub", "plain", "other")
	never := func(string) bool { return false }
	always := func(string) bool { return true }
	if !repositories.Agrees(evaluate(root, "", "sub", "plain"), always) {
		t.Error("identical evaluation doesn't agree")
	} else if !repositories.Agrees(evaluate(root, "sub"), always) {
		t.Error("partial evaluation doesn't agree")
	}

	// A directory that wasn't recorded becomes a repository's top level. That
	// only agrees if no decisions were reused beneath it.
	writeTree(t, root, withGitDirectories(nil, "plain/.git"))
	if !repositories.Agrees(evaluate(root, "", "plain"), never) {
		t.Error("new repository doesn't agree without reused decisions")
	} else if repositories.Agrees(evaluate(root, "", "plain"), func(directory string) bool { return directory == "plain" }) {
		t.Error("new repository agrees with reused decisions")
	} else if !repositories.Agrees(evaluate(root, "", "other"), always) {
		t.Error("evaluation without the new repository doesn't agree")
	}

	// A recorded directory's exclude file changes, which never agrees.
	writeTree(t, root, map[string]string{"sub/.git/info/exclude": "x\n"})
	if repositories.Agrees(evaluate(root, "sub"), never) {
		t.Error("changed exclude file agrees")
	}

	// A recorded directory's Git entry is removed, which never agrees either,
	// unless the directory isn't evaluated.
	repositories = record(t, root, "", "sub")
	if err := os.RemoveAll(filepath.Join(root, "sub", ".git")); err != nil {
		t.Fatal(err)
	}
	if repositories.Agrees(evaluate(root, "sub"), never) {
		t.Error("removed Git entry agrees")
	} else if !repositories.Agrees(evaluate(root, "other"), never) {
		t.Error("evaluation without the removed Git entry doesn't agree")
	}

	// Other ignorers agree, while a nil receiver doesn't.
	if !repositories.Agrees(nil, always) {
		t.Error("nil ignorer doesn't agree")
	}
	var missing *Repositories
	if missing.Agrees(evaluate(root, ""), never) {
		t.Error("nil receiver agrees")
	}
}
