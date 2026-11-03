package gitignore

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// TestIsRulePath tests that IsRulePath behaves as expected.
func TestIsRulePath(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{".gitignore", true},
		{"a/.gitignore", true},
		{"a/b/.gitignore", true},
		{".git/info/exclude", true},
		{"", false},
		{"a", false},
		{"gitignore", false},
		{".Gitignore", false},
		{".gitignore2", false},
		{"x.gitignore", false},
		{"a/x.gitignore", false},
		{".gitignore/x", false},
		{".git", false},
		{".git/info", false},
		{".git/info/Exclude", false},
		{".git/info/exclude/x", false},
		{".git/config", false},
		{"a/.git/info/exclude", false},
		{"a/.gitignore/.git/info/exclude", false},
	}
	for _, test := range tests {
		if IsRulePath(test.path) != test.expected {
			t.Errorf("path %q: result did not match expected: %v != %v", test.path, !test.expected, test.expected)
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

// TestIgnoreNonUTF8 tests that the ignorer evaluates names that aren't valid
// UTF-8, whose bytes rules match as they match any others.
func TestIgnoreNonUTF8(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{".gitignore": "*.log\nd\xff/\n[!a]\xe9\n"})
	ignorer := NewIgnorer(root, nil)
	if evaluator, ok := ignorer.(ignore.NonUTF8Ignorer); !ok || !evaluator.EvaluatesNonUTF8() {
		t.Fatal("ignorer doesn't evaluate names that aren't valid UTF-8")
	}
	expectIgnored(t, ignorer, "caf\xe9.log", false, true)
	expectIgnored(t, ignorer, "caf\xe9.txt", false, false)
	expectIgnored(t, ignorer, "d\xff", true, true)
	expectIgnored(t, ignorer, "d\xff", false, false)
	expectIgnored(t, ignorer, "b\xe9", false, true)
	expectIgnored(t, ignorer, "a\xe9", false, false)
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

// TestExcludeFileLocation tests that the exclude file of a repository is found
// correctly.
func TestExcludeFileLocation(t *testing.T) {
	// Create a function that sets up a root and returns the expected location.
	tests := []struct {
		name  string
		setup func(t *testing.T, root string) string
	}{
		{"no repository", func(t *testing.T, root string) string {
			return ""
		}},
		{"directory", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git/HEAD": ""})
			return filepath.Join(root, ".git", "info", "exclude")
		}},
		{"relative Git file", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir: modules/sub\n"})
			return filepath.Join(root, "modules", "sub", "info", "exclude")
		}},
		{"relative Git file with parent reference", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir: ../repository/.git/modules/sub\n"})
			return filepath.Join(filepath.Dir(root), "repository", ".git", "modules", "sub", "info", "exclude")
		}},
		{"absolute Git file", func(t *testing.T, root string) string {
			target := filepath.Join(t.TempDir(), "gitdir")
			writeTree(t, root, map[string]string{".git": "gitdir: " + filepath.ToSlash(target) + "\n"})
			return filepath.Join(target, "info", "exclude")
		}},
		{"Git file with Windows line ending", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir: modules/sub\r\n"})
			return filepath.Join(root, "modules", "sub", "info", "exclude")
		}},
		{"Git file without space", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir:modules/sub"})
			return filepath.Join(root, "modules", "sub", "info", "exclude")
		}},
		{"Git file with trailing lines", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir: modules/sub\nsomething else\n"})
			return filepath.Join(root, "modules", "sub", "info", "exclude")
		}},
		{"relative common directory", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{
				".git": "gitdir: repository/.git/worktrees/work\n",
				"repository/.git/worktrees/work/commondir": "../..\n",
			})
			return filepath.Join(root, "repository", ".git", "info", "exclude")
		}},
		{"absolute common directory", func(t *testing.T, root string) string {
			common := filepath.Join(t.TempDir(), "common")
			writeTree(t, root, map[string]string{
				".git":               "gitdir: git/work\n",
				"git/work/commondir": filepath.ToSlash(common) + "\n",
			})
			return filepath.Join(common, "info", "exclude")
		}},
		{"empty common directory", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir: git/work\n", "git/work/commondir": "\n"})
			return filepath.Join(root, "git", "work", "info", "exclude")
		}},
		{"invalid Git file", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "something else\n"})
			return ""
		}},
		{"empty Git file", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": ""})
			return ""
		}},
		{"Git file without a path", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{".git": "gitdir:\n"})
			return ""
		}},
		{"Git symbolic link", func(t *testing.T, root string) string {
			writeTree(t, root, map[string]string{"elsewhere/.git/HEAD": ""})
			if err := os.Symlink("elsewhere/.git", filepath.Join(root, ".git")); err != nil {
				t.Skip("unable to create symbolic link:", err)
			}
			return ""
		}},
		{"nested repositories are not consulted", func(t *testing.T, root string) string {
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
			if actual, err := excludeFileLocation(root); err != nil || actual != expected {
				t.Errorf("location did not match expected: %q != %q", actual, expected)
			}
		})
	}
}

// TestIgnoreExclude tests that the exclude file is read from the appropriate
// location and applied.
func TestIgnoreExclude(t *testing.T) {
	// Test with a Git directory.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/info/exclude":     "# comment\n*.log\n/top\nnested/\n",
		"sub/.git/info/exclude": "*\n",
		"sub/.gitignore":        "!keep.log\n",
	})
	ignorer := NewIgnorer(root, nil)
	expectIgnored(t, ignorer, "a.log", false, true)
	expectIgnored(t, ignorer, "sub/a.log", false, true)
	expectIgnored(t, ignorer, "sub/keep.log", false, false)
	expectIgnored(t, ignorer, "top", false, true)
	expectIgnored(t, ignorer, "sub/top", false, false)
	expectIgnored(t, ignorer, "sub/nested", true, true)
	expectIgnored(t, ignorer, "sub/other", false, false)

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
