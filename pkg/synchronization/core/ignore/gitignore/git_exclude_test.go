package gitignore

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
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

// TestGitExcludeSymbolicLinkRoot tests exclude files in repositories whose
// working tree is the root, specified by a symbolic link in another directory,
// and whose ".git" file refers to the Git directory by a relative location that
// leads out of the working tree, as those of submodules do, and as those of
// linked worktrees do with relative paths. Git resolves such a location against
// the working tree's physical location, rather than cancelling the link's name
// with the parent reference, and so must the ignorer, whether it's given the
// root as specified, as scans are, or with the link resolved, as remote is.
// Git for Windows resolves parent references lexically, against the location
// as specified, so the test doesn't apply there.
func TestGitExcludeSymbolicLinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git for Windows resolves parent references lexically")
	}
	h := newGitHarness(t)
	layouts := []struct {
		name string
		// setup creates the working tree in a base directory and returns its
		// location and that of its exclude file.
		setup func(t *testing.T, base string) (string, string)
	}{
		{"submodule", func(t *testing.T, base string) (string, string) {
			superproject := filepath.Join(base, "super")
			work := filepath.Join(superproject, "sub")
			gitDirectory := filepath.Join(superproject, ".git", "modules", "sub")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal("unable to create working tree:", err)
			}
			h.initialize(superproject)
			if err := os.MkdirAll(filepath.Dir(gitDirectory), 0o755); err != nil {
				t.Fatal("unable to create Git directory parent:", err)
			}
			h.run(work, nil, "init", "--quiet", "--separate-git-dir", gitDirectory)
			if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: ../.git/modules/sub\n"), 0o644); err != nil {
				t.Fatal("unable to write Git file:", err)
			}
			return work, filepath.Join(gitDirectory, "info", "exclude")
		}},
		{"relative worktree", func(t *testing.T, base string) (string, string) {
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
			work := filepath.Join(base, "work")
			h.run(repository, nil, "worktree", "add", "--quiet", "--detach", work)
			if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: ../main/.git/worktrees/work\n"), 0o644); err != nil {
				t.Fatal("unable to write Git file:", err)
			}
			return work, filepath.Join(repository, ".git", "info", "exclude")
		}},
		{"parent reference after a symbolic link", func(t *testing.T, base string) (string, string) {
			work := filepath.Join(base, "work")
			gitDirectory := filepath.Join(base, "real", "deep", "git")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal("unable to create working tree:", err)
			} else if err := os.MkdirAll(filepath.Join(base, "real", "deep", "target"), 0o755); err != nil {
				t.Fatal("unable to create link target:", err)
			} else if err := os.Symlink(filepath.Join(base, "real", "deep", "target"), filepath.Join(base, "hop")); err != nil {
				t.Skip("unable to create symbolic link:", err)
			}
			h.run(work, nil, "init", "--quiet", "--separate-git-dir", gitDirectory)
			if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: ../hop/../git\n"), 0o644); err != nil {
				t.Fatal("unable to write Git file:", err)
			}
			return work, filepath.Join(gitDirectory, "info", "exclude")
		}},
	}
	for _, layout := range layouts {
		for _, c := range excludeCases {
			t.Run(layout.name+" "+c.name, func(t *testing.T) {
				// Create the working tree and a symbolic link to it with
				// another name in another directory.
				base := t.TempDir()
				work, exclude := layout.setup(t, base)
				root := filepath.Join(base, "links", "root")
				if err := os.Mkdir(filepath.Dir(root), 0o755); err != nil {
					t.Fatal("unable to create link directory:", err)
				} else if err := os.Symlink(work, root); err != nil {
					t.Skip("unable to create symbolic link:", err)
				}

				// Compare the ignorer for the root as specified with Git run
				// in it, and with the ignorer for the working tree itself.
				runExcludeCase(t, h, root, exclude, c)
				if !maps.Equal(scan(t, root, NewIgnorer(root, nil)), scan(t, work, NewIgnorer(work, nil))) {
					t.Error("ignorers for the root and for the working tree disagree")
				}
			})
		}
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
