package git

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// requireGit skips a test if Git isn't available.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is not available:", err)
	}
}

// isolate insulates the Git commands of a test from the user's configuration,
// such as commit signing.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.org")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.org")
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

// runCommand runs a command created by Command and returns its trimmed standard
// output, along with its standard error in any error that it returns.
func runCommand(directory string, args ...string) (string, error) {
	command := Command(context.Background(), directory, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", &testCommandError{err, strings.TrimSpace(stderr.String())}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// testCommandError is a command failure that includes the command's output.
type testCommandError struct {
	err    error
	stderr string
}

// Error implements error.Error.
func (e *testCommandError) Error() string {
	return e.err.Error() + ": " + e.stderr
}

// initializeRepository creates a Git repository in a directory, which is
// created if necessary.
func initializeRepository(t *testing.T, directory string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal("unable to create repository directory:", err)
	} else if _, err := runCommand(directory, append([]string{"init", "--quiet"}, args...)...); err != nil {
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

// TestCommandInvalidGitDirectory verifies that an invalid .git entry in the
// directory of a command makes Git fail instead of operating on an enclosing
// repository.
func TestCommandInvalidGitDirectory(t *testing.T) {
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
	control := Command(context.Background(), directory, "rev-parse", "--show-toplevel")
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
	if output, err := runCommand(directory, "rev-parse", "--show-toplevel"); err == nil {
		t.Errorf("command succeeded in invalid repository with output: %s", output)
	}
	if output, err := runCommand(directory, "status", "--porcelain"); err == nil {
		t.Errorf("status succeeded in invalid repository with output: %s", output)
	}

	// Verify that the enclosing repository is still usable, as is a nested
	// directory whose .git entry is valid.
	if output, err := runCommand(root, "rev-parse", "--show-toplevel"); err != nil {
		t.Error("command failed in enclosing repository:", err)
	} else if !samePath(output, root) {
		t.Errorf("incorrect top level for enclosing repository: %s", output)
	}
	valid := filepath.Join(root, "valid")
	initializeRepository(t, valid)
	if output, err := runCommand(valid, "rev-parse", "--show-toplevel"); err != nil {
		t.Error("command failed in nested repository:", err)
	} else if !samePath(output, valid) {
		t.Errorf("incorrect top level for nested repository: %s", output)
	}
}

// TestCommandStrayRepositoryVariables verifies that environment variables
// that redirect Git to another repository have no effect on commands.
func TestCommandStrayRepositoryVariables(t *testing.T) {
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
			control := Command(context.Background(), repository, test.args...)
			control.Env = append(control.Env, test.variable+"="+test.value)
			if output, err := control.Output(); err == nil && samePath(strings.TrimSpace(string(output)), test.expected) {
				t.Skipf("%s does not affect Git: %s", test.variable, output)
			}

			// Verify that it has no effect on commands created by Command.
			output, err := runCommand(repository, test.args...)
			if err != nil {
				t.Fatal("command failed:", err)
			} else if !samePath(output, test.expected) {
				t.Errorf("incorrect output with %s set: %s", test.variable, output)
			}
		})
	}
}

// TestCommandEnvironment verifies the environment of commands.
func TestCommandEnvironment(t *testing.T) {
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
	t.Setenv("GIT_ASKPASS", "/usr/bin/askpass")
	t.Setenv("SSH_ASKPASS_REQUIRE", "force")
	t.Setenv("GCM_INTERACTIVE", "always")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Join(parent, "other"))

	// Create the command.
	command := Command(context.Background(), directory, "status", "--short")
	if command.Dir != directory {
		t.Error("incorrect directory:", command.Dir)
	} else if command.WaitDelay <= 0 {
		t.Error("no wait delay")
	} else if !slices.Equal(command.Args[1:], []string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "safe.bareRepository=explicit",
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
		"GIT_ASKPASS":             "",
		"SSH_ASKPASS_REQUIRE":     "never",
		"GCM_INTERACTIVE":         "never",
		"LC_ALL":                  "C",
		"GIT_CEILING_DIRECTORIES": parent,
	} {
		if values := environmentValues(command.Env, name); !slices.Equal(values, []string{expected}) {
			t.Errorf("%s incorrect: %v (expected %s)", name, values, expected)
		}
	}
}

// TestCommandSymbolicLinkDirectory verifies that repository discovery is
// confined to a directory that's a symbolic link, whose target may be within
// a repository that its own parent isn't within.
func TestCommandSymbolicLinkDirectory(t *testing.T) {
	requireGit(t)

	// Create a repository with a subdirectory whose .git entry is invalid, and
	// a symbolic link to that subdirectory from outside the repository.
	base := realTemporaryDirectory(t)
	repository := filepath.Join(base, "repository")
	initializeRepository(t, repository)
	target := filepath.Join(repository, "sub", "project")
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("unable to create symbolic link:", err)
	}

	// Verify that Git fails rather than using the enclosing repository.
	if output, err := runCommand(link, "rev-parse", "--show-toplevel"); err == nil {
		t.Error("enclosing repository used:", output)
	}
}

// TestCommandCeilingWithSeparator verifies that a command fails to start,
// rather than running without a ceiling, if the path of its directory's parent
// contains the path list separator, which Git would split the ceiling at.
func TestCommandCeilingWithSeparator(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "project"+string(filepath.ListSeparator)+"v2")
	directory := filepath.Join(parent, "subdirectory")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Skip("unable to create a directory whose path contains the separator:", err)
	}
	err := Command(context.Background(), directory, "version").Run()
	if err == nil || !strings.Contains(err.Error(), "unable to confine Git") {
		t.Error("command not refused:", err)
	}
}

// TestCommandDiscoveredBareRepository verifies that a command doesn't use a
// bare repository that Git discovers in its directory, which could only be
// there as ordinary content (such as synchronized files) if the directory's
// .git entry is invalid, and whose configuration Git would otherwise apply.
func TestCommandDiscoveredBareRepository(t *testing.T) {
	requireGit(t)

	// Create a bare repository with an invalid .git entry, and verify that
	// Git would otherwise use it, so that the test is meaningful.
	directory := realTemporaryDirectory(t)
	if _, err := runCommand(directory, "init", "--bare", "--quiet", "."); err != nil {
		t.Fatal("unable to create bare repository:", err)
	} else if err := os.Mkdir(filepath.Join(directory, ".git"), 0700); err != nil {
		t.Fatal("unable to create invalid .git entry:", err)
	}
	control := exec.Command("git", "rev-parse", "--git-dir")
	control.Dir = directory
	control.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(directory))
	if output, err := control.Output(); err != nil || strings.TrimSpace(string(output)) != "." {
		t.Skipf("Git doesn't discover the bare repository: %q: %v", output, err)
	}
	control = exec.Command("git", "-c", "safe.bareRepository=explicit", "rev-parse", "--git-dir")
	control.Dir = directory
	control.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(directory))
	if err := control.Run(); err == nil {
		t.Skip("Git doesn't support safe.bareRepository (it requires Git 2.38 or later)")
	}

	// Verify that commands created by Command don't use it.
	if output, err := runCommand(directory, "rev-parse", "--git-dir"); err == nil {
		t.Error("bare repository used:", output)
	}
}

// TestCommandNoAskPass verifies, with Git itself, that commands never run a
// program to ask for credentials, from any of the places that Git looks for
// one, and fail instead.
func TestCommandNoAskPass(t *testing.T) {
	requireGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("the askpass program is a POSIX shell script")
	}

	// Isolate Git from the user's configuration, which may contain credential
	// helpers that would supply credentials without asking.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	// Create an askpass program that records being run, and name it in every
	// place that Git looks for one.
	directory := realTemporaryDirectory(t)
	marker := filepath.Join(directory, "asked")
	program := filepath.Join(directory, "askpass")
	script := "#!/bin/sh\ntouch '" + marker + "'\necho secret\n"
	if err := os.WriteFile(program, []byte(script), 0700); err != nil {
		t.Fatal("unable to create askpass program:", err)
	}
	t.Setenv("GIT_ASKPASS", program)
	t.Setenv("SSH_ASKPASS", program)

	// Ask Git for credentials.
	command := Command(context.Background(), directory,
		"-c", "core.askPass="+program, "credential", "fill",
	)
	command.Stdin = strings.NewReader("protocol=https\nhost=example.com\n\n")
	if output, err := command.CombinedOutput(); err == nil {
		t.Errorf("credentials obtained: %s", output)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("askpass program was run")
	} else if !os.IsNotExist(err) {
		t.Fatal("unable to check for askpass marker:", err)
	}
}

// TestCommandAllowedProtocols verifies that commands use a restrictive
// default list of protocols and respect one that's already set.
func TestCommandAllowedProtocols(t *testing.T) {
	// Verify that the default list is used if nothing is set.
	unsetEnvironmentVariable(t, "GIT_ALLOW_PROTOCOL")
	command := Command(context.Background(), t.TempDir(), "version")
	if values := environmentValues(command.Env, "GIT_ALLOW_PROTOCOL"); !slices.Equal(values, []string{"https:http:ssh:git"}) {
		t.Errorf("incorrect default allowed protocols: %v", values)
	}

	// Verify that values that are already set are respected, including an
	// empty one, which is a deliberate choice to allow no protocols.
	for _, value := range []string{"file", "file:https", ""} {
		t.Setenv("GIT_ALLOW_PROTOCOL", value)
		command := Command(context.Background(), t.TempDir(), "version")
		if values := environmentValues(command.Env, "GIT_ALLOW_PROTOCOL"); !slices.Equal(values, []string{value}) {
			t.Errorf("allowed protocols %q not respected: %v", value, values)
		}
	}
}

// TestCommandFileProtocol verifies, with Git itself, that the file protocol
// is disallowed by default and can be enabled by the environment.
func TestCommandFileProtocol(t *testing.T) {
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
	if _, err := runCommand(root, "ls-remote", "--", remoteURL); err == nil {
		t.Error("file protocol allowed by default")
	} else {
		t.Log("default protocols reject the file protocol:", err)
	}

	// Verify that the environment can enable it.
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	if _, err := runCommand(root, "ls-remote", "--", remoteURL); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("file URLs are not supported:", err)
		}
		t.Error("file protocol not allowed when enabled:", err)
	}
}

// TestStripCredentials tests the removal of credentials from URLs.
func TestStripCredentials(t *testing.T) {
	tests := []struct {
		url      string
		expected string
	}{
		// Web URLs lose all user information, along with queries and fragments.
		{"https://example.org/repository.git", "https://example.org/repository.git"},
		{"https://user@example.org/repository.git", "https://example.org/repository.git"},
		{"https://user:token@example.org/repository.git", "https://example.org/repository.git"},
		{"http://user:token@example.org:8080/repository.git", "http://example.org:8080/repository.git"},
		{"HTTPS://user:token@example.org/repository.git", "HTTPS://example.org/repository.git"},
		{"https://user:p@ss@example.org/repository.git", "https://example.org/repository.git"},
		{"https://example.org/repository@v2.git", "https://example.org/repository@v2.git"},
		{"https://example.org/repository.git?token=secret", "https://example.org/repository.git"},
		{"https://example.org/repository.git#secret", "https://example.org/repository.git"},
		{"https://user:token@example.org", "https://example.org"},
		{"https://user:token@example.org?token=secret", "https://example.org"},

		// URLs that use SSH keep the user, but not the password.
		{"ssh://git@example.org/repository.git", "ssh://git@example.org/repository.git"},
		{"ssh://git:secret@example.org:22/repository.git", "ssh://git@example.org:22/repository.git"},
		{"git+ssh://git:secret@example.org/repository.git", "git+ssh://git@example.org/repository.git"},
		{"ssh+git://git:secret@example.org/repository.git", "ssh+git://git@example.org/repository.git"},
		{"ssh://example.org/repository.git", "ssh://example.org/repository.git"},
		{"ssh://[::1]:22/repository.git", "ssh://[::1]:22/repository.git"},
		{"git@example.org:repository.git", "git@example.org:repository.git"},
		{"example.org:repository.git", "example.org:repository.git"},

		// Other transports lose all user information.
		{"git://user:secret@example.org/repository.git", "git://example.org/repository.git"},
		{"ftp://user:secret@example.org/repository.git", "ftp://example.org/repository.git"},
		{"file://user:secret@/path/repository.git", "file:///path/repository.git"},
		{"file:///path/repository.git", "file:///path/repository.git"},

		// Remote helpers have the same treatment applied to their addresses.
		{"helper::https://user:token@example.org/repository.git", "helper::https://example.org/repository.git"},
		{"helper::address", "helper::address"},

		// Paths don't have anything to remove.
		{"/path/repository.git", "/path/repository.git"},
		{"../repository.git", "../repository.git"},
	}
	for _, test := range tests {
		if result := StripCredentials(test.url); result != test.expected {
			t.Errorf("incorrect result for %q: %q != %q", test.url, result, test.expected)
		}
	}
}

// TestUsableURL tests the validation of URLs.
func TestUsableURL(t *testing.T) {
	tests := []struct {
		url      string
		expected string
		failure  string
	}{
		{url: "https://user:token@example.org/repository.git", expected: "https://example.org/repository.git"},
		{url: "ssh://git@example.org/repository.git", expected: "ssh://git@example.org/repository.git"},
		{url: "git@example.org:path/repository.git", expected: "git@example.org:path/repository.git"},
		{url: "git@example.org:path/repository@v2.git", expected: "git@example.org:path/repository@v2.git"},
		{url: "example.org:repository.git", expected: "example.org:repository.git"},
		{url: "/path/repository.git", expected: "/path/repository.git"},
		{url: "file:///path/repository.git", expected: "file:///path/repository.git"},
		{url: "helper::address", expected: "helper::address"},
		{url: "-https://example.org", failure: "dash"},
		{url: "--upload-pack=touch", failure: "dash"},
		{url: "relative", failure: "relative"},
		{url: "../relative", failure: "relative"},
		{url: "./colon:in/path", failure: "relative"},
		{url: "user:password@example.org:repository.git", failure: "password"},
		{url: "https://example.org/\nrepository.git", failure: "invalid characters"},
		{url: "https://example.org/\xffrepository.git", failure: "invalid characters"},
	}
	for _, test := range tests {
		result, err := UsableURL(test.url)
		if test.failure != "" {
			if err == nil {
				t.Errorf("no error for %q", test.url)
			} else if !strings.Contains(err.Error(), test.failure) {
				t.Errorf("incorrect error for %q: %v", test.url, err)
			} else if strings.Contains(err.Error(), "password@") {
				t.Errorf("error for %q contains credentials: %v", test.url, err)
			}
		} else if err != nil {
			t.Errorf("unexpected error for %q: %v", test.url, err)
		} else if result != test.expected {
			t.Errorf("incorrect result for %q: %q != %q", test.url, result, test.expected)
		}
	}
}

// TestOutputRecorderMessage tests the summaries of commands' output, which
// must omit progress and must not contain credentials, whatever Git reports.
func TestOutputRecorderMessage(t *testing.T) {
	// Progress and hints are removed, and credentials don't survive.
	recorder := NewOutputRecorder()
	output := "Cloning into '.'...\n" +
		"remote: Enumerating objects: 6, done.\n" +
		"remote: Counting objects:  16% (1/6)\rremote: Counting objects: 100% (6/6), done.\n" +
		"Receiving objects:  50% (3/6)\rReceiving objects: 100% (6/6), done.\n" +
		"From https://user:secret@example.org/repository\n" +
		" * branch            1234 -> FETCH_HEAD\n" +
		"hint: Using something\n" +
		"fatal: unable to access 'https://user:token@example.org/repository.git/': error\n" +
		"fatal: could not read from ssh://git:password@example.org/repository.git, or http://example.org/x?token=abc\n"
	if _, err := recorder.Write([]byte(output)); err != nil {
		t.Fatal(err)
	}
	expected := "fatal: unable to access 'https://example.org/repository.git/': error; " +
		"fatal: could not read from ssh://git@example.org/repository.git, or http://example.org/x"
	if message := recorder.Message(); message != expected {
		t.Errorf("incorrect message: %q != %q", message, expected)
	}

	// A URL that was cut in half by the limit on what's retained could
	// otherwise contain what's left of its credentials without any way to
	// recognize them.
	recorder = NewOutputRecorder()
	url := "https://user:secret@example.org/repository.git"
	padding := strings.Repeat("x", outputTailSize-len(url)/2)
	for i := 0; i < 3; i++ {
		if _, err := recorder.Write([]byte(url + padding)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := recorder.Write([]byte("\nfatal: failure\n")); err != nil {
		t.Fatal(err)
	}
	if message := recorder.Message(); message != "fatal: failure" {
		t.Errorf("incorrect message after truncation: %q", message)
	}

	// Output has to be usable in Protocol Buffers messages.
	recorder = NewOutputRecorder()
	if _, err := recorder.Write([]byte("fatal: invalid \xff UTF-8\n")); err != nil {
		t.Fatal(err)
	}
	if message := recorder.Message(); !utf8.ValidString(message) {
		t.Errorf("message isn't valid UTF-8: %q", message)
	}
	recorder = NewOutputRecorder()
	if _, err := recorder.Write([]byte("fatal: " + strings.Repeat("x", 4*maximumMessageSize) + "\n")); err != nil {
		t.Fatal(err)
	}
	if message := recorder.Message(); len(message) > maximumMessageSize+3 {
		t.Errorf("message isn't limited: %d", len(message))
	}
}

// newCommittedRepository creates a repository with a single commit in a new
// directory, with the specified URL as its origin if it isn't empty.
func newCommittedRepository(t *testing.T, directory, url string) string {
	t.Helper()
	initializeRepository(t, directory)
	if _, err := runCommand(directory, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "first"); err != nil {
		t.Fatal("unable to create commit:", err)
	}
	if url != "" {
		if _, err := runCommand(directory, "config", "remote.origin.url", url); err != nil {
			t.Fatal("unable to set origin:", err)
		}
	}
	return directory
}

// TestProbe tests the examination of nested repositories. The variety of
// invalid .git entries is tested along with the failures that they cause in
// scans, by TestGitProviderProbe in the package of the local endpoint.
func TestProbe(t *testing.T) {
	requireGit(t)
	isolate(t)
	base := realTemporaryDirectory(t)
	ctx := context.Background()

	// A repository with a commit and an origin is a submodule, whose URL is
	// returned without credentials.
	t.Run("submodule", func(t *testing.T) {
		directory := newCommittedRepository(t, filepath.Join(base, "submodule"), "https://user:secret@example.org/repository.git")
		head, err := runCommand(directory, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		repository, err := Probe(ctx, directory)
		if err != nil {
			t.Fatal("probe failed:", err)
		} else if repository == nil {
			t.Fatal("repository not recognized as a submodule")
		} else if hex.EncodeToString(repository.Commit) != head {
			t.Errorf("incorrect commit: %x != %s", repository.Commit, head)
		} else if repository.URL != "https://example.org/repository.git" {
			t.Error("incorrect URL:", repository.URL)
		} else if repository.AutoCRLF != "" || repository.EOL != "" {
			t.Errorf("unset line ending settings reported: %q %q", repository.AutoCRLF, repository.EOL)
		}
	})

	// The line ending settings are reported as they apply, with the last value
	// of a setting winning, and with a boolean setting without a value meaning
	// true. Values that can't be passed to Git elsewhere are left out.
	t.Run("line ending settings", func(t *testing.T) {
		directory := newCommittedRepository(t, filepath.Join(base, "settings"), "https://example.org/repository.git")
		configuration := filepath.Join(directory, ".git", "config")
		contents, err := os.ReadFile(configuration)
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, []byte("[core]\n\tautocrlf = false\n\teol = crlf\n[core]\n\tautocrlf\n")...)
		if err := os.WriteFile(configuration, contents, 0600); err != nil {
			t.Fatal(err)
		}
		if repository, err := Probe(ctx, directory); err != nil {
			t.Fatal("probe failed:", err)
		} else if repository == nil {
			t.Fatal("repository not recognized as a submodule")
		} else if repository.AutoCRLF != "true" || repository.EOL != "crlf" {
			t.Errorf("incorrect line ending settings: %q %q", repository.AutoCRLF, repository.EOL)
		}
		if _, err := runCommand(directory, "config", "core.eol", strings.Repeat("x", maximumSettingLength+1)); err != nil {
			t.Fatal(err)
		} else if repository, err := Probe(ctx, directory); err != nil {
			t.Fatal("probe failed:", err)
		} else if repository.EOL != "" {
			t.Error("unusable setting reported:", repository.EOL)
		}
	})

	// A repository with a separate Git directory, which is what submodules and
	// linked worktrees have, is examined in the same way.
	t.Run("separate Git directory", func(t *testing.T) {
		directory := filepath.Join(base, "separate")
		initializeRepository(t, directory, "--separate-git-dir", filepath.Join(base, "separate.git"))
		if _, err := runCommand(directory, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "first"); err != nil {
			t.Fatal(err)
		} else if _, err := runCommand(directory, "config", "remote.origin.url", "https://example.org/repository.git"); err != nil {
			t.Fatal(err)
		}
		if repository, err := Probe(ctx, directory); err != nil {
			t.Fatal("probe failed:", err)
		} else if repository == nil || len(repository.Commit) == 0 || repository.URL != "https://example.org/repository.git" {
			t.Errorf("repository not recognized as a submodule: %+v", repository)
		}
	})

	// Git fetches from the first of several origin URLs, so that's the one
	// that's used.
	t.Run("several origin URLs", func(t *testing.T) {
		directory := newCommittedRepository(t, filepath.Join(base, "several-origins"), "https://example.org/repository.git")
		if _, err := runCommand(directory, "config", "--add", "remote.origin.url", "https://example.org/mirror.git"); err != nil {
			t.Fatal(err)
		}
		repository, err := Probe(ctx, directory)
		if err != nil {
			t.Fatal("probe failed:", err)
		} else if repository == nil || repository.URL != "https://example.org/repository.git" {
			t.Errorf("incorrect repository: %+v", repository)
		}
	})

	// A repository without a commit or an origin is an ordinary directory.
	t.Run("no origin", func(t *testing.T) {
		directory := newCommittedRepository(t, filepath.Join(base, "no-origin"), "")
		if repository, err := Probe(ctx, directory); err != nil {
			t.Fatal("probe failed:", err)
		} else if repository != nil {
			t.Errorf("repository without origin treated as submodule: %+v", repository)
		}
	})
	t.Run("no commits", func(t *testing.T) {
		directory := filepath.Join(base, "no-commits")
		initializeRepository(t, directory)
		if _, err := runCommand(directory, "config", "remote.origin.url", "https://example.org/repository.git"); err != nil {
			t.Fatal(err)
		}
		if repository, err := Probe(ctx, directory); err != nil {
			t.Fatal("probe failed:", err)
		} else if repository != nil {
			t.Errorf("repository without commits treated as submodule: %+v", repository)
		}
	})

	// Anything else is a failure, with a message that doesn't include
	// credentials and that says what to do.
	t.Run("unusable origin", func(t *testing.T) {
		directory := newCommittedRepository(t, filepath.Join(base, "unusable"), "-https://user:secret@example.org/repository.git")
		_, err := Probe(ctx, directory)
		if err == nil {
			t.Fatal("probe succeeded")
		} else if strings.Contains(err.Error(), "secret") {
			t.Error("error contains credentials:", err)
		} else if !strings.Contains(err.Error(), "set-url") {
			t.Error("error doesn't say what to do:", err)
		}
	})
	t.Run("invalid repository", func(t *testing.T) {
		directory := filepath.Join(base, "invalid")
		if err := os.MkdirAll(filepath.Join(directory, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
		_, err := Probe(ctx, directory)
		if err == nil {
			t.Fatal("probe succeeded")
		} else if !strings.Contains(err.Error(), "isn't a valid repository") {
			t.Error("incorrect error:", err)
		} else if !strings.Contains(err.Error(), "(not the directory)") {
			t.Error("error doesn't warn against removing the directory:", err)
		}
	})
	// A real repository that Git can't read gets the same advice, which mustn't
	// be to remove the directory, since that would lose its commits.
	t.Run("invalid configuration", func(t *testing.T) {
		directory := newCommittedRepository(t, filepath.Join(base, "invalid-configuration"), "https://example.org/repository.git")
		configuration, err := os.OpenFile(filepath.Join(directory, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		} else if _, err := configuration.WriteString("[invalid\n"); err != nil {
			configuration.Close()
			t.Fatal(err)
		} else if err := configuration.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = Probe(ctx, directory)
		if err == nil {
			t.Fatal("probe succeeded")
		} else if strings.Contains(err.Error(), "remove the directory") || !strings.Contains(err.Error(), "(not the directory)") {
			t.Error("incorrect advice:", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		directory := newCommittedRepository(t, filepath.Join(base, "cancelled"), "https://example.org/repository.git")
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := Probe(cancelled, directory)
		if !errors.Is(err, context.Canceled) {
			t.Error("incorrect error:", err)
		}
	})
}
