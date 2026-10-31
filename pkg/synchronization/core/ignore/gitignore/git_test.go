package gitignore

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// This file contains differential tests that compare the behavior of this
// package with the behavior of the git executable, treating the latter as a
// black box. They're skipped if git isn't available.

// gitHarness runs git in a hermetic environment.
type gitHarness struct {
	// t is the test being run.
	t *testing.T
	// path is the path to the git executable.
	path string
	// environment is the environment that git is run in.
	environment []string
}

// newGitHarness creates a gitHarness, skipping the test if git isn't available.
func newGitHarness(t *testing.T) *gitHarness {
	t.Helper()

	// Locate git.
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}

	// Create an environment that doesn't allow the user's or system's
	// configuration, or any other repository, to influence the results.
	home := t.TempDir()
	environment := make([]string, 0, len(os.Environ())+8)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(name, "GIT_") || name == "HOME" || name == "XDG_CONFIG_HOME" {
			continue
		}
		environment = append(environment, variable)
	}
	environment = append(
		environment,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "xdg"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	return &gitHarness{t: t, path: path, environment: environment}
}

// run runs git in a directory, returning its output and exit code. Exit codes
// of zero and one are returned, any other failure fails the test.
func (h *gitHarness) run(directory string, stdin []byte, arguments ...string) ([]byte, int) {
	h.t.Helper()
	command := exec.Command(h.path, arguments...)
	command.Dir = directory
	command.Env = h.environment
	command.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return stdout.Bytes(), 1
		}
		h.t.Fatalf("git %s failed: %v\n%s", strings.Join(arguments, " "), err, stderr.String())
	}
	return stdout.Bytes(), 0
}

// initialize creates a repository in a directory.
func (h *gitHarness) initialize(directory string, arguments ...string) {
	h.t.Helper()
	h.run(directory, nil, append([]string{"init", "--quiet"}, arguments...)...)
}

// checkIgnore returns the set of paths, relative to the root of a repository,
// that git considers ignored. Paths are looked up in the filesystem, so they
// must exist, and they must not include any symbolic link in a parent position.
func (h *gitHarness) checkIgnore(root string, paths []string) map[string]bool {
	h.t.Helper()
	var input bytes.Buffer
	for _, path := range paths {
		input.WriteString(path)
		input.WriteByte(0)
	}
	output, _ := h.run(
		root, input.Bytes(),
		"-c", "core.ignorecase=false",
		"check-ignore", "--no-index", "--stdin", "-z",
	)
	ignored := make(map[string]bool)
	for _, path := range bytes.Split(output, []byte{0}) {
		if len(path) > 0 {
			ignored[string(path)] = true
		}
	}
	return ignored
}

// explain returns git's explanation for its treatment of a path, for use in
// diagnostics. It never fails the test.
func (h *gitHarness) explain(root, path string) string {
	command := exec.Command(
		h.path, "-c", "core.ignorecase=false",
		"check-ignore", "--no-index", "-v", "-n", "--", path,
	)
	command.Dir = root
	command.Env = h.environment
	output, _ := command.CombinedOutput()
	return strings.TrimSpace(string(output))
}

// fixtureEntries describes the tree of entries that's created for each
// scenario. Directories have a trailing slash and entries with an arrow are
// symbolic links. The names are chosen to line up with the fragments that the
// random pattern generator uses.
var fixtureEntries = []string{
	"a", "ab", "a.b", "a b", "[a]", "é", "foo", "x.txt", "c",
	"b/", "b/a/", "b/a/a", "b/a/ab", "b/a/b/", "b/a/b/a", "b/a/b/c.txt",
	"b/b/", "b/b/a", "b/ab",
	"ba/", "ba/a", "ba/ab/", "ba/ab/a", "ba/ab/b",
	"d/", "d/a", "d/foo/", "d/foo/bar", "d/foo/a", "d/x.txt", "d/a.b/",
	"d/a.b/a", "d/n -> foo", "d/foo/x/", "d/foo/x/bar", "d/foobar",
	"e/", "e/a/", "e/a/b/", "e/a/b/c/", "e/a/b/c/a", "e/a/b/c/b",
	"e/a/b/c/foo",
	"l -> d", "m -> a",
}

// createFixture creates the fixture tree in a directory.
func createFixture(t *testing.T, directory string) {
	t.Helper()
	for _, entry := range fixtureEntries {
		if name, target, ok := strings.Cut(entry, " -> "); ok {
			if err := os.Symlink(target, filepath.Join(directory, filepath.FromSlash(name))); err != nil {
				t.Logf("unable to create symbolic link, skipping: %v", err)
			}
		} else if strings.HasSuffix(entry, "/") {
			if err := os.MkdirAll(filepath.Join(directory, filepath.FromSlash(entry)), 0o755); err != nil {
				t.Fatal("unable to create fixture directory:", err)
			}
		} else if err := os.WriteFile(filepath.Join(directory, filepath.FromSlash(entry)), nil, 0o644); err != nil {
			t.Fatal("unable to create fixture file:", err)
		}
	}
}

// scenario is a copy of the fixture tree with a set of rule files.
type scenario struct {
	// name is the name of the scenario, which is also the name of its
	// directory.
	name string
	// files maps the paths of rule files, relative to the scenario, to their
	// contents.
	files map[string]string
}

// entry is a filesystem entry.
type entry struct {
	// path is the slash-separated path relative to the repository root.
	path string
	// directory indicates whether or not the entry is a directory (a symbolic
	// link to a directory isn't).
	directory bool
}

// listEntries lists every entry in a repository other than the Git directory.
func listEntries(t *testing.T, root string) []entry {
	t.Helper()
	var entries []entry
	var walk func(string)
	walk = func(directory string) {
		contents, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(directory)))
		if err != nil {
			t.Fatal("unable to list directory:", err)
		}
		for _, content := range contents {
			path := content.Name()
			if directory != "" {
				path = directory + "/" + path
			}
			if path == ".git" {
				continue
			}
			entries = append(entries, entry{path, content.IsDir()})
			if content.IsDir() {
				walk(path)
			}
		}
	}
	walk("")
	return entries
}

// scan simulates a scan by walking the entries of a repository from the top,
// not descending into ignored directories, and returns the paths that the
// ignorer ignores, including those that are implicitly ignored because a parent
// is ignored. That's the notion of ignoring that git reports.
func scan(t *testing.T, root string, ignorer ignore.Ignorer) map[string]bool {
	t.Helper()
	ignored := make(map[string]bool)
	var walk func(directory string, parentIgnored bool)
	walk = func(directory string, parentIgnored bool) {
		contents, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(directory)))
		if err != nil {
			t.Fatal("unable to list directory:", err)
		}
		for _, content := range contents {
			path := content.Name()
			if directory != "" {
				path = directory + "/" + path
			}
			if path == ".git" {
				continue
			}
			entryIgnored := parentIgnored
			if !entryIgnored {
				status, _ := ignorer.Ignore(path, content.IsDir())
				entryIgnored = status == ignore.IgnoreStatusIgnored
			}
			if entryIgnored {
				ignored[path] = true
			}
			if content.IsDir() {
				walk(path, entryIgnored)
			}
		}
	}
	walk("", false)
	return ignored
}

// compare compares the ignored paths found by scanning a repository with those
// that git reports, failing the test if they differ. The describe callback
// returns text to include in the report for a path that's mishandled. The
// number of paths that git ignores is returned.
func compare(t *testing.T, h *gitHarness, root string, ignorer ignore.Ignorer, describe func(path string) string) int {
	t.Helper()

	// Compute both sets.
	entries := listEntries(t, root)
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.path
	}
	expected := h.checkIgnore(root, paths)
	actual := scan(t, root, ignorer)

	// Log how much of the tree is ignored, which gives a sense of how well the
	// rules being tested exercise the matcher.
	t.Logf("git ignores %d of %d entries", len(expected), len(entries))

	// Gather differences by scenario.
	var order []string
	differences := make(map[string][]entry)
	for _, e := range entries {
		if expected[e.path] == actual[e.path] {
			continue
		}
		scenario, _, _ := strings.Cut(e.path, "/")
		if differences[scenario] == nil {
			order = append(order, scenario)
		}
		differences[scenario] = append(differences[scenario], e)
	}

	// Report differences, limiting the volume of output.
	for i, scenario := range order {
		if i >= 12 {
			t.Errorf("%d more scenarios with mismatches not shown", len(order)-i)
			break
		}
		var builder strings.Builder
		for j, e := range differences[scenario] {
			if j >= 16 {
				fmt.Fprintf(&builder, "\n... and %d more", len(differences[scenario])-j)
				break
			}
			fmt.Fprintf(
				&builder, "\n%q (directory: %v): git ignores: %v, we ignore: %v",
				e.path, e.directory, expected[e.path], actual[e.path],
			)
		}
		first := differences[scenario][0].path
		t.Errorf(
			"mismatches in scenario %s:%s\ngit says about %q: %s\n%s",
			scenario, builder.String(), first, h.explain(root, first), describe(first),
		)
	}
	return len(expected)
}

// describeScenarios returns a describe callback for compare that prints the
// rule files of the scenario that contains a path, with their contents quoted.
func describeScenarios(scenarios []scenario) func(string) string {
	return func(path string) string {
		name, _, _ := strings.Cut(path, "/")
		for _, s := range scenarios {
			if s.name != name {
				continue
			}
			names := make([]string, 0, len(s.files))
			for file := range s.files {
				names = append(names, file)
			}
			sort.Strings(names)
			var builder strings.Builder
			for _, file := range names {
				fmt.Fprintf(&builder, "%s/%s: %q\n", s.name, file, s.files[file])
			}
			return builder.String()
		}
		return ""
	}
}

// writeScenarios creates scenarios in a repository.
func writeScenarios(t *testing.T, root string, scenarios []scenario) {
	t.Helper()
	for _, s := range scenarios {
		directory := filepath.Join(root, s.name)
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal("unable to create scenario directory:", err)
		}
		createFixture(t, directory)
		for file, contents := range s.files {
			path := filepath.Join(directory, filepath.FromSlash(file))
			if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
				t.Fatal("unable to write rule file:", err)
			}
		}
	}
}

// fixedScenarioFiles are scenarios with hand-written rule files.
var fixedScenarioFiles = []map[string]string{
	// Basic forms.
	{".gitignore": "a\n"},
	{".gitignore": "/a\n"},
	{".gitignore": "a/\n"},
	{".gitignore": "/a/\n"},
	{".gitignore": "b/a\n"},
	{".gitignore": "b/a/\n"},
	{".gitignore": "/b/a/b\n"},
	{".gitignore": "*.txt\n"},
	{".gitignore": "/*.txt\n"},
	{".gitignore": "*\n"},
	{".gitignore": "/*\n"},
	{".gitignore": "**\n"},
	{".gitignore": "/**\n"},
	{".gitignore": "*/\n"},
	{".gitignore": "**/\n"},
	{".gitignore": "/\n!a\n"},
	{".gitignore": "//\n"},
	{".gitignore": "a//\n"},
	{".gitignore": "\\\n"},
	{".gitignore": "!\n"},
	{".gitignore": "!a\n"},
	{".gitignore": "!/\n"},
	{".gitignore": " \n"},
	{".gitignore": "a\nb\n"},

	// Negation.
	{".gitignore": "*\n!a\n"},
	{".gitignore": "*\n!a\n!/b\n"},
	{".gitignore": "*.txt\n!x.txt\n"},
	{".gitignore": "b\n!b/a\n"},
	{".gitignore": "b/*\n!b/a\n"},
	{".gitignore": "b/*\n!b/a\nb/a/b\n"},
	{".gitignore": "!a\na\n"},
	{".gitignore": "e/**\n!e/a\n"},
	{".gitignore": "e/**/\n!e/a/b/\n"},
	{".gitignore": "/e/a/b/c/*\n!/e/a/b/c/a\n"},
	{".gitignore": "d\n!d\n"},
	{".gitignore": "*/\n!b/\n"},

	// Comments, escapes, and spaces.
	{".gitignore": "#a\n"},
	{".gitignore": "\\#a\n"},
	{".gitignore": "\\!a\n"},
	{".gitignore": " a\n"},
	{".gitignore": "a \n"},
	{".gitignore": "a   \n"},
	{".gitignore": "a\\ \n"},
	{".gitignore": "a\\  \n"},
	{".gitignore": "a\\\\ \n"},
	{".gitignore": "a b\n"},
	{".gitignore": "a b \n"},
	{".gitignore": "a\\ b\n"},
	{".gitignore": "a\t\n"},
	{".gitignore": "a/ \n"},
	{".gitignore": "! a\n"},
	{".gitignore": "!  \n"},
	{".gitignore": "\\a\n"},
	{".gitignore": "\\*\n"},
	{".gitignore": "\\[a]\n"},
	{".gitignore": "[a]\n"},
	{".gitignore": "[[]a]\n"},
	{".gitignore": "\\[a\\]\n"},
	{".gitignore": "a\\/\n"},
	{".gitignore": "\\/a\n"},
	{".gitignore": "b\\/a\n"},
	{".gitignore": "b\\/a/\n"},
	{".gitignore": "b/\\a\n"},

	// Line endings, byte order marks, null bytes, and missing newlines.
	{".gitignore": "a\r\n"},
	{".gitignore": "a\r\nb\r\n"},
	{".gitignore": "a\r\r\n"},
	{".gitignore": "a\r"},
	{".gitignore": "a \r\n"},
	{".gitignore": "a\\ \r\n"},
	{".gitignore": "b/\r\n"},
	{".gitignore": "\r\na\r\n"},
	{".gitignore": "\r\n"},
	{".gitignore": "#a\r\nb\r\n"},
	{".gitignore": "\xef\xbb\xbfa\n"},
	{".gitignore": "\xef\xbb\xbf#a\nb\n"},
	{".gitignore": "\xef\xbb\xbf\xef\xbb\xbfa\n"},
	{".gitignore": "\xef\xbb\xbf\n"},
	{".gitignore": "a\x00b\n"},
	{".gitignore": "a\x00\nb\n"},
	{".gitignore": "\x00a\nb\n"},
	{".gitignore": "b\r\x00\n"},
	{".gitignore": "b\x00\r\n"},
	{".gitignore": "a"},
	{".gitignore": "a\n\n\nb"},
	{".gitignore": "a\nb\n!b"},
	{".gitignore": ""},

	// Wildcards.
	{".gitignore": "a?\n"},
	{".gitignore": "?\n"},
	{".gitignore": "??\n"},
	{".gitignore": "?b\n"},
	{".gitignore": "a*\n"},
	{".gitignore": "*a\n"},
	{".gitignore": "*a*\n"},
	{".gitignore": "a*b\n"},
	{".gitignore": "a**b\n"},
	{".gitignore": "a?b\n"},
	{".gitignore": "a.?\n"},
	{".gitignore": "???\n"},
	{".gitignore": "/???\n"},
	{".gitignore": "b/?\n"},
	{".gitignore": "b/*\n"},
	{".gitignore": "b/*/\n"},
	{".gitignore": "b/*/a\n"},
	{".gitignore": "*/a\n"},
	{".gitignore": "*/*/a\n"},
	{".gitignore": "*/*/*\n"},
	{".gitignore": "?/a\n"},
	{".gitignore": "/?/?\n"},
	{".gitignore": "b*/a\n"},
	{".gitignore": "b*a\n"},
	{".gitignore": "/b*a\n"},
	{".gitignore": "/b*/a\n"},
	{".gitignore": "d/x*\n"},

	// Double asterisks.
	{".gitignore": "**/a\n"},
	{".gitignore": "**/a/\n"},
	{".gitignore": "**/b/a\n"},
	{".gitignore": "**/b/a/b\n"},
	{".gitignore": "**/**/a\n"},
	{".gitignore": "/**/a\n"},
	{".gitignore": "b/**\n"},
	{".gitignore": "b/**/\n"},
	{".gitignore": "b/a/**\n"},
	{".gitignore": "b/**/a\n"},
	{".gitignore": "b/**/b\n"},
	{".gitignore": "b/**/b/a\n"},
	{".gitignore": "b/**/**/a\n"},
	{".gitignore": "e/**/c/a\n"},
	{".gitignore": "e/**/c\n"},
	{".gitignore": "e/**/a\n"},
	{".gitignore": "e/a/**/a\n"},
	{".gitignore": "e/a/**/b\n"},
	{".gitignore": "e/**/b/**/a\n"},
	{".gitignore": "e/**/**\n"},
	{".gitignore": "**/e/**\n"},
	{".gitignore": "**/foo\n"},
	{".gitignore": "**/foo/bar\n"},
	{".gitignore": "**/foo/**\n"},
	{".gitignore": "d/**/*.txt\n"},
	{".gitignore": "**/*.txt\n"},
	{".gitignore": "**/x*\n"},
	{".gitignore": "***/a\n"},
	{".gitignore": "b/***/a\n"},
	{".gitignore": "b/***\n"},
	{".gitignore": "b\\/**\n"},
	{".gitignore": "b\\/**/a\n"},
	{".gitignore": "e\\/**/a\n"},
	{".gitignore": "\\/**/a\n"},
	{".gitignore": "\\/**\n"},
	{".gitignore": "e/a\\/**/a\n"},
	{".gitignore": "e/a/**\\/**/a\n"},
	{".gitignore": "e/**/**\\/a\n"},
	{".gitignore": "e/**/b/**\\/a\n"},
	{".gitignore": "e/a/b/c/**\\/a\n"},
	{".gitignore": "e/a/b/c/**\\/*\n"},
	{".gitignore": "b/a**\n"},
	{".gitignore": "b/**a\n"},
	{".gitignore": "b**/a\n"},
	{".gitignore": "b**/a/b\n"},
	{".gitignore": "ba**/a\n"},
	{".gitignore": "ba/ab**/a\n"},
	{".gitignore": "d/foo**/bar\n"},
	{".gitignore": "d/foo**\n"},
	{".gitignore": "d/fo**/bar\n"},
	{".gitignore": "d/f**/bar\n"},
	{".gitignore": "d**/foo\n"},
	{".gitignore": "d**/foo/bar\n"},
	{".gitignore": "e**\n"},
	{".gitignore": "e/a**\n"},
	{".gitignore": "e/a/b**\n"},
	{".gitignore": "e/a/b/c**\n"},
	{".gitignore": "e/a/b/c**/a\n"},
	{".gitignore": "e/a/b/c/**a\n"},
	{".gitignore": "e/a/b/c/**/a\n"},
	{".gitignore": "e/a/b/**/\n"},
	{".gitignore": "e/*/**\n"},
	{".gitignore": "e/*/**/c/a\n"},
	{".gitignore": "e/*/b/**/a\n"},
	{".gitignore": "*/**/a\n"},
	{".gitignore": "*/**\n"},
	{".gitignore": "*/**/\n"},
	{".gitignore": "**/**\n"},
	{".gitignore": "**/**/**\n"},
	{".gitignore": "e/a/\\**\n"},
	{".gitignore": "e/\\**/a\n"},
	{".gitignore": "e/**\\/a/b\n"},
	{".gitignore": "e/**\\/c/a\n"},
	{".gitignore": "e/a/b/**\\/\n"},
	{".gitignore": "e/a/**\\/b\n"},
	{".gitignore": "e/a/b/c/**\\\n"},
	{".gitignore": "e/a/b/c/**\\x\n"},
	{".gitignore": "**\\/a\n"},
	{".gitignore": "x/**\\/a\n"},
	{".gitignore": "[a]**/b\n"},
	{".gitignore": "b/[a]**/b\n"},
	{".gitignore": "b/?**/b\n"},
	{".gitignore": "b/?/**/b\n"},
	{".gitignore": "b/\\a**/a\n"},
	{".gitignore": "b/a\\**/a\n"},

	// Classes.
	{".gitignore": "[ab]\n"},
	{".gitignore": "[!a]\n"},
	{".gitignore": "[^a]\n"},
	{".gitignore": "[!ab]\n"},
	{".gitignore": "[a-c]\n"},
	{".gitignore": "[c-a]\n"},
	{".gitignore": "[a-a]\n"},
	{".gitignore": "[!a-c]\n"},
	{".gitignore": "[^a-c]\n"},
	{".gitignore": "[]a]\n"},
	{".gitignore": "[]]\n"},
	{".gitignore": "[!]a]\n"},
	{".gitignore": "[^]a]\n"},
	{".gitignore": "[!]]\n"},
	{".gitignore": "[a-]\n"},
	{".gitignore": "[-a]\n"},
	{".gitignore": "[a-]b\n"},
	{".gitignore": "[!-]\n"},
	{".gitignore": "[--/]\n"},
	{".gitignore": "[+--]\n"},
	{".gitignore": "[a\n"},
	{".gitignore": "[\n"},
	{".gitignore": "[]\n"},
	{".gitignore": "[!\n"},
	{".gitignore": "[!]\n"},
	{".gitignore": "[^]\n"},
	{".gitignore": "[a-\n"},
	{".gitignore": "[a-]\n"},
	{".gitignore": "[ab\n"},
	{".gitignore": "a[\n"},
	{".gitignore": "a[b\n"},
	{".gitignore": "*[\n"},
	{".gitignore": "*[a\n"},
	{".gitignore": "[a]\n!a\n"},
	{".gitignore": "[[:alpha:]]\n"},
	{".gitignore": "[![:alpha:]]\n"},
	{".gitignore": "[^[:alpha:]]\n"},
	{".gitignore": "[[:alpha:]]*\n"},
	{".gitignore": "[[:digit:]]\n"},
	{".gitignore": "[[:space:]]\n"},
	{".gitignore": "a[[:space:]]b\n"},
	{".gitignore": "[[:punct:]]\n"},
	{".gitignore": "a[[:punct:]]b\n"},
	{".gitignore": "[[:lower:]]\n"},
	{".gitignore": "[[:upper:]]\n"},
	{".gitignore": "[[:alnum:]]\n"},
	{".gitignore": "[[:xdigit:]]\n"},
	{".gitignore": "[[:blank:]]\n"},
	{".gitignore": "[[:cntrl:]]\n"},
	{".gitignore": "[[:graph:]]\n"},
	{".gitignore": "[[:print:]]\n"},
	{".gitignore": "[[:foo:]]\n"},
	{".gitignore": "[[:alpha:]\n"},
	{".gitignore": "[[:alpha:]x]\n"},
	{".gitignore": "[[:alpha:]-z]\n"},
	{".gitignore": "[a-[:alpha:]]\n"},
	{".gitignore": "[[:alpha:][:digit:]]\n"},
	{".gitignore": "[[:alpha]]\n"},
	{".gitignore": "[[:alpha]\n"},
	{".gitignore": "[[:]\n"},
	{".gitignore": "[[:]]\n"},
	{".gitignore": "[[::]]\n"},
	{".gitignore": "[[:\n"},
	{".gitignore": "[[]\n"},
	{".gitignore": "[[]]\n"},
	{".gitignore": "[[a]\n"},
	{".gitignore": "[[a]]\n"},
	{".gitignore": "[:alpha:]\n"},
	{".gitignore": "[\\]a]\n"},
	{".gitignore": "[\\a]\n"},
	{".gitignore": "[a\\]\n"},
	{".gitignore": "[a\\-c]\n"},
	{".gitignore": "[\\a-c]\n"},
	{".gitignore": "[a-\\c]\n"},
	{".gitignore": "[a-\\]\n"},
	{".gitignore": "[a-\\]]\n"},
	{".gitignore": "[\\]\n"},
	{".gitignore": "[\\\n"},
	{".gitignore": "b/a[/]b\n"},
	{".gitignore": "b[/]a\n"},
	{".gitignore": "b[!x]a\n"},
	{".gitignore": "b[^x]a/b\n"},
	{".gitignore": "b[!x]a/b\n"},
	{".gitignore": "b[!x]a\n"},
	{".gitignore": "b?a\n"},
	{".gitignore": "b?a/b\n"},
	{".gitignore": "b*a/b\n"},
	{".gitignore": "[!a-z]/a\n"},
	{".gitignore": "[b-d]/a\n"},
	{".gitignore": "[b-d]*/a\n"},
	{".gitignore": "/[b-d]*/a\n"},
	{".gitignore": "[!a]*\n"},
	{".gitignore": "[é]\n"},
	{".gitignore": "[!é]\n"},
	{".gitignore": "?\n!é\n"},
	{".gitignore": "é\n"},
	{".gitignore": "\\é\n"},
	{".gitignore": "??\n"},
	{".gitignore": "\xc3\n"},
	{".gitignore": "\xc3?\n"},
	{".gitignore": "[\xc3]?\n"},
	{".gitignore": "[\xc3-\xc4]?\n"},
	{".gitignore": "[!\xc3]?\n"},
	{".gitignore": "[\x80-\xff]?\n"},

	// Rules in several files.
	{".gitignore": "a\n", "b/.gitignore": "!a\n"},
	{".gitignore": "a\n", "b/.gitignore": "!a\nb\n"},
	{".gitignore": "*.txt\n", "d/.gitignore": "!x.txt\n"},
	{".gitignore": "*.txt\n", "d/.gitignore": "x.txt\n!*\n"},
	{".gitignore": "!a\n", "b/.gitignore": "a\n"},
	{".gitignore": "/a\n", "b/.gitignore": "/a\n"},
	{".gitignore": "b/a\n", "b/.gitignore": "!a\n"},
	{".gitignore": "b/a\n", "b/.gitignore": "!/a\n"},
	{"b/.gitignore": "/a\n"},
	{"b/.gitignore": "a/b\n"},
	{"b/.gitignore": "/a/b\n"},
	{"b/.gitignore": "/b\n"},
	{"b/.gitignore": "b\n"},
	{"b/.gitignore": "ab\n"},
	{"b/.gitignore": "a/\n"},
	{"b/.gitignore": "**/a\n"},
	{"b/.gitignore": "a/**\n"},
	{"b/.gitignore": "*\n", "b/a/.gitignore": "!a\n"},
	{"b/a/.gitignore": "*\n!b\n"},
	{"b/a/.gitignore": "b/\n", "b/.gitignore": "!a/b/\n"},
	{"d/.gitignore": "foo\n", "d/foo/.gitignore": "!bar\n"},
	{"d/.gitignore": "foo/*\n", "d/foo/.gitignore": "!bar\n"},
	{"d/.gitignore": "foo/*\n!foo/bar\n", "d/foo/.gitignore": "bar\n"},
	{"e/a/b/.gitignore": "/c/a\n"},
	{"e/a/b/.gitignore": "c/a\n"},
	{"e/a/b/.gitignore": "c\n", "e/a/b/c/.gitignore": "!a\n"},
	{".gitignore": "e/a/b/c/a\n", "e/a/b/c/.gitignore": "!a\n"},
	{".gitignore": "\xef\xbb\xbfa\n", "b/.gitignore": "\xef\xbb\xbfab\n"},
	{".gitignore": "a\r\n", "b/.gitignore": "ab\r\n"},
	{"d/.gitignore": "/foo\n"},
	{"d/.gitignore": "foo/bar\n"},
	{"d/.gitignore": "foo/**\n"},
	{"d/.gitignore": "**/bar\n"},
	{"d/.gitignore": "*\n", "d/foo/.gitignore": "!bar\n"},
	{"d/foo/.gitignore": "*\n!bar\n"},
	{"d/foo/.gitignore": "bar\n!bar\n"},
	{".gitignore": ".gitignore\n"},
	{".gitignore": "!.gitignore\n*\n"},
	{".gitignore": "*\n!.gitignore\n"},
	{".gitignore": "*\n!*/\n!.gitignore\n", "b/.gitignore": "ab\n"},
	{".gitignore": ".git\n"},
	{".gitignore": ".git*\n"},
	{".gitignore": "/.gitignore\n"},
	{".gitignore": "b/.gitignore\n", "b/.gitignore": "x\n"},
	{".gitignore": "l\n"},
	{".gitignore": "l/\n"},
	{".gitignore": "m/\n"},
	{".gitignore": "d/n/\n"},
	{".gitignore": "d/n\n"},
	{".gitignore": "*/\n!l/\n"},
	{".gitignore": "[lm]\n"},
	{".gitignore": "[lm]/\n"},
}

// randomRule generates a random line for a rule file. The fragments it's built
// from are chosen to exercise all of the syntax that has a special meaning.
func randomRule(r *rand.Rand) string {
	fragments := []string{
		"a", "b", "ab", "a", "b", "foo", "x", "c", "d", "e", "bar", ".txt",
		"é", " ", " ", "*", "*", "**", "?", "?", "/", "/", "[ab]", "[!a]",
		"[a-c]", "\\", "!", ".", "**/", "/**", "[[:alpha:]]", "[]a]", "[a-",
		"[[:foo:]]", "[^b]", "\\*", "\\?", "\\[", "\\/", "\\ ", "#", "[", "]",
		"-", "^", "[[:digit:]]", "[a-]", "[-a]", "[!]a]", "**/", "/**/",
	}

	// Generate a pattern either from a random sequence of fragments or from a
	// mutation of the path of one of the fixture entries. The latter are far
	// more likely to match something.
	var pattern string
	if r.Intn(2) == 0 {
		for i, count := 0, 1+r.Intn(5); i < count; i++ {
			pattern += fragments[r.Intn(len(fragments))]
		}
	} else {
		path := strings.TrimSuffix(fixtureEntries[r.Intn(len(fixtureEntries))], "/")
		path, _, _ = strings.Cut(path, " -> ")
		components := strings.Split(path, "/")
		var builder strings.Builder
		for i, component := range components {
			if i > 0 {
				builder.WriteByte('/')
			}
			switch r.Intn(10) {
			case 0:
				builder.WriteString("*")
			case 1:
				builder.WriteString("**")
			case 2:
				builder.WriteString("?")
			case 3:
				builder.WriteString("[ab]")
			case 4:
				builder.WriteString(component[:len(component)/2] + "*")
			case 5:
				builder.WriteString(component + "**")
			case 6:
				builder.WriteString("[!x]" + component[len(component)/2:])
			default:
				builder.WriteString(component)
			}
		}
		pattern = builder.String()
	}

	// Add decorations.
	if r.Intn(5) == 0 {
		pattern = "/" + pattern
	}
	if r.Intn(5) == 0 {
		pattern += "/"
	}
	if r.Intn(4) == 0 {
		pattern = "!" + pattern
	}
	switch r.Intn(12) {
	case 0:
		pattern += " "
	case 1:
		pattern += "\r"
	}
	return pattern
}

// randomScenarios generates scenarios with random rule files.
func randomScenarios(r *rand.Rand, count int, prefix string) []scenario {
	scenarios := make([]scenario, count)
	for i := range scenarios {
		files := make(map[string]string)
		locations := []struct {
			path        string
			probability int
		}{{".gitignore", 1}, {"d/.gitignore", 3}, {"b/a/.gitignore", 3}, {"e/a/b/.gitignore", 5}, {"b/.gitignore", 4}}
		for _, location := range locations {
			if r.Intn(location.probability) != 0 {
				continue
			}
			var contents strings.Builder
			for j, lines := 0, 1+r.Intn(3); j < lines; j++ {
				contents.WriteString(randomRule(r))
				contents.WriteByte('\n')
			}
			files[location.path] = contents.String()
		}
		scenarios[i] = scenario{name: prefix + strconv.Itoa(i), files: files}
	}
	return scenarios
}

// fuzzParameters returns the seed and the number of random scenarios for a test
// that uses random rules. The defaults keep the tests fast and deterministic,
// but they can be overridden using the MUTAGEN_GITIGNORE_SEED and
// MUTAGEN_GITIGNORE_COUNT environment variables in order to explore further.
func fuzzParameters(t *testing.T, defaultSeed int64, defaultCount int) (int64, int) {
	t.Helper()
	seed, count := defaultSeed, defaultCount
	if value := os.Getenv("MUTAGEN_GITIGNORE_SEED"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatal("invalid seed:", err)
		}
		seed = parsed
	}
	if value := os.Getenv("MUTAGEN_GITIGNORE_COUNT"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal("invalid count:", err)
		}
		count = parsed
	}
	t.Logf("using seed %d and %d random scenarios", seed, count)
	return seed, count
}

// TestGitDifferential compares the results of scans using this package with
// the results of git for fixed and random rule files.
func TestGitDifferential(t *testing.T) {
	// Create a harness and a repository.
	h := newGitHarness(t)
	root := t.TempDir()
	h.initialize(root)

	// Create fixed scenarios.
	var scenarios []scenario
	for i, files := range fixedScenarioFiles {
		scenarios = append(scenarios, scenario{name: "f" + strconv.Itoa(i), files: files})
	}

	// Create random scenarios.
	seed, count := fuzzParameters(t, 1, 300)
	scenarios = append(scenarios, randomScenarios(rand.New(rand.NewSource(seed)), count, "r")...)

	// Write the scenarios and compare.
	writeScenarios(t, root, scenarios)
	if compare(t, h, root, NewIgnorer(root, nil), describeScenarios(scenarios)) == 0 {
		t.Error("git did not ignore anything")
	}
}
