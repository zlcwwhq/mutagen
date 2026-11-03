package local

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/git"
)

// requireGit skips a test if Git isn't available.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is not available:", err)
	}
}

// realTemporaryDirectory creates a temporary directory whose path has no
// symbolic links, which is the form in which Git reports paths.
func realTemporaryDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("unable to resolve temporary directory:", err)
	}
	return directory
}

// runGit runs a command created by git.Command and returns its trimmed standard
// output, along with its standard error in any error that it returns.
func runGit(directory string, args ...string) (string, error) {
	command := git.Command(context.Background(), directory, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", &gitTestError{err, strings.TrimSpace(stderr.String())}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitTestError is a command failure that includes the command's output.
type gitTestError struct {
	err    error
	stderr string
}

// Error implements error.Error.
func (e *gitTestError) Error() string {
	return e.err.Error() + ": " + e.stderr
}

// initializeRepository creates a Git repository in a directory, which is
// created if necessary.
func initializeRepository(t *testing.T, directory string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal("unable to create repository directory:", err)
	} else if _, err := runGit(directory, append([]string{"init", "--quiet"}, args...)...); err != nil {
		t.Fatal("unable to initialize repository:", err)
	}
}

// TestGitModeEventRelevant tests gitModeEventRelevant.
func TestGitModeEventRelevant(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		// Ordinary content is relevant, including rule files and names that
		// merely resemble those that the function treats specially.
		{"", true},
		{"file", true},
		{"directory/file", true},
		{".gitignore", true},
		{"directory/.gitignore", true},
		{".gitmodules", true},
		{".github/workflows/ci.yml", true},
		{"HEAD", true},
		{"refs/heads/main", true},
		{"info/exclude", true},
		{"objects/ab/cdef", true},
		{"logs/HEAD", true},
		{"directory.git/index", true},
		{"git/index", true},

		// Temporary content isn't relevant, wherever it is, whatever it
		// contains.
		{".mutagen-temporary-file", false},
		{"directory/.mutagen-temporary-file", false},
		{".mutagen-temporary-directory/objects/pack/file", false},
		{"third_party/.mutagen-temporary-x", false},
		{"third_party/.mutagen-temporary-x/objects/ab/cdef", false},
		{"third_party/.mutagen-temporary-x/file", false},
		{"third_party/.mutagen-temporary-x/.git/HEAD", false},
		{"a/b/.mutagen-temporary-x/c/d", false},
		{"not.mutagen-temporary-x/file", true},

		// The exclude files of each kind of repository are.
		{".git/info/exclude", true},
		{"sub/.git/info/exclude", true},
		{".git/modules/x/info/exclude", true},
		{".git/modules/x/y/info/exclude", true},
		{".GIT/info/exclude", true},

		// Everything else in a Git directory isn't relevant.
		{".git", false},
		{"sub/.git", false},
		{".GIT", false},
		{".git/HEAD", false},
		{"sub/.git/HEAD", false},
		{".git/modules/x/HEAD", false},
		{".git/packed-refs", false},
		{".git/refs", false},
		{".git/refs/heads/main", false},
		{".git/refs/remotes/origin/main", false},
		{".git/refs/remotes/origin/HEAD.lock", false},
		{".git/index", false},
		{"sub/.git/index", false},
		{".git/modules/x/index", false},
		{".git/modules/x/y/index", false},
		{".git/index.lock", false},
		{".git/HEAD.lock", false},
		{".git/packed-refs.lock", false},
		{".git/FETCH_HEAD", false},
		{"sub/.git/FETCH_HEAD", false},
		{".git/modules/x/FETCH_HEAD", false},
		{".git/ORIG_HEAD", false},
		{".git/MERGE_HEAD", false},
		{".git/config", false},
		{".git/description", false},
		{".git/shallow", false},
		{".git/objects", false},
		{".git/objects/ab/cdef", false},
		{".git/objects/pack/pack-1.pack", false},
		{".git/modules/x/objects/ab/cdef", false},
		{".git/hooks/pre-commit", false},
		{".git/info", false},
		{".git/info/attributes", false},
		{".git/info/exclude.lock", false},
		{".git/exclude", false},
		{".git/modules", false},
		{".git/modules/x", false},

		// That includes reference logs.
		{".git/logs", false},
		{".git/logs/HEAD", false},
		{"sub/.git/logs/HEAD", false},
		{".git/logs/refs/heads/main", false},
		{"sub/.git/logs/refs/heads/main", false},
		{".git/modules/x/logs/HEAD", false},
		{".git/modules/x/y/logs/HEAD", false},
		{".git/modules/x/logs/refs/heads/main", false},
	}
	for _, test := range tests {
		if result := gitModeEventRelevant(test.path); result != test.expected {
			t.Errorf("incorrect result for %q: %t != %t", test.path, result, test.expected)
		}
	}
}
