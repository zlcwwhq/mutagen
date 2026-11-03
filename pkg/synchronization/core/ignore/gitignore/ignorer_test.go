package gitignore

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// writeTree writes files, specified by slash-separated paths relative to a
// root, creating parent directories as necessary.
func writeTree(t testing.TB, root string, files map[string]string) {
	t.Helper()
	for path, contents := range files {
		target := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal("unable to create directory:", err)
		}
		if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
			t.Fatal("unable to write file:", err)
		}
	}
}

// sessionIgnorer is an ignorer implemented by a function.
type sessionIgnorer func(path string, directory bool) (ignore.IgnoreStatus, bool)

// Ignore implements ignore.Ignorer.Ignore.
func (s sessionIgnorer) Ignore(path string, directory bool) (ignore.IgnoreStatus, bool) {
	return s(path, directory)
}

// result is the result of an Ignore call.
type result struct {
	// status is the ignore status.
	status ignore.IgnoreStatus
	// continueTraversal is the traversal directive.
	continueTraversal bool
}

// expectIgnored checks the result of an Ignore call that's expected to be
// either nominal or ignored, with no traversal directive.
func expectIgnored(t *testing.T, ignorer ignore.Ignorer, path string, directory, expected bool) {
	t.Helper()
	status, continueTraversal := ignorer.Ignore(path, directory)
	if continueTraversal {
		t.Errorf("path %q: unexpected traversal continuation", path)
	}
	switch status {
	case ignore.IgnoreStatusNominal:
		if expected {
			t.Errorf("path %q: expected to be ignored", path)
		}
	case ignore.IgnoreStatusIgnored:
		if !expected {
			t.Errorf("path %q: not expected to be ignored", path)
		}
	default:
		t.Errorf("path %q: unexpected status: %d", path, status)
	}
}

// TestRuleFilePredicates tests AffectsIgnoreEvaluation and
// IsDeliverableRuleFile. They only disagree about the exclude files of
// repositories, which affect ignore evaluation, but which are inside Git
// directories.
func TestRuleFilePredicates(t *testing.T) {
	tests := []struct {
		path        string
		affects     bool
		deliverable bool
	}{
		// Rule files, including beneath names that merely resemble a Git entry.
		{".gitignore", true, true},
		{"a/.gitignore", true, true},
		{"a/b/.gitignore", true, true},
		{".gitignore/.gitignore", true, true},
		{".gitx/.gitignore", true, true},
		{"x.git/.gitignore", true, true},
		{"git/.gitignore", true, true},
		{".git2/hooks/.gitignore", true, true},

		// The exclude files of the root repository and of nested repositories,
		// whether in their own Git directories or in Git directories that their
		// ".git" files point to, such as those of submodules and worktrees.
		{".git/info/exclude", true, false},
		{"a/.git/info/exclude", true, false},
		{"a/b/.git/info/exclude", true, false},
		{"a/.gitignore/.git/info/exclude", true, false},
		{".git/modules/sub/info/exclude", true, false},
		{".git/modules/a/modules/b/info/exclude", true, false},
		{".git/worktrees/w/info/exclude", true, false},
		{"a/.git/modules/b/info/exclude", true, false},
		{".GIT/info/exclude", true, false},
		{"a/.Git/info/exclude", true, false},

		// Anything else beneath a Git entry, whatever its name.
		{".git/.gitignore", false, false},
		{".git/hooks/.gitignore", false, false},
		{".git/info/.gitignore", false, false},
		{"a/.git/.gitignore", false, false},
		{"a/.git/x/.gitignore", false, false},
		{".gitignore/.git/.gitignore", false, false},
		{".GIT/.gitignore", false, false},
		{".Git/hooks/.gitignore", false, false},
		{"a/.gIt/.gitignore", false, false},
		{".git/hooks/pre-commit", false, false},
		{".git/config", false, false},
		{".git/info", false, false},
		{".git/info/Exclude", false, false},
		{".git/Info/exclude", false, false},
		{".git/info/exclude/x", false, false},
		{".git/info/exclude.lock", false, false},
		{".git/info/xexclude", false, false},
		{".git/xinfo/exclude", false, false},
		{".git/exclude", false, false},
		{".git/modules/sub/info", false, false},
		{"a/.git/info", false, false},
		{".git", false, false},
		{"a/.git", false, false},

		// Anything else.
		{"", false, false},
		{"a", false, false},
		{"gitignore", false, false},
		{".Gitignore", false, false},
		{".gitignore2", false, false},
		{"x.gitignore", false, false},
		{"a/x.gitignore", false, false},
		{".gitignore/x", false, false},
		{"exclude", false, false},
		{"info/exclude", false, false},
		{"a/info/exclude", false, false},
		{"git/info/exclude", false, false},
		{".gitx/info/exclude", false, false},
		{"a.git/info/exclude", false, false},
	}
	for _, test := range tests {
		if result := AffectsIgnoreEvaluation(test.path); result != test.affects {
			t.Errorf("path %q: AffectsIgnoreEvaluation did not match expected: %v != %v", test.path, result, test.affects)
		}
		if result := IsDeliverableRuleFile(test.path); result != test.deliverable {
			t.Errorf("path %q: IsDeliverableRuleFile did not match expected: %v != %v", test.path, result, test.deliverable)
		}
	}
}

// TestShallowestGitEntry tests ShallowestGitEntry, along with the fact that the
// ignorer agrees about which entries are Git entries.
func TestShallowestGitEntry(t *testing.T) {
	ignorer := NewIgnorer(t.TempDir(), nil)
	tests := []struct {
		path     string
		expected string
	}{
		{"", ""},
		{"a", ""},
		{"a/b", ""},
		{".gitignore", ""},
		{".gitx/a", ""},
		{"x.git/a", ""},
		{"git/a", ""},
		{".gi/a", ""},
		{".git /a", ""},
		{" .git/a", ""},
		{".git", ".git"},
		{".GIT", ".GIT"},
		{".Git", ".Git"},
		{".gIt/a", ".gIt"},
		{".git/a", ".git"},
		{".git/a/b", ".git"},
		{".git/.git/a", ".git"},
		{"a/.git", "a/.git"},
		{"a/.git/b", "a/.git"},
		{"a/b/.Git/c/d", "a/b/.Git"},
	}
	for _, test := range tests {
		if result := ShallowestGitEntry(test.path); result != test.expected {
			t.Errorf("path %q: result did not match expected: %q != %q", test.path, result, test.expected)
		}
		if excluded, err := ignore.Excluded(ignorer, test.path, false); err != nil {
			t.Errorf("path %q: unable to evaluate: %v", test.path, err)
		} else if excluded != test.expected {
			t.Errorf("path %q: ignorer did not agree: %q != %q", test.path, excluded, test.expected)
		}
	}
}

// TestIgnoreGitMetadata tests that Git metadata is always ignored.
func TestIgnoreGitMetadata(t *testing.T) {
	// Create a rule file that tries to include everything, and a session
	// ignorer that tries to do the same. Neither should have any effect.
	root := t.TempDir()
	writeTree(t, root, map[string]string{".gitignore": "!.git\n!.*\n!*\n", "sub/.gitignore": "!.git\n"})
	var sessionCalls int
	session := sessionIgnorer(func(string, bool) (ignore.IgnoreStatus, bool) {
		sessionCalls++
		return ignore.IgnoreStatusUnignored, true
	})
	for _, ignorer := range []ignore.Ignorer{NewIgnorer(root, nil), NewIgnorer(root, session)} {
		for _, path := range []string{".git", "sub/.git", "sub/sub/.git", ".GIT", ".Git", "sub/.gIt", "sub/.GIT"} {
			for _, directory := range []bool{false, true} {
				if status, continueTraversal := ignorer.Ignore(path, directory); status != ignore.IgnoreStatusIgnored || continueTraversal {
					t.Errorf("path %q (directory: %v): result did not match expected: (%d, %v)", path, directory, status, continueTraversal)
				}
			}
		}
	}
	if sessionCalls != 0 {
		t.Error("session ignorer was consulted for Git metadata")
	}

	// Check that similar names aren't affected.
	ignorer := NewIgnorer(root, nil)
	for _, path := range []string{".gitx", "x.git", "git", ".gi", ".git ", " .git", ".gitignore", "a.git/b", ".git/b"} {
		expectIgnored(t, ignorer, path, false, false)
	}
}

// TestIgnoreRoot tests that the root is never ignored.
func TestIgnoreRoot(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{".gitignore": "*\n**\n"})
	expectIgnored(t, NewIgnorer(root, nil), "", true, false)
}

// TestIgnoreSession tests the interaction of rule files and the session
// ignorer.
func TestIgnoreSession(t *testing.T) {
	// Create rules that ignore "ignored" and un-ignore "included".
	root := t.TempDir()
	writeTree(t, root, map[string]string{".gitignore": "ignored\n!included\n"})

	tests := []struct {
		name     string
		session  result
		path     string
		expected result
	}{
		// Nominal results from the session ignorer defer to the rule files, but
		// retain the traversal directive.
		{"nominal ignored", result{ignore.IgnoreStatusNominal, false}, "ignored", result{ignore.IgnoreStatusIgnored, false}},
		{"nominal ignored with continue", result{ignore.IgnoreStatusNominal, true}, "ignored", result{ignore.IgnoreStatusIgnored, true}},
		{"nominal other", result{ignore.IgnoreStatusNominal, false}, "other", result{ignore.IgnoreStatusNominal, false}},
		{"nominal other with continue", result{ignore.IgnoreStatusNominal, true}, "other", result{ignore.IgnoreStatusNominal, true}},

		// A negated rule never results in an unignored status.
		{"negated", result{ignore.IgnoreStatusNominal, false}, "included", result{ignore.IgnoreStatusNominal, false}},
		{"negated with continue", result{ignore.IgnoreStatusNominal, true}, "included", result{ignore.IgnoreStatusNominal, true}},

		// Other results from the session ignorer are returned unchanged.
		{"session ignored", result{ignore.IgnoreStatusIgnored, false}, "other", result{ignore.IgnoreStatusIgnored, false}},
		{"session ignored with continue", result{ignore.IgnoreStatusIgnored, true}, "other", result{ignore.IgnoreStatusIgnored, true}},
		{"session ignored included", result{ignore.IgnoreStatusIgnored, true}, "included", result{ignore.IgnoreStatusIgnored, true}},
		{"session unignored", result{ignore.IgnoreStatusUnignored, false}, "ignored", result{ignore.IgnoreStatusUnignored, false}},
		{"session unignored with continue", result{ignore.IgnoreStatusUnignored, true}, "ignored", result{ignore.IgnoreStatusUnignored, true}},
	}
	for _, test := range tests {
		var calls []string
		session := sessionIgnorer(func(path string, directory bool) (ignore.IgnoreStatus, bool) {
			calls = append(calls, fmt.Sprintf("%s:%v", path, directory))
			return test.session.status, test.session.continueTraversal
		})
		status, continueTraversal := NewIgnorer(root, session).Ignore(test.path, true)
		if actual := (result{status, continueTraversal}); actual != test.expected {
			t.Errorf("%s: result did not match expected: %v != %v", test.name, actual, test.expected)
		}
		if len(calls) != 1 || calls[0] != test.path+":true" {
			t.Errorf("%s: unexpected session ignorer calls: %v", test.name, calls)
		}
	}
}

// TestIgnorePrecedence tests which of the rule files takes precedence.
func TestIgnorePrecedence(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		// ignored maps paths to whether or not they are ignored.
		ignored map[string]bool
	}{
		{
			name:    "root file",
			files:   map[string]string{".gitignore": "*.log\n"},
			ignored: map[string]bool{"a.log": true, "x/a.log": true, "x/y/a.log": true, "a.txt": false},
		},
		{
			name:    "last matching rule wins",
			files:   map[string]string{".gitignore": "x\n!x\ny\n!y\ny\n"},
			ignored: map[string]bool{"x": false, "y": true, "a/x": false, "a/y": true},
		},
		{
			name:    "deeper file wins over parent",
			files:   map[string]string{".gitignore": "a.log\n", "sub/.gitignore": "!a.log\n"},
			ignored: map[string]bool{"a.log": true, "sub/a.log": false, "sub/deeper/a.log": false, "other/a.log": true},
		},
		{
			name:    "deeper file ignoring over parent negation",
			files:   map[string]string{".gitignore": "!a.log\n", "sub/.gitignore": "a.log\n"},
			ignored: map[string]bool{"a.log": false, "sub/a.log": true, "sub/deeper/a.log": true},
		},
		{
			name:    "deeper file without a match defers to parent",
			files:   map[string]string{".gitignore": "*.log\n", "sub/.gitignore": "*.tmp\n!other.log\n"},
			ignored: map[string]bool{"sub/a.log": true, "sub/a.tmp": true, "sub/b.txt": false, "sub/other.log": false, "other.log": true},
		},
		{
			name:    "deepest file with a match wins",
			files:   map[string]string{".gitignore": "x\n", "a/.gitignore": "!x\n", "a/b/.gitignore": "x\n", "a/b/c/.gitignore": "unrelated\n"},
			ignored: map[string]bool{"x": true, "a/x": false, "a/b/x": true, "a/b/c/x": true, "a/b/c/d/x": true},
		},
		{
			name:    "anchoring is relative to the file",
			files:   map[string]string{"sub/.gitignore": "/x\ny/z\n"},
			ignored: map[string]bool{"x": false, "sub/x": true, "sub/y/x": false, "y/z": false, "sub/y/z": true, "sub/w/y/z": false, "sub/z": false},
		},
		{
			name:    "anchoring with several levels",
			files:   map[string]string{"a/b/.gitignore": "c/*.o\n/d/\n**/e\n"},
			ignored: map[string]bool{"a/b/c/x.o": true, "a/c/x.o": false, "a/b/c/d/x.o": false, "a/b/x.o": false, "a/b/f/c/x.o": false, "a/b/e": true, "a/b/f/g/e": true, "a/e": false},
		},
		{
			name:    "rule files elsewhere don't apply",
			files:   map[string]string{"a/.gitignore": "x\n", "b/c": ""},
			ignored: map[string]bool{"x": false, "b/x": false, "a/x": true, "ab/x": false},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, test.files)
			ignorer := NewIgnorer(root, nil)
			for path, ignored := range test.ignored {
				expectIgnored(t, ignorer, path, false, ignored)
			}
		})
	}
}

// TestIgnoreMemoization tests that rule files are read lazily and only once.
func TestIgnoreMemoization(t *testing.T) {
	// Create the initial rule files.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":        "a\n",
		".git/info/exclude": "e\n",
		"sub/.gitignore":    "s\n",
	})
	ignorer := NewIgnorer(root, nil)
	expectIgnored(t, ignorer, "a", false, true)
	expectIgnored(t, ignorer, "b", false, false)
	expectIgnored(t, ignorer, "e", false, true)

	// Rewrite the files that have been read and create files that haven't been.
	// The ones that have been read should still have their original effect,
	// and the ones that haven't been read should be read when first needed.
	writeTree(t, root, map[string]string{
		".gitignore":        "b\n",
		".git/info/exclude": "f\n",
		"sub/.gitignore":    "t\n",
		"other/.gitignore":  "o\n",
	})
	expectIgnored(t, ignorer, "a", false, true)
	expectIgnored(t, ignorer, "b", false, false)
	expectIgnored(t, ignorer, "e", false, true)
	expectIgnored(t, ignorer, "f", false, false)
	expectIgnored(t, ignorer, "other/o", false, true)
	expectIgnored(t, ignorer, "sub/s", false, false)
	expectIgnored(t, ignorer, "sub/t", false, true)

	// A new ignorer should see the new contents.
	ignorer = NewIgnorer(root, nil)
	expectIgnored(t, ignorer, "a", false, false)
	expectIgnored(t, ignorer, "b", false, true)
	expectIgnored(t, ignorer, "e", false, false)
	expectIgnored(t, ignorer, "f", false, true)
	expectIgnored(t, ignorer, "sub/s", false, false)
	expectIgnored(t, ignorer, "sub/t", false, true)
}

// TestIgnoreDirectoryAlternation tests that results are correct when
// alternating between directories.
func TestIgnoreDirectoryAlternation(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":     "r\n",
		"a/.gitignore":   "x\n",
		"a/b/.gitignore": "y\n",
		"c/d":            "",
	})
	ignorer := NewIgnorer(root, nil)
	for i := 0; i < 3; i++ {
		expectIgnored(t, ignorer, "a/x", false, true)
		expectIgnored(t, ignorer, "x", false, false)
		expectIgnored(t, ignorer, "a/b/x", false, true)
		expectIgnored(t, ignorer, "a/b/y", false, true)
		expectIgnored(t, ignorer, "a/y", false, false)
		expectIgnored(t, ignorer, "c/x", false, false)
		expectIgnored(t, ignorer, "c/r", false, true)
		expectIgnored(t, ignorer, "a/b/r", false, true)
		expectIgnored(t, ignorer, "a", true, false)
		expectIgnored(t, ignorer, "r", true, true)
	}
}

// TestIgnoreRuleFileTypes tests that only regular rule files are read.
func TestIgnoreRuleFileTypes(t *testing.T) {
	root := t.TempDir()

	// A directory with the name of a rule file doesn't provide rules.
	writeTree(t, root, map[string]string{"dir/.gitignore/.gitignore": "*\n", "dir/a": ""})
	expectIgnored(t, NewIgnorer(root, nil), "dir/a", false, false)

	// A path whose parent isn't a directory doesn't cause problems.
	writeTree(t, root, map[string]string{"file": ""})
	expectIgnored(t, NewIgnorer(root, nil), "file/a", false, false)
	if err := NewIgnorer(root, nil).EnsureDirectory("file"); err != nil {
		t.Error("non-directory parent treated as read failure:", err)
	}
	expectIgnored(t, NewIgnorer(root, nil), "missing/a", false, false)

	// A symbolic link to a rule file isn't followed.
	writeTree(t, root, map[string]string{"link/rules": "*\n", "link/a": ""})
	if err := os.Symlink("rules", filepath.Join(root, "link", ".gitignore")); err != nil {
		t.Logf("unable to create symbolic link, skipping: %v", err)
	} else {
		expectIgnored(t, NewIgnorer(root, nil), "link/a", false, false)
	}

	// A rule file that can't be read blocks its subtree.
	writeTree(t, root, map[string]string{"unreadable/.gitignore": "*\n"})
	unreadable := filepath.Join(root, "unreadable", ".gitignore")
	if runtime.GOOS != "windows" && os.Chmod(unreadable, 0) == nil {
		if file, err := os.Open(unreadable); err == nil {
			file.Close()
			t.Log("unable to make rule file unreadable, skipping")
		} else {
			if err := NewIgnorer(root, nil).EnsureDirectory("unreadable"); err == nil {
				t.Error("unreadable rule file did not block its subtree")
			}
		}
	}

	// A synchronization root that doesn't exist doesn't cause problems.
	expectIgnored(t, NewIgnorer(filepath.Join(root, "nonexistent"), nil), "a", false, false)
}

// TestExcludeFileLocation tests that the top level of a repository's working
// tree is recognized, and that the exclude file of its repository is found
// correctly.
func TestExcludeFileLocation(t *testing.T) {
	// Create a function that sets up a directory and returns the expected
	// location, along with whether or not the directory is expected to be the
	// top level of a repository.
	tests := []struct {
		name       string
		repository bool
		setup      func(t *testing.T, root string) string
	}{
		{"no repository", false, func(t *testing.T, root string) string {
			return ""
		}},
		{"directory", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git/HEAD": ""})
			return filepath.Join(root, ".git", "info", "exclude")
		}},
		{"relative Git file", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir: modules/sub\n"})
			return filepath.Join(root, "modules", "sub", "info", "exclude")
		}},
		{"relative Git file with parent reference", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir: ../repository/.git/modules/sub\n"})
			return filepath.Join(filepath.Dir(root), "repository", ".git", "modules", "sub", "info", "exclude")
		}},
		{"absolute Git file", true, func(t *testing.T, root string) string {
			target := filepath.Join(t.TempDir(), "gitdir")
			writeTree(t, root, map[string]string{".git": "gitdir: " + filepath.ToSlash(target) + "\n"})
			return filepath.Join(target, "info", "exclude")
		}},
		{"Git file with Windows line ending", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir: modules/sub\r\n"})
			return filepath.Join(root, "modules", "sub", "info", "exclude")
		}},
		{"Git file without space", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir:modules/sub"})
			return filepath.Join(root, "modules", "sub", "info", "exclude")
		}},
		{"Git file with trailing lines", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir: modules/sub\nsomething else\n"})
			return filepath.Join(root, "modules", "sub", "info", "exclude")
		}},
		{"relative common directory", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{
				".git": "gitdir: repository/.git/worktrees/work\n",
				"repository/.git/worktrees/work/commondir": "../..\n",
			})
			return filepath.Join(root, "repository", ".git", "info", "exclude")
		}},
		{"absolute common directory", true, func(t *testing.T, root string) string {
			common := filepath.Join(t.TempDir(), "common")
			writeTree(t, root, map[string]string{
				".git":               "gitdir: git/work\n",
				"git/work/commondir": filepath.ToSlash(common) + "\n",
			})
			return filepath.Join(common, "info", "exclude")
		}},
		{"empty common directory", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir: git/work\n", "git/work/commondir": "\n"})
			return filepath.Join(root, "git", "work", "info", "exclude")
		}},
		{"invalid Git file", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "something else\n"})
			return ""
		}},
		{"empty Git file", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": ""})
			return ""
		}},
		{"Git file without a path", true, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir:\n"})
			return ""
		}},
		{"Git symbolic link", false, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{"elsewhere/.git/HEAD": ""})
			if err := os.Symlink("elsewhere/.git", filepath.Join(root, ".git")); err != nil {
				t.Skip("unable to create symbolic link:", err)
			}
			return ""
		}},
		{"nested repositories are not consulted", false, func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{"sub/.git/info/exclude": "*\n"})
			return ""
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "root")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal("unable to create root:", err)
			}
			expected := test.setup(t, root)
			repository, actual, err := excludeFileLocation(root)
			if err != nil {
				t.Fatal("unable to locate exclude file:", err)
			} else if actual != expected {
				t.Errorf("location did not match expected: %q != %q", actual, expected)
			}
			if repository != test.repository {
				t.Errorf("repository status did not match expected: %v != %v", repository, test.repository)
			}
		})
	}
}

// TestIgnoreExclude tests that the exclude file is read from the appropriate
// location and applied.
func TestIgnoreExclude(t *testing.T) {
	// Test with a Git directory. The exclude file applies throughout the
	// root repository, but not in a nested repository, which has its own.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/info/exclude":     "# comment\n*.log\n/top\nnested/\n",
		"plain/.gitignore":      "!keep.log\n",
		"sub/.git/info/exclude": "*\n",
		"sub/.gitignore":        "!keep.log\n",
	})
	ignorer := NewIgnorer(root, nil)
	expectIgnored(t, ignorer, "a.log", false, true)
	expectIgnored(t, ignorer, "plain/a.log", false, true)
	expectIgnored(t, ignorer, "plain/keep.log", false, false)
	expectIgnored(t, ignorer, "top", false, true)
	expectIgnored(t, ignorer, "plain/top", false, false)
	expectIgnored(t, ignorer, "plain/nested", true, true)
	expectIgnored(t, ignorer, "plain/other", false, false)
	expectIgnored(t, ignorer, "sub", true, false)
	expectIgnored(t, ignorer, "sub/a.log", false, true)
	expectIgnored(t, ignorer, "sub/keep.log", false, false)
	expectIgnored(t, ignorer, "sub/other", false, true)

	// Test with a Git file that references a common directory.
	root = t.TempDir()
	writeTree(t, root, map[string]string{
		"work/.git": "gitdir: ../repository/.git/worktrees/work\n",
		"repository/.git/worktrees/work/commondir":    "../..\n",
		"repository/.git/worktrees/work/info/exclude": "worktree\n",
		"repository/.git/info/exclude":                "common\n",
	})
	ignorer = NewIgnorer(filepath.Join(root, "work"), nil)
	expectIgnored(t, ignorer, "common", false, true)
	expectIgnored(t, ignorer, "worktree", false, false)
}

// check is an expected result of evaluating a path.
type check struct {
	// path is the root-relative path.
	path string
	// directory indicates whether or not the path is evaluated as a directory.
	directory bool
	// ignored indicates whether or not the path is expected to be ignored.
	ignored bool
}

// TestNestedRepositories tests that the rules of the directories above the top
// level of a nested repository's working tree, which a ".git" entry
// identifies, don't apply beneath it, and that the exclude file of the nested
// repository applies there instead.
func TestNestedRepositories(t *testing.T) {
	tests := []struct {
		name string
		// files are the files to create.
		files map[string]string
		// directories are the empty directories to create.
		directories []string
		// checks are the expected results.
		checks []check
	}{
		{
			name: "rules above don't apply beneath a nested repository",
			files: map[string]string{
				".gitignore":        "*.log\n/top\nd/x\n**/deep\nsub/keep\n",
				".git/info/exclude": "*.tmp\n",
				"sub/.git/HEAD":     "",
				"plain/x":           "",
			},
			checks: []check{
				{"x.log", false, true},
				{"a.tmp", false, true},
				{"plain/x.log", false, true},
				{"plain/a.tmp", false, true},
				{"plain/d/deep", false, true},
				{"sub", true, false},
				{"sub/x.log", false, false},
				{"sub/a.tmp", false, false},
				{"sub/d/x.log", false, false},
				{"sub/d/e/x.log", false, false},
				{"sub/top", false, false},
				{"sub/d/x", false, false},
				{"sub/d/deep", false, false},
				{"sub/keep", false, false},
				{"missing/x.log", false, true},
			},
		},
		{
			name: "rules above still decide about the nested repository's directory",
			files: map[string]string{
				".gitignore":      "sub/\n/other\n**/deep/\n",
				"sub/.git/HEAD":   "",
				"other/.git/HEAD": "",
				"d/deep/.git":     "gitdir: ../../.git/modules/deep\n",
				"d/kept/.git":     "gitdir: ../../.git/modules/kept\n",
			},
			checks: []check{
				{"sub", true, true},
				{"other", true, true},
				{"d/deep", true, true},
				{"d/kept", true, false},
				{"sub/sub", true, false},
				{"sub/other", true, false},
				{"sub/x", false, false},
				{"other/x", false, false},
				{"d/deep/deep", true, false},
			},
		},
		{
			name: "the nested repository's exclude file applies relative to it",
			files: map[string]string{
				"sub/.git/info/exclude": "/top\nd/x\n*.log\n",
			},
			checks: []check{
				{"top", false, false},
				{"d/x", false, false},
				{"x.log", false, false},
				{"other/x.log", false, false},
				{"sub/top", false, true},
				{"sub/d/top", false, false},
				{"sub/d/x", false, true},
				{"sub/e/d/x", false, false},
				{"sub/x.log", false, true},
				{"sub/d/x.log", false, true},
			},
		},
		{
			name: "the nested repository's rule files take precedence over its exclude file",
			files: map[string]string{
				".gitignore":            "!x.log\n!foo\n",
				"sub/.git/info/exclude": "*.log\nfoo\n",
				"sub/.gitignore":        "!keep.log\n",
				"sub/d/.gitignore":      "!foo\n/bar\n",
			},
			checks: []check{
				{"sub/x.log", false, true},
				{"sub/d/x.log", false, true},
				{"sub/keep.log", false, false},
				{"sub/d/keep.log", false, false},
				{"sub/foo", false, true},
				{"sub/d/foo", false, false},
				{"sub/d/bar", false, true},
				{"sub/bar", false, false},
			},
		},
		{
			name: "a Git file pointing into the root repository's Git directory",
			files: map[string]string{
				".gitignore":                    "*.log\n",
				".git/info/exclude":             "*.tmp\n",
				".git/modules/sub/info/exclude": "*.bak\n",
				"sub/.git":                      "gitdir: ../.git/modules/sub\n",
			},
			checks: []check{
				{"x.log", false, true},
				{"x.tmp", false, true},
				{"x.bak", false, false},
				{"sub/x.log", false, false},
				{"sub/x.tmp", false, false},
				{"sub/x.bak", false, true},
				{"sub/d/x.bak", false, true},
			},
		},
		{
			name: "a Git file pointing to a Git directory with a common directory",
			files: map[string]string{
				"wt/.git":                             "gitdir: ../repo/.git/worktrees/wt\n",
				"repo/.git/worktrees/wt/commondir":    "../..\n",
				"repo/.git/worktrees/wt/info/exclude": "worktree\n",
				"repo/.git/info/exclude":              "common\n",
			},
			checks: []check{
				{"common", false, false},
				{"wt/common", false, true},
				{"wt/worktree", false, false},
				{"repo/common", false, true},
				{"repo/worktree", false, false},
			},
		},
		{
			name: "Git entries that Git wouldn't accept",
			files: map[string]string{
				".gitignore":    "*.log\n",
				"invalid/.git":  "nonsense\n",
				"empty/.git":    "",
				"dangling/.git": "gitdir: ../nowhere\n",
				"modules/.git":  "gitdir: ../.git/modules/missing\n",
			},
			directories: []string{"directory/.git"},
			checks: []check{
				{"invalid/x.log", false, false},
				{"empty/x.log", false, false},
				{"dangling/x.log", false, false},
				{"modules/x.log", false, false},
				{"directory/x.log", false, false},
				{"x.log", false, true},
			},
		},
		{
			name: "nested repositories within nested repositories",
			files: map[string]string{
				".gitignore":                  "*.a\n",
				".git/info/exclude":           "*.e\n",
				"sub/.git/info/exclude":       "*.b\n",
				"sub/.gitignore":              "*.c\ninner2/\n",
				"sub/inner/.git/info/exclude": "*.d\n",
				"sub/inner2/.git/HEAD":        "",
			},
			checks: []check{
				{"x.a", false, true},
				{"x.b", false, false},
				{"x.c", false, false},
				{"x.e", false, true},
				{"sub/x.a", false, false},
				{"sub/x.b", false, true},
				{"sub/x.c", false, true},
				{"sub/x.d", false, false},
				{"sub/x.e", false, false},
				{"sub/inner", true, false},
				{"sub/inner2", true, true},
				{"sub/inner/x.a", false, false},
				{"sub/inner/x.b", false, false},
				{"sub/inner/x.c", false, false},
				{"sub/inner/x.d", false, true},
				{"sub/inner/x.e", false, false},
				{"sub/inner/deep/x.c", false, false},
				{"sub/inner/deep/x.d", false, true},
				{"sub/plain/x.c", false, true},
				{"sub/plain/x.d", false, false},
			},
		},
		{
			name: "directories without a Git entry inherit",
			files: map[string]string{
				".gitignore":          "*.log\n",
				"a/.gitignore":        "*.tmp\n",
				"a/b/x":               "",
				"a/.gitx/HEAD":        "",
				"a/git/HEAD":          "",
				"a/x.git/HEAD":        "",
				"a/.gitignore2/.gitx": "",
			},
			checks: []check{
				{"a/x.log", false, true},
				{"a/b/x.log", false, true},
				{"a/b/x.tmp", false, true},
				{"a/.gitx/x.log", false, true},
				{"a/git/x.log", false, true},
				{"a/x.git/x.log", false, true},
				{"a/.gitignore2/x.tmp", false, true},
				{"x.tmp", false, false},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create the tree.
			root := t.TempDir()
			writeTree(t, root, test.files)
			for _, directory := range test.directories {
				if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(directory)), 0o755); err != nil {
					t.Fatal("unable to create directory:", err)
				}
			}

			// Evaluate the paths in order and in reverse order, so that the
			// result doesn't depend on which directories are loaded first.
			ignorer := NewIgnorer(root, nil)
			for _, c := range test.checks {
				expectIgnored(t, ignorer, c.path, c.directory, c.ignored)
			}
			ignorer = NewIgnorer(root, nil)
			for index := len(test.checks) - 1; index >= 0; index-- {
				c := test.checks[index]
				expectIgnored(t, ignorer, c.path, c.directory, c.ignored)
			}

			// There are no failures to read rules.
			for _, c := range test.checks {
				directory := c.path
				if !c.directory {
					directory = directory[:max(strings.LastIndexByte(directory, '/'), 0)]
				}
				if err := NewIgnorer(root, nil).EnsureDirectory(directory); err != nil {
					t.Errorf("unable to load rules for %q: %v", directory, err)
				}
			}
		})
	}
}

// TestNestedRepositoryExclusion tests that rules above a nested repository
// exclude its contents by excluding its directory.
func TestNestedRepositoryExclusion(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":              "sub/\n",
		"sub/.git/HEAD":           "",
		"other/.git/info/exclude": "*.log\n",
	})
	for path, expected := range map[string]string{
		"sub":             "sub",
		"sub/x":           "sub",
		"sub/d/x":         "sub",
		"other/x":         "",
		"other/x.log":     "other/x.log",
		"other/d/x.log":   "other/d/x.log",
		"other/sub/x.log": "other/sub/x.log",
		"other/sub/x":     "",
	} {
		if excluded, err := ignore.Excluded(NewIgnorer(root, nil), path, path == "sub"); err != nil {
			t.Errorf("path %q: unable to evaluate: %v", path, err)
		} else if excluded != expected {
			t.Errorf("path %q: exclusion did not match expected: %q != %q", path, excluded, expected)
		}
	}
}

// TestNestedRepositorySession tests that the session ignorer takes precedence
// over the rules of a nested repository, and that Git entries are ignored in
// nested repositories, as they are everywhere.
func TestNestedRepositorySession(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":            "*.log\n",
		"sub/.git/info/exclude": "*.tmp\n",
		"sub/.gitignore":        "!.git\n",
		"sub/inner/.git/HEAD":   "",
	})
	session := sessionIgnorer(func(path string, _ bool) (ignore.IgnoreStatus, bool) {
		switch path {
		case "sub/keep.tmp", "sub/.git", "sub/inner/.git":
			return ignore.IgnoreStatusUnignored, false
		case "sub/x.txt", "sub/x.log":
			return ignore.IgnoreStatusIgnored, false
		}
		return ignore.IgnoreStatusNominal, false
	})
	ignorer := NewIgnorer(root, session)
	tests := []struct {
		path     string
		expected ignore.IgnoreStatus
	}{
		{"sub/keep.tmp", ignore.IgnoreStatusUnignored},
		{"sub/x.tmp", ignore.IgnoreStatusIgnored},
		{"sub/x.txt", ignore.IgnoreStatusIgnored},
		{"sub/x.log", ignore.IgnoreStatusIgnored},
		{"sub/y.log", ignore.IgnoreStatusNominal},
		{"sub/.git", ignore.IgnoreStatusIgnored},
		{"sub/inner/.git", ignore.IgnoreStatusIgnored},
		{"sub/inner/x.tmp", ignore.IgnoreStatusNominal},
	}
	for _, test := range tests {
		for _, directory := range []bool{false, true} {
			if status, _ := ignorer.Ignore(test.path, directory); status != test.expected {
				t.Errorf("path %q (directory: %v): status did not match expected: %d != %d", test.path, directory, status, test.expected)
			}
		}
	}
}

// TestNestedRepositoryReadFailures tests that a failure to read rules above a
// nested repository applies beneath it too, and that a failure to read the
// rules of a nested repository applies to its subtree, but not beyond it.
func TestNestedRepositoryReadFailures(t *testing.T) {
	tests := []struct {
		// file is the file that's made too large to be read.
		file string
		// git is the contents of the nested repository's Git file, if it has
		// one rather than a Git directory.
		git string
		// blocked are the directories whose rules can't be loaded.
		blocked []string
		// unaffected are the directories whose rules can still be loaded.
		unaffected []string
	}{
		{".gitignore", "", []string{"", "a", "a/sub", "a/sub/d"}, nil},
		{".git/info/exclude", "", []string{"", "a", "a/sub", "a/sub/d"}, nil},
		{"a/.gitignore", "", []string{"a", "a/sub", "a/sub/d"}, []string{"", "b"}},
		{"a/sub/.gitignore", "", []string{"a/sub", "a/sub/d"}, []string{"", "a", "a/other", "b"}},
		{"a/sub/.git/info/exclude", "", []string{"a/sub", "a/sub/d"}, []string{"", "a", "a/other", "b"}},
		{"a/sub/.git", "gitdir: ../../modules/sub\n", []string{"a/sub", "a/sub/d"}, []string{"", "a", "a/other", "b"}},
		{"modules/sub/info/exclude", "gitdir: ../../modules/sub\n", []string{"a/sub", "a/sub/d"}, []string{"", "a", "a/other", "modules"}},
		{"modules/sub/commondir", "gitdir: ../../modules/sub\n", []string{"a/sub", "a/sub/d"}, []string{"", "a", "a/other", "modules"}},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			// Create the tree, with a nested repository whose rules are fine
			// unless they're the ones that can't be read.
			root := t.TempDir()
			files := map[string]string{
				"a/sub/d/x": "",
				"a/other/x": "",
				"b/x":       "",
			}
			if test.git == "" {
				files["a/sub/.git/info/exclude"] = "*.tmp\n"
			} else {
				files["a/sub/.git"] = test.git
				files["modules/sub/info/exclude"] = "*.tmp\n"
			}
			files[test.file] = "secret\n"
			writeTree(t, root, files)
			limit := int64(maximumRuleFileSize)
			if path.Base(test.file) == ".git" || path.Base(test.file) == "commondir" {
				limit = maximumPointerFileSize
			}
			if err := os.Truncate(filepath.Join(root, filepath.FromSlash(test.file)), limit+1); err != nil {
				t.Fatal(err)
			}

			// Check which directories are affected, with an ignorer for each
			// directory, so that each loads its chain from scratch.
			for _, directory := range test.blocked {
				if err := NewIgnorer(root, nil).EnsureDirectory(directory); err == nil {
					t.Errorf("rules for %q loaded despite read failure", directory)
				}
			}
			for _, directory := range test.unaffected {
				if err := NewIgnorer(root, nil).EnsureDirectory(directory); err != nil {
					t.Errorf("rules for %q affected by read failure: %v", directory, err)
				}
			}
		})
	}
}

// TestNestedRepositoryUnreadableExclude tests that a nested repository's
// exclude file that can't be read blocks the repository's subtree.
func TestNestedRepositoryUnreadableExclude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permissions can't make files unreadable on Windows")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{"sub/.git/info/exclude": "*\n", "sub/d/x": ""})
	exclude := filepath.Join(root, "sub", ".git", "info", "exclude")
	if err := os.Chmod(exclude, 0); err != nil {
		t.Fatal("unable to change permissions:", err)
	} else if file, err := os.Open(exclude); err == nil {
		file.Close()
		t.Skip("unable to make exclude file unreadable")
	}
	ignorer := NewIgnorer(root, nil)
	if err := ignorer.EnsureDirectory(""); err != nil {
		t.Error("unreadable nested exclude file blocked the root:", err)
	}
	for _, directory := range []string{"sub", "sub/d"} {
		if err := ignorer.EnsureDirectory(directory); err == nil {
			t.Errorf("unreadable nested exclude file did not block %q", directory)
		}
	}
}

// TestNestedRepositorySymbolicLinks tests that a symbolic link named ".git"
// doesn't make its directory the top level of a nested repository, and that a
// symbolic link to a nested repository's directory doesn't either, since rule
// files are never read through symbolic links.
func TestNestedRepositorySymbolicLinks(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":             "*.log\n",
		"store/gd/HEAD":          "",
		"store/gd/info/exclude":  "*.tmp\n",
		"link/x":                 "",
		"real/.git/info/exclude": "*.tmp\n",
	})
	if err := os.Symlink(filepath.Join("..", "store", "gd"), filepath.Join(root, "link", ".git")); err != nil {
		t.Skip("unable to create symbolic link:", err)
	} else if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal("unable to create symbolic link:", err)
	}
	ignorer := NewIgnorer(root, nil)
	expectIgnored(t, ignorer, "link/.git", false, true)
	expectIgnored(t, ignorer, "link/x.log", false, true)
	expectIgnored(t, ignorer, "link/x.tmp", false, false)
	expectIgnored(t, ignorer, "link/d/x.log", false, true)
	expectIgnored(t, ignorer, "real/x.log", false, false)
	expectIgnored(t, ignorer, "real/x.tmp", false, true)
	expectIgnored(t, ignorer, "alias/x.log", false, true)
	expectIgnored(t, ignorer, "alias/x.tmp", false, false)
	for _, directory := range []string{"link", "link/d", "real", "alias", "alias/d"} {
		if err := ignorer.EnsureDirectory(directory); err != nil {
			t.Errorf("unable to load rules for %q: %v", directory, err)
		}
	}
}

// TestNestedRepositoryGitEntryCase tests that a Git entry whose name differs
// from ".git" in case identifies a nested repository if and only if the
// filesystem finds it by that name, as Git would.
func TestNestedRepositoryGitEntryCase(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{".gitignore": "*.log\n", "sub/.GIT/info/exclude": "*.tmp\n"})
	_, err := os.Lstat(filepath.Join(root, "sub", ".git"))
	caseInsensitive := err == nil
	ignorer := NewIgnorer(root, nil)
	expectIgnored(t, ignorer, "sub/.GIT", true, true)
	expectIgnored(t, ignorer, "sub/x.log", false, !caseInsensitive)
	expectIgnored(t, ignorer, "sub/x.tmp", false, caseInsensitive)
}

// TestNestedRepositoryMemoization tests that a nested repository's rules are
// read lazily and only once.
func TestNestedRepositoryMemoization(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"sub/.git/info/exclude": "a\n", "other/x": ""})
	ignorer := NewIgnorer(root, nil)
	expectIgnored(t, ignorer, "sub/a", false, true)
	expectIgnored(t, ignorer, "other/a", false, false)

	// Rewrite the exclude file, and turn another directory into a nested
	// repository. Neither change has an effect on the existing ignorer.
	writeTree(t, root, map[string]string{"sub/.git/info/exclude": "b\n", "other/.git/info/exclude": "a\n"})
	expectIgnored(t, ignorer, "sub/a", false, true)
	expectIgnored(t, ignorer, "sub/b", false, false)
	expectIgnored(t, ignorer, "other/a", false, false)

	// A new ignorer sees the changes.
	ignorer = NewIgnorer(root, nil)
	expectIgnored(t, ignorer, "sub/a", false, false)
	expectIgnored(t, ignorer, "sub/b", false, true)
	expectIgnored(t, ignorer, "other/a", false, true)
}

// TestIgnoreSessionTraversalEndToEnd tests that traversal directives from the
// session ignorer are retained alongside rule file results.
func TestIgnoreSessionTraversalEndToEnd(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{".gitignore": "build/\n"})
	session := sessionIgnorer(func(path string, directory bool) (ignore.IgnoreStatus, bool) {
		return ignore.IgnoreStatusNominal, directory
	})
	ignorer := NewIgnorer(root, session)
	if status, continueTraversal := ignorer.Ignore("build", true); status != ignore.IgnoreStatusIgnored || !continueTraversal {
		t.Errorf("unexpected result for ignored directory: (%d, %v)", status, continueTraversal)
	}
	if status, continueTraversal := ignorer.Ignore("build", false); status != ignore.IgnoreStatusNominal || continueTraversal {
		t.Errorf("unexpected result for file: (%d, %v)", status, continueTraversal)
	}
	if status, continueTraversal := ignorer.Ignore("src", true); status != ignore.IgnoreStatusNominal || !continueTraversal {
		t.Errorf("unexpected result for directory: (%d, %v)", status, continueTraversal)
	}
}

// benchmarkRules generates a rule file of about one hundred mixed rules.
func benchmarkRules() string {
	var rules string
	for i := 0; i < 12; i++ {
		rules += fmt.Sprintf("name%d\n", i)
		rules += fmt.Sprintf("*.ext%d\n", i)
		rules += fmt.Sprintf("/top%d/\n", i)
		rules += fmt.Sprintf("**/cache%d/\n", i)
		rules += fmt.Sprintf("docs/**/*.tmp%d\n", i)
		rules += fmt.Sprintf("[Tt]humbs%d.db\n", i)
		rules += fmt.Sprintf("out%d-*-[0-9]\n", i)
		rules += fmt.Sprintf("!keep%d.ext%d\n", i, i)
	}
	return rules
}

// BenchmarkIgnore benchmarks Ignore with a rule file of about one hundred
// mixed rules, in the root and in a subdirectory, for a mixture of paths that
// do and don't match.
func BenchmarkIgnore(b *testing.B) {
	// Create the rules.
	root := b.TempDir()
	writeTree(b, root, map[string]string{
		".gitignore":         benchmarkRules(),
		"src/lib/.gitignore": "*.generated.go\n/gen/\nvendor/**/*.tmp\n",
		".git/info/exclude":  "*.swp\n.idea/\n",
	})

	// Define the paths to test.
	type entry struct {
		path      string
		directory bool
	}
	entries := []entry{
		{"README.md", false},
		{"main.go", false},
		{"src", true},
		{"src/main.go", false},
		{"src/lib", true},
		{"src/lib/util.go", false},
		{"src/lib/util_test.go", false},
		{"src/lib/parser.generated.go", false},
		{"src/lib/internal", true},
		{"src/lib/internal/deep/er/file.txt", false},
		{"docs", true},
		{"docs/guide/index.md", false},
		{"docs/guide/page.tmp3", false},
		{"name7", false},
		{"pkg/name7", true},
		{"a.ext4", false},
		{"pkg/b.ext11", false},
		{"keep4.ext4", false},
		{"top3", true},
		{"pkg/top3", true},
		{"pkg/cache5", true},
		{"pkg/Thumbs2.db", false},
		{"pkg/out3-abc-7", false},
		{"pkg/out3-abc-x", false},
		{".idea", true},
		{"pkg/file.swp", false},
		{"node_modules/package/index.js", false},
		{"build/output/lib.o", false},
		{"assets/images/logo.png", false},
		{"tests/data/fixtures/case.json", false},
	}

	// Run the benchmark.
	ignorer := NewIgnorer(root, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := entries[i%len(entries)]
		ignorer.Ignore(e.path, e.directory)
	}
}

// TestRuleReadFailures covers failures independently of process privileges and
// verifies that an error is retained for descendants until the next scan.
func TestRuleReadFailures(t *testing.T) {
	for _, path := range []string{".gitignore", "nested/.gitignore", ".git/info/exclude", ".git", "gitdir/commondir"} {
		t.Run(path, func(t *testing.T) {
			root := t.TempDir()
			if path == "gitdir/commondir" {
				writeTree(t, root, map[string]string{".git": "gitdir: gitdir\n"})
			}
			writeTree(t, root, map[string]string{path: "secret.env\n"})
			fullPath := filepath.Join(root, filepath.FromSlash(path))
			limit := int64(maximumRuleFileSize)
			if path == ".git" || path == "gitdir/commondir" {
				limit = maximumPointerFileSize
			}
			if err := os.Truncate(fullPath, limit+1); err != nil {
				t.Fatal(err)
			}
			ignorer := NewIgnorer(root, nil)
			blocked := ""
			if path == "nested/.gitignore" {
				blocked = "nested"
			}
			if err := ignorer.EnsureDirectory(blocked); err == nil {
				t.Fatal("oversized rules accepted")
			}
			if err := ignorer.EnsureDirectory(blocked + "/child"); err == nil {
				t.Error("error not inherited")
			}
			if err := os.Remove(fullPath); err != nil {
				t.Fatal(err)
			}
			if err := ignorer.EnsureDirectory(blocked); err == nil {
				t.Error("read failure not retained")
			}
			if err := NewIgnorer(root, nil).EnsureDirectory(blocked); err != nil {
				t.Error("missing file treated as failure:", err)
			}
		})
	}
}

// TestIgnorerSymbolicLinkDirectories tests that rules aren't read through
// symbolic links, which scans don't traverse, but which can be in the way of a
// directory that a transition is about to create. The rules of the parents of a
// symbolic link still apply beneath it.
func TestIgnorerSymbolicLinkDirectories(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":               "*.log\n",
		"elsewhere/.gitignore":     "secret\n",
		"elsewhere/sub/.gitignore": "deep\n",
		"real/.gitignore":          "local\n",
	})
	for link, target := range map[string]string{
		"link":      "elsewhere",
		"loop":      "loop",
		"dangling":  "missing",
		"real/link": "../elsewhere",
	} {
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(link))); err != nil {
			t.Skip("unable to create symbolic link:", err)
		}
	}

	// There are no failures to read rules, regardless of the order in which
	// directories are visited. A cycle would otherwise cause one.
	for _, directories := range [][]string{
		{"link", "link/sub", "loop", "loop/sub", "dangling", "dangling/sub", "real/link", "real/link/sub"},
		{"real/link/sub", "real/link", "dangling/sub", "dangling", "loop/sub", "loop", "link/sub", "link"},
	} {
		ignorer := NewIgnorer(root, nil)
		for _, directory := range directories {
			if err := ignorer.EnsureDirectory(directory); err != nil {
				t.Errorf("unable to load rules for %s: %v", directory, err)
			}
		}
	}

	// The rules at the targets don't apply, but those of the parents do.
	ignorer := NewIgnorer(root, nil)
	expectIgnored(t, ignorer, "link/secret", false, false)
	expectIgnored(t, ignorer, "link/sub/deep", false, false)
	expectIgnored(t, ignorer, "loop/secret", false, false)
	expectIgnored(t, ignorer, "real/link/secret", false, false)
	expectIgnored(t, ignorer, "link/out.log", false, true)
	expectIgnored(t, ignorer, "link/sub/out.log", false, true)
	expectIgnored(t, ignorer, "loop/out.log", false, true)
	expectIgnored(t, ignorer, "dangling/out.log", false, true)
	expectIgnored(t, ignorer, "real/link/local", false, true)

	// The rules still apply at the targets themselves.
	expectIgnored(t, ignorer, "elsewhere/secret", false, true)
	expectIgnored(t, ignorer, "elsewhere/sub/deep", false, true)
}

// TestPathsThatLeaveRoot tests that a path that a scan could never reach can't
// cause rule files outside of the root to be read, and that names that merely
// resemble the ones that do this aren't affected.
func TestPathsThatLeaveRoot(t *testing.T) {
	// Create rules outside of the root that would exclude the name that's used
	// in the paths below, and rules inside of the root that don't.
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	tree := map[string]string{
		".gitignore":          "secret\n",
		"outside/.gitignore":  "secret\n",
		"root/.gitignore":     "public\n",
		"root/a/b/file":       "file",
		"root/..x/.gitignore": "secret\n",
	}
	dotted := []string{"..x/secret"}

	// Windows doesn't have directories whose names consist of dots.
	if runtime.GOOS != "windows" {
		tree["root/.../.gitignore"] = "secret\n"
		dotted = append(dotted, ".../secret")
	}
	writeTree(t, parent, tree)

	// Nothing but the root's rules apply to these, whatever they refer to.
	separator := string(filepath.Separator)
	for _, path := range []string{
		"../secret",
		"../outside/secret",
		"a/../../secret",
		"a/b/../../../secret",
		"../../" + filepath.Base(parent) + "/secret",
		"./../secret",
		".." + separator + "outside/secret",
	} {
		ignorer := NewIgnorer(root, nil)
		if excluded, err := ignore.Excluded(ignorer, path, false); err != nil {
			t.Errorf("path %q: unable to evaluate: %v", path, err)
		} else if excluded != "" {
			t.Errorf("path %q: rules outside of the root used to exclude %q", path, excluded)
		}
	}

	// The root's rules do apply to them.
	if excluded, err := ignore.Excluded(NewIgnorer(root, nil), "../public", false); err != nil {
		t.Error("unable to evaluate:", err)
	} else if excluded != "../public" {
		t.Error("rules of the root not used:", excluded)
	}

	// Names that merely begin with dots are ordinary names.
	for _, path := range dotted {
		if excluded, err := ignore.Excluded(NewIgnorer(root, nil), path, false); err != nil {
			t.Errorf("path %q: unable to evaluate: %v", path, err)
		} else if excluded != path {
			t.Errorf("path %q: rules of directory not used: %q", path, excluded)
		}
	}

	// A backslash is only special where it's a path separator.
	if runtime.GOOS != "windows" {
		writeTree(t, root, map[string]string{`b\c/.gitignore`: "secret\n"})
		if excluded, err := ignore.Excluded(NewIgnorer(root, nil), `b\c/secret`, false); err != nil {
			t.Error("unable to evaluate:", err)
		} else if excluded != `b\c/secret` {
			t.Error("rules of directory with a backslash in its name not used:", excluded)
		}
	}
}
