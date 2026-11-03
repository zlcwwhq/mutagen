package gitignore

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// excludeCase is a test case for the root repository's exclude file.
type excludeCase struct {
	// name is the name of the test case.
	name string
	// exclude is the contents of the exclude file.
	exclude string
	// files maps the paths of rule files, relative to the root, to their
	// contents.
	files map[string]string
}

// excludeCases are the exclude file test cases.
var excludeCases = []excludeCase{
	{"literal", "a\n", nil},
	{"directory only", "b/\n", nil},
	{"anchored", "/d\n", nil},
	{"wildcard and negation", "*.txt\n!x.txt\n", nil},
	{"windows line endings", "a\r\nab\r\n", nil},
	{"byte order mark", "\xef\xbb\xbfa\n", nil},
	{"double asterisk", "e/**/a\n", nil},
	{"rooted at repository", "b/a\n", nil},
	{"empty", "", nil},
	{"comments", "# a\n\n", nil},
	{"ignore file negates", "a\n*.txt\n", map[string]string{".gitignore": "!a\n!x.txt\n"}},
	{"exclude negates", "!a\n", map[string]string{".gitignore": "a\n"}},
	{"nested ignore file negates", "a\nfoo\n", map[string]string{"d/.gitignore": "!a\n"}},
	{"nested ignore file ignores", "!a\n", map[string]string{"d/.gitignore": "a\n"}},
	{"exclude fills gaps", "ab\n", map[string]string{".gitignore": "a\n", "d/.gitignore": "foo\n"}},
	{"closest file with a match wins", "b/a/b\n", map[string]string{".gitignore": "!b/a/b\n", "b/.gitignore": "a/b\n"}},
}

// runExcludeCase writes an exclude file and rule files into a repository whose
// working tree is at root and whose exclude file is at the specified path, then
// compares the behavior of the ignorer to that of git.
func runExcludeCase(t *testing.T, h *gitHarness, root, excludePath string, c excludeCase) {
	t.Helper()

	// Create the files.
	createFixture(t, root)
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		t.Fatal("unable to create exclude file directory:", err)
	}
	if err := os.WriteFile(excludePath, []byte(c.exclude), 0o644); err != nil {
		t.Fatal("unable to write exclude file:", err)
	}
	for path, contents := range c.files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(contents), 0o644); err != nil {
			t.Fatal("unable to write rule file:", err)
		}
	}

	// Compare, and make sure that the exclude file actually had an effect (or
	// not) as intended, otherwise this test isn't testing what it should.
	ignored := compare(t, h, root, NewIgnorer(root, nil), func(string) string {
		return fmt.Sprintf("exclude: %q", c.exclude)
	})
	if excludeIsEmpty := c.name == "empty" || c.name == "comments"; excludeIsEmpty && ignored != 0 {
		t.Error("git ignored entries without any rules")
	} else if !excludeIsEmpty && ignored == 0 {
		t.Error("git did not ignore anything")
	}
}

// TestGitExcludeDirectory tests exclude files in repositories where the Git
// directory is a directory.
func TestGitExcludeDirectory(t *testing.T) {
	h := newGitHarness(t)
	for _, c := range excludeCases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			h.initialize(root)
			runExcludeCase(t, h, root, filepath.Join(root, ".git", "info", "exclude"), c)
		})
	}
}

// TestGitExcludeGitFile tests exclude files in repositories where the Git
// directory is located elsewhere and referenced by a ".git" file, using both
// absolute and relative references.
func TestGitExcludeGitFile(t *testing.T) {
	h := newGitHarness(t)
	for _, relative := range []bool{false, true} {
		for _, c := range excludeCases {
			name := c.name + " absolute"
			if relative {
				name = c.name + " relative"
			}
			t.Run(name, func(t *testing.T) {
				// Create a repository with a separate Git directory.
				base := t.TempDir()
				root := filepath.Join(base, "work")
				gitDirectory := filepath.Join(base, "separate", "git")
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal("unable to create working tree:", err)
				} else if err = os.MkdirAll(filepath.Dir(gitDirectory), 0o755); err != nil {
					t.Fatal("unable to create Git directory parent:", err)
				}
				h.run(root, nil, "init", "--quiet", "--separate-git-dir", gitDirectory)

				// Switch to a relative reference if requested.
				if relative {
					reference := "gitdir: ../separate/git\n"
					if err := os.WriteFile(filepath.Join(root, ".git"), []byte(reference), 0o644); err != nil {
						t.Fatal("unable to write Git file:", err)
					}
				}

				// Run the test.
				runExcludeCase(t, h, root, filepath.Join(gitDirectory, "info", "exclude"), c)
			})
		}
	}
}

// TestGitExcludeWorktree tests exclude files in linked worktrees, where the
// Git file references a Git directory that in turn references the common
// directory containing the exclude file.
func TestGitExcludeWorktree(t *testing.T) {
	h := newGitHarness(t)
	for _, c := range excludeCases {
		t.Run(c.name, func(t *testing.T) {
			// Create a repository and a linked worktree.
			base := t.TempDir()
			repository := filepath.Join(base, "main")
			if err := os.Mkdir(repository, 0o755); err != nil {
				t.Fatal("unable to create repository:", err)
			}
			h.initialize(repository)
			h.run(
				repository, nil,
				"-c", "user.name=Test", "-c", "user.email=test@example.com",
				"commit", "--quiet", "--allow-empty", "--message=initial",
			)
			root := filepath.Join(base, "work")
			h.run(repository, nil, "worktree", "add", "--quiet", "--detach", root)

			// Verify that the setup is what we expect.
			if info, err := os.Lstat(filepath.Join(root, ".git")); err != nil || !info.Mode().IsRegular() {
				t.Fatal("linked worktree does not have a Git file")
			}

			// Run the test, using the exclude file in the common directory.
			runExcludeCase(t, h, root, filepath.Join(repository, ".git", "info", "exclude"), c)
		})
	}
}

// TestGitSymbolicLinkRuleFile tests that rule files that are symbolic links
// are ignored, which is not something that we can rely on git to do on every
// version, so it's also checked explicitly.
func TestGitSymbolicLinkRuleFile(t *testing.T) {
	h := newGitHarness(t)
	root := t.TempDir()
	h.initialize(root)
	createFixture(t, root)
	if err := os.WriteFile(filepath.Join(root, "rules"), []byte("a\n"), 0o644); err != nil {
		t.Fatal("unable to write rules:", err)
	}
	if err := os.Symlink("rules", filepath.Join(root, ".gitignore")); err != nil {
		t.Skip("unable to create symbolic link:", err)
	}
	if err := os.Symlink("rules", filepath.Join(root, "d", ".gitignore")); err != nil {
		t.Fatal("unable to create symbolic link:", err)
	}
	ignorer := NewIgnorer(root, nil)
	for _, path := range []string{"a", "d/a", "b/a"} {
		if status, _ := ignorer.Ignore(path, false); status != ignore.IgnoreStatusNominal {
			t.Errorf("symbolic link rule file was followed for %s", path)
		}
	}
	compare(t, h, root, NewIgnorer(root, nil), func(string) string { return "" })
}
