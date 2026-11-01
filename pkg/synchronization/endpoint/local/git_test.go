package local

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
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

// runGit runs a command created by gitCommand and returns its trimmed standard
// output, along with its standard error in any error that it returns.
func runGit(directory string, args ...string) (string, error) {
	command := gitCommand(context.Background(), directory, args...)
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

// samePath returns whether or not a path reported by Git, which uses forward
// slashes on all platforms, is the same as an expected path.
func samePath(reported, expected string) bool {
	return filepath.Clean(filepath.FromSlash(reported)) == filepath.Clean(expected)
}

// environmentValues returns the values of the named variable in an environment
// in the order in which they appear.
func environmentValues(environment []string, name string) []string {
	var result []string
	for _, entry := range environment {
		if key, value, ok := strings.Cut(entry, "="); ok && key == name {
			result = append(result, value)
		}
	}
	return result
}

// unsetEnvironmentVariable removes a variable from the environment for the
// duration of a test.
func unsetEnvironmentVariable(t *testing.T, name string) {
	t.Helper()

	// Setting the variable registers its restoration when the test ends.
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unable to unset %s: %v", name, err)
	}
}

// TestGitCommandInvalidGitDirectory verifies that an invalid .git entry in the
// directory of a command makes Git fail instead of operating on an enclosing
// repository.
func TestGitCommandInvalidGitDirectory(t *testing.T) {
	requireGit(t)

	// Create a repository with a subdirectory that has an empty .git
	// directory, which Git doesn't recognize as a repository.
	root := realTemporaryDirectory(t)
	initializeRepository(t, root)
	directory := filepath.Join(root, "directory")
	if err := os.MkdirAll(filepath.Join(directory, ".git"), 0700); err != nil {
		t.Fatal("unable to create invalid .git directory:", err)
	}

	// Verify that the premise of the test holds, which is that Git would
	// otherwise operate on the enclosing repository.
	control := gitCommand(context.Background(), directory, "rev-parse", "--show-toplevel")
	control.Env = slices.DeleteFunc(control.Env, func(entry string) bool {
		return strings.HasPrefix(entry, "GIT_CEILING_DIRECTORIES=")
	})
	if output, err := control.Output(); err != nil {
		t.Skip("Git does not climb out of an invalid .git directory:", err)
	} else if !samePath(strings.TrimSpace(string(output)), root) {
		t.Skip("Git does not climb out of an invalid .git directory:", string(output))
	}

	// Verify that the command fails instead of reporting the enclosing
	// repository.
	if output, err := runGit(directory, "rev-parse", "--show-toplevel"); err == nil {
		t.Errorf("command succeeded in invalid repository with output: %s", output)
	}
	if output, err := runGit(directory, "status", "--porcelain"); err == nil {
		t.Errorf("status succeeded in invalid repository with output: %s", output)
	}

	// Verify that the enclosing repository is still usable, as is a nested
	// directory whose .git entry is valid.
	if output, err := runGit(root, "rev-parse", "--show-toplevel"); err != nil {
		t.Error("command failed in enclosing repository:", err)
	} else if !samePath(output, root) {
		t.Errorf("incorrect top level for enclosing repository: %s", output)
	}
	valid := filepath.Join(root, "valid")
	initializeRepository(t, valid)
	if output, err := runGit(valid, "rev-parse", "--show-toplevel"); err != nil {
		t.Error("command failed in nested repository:", err)
	} else if !samePath(output, valid) {
		t.Errorf("incorrect top level for nested repository: %s", output)
	}
}

// TestGitCommandStrayRepositoryVariables verifies that environment variables
// that redirect Git to another repository have no effect on commands.
func TestGitCommandStrayRepositoryVariables(t *testing.T) {
	requireGit(t)

	// Create the repository that commands operate on and another one that the
	// environment points to.
	root := realTemporaryDirectory(t)
	repository := filepath.Join(root, "repository")
	stray := filepath.Join(root, "stray")
	initializeRepository(t, repository)
	initializeRepository(t, stray)

	// Define test cases, each of which has a command that reports something
	// that the variable changes.
	tests := []struct {
		variable string
		value    string
		args     []string
		expected string
	}{
		{"GIT_DIR", filepath.Join(stray, ".git"), []string{"rev-parse", "--absolute-git-dir"}, filepath.Join(repository, ".git")},
		{"GIT_WORK_TREE", stray, []string{"rev-parse", "--show-toplevel"}, repository},
		{"GIT_COMMON_DIR", filepath.Join(stray, ".git"), []string{"rev-parse", "--git-common-dir"}, ".git"},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.variable, func(t *testing.T) {
			// Verify that the variable affects Git when it's not removed, so
			// that the test is meaningful.
			t.Setenv(test.variable, test.value)
			control := gitCommand(context.Background(), repository, test.args...)
			control.Env = append(control.Env, test.variable+"="+test.value)
			if output, err := control.Output(); err == nil && samePath(strings.TrimSpace(string(output)), test.expected) {
				t.Skipf("%s does not affect Git: %s", test.variable, output)
			}

			// Verify that it has no effect on commands created by gitCommand.
			output, err := runGit(repository, test.args...)
			if err != nil {
				t.Fatal("command failed:", err)
			} else if !samePath(output, test.expected) {
				t.Errorf("incorrect output with %s set: %s", test.variable, output)
			}
		})
	}
}

// TestGitCommandEnvironment verifies the environment of commands.
func TestGitCommandEnvironment(t *testing.T) {
	directory := realTemporaryDirectory(t)
	parent := filepath.Dir(directory)

	// Set variables that must be removed, variables that must be overridden,
	// and variables with similar names that must be retained.
	removed := []string{
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR",
		"GIT_NAMESPACE",
		"GIT_PREFIX",
	}
	for _, name := range removed {
		t.Setenv(name, "stray")
	}
	retained := []string{"GIT_DIR_EXTRA", "MY_GIT_DIR", "GIT_DIRECTORY", "GIT_AUTHOR_NAME"}
	for _, name := range retained {
		t.Setenv(name, "retained")
	}
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Join(parent, "other"))

	// Create the command.
	command := gitCommand(context.Background(), directory, "status", "--short")
	if command.Dir != directory {
		t.Error("incorrect directory:", command.Dir)
	} else if command.WaitDelay <= 0 {
		t.Error("no wait delay")
	} else if !slices.Equal(command.Args[1:], []string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"--no-optional-locks",
		"status", "--short",
	}) {
		t.Error("incorrect arguments:", command.Args)
	}

	// Verify the environment.
	for _, name := range removed {
		if values := environmentValues(command.Env, name); len(values) != 0 {
			t.Errorf("%s not removed: %v", name, values)
		}
	}
	for _, name := range retained {
		if values := environmentValues(command.Env, name); !slices.Equal(values, []string{"retained"}) {
			t.Errorf("%s not retained: %v", name, values)
		}
	}
	for name, expected := range map[string]string{
		"GIT_TERMINAL_PROMPT":     "0",
		"LC_ALL":                  "C",
		"GIT_CEILING_DIRECTORIES": parent,
	} {
		if values := environmentValues(command.Env, name); !slices.Equal(values, []string{expected}) {
			t.Errorf("%s incorrect: %v (expected %s)", name, values, expected)
		}
	}
}

// TestGitCommandAllowedProtocols verifies that commands use a restrictive
// default list of protocols and respect one that's already set.
func TestGitCommandAllowedProtocols(t *testing.T) {
	// Verify that the default list is used if nothing is set.
	unsetEnvironmentVariable(t, "GIT_ALLOW_PROTOCOL")
	command := gitCommand(context.Background(), t.TempDir(), "version")
	if values := environmentValues(command.Env, "GIT_ALLOW_PROTOCOL"); !slices.Equal(values, []string{"https:http:ssh:git"}) {
		t.Errorf("incorrect default allowed protocols: %v", values)
	}

	// Verify that values that are already set are respected, including an
	// empty one, which is a deliberate choice to allow no protocols.
	for _, value := range []string{"file", "file:https", ""} {
		t.Setenv("GIT_ALLOW_PROTOCOL", value)
		command := gitCommand(context.Background(), t.TempDir(), "version")
		if values := environmentValues(command.Env, "GIT_ALLOW_PROTOCOL"); !slices.Equal(values, []string{value}) {
			t.Errorf("allowed protocols %q not respected: %v", value, values)
		}
	}
}

// TestGitCommandFileProtocol verifies, with Git itself, that the file protocol
// is disallowed by default and can be enabled by the environment.
func TestGitCommandFileProtocol(t *testing.T) {
	requireGit(t)

	// Create an empty repository to refer to by URL.
	root := realTemporaryDirectory(t)
	remote := filepath.Join(root, "remote.git")
	initializeRepository(t, remote, "--bare")
	remotePath := filepath.ToSlash(remote)
	if !strings.HasPrefix(remotePath, "/") {
		remotePath = "/" + remotePath
	}
	remoteURL := "file://" + remotePath

	// Verify that the file protocol is disallowed by default. That the failure
	// is due to the protocol rather than the repository is established by the
	// same command succeeding when the environment enables the protocol.
	unsetEnvironmentVariable(t, "GIT_ALLOW_PROTOCOL")
	if _, err := runGit(root, "ls-remote", "--", remoteURL); err == nil {
		t.Error("file protocol allowed by default")
	} else {
		t.Log("default protocols reject the file protocol:", err)
	}

	// Verify that the environment can enable it.
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	if _, err := runGit(root, "ls-remote", "--", remoteURL); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("file URLs are not supported:", err)
		}
		t.Error("file protocol not allowed when enabled:", err)
	}
}
