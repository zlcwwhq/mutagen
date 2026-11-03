package gitignore

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// This file contains differential tests for nested repositories. Git evaluates
// the paths beneath the top level of a nested repository's working tree for
// that repository alone when it runs in it, without the rules of the
// directories above it, so the ignorer's decisions for those paths are
// compared with those of git run in the nested repository.

// testRepository is a repository in a differential test of nested
// repositories.
type testRepository struct {
	// path is the root-relative, slash-separated path of the top level of the
	// repository's working tree, which is empty for the repository at the
	// root.
	path string
	// submodule indicates whether or not the repository is added as a
	// submodule of the repository that contains it, which makes its ".git"
	// entry a file that points into the Git directory of that repository.
	submodule bool
	// exclude is the contents of the repository's exclude file.
	exclude string
	// files maps the paths of rule files, relative to the top level of the
	// repository's working tree, to their contents.
	files map[string]string
}

// enclosingRepository returns the root-relative path of the repository that
// contains the one at the specified path, which is the deepest of the
// repositories whose working tree contains it. It must not be called for the
// repository at the root.
func enclosingRepository(repositories []testRepository, path string) string {
	var enclosing string
	for _, repository := range repositories {
		if repository.path != "" && strings.HasPrefix(path, repository.path+"/") && len(repository.path) > len(enclosing) {
			enclosing = repository.path
		}
	}
	return enclosing
}

// createRepositories creates repositories beneath a root, each of which
// contains the fixture tree (see createFixture), and writes their rule files.
// A repository must follow the repository that contains it.
func createRepositories(t *testing.T, h *gitHarness, root string, repositories []testRepository) {
	t.Helper()
	var source string
	for _, repository := range repositories {
		// Create the repository.
		directory := filepath.Join(root, filepath.FromSlash(repository.path))
		if repository.submodule {
			// Create a repository with a commit to add as a submodule, if
			// that hasn't been done yet.
			if source == "" {
				source = t.TempDir()
				h.initialize(source)
				h.run(
					source, nil,
					"-c", "user.name=Test", "-c", "user.email=test@example.com",
					"commit", "--quiet", "--allow-empty", "--message=initial",
				)
			}

			// Add it to the repository that contains it, and verify that the
			// result is what we expect.
			enclosing := enclosingRepository(repositories, repository.path)
			relative := strings.TrimPrefix(repository.path, enclosing+"/")
			h.run(
				filepath.Join(root, filepath.FromSlash(enclosing)), nil,
				"-c", "protocol.file.allow=always",
				"submodule", "add", "--quiet", source, relative,
			)
			if info, err := os.Lstat(filepath.Join(directory, ".git")); err != nil || !info.Mode().IsRegular() {
				t.Fatal("submodule does not have a Git file")
			}
		} else {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal("unable to create repository directory:", err)
			}
			h.initialize(directory)
		}

		// Create the fixture.
		createFixture(t, directory)

		// Write the exclude file where git expects it, which, for a submodule,
		// is in the Git directory of the repository at the root.
		output, _ := h.run(directory, nil, "rev-parse", "--git-path", "info/exclude")
		exclude := strings.TrimRight(string(output), "\r\n")
		if !filepath.IsAbs(exclude) {
			exclude = filepath.Join(directory, exclude)
		}
		if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
			t.Fatal("unable to create exclude file directory:", err)
		} else if err := os.WriteFile(exclude, []byte(repository.exclude), 0o644); err != nil {
			t.Fatal("unable to write exclude file:", err)
		}

		// Write the rule files.
		for path, contents := range repository.files {
			if err := os.WriteFile(filepath.Join(directory, filepath.FromSlash(path)), []byte(contents), 0o644); err != nil {
				t.Fatal("unable to write rule file:", err)
			}
		}
	}
}

// listWorkingTree lists the entries of a repository's working tree, other than
// its Git entry, without descending into nested repositories, whose top-level
// directories are listed, but whose contents aren't. Entries are listed after
// their parents.
func listWorkingTree(t *testing.T, top string) []entry {
	t.Helper()
	var entries []entry
	var walk func(string)
	walk = func(directory string) {
		contents, err := os.ReadDir(filepath.Join(top, filepath.FromSlash(directory)))
		if err != nil {
			t.Fatal("unable to list directory:", err)
		}
		for _, content := range contents {
			path := content.Name()
			if directory != "" {
				path = directory + "/" + path
			} else if path == ".git" {
				continue
			}
			entries = append(entries, entry{path, content.IsDir()})
			if content.IsDir() {
				if _, err := os.Lstat(filepath.Join(top, filepath.FromSlash(path), ".git")); err != nil {
					walk(path)
				}
			}
		}
	}
	walk("")
	return entries
}

// compareRepositories compares the decisions of an ignorer for the root with
// those of git for the entries of each repository's working tree, failing the
// test if they differ. Git is run in the repository whose working tree
// contains the entries, and the ignorer is used as a scan would use it,
// starting at the top level of that working tree, so that what the
// repositories that contain it decide about it doesn't matter. The top level
// of a nested repository's working tree is an entry of the repository that
// contains it. The number of entries that git ignores in each repository is
// returned, along with the number of entries in each nested repository that
// git run in the repository that contains it would treat differently, which
// shows how much the comparison depends on the rules of each nested repository
// replacing those above it.
func compareRepositories(t *testing.T, h *gitHarness, root string, ignorer ignore.Ignorer, repositories []testRepository) (map[string]int, map[string]int) {
	t.Helper()
	ignored := make(map[string]int)
	different := make(map[string]int)
	for _, repository := range repositories {
		// Ask git which entries of the working tree it ignores.
		top := filepath.Join(root, filepath.FromSlash(repository.path))
		entries := listWorkingTree(t, top)
		paths := make([]string, len(entries))
		for i, e := range entries {
			paths[i] = e.path
		}
		expected := h.checkIgnore(top, paths)
		ignored[repository.path] = len(expected)

		// Ask the ignorer the same, skipping the evaluation of the contents of
		// ignored directories, as a scan would.
		var prefix string
		if repository.path != "" {
			prefix = repository.path + "/"
		}
		actual := make(map[string]bool)
		for _, e := range entries {
			if slash := strings.LastIndexByte(e.path, '/'); slash >= 0 && actual[e.path[:slash]] {
				actual[e.path] = true
			} else if status, _ := ignorer.Ignore(prefix+e.path, e.directory); status == ignore.IgnoreStatusIgnored {
				actual[e.path] = true
			}
		}

		// Report differences, limiting the volume of output.
		var mismatches int
		for _, e := range entries {
			if expected[e.path] == actual[e.path] {
				continue
			} else if mismatches++; mismatches > 16 {
				continue
			}
			t.Errorf(
				"repository %q: %q (directory: %v): git ignores: %v, we ignore: %v\ngit says: %s",
				repository.path, e.path, e.directory, expected[e.path], actual[e.path], h.explain(top, e.path),
			)
		}
		if mismatches > 16 {
			t.Errorf("repository %q: %d more mismatches not shown", repository.path, mismatches-16)
		}

		// For a nested repository, count the entries that git run in the
		// repository that contains it would treat differently, which it
		// evaluates with the rules of the containing repository (along with
		// the nested repository's rule files, but not its exclude file).
		if repository.path != "" {
			enclosing := enclosingRepository(repositories, repository.path)
			relative := strings.TrimPrefix(repository.path, enclosing+"/")
			outer := make([]string, len(paths))
			for i, path := range paths {
				outer[i] = relative + "/" + path
			}
			fromEnclosing := h.checkIgnore(filepath.Join(root, filepath.FromSlash(enclosing)), outer)
			for i, path := range paths {
				if fromEnclosing[outer[i]] != expected[path] {
					different[repository.path]++
				}
			}
		}
		t.Logf(
			"repository %q: git ignores %d of %d entries, and git run in the enclosing repository differs on %d",
			repository.path, len(expected), len(entries), different[repository.path],
		)
	}
	return ignored, different
}

// TestGitNestedRepositories compares the treatment of the contents of nested
// repositories with that of git run in those repositories, for fixed rules.
func TestGitNestedRepositories(t *testing.T) {
	tests := []struct {
		// name is the name of the test.
		name string
		// repositories are the repositories, the first of which must be at
		// the root.
		repositories []testRepository
		// unignoring is the path of a nested repository whose rules don't
		// ignore anything (with nothing else nested in it), for which it's
		// only checked that the rules above it don't apply.
		unignoring string
	}{
		{
			name: "rules above a nested repository without rules",
			repositories: []testRepository{
				{exclude: "ab\n[a]\n", files: map[string]string{
					".gitignore":   "a\n*.txt\nb/\n/c\nd/foo\n**/bar\ne/**\nn/a\n",
					"d/.gitignore": "a\n",
				}},
				{path: "n"},
			},
			unignoring: "n",
		},
		{
			name: "rules above and within a nested repository",
			repositories: []testRepository{
				{files: map[string]string{".gitignore": "a\n*.txt\nb/\n"}},
				{path: "n", exclude: "/ab\nb/a/\ne/**/c\n", files: map[string]string{
					".gitignore":   "d/*.txt\n/x.txt\n",
					"d/.gitignore": "foo/\n!a\n",
				}},
			},
		},
		{
			name: "negations above a nested repository",
			repositories: []testRepository{
				{exclude: "!foo\nab\n", files: map[string]string{".gitignore": "!*.txt\n!a\nc\n"}},
				{path: "n", exclude: "*.txt\na\nfoo\n", files: map[string]string{
					".gitignore":   "!x.txt\n",
					"d/.gitignore": "!foo\n",
				}},
			},
		},
		{
			name: "rules above that ignore a nested repository",
			repositories: []testRepository{
				{files: map[string]string{".gitignore": "n/\n*.txt\n"}},
				{path: "n", exclude: "a\n"},
			},
		},
		{
			name: "anchored rules above that ignore a nested repository",
			repositories: []testRepository{
				{exclude: "/n\n", files: map[string]string{".gitignore": "a\n"}},
				{path: "n", files: map[string]string{".gitignore": "/ab\n"}},
			},
		},
		{
			name: "nested repository in a subdirectory",
			repositories: []testRepository{
				{files: map[string]string{".gitignore": "*.txt\nb/n/a\n", "b/.gitignore": "a\nfoo/\n"}},
				{path: "b/n", files: map[string]string{"d/.gitignore": "bar\n"}},
			},
		},
		{
			name: "submodule",
			repositories: []testRepository{
				{exclude: "foo\n", files: map[string]string{".gitignore": "*.txt\na\n"}},
				{path: "n", submodule: true, exclude: "b/\nab\n", files: map[string]string{".gitignore": "!a\nc\n"}},
			},
		},
		{
			name: "repository nested in a nested repository",
			repositories: []testRepository{
				{files: map[string]string{".gitignore": "*.txt\n"}},
				{path: "n", exclude: "a\n", files: map[string]string{".gitignore": "foo\ni/c\n", "d/.gitignore": "!a\n"}},
				{path: "n/i", exclude: "b/\n", files: map[string]string{".gitignore": "c\n"}},
			},
		},
		{
			name: "submodule nested in a submodule",
			repositories: []testRepository{
				{exclude: "*.txt\n"},
				{path: "n", submodule: true, files: map[string]string{".gitignore": "a\n"}},
				{path: "n/i", submodule: true, exclude: "e/\n"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create the repositories and compare.
			h := newGitHarness(t)
			root := t.TempDir()
			createRepositories(t, h, root, test.repositories)
			ignored, different := compareRepositories(t, h, root, NewIgnorer(root, nil), test.repositories)

			// Make sure that the rules above each nested repository would
			// have made a difference, and that each repository's rules are
			// effective, otherwise this test isn't testing what it should.
			for _, repository := range test.repositories {
				if repository.path != "" && different[repository.path] == 0 {
					t.Errorf("repository %q: rules above it don't make a difference", repository.path)
				}
				if test.unignoring != "" && repository.path == test.unignoring {
					if ignored[repository.path] != 0 {
						t.Errorf("repository %q: git ignored entries without any rules", repository.path)
					}
				} else if ignored[repository.path] == 0 {
					t.Errorf("repository %q: git did not ignore anything", repository.path)
				}
			}
		})
	}
}

// randomExclude generates the contents of a random exclude file.
func randomExclude(r *rand.Rand) string {
	var contents strings.Builder
	for i, lines := 0, r.Intn(4); i < lines; i++ {
		contents.WriteString(randomRule(r))
		contents.WriteByte('\n')
	}
	return contents.String()
}

// TestGitNestedRepositoriesRandom compares the treatment of the contents of
// nested repositories with that of git run in those repositories, for random
// rules.
func TestGitNestedRepositoriesRandom(t *testing.T) {
	// Create a harness and generate random repositories in a parent, some of
	// which contain nested repositories of their own. Older versions of Git
	// can't be compared for some patterns, so rules with those are dropped.
	h := newGitHarness(t)
	seed, count := fuzzParameters(t, 11, 12)
	r := rand.New(rand.NewSource(seed))
	predatesFix := h.predatesPrefixedAsterisksFix()
	random := func(path string) testRepository {
		repository := testRepository{path: path, exclude: randomExclude(r)}
		repository.files = randomScenarios(r, 1, "")[0].files
		if predatesFix {
			if rulesHavePrefixedAsterisks(repository.exclude) {
				repository.exclude = ""
			}
			for file, contents := range repository.files {
				if rulesHavePrefixedAsterisks(contents) {
					delete(repository.files, file)
				}
			}
		}
		return repository
	}
	// The repository at the root also gets fixed rules that would ignore
	// content in the nested repositories if they applied there.
	repositories := []testRepository{random("")}
	repositories[0].exclude += "*.txt\nfoo\nb/\n"
	for i := 0; i < count; i++ {
		path := "n" + strconv.Itoa(i)
		repositories = append(repositories, random(path))
		if i%3 == 2 {
			repositories = append(repositories, random(path+"/i"))
		}
	}

	// Create the repositories and compare.
	root := t.TempDir()
	createRepositories(t, h, root, repositories)
	ignored, _ := compareRepositories(t, h, root, NewIgnorer(root, nil), repositories)
	var total int
	for _, count := range ignored {
		total += count
	}
	if total == 0 {
		t.Error("git did not ignore anything")
	}
	if t.Failed() {
		for _, repository := range repositories {
			t.Log(describeRepository(repository))
		}
	}
}

// describeRepository describes the rules of a repository for diagnostics.
func describeRepository(repository testRepository) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "repository %q: exclude: %q", repository.path, repository.exclude)
	for file, contents := range repository.files {
		fmt.Fprintf(&builder, ", %s: %q", file, contents)
	}
	return builder.String()
}

// TestGitNestedRepositoryAcceptance compares the treatment of directories with
// ".git" entries that Git may or may not accept with that of git status run in
// the repository that contains them. Git treats a directory whose ".git" entry
// doesn't locate a Git directory that it accepts as part of the enclosing
// repository, whose rules then apply in it, and it lists the directory's
// contents. Otherwise, it lists the directory as an untracked nested
// repository, and the rules above it don't apply in it, while its own exclude
// file does.
func TestGitNestedRepositoryAcceptance(t *testing.T) {
	// Create a harness and a repository whose rules ignore a file in each
	// directory.
	h := newGitHarness(t)
	root := t.TempDir()
	h.initialize(root)
	writeTree(t, root, map[string]string{".gitignore": "*.env\n"})

	// Create the directories, each with a file that the rules above it ignore,
	// a file that its own exclude file would ignore (where one can be located),
	// and a file that nothing ignores.
	type variant struct {
		// accepted indicates whether or not Git is expected to accept the
		// directory's ".git" entry.
		accepted bool
		// setup creates the directory's ".git" entry, given the directory's
		// path, and returns the path of the exclude file that it would locate,
		// or an empty string if it wouldn't locate any.
		setup func(t *testing.T, directory string) string
	}
	gitFile := func(t *testing.T, directory, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(directory, ".git"), []byte(contents), 0o644); err != nil {
			t.Fatal("unable to write Git file:", err)
		}
	}
	modules := filepath.Join(root, ".git", "modules")
	bare := func(t *testing.T, name string) {
		t.Helper()
		directory := filepath.Join(modules, name)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal("unable to create Git directory:", err)
		}
		h.initialize(directory, "--bare")
	}
	variants := map[string]variant{
		"repository": {true, func(t *testing.T, directory string) string {
			h.initialize(directory)
			return filepath.Join(directory, ".git", "info", "exclude")
		}},
		"submodule": {true, func(t *testing.T, directory string) string {
			bare(t, "submodule")
			gitFile(t, directory, "gitdir: ../.git/modules/submodule\n")
			return filepath.Join(modules, "submodule", "info", "exclude")
		}},
		"detached": {true, func(t *testing.T, directory string) string {
			h.initialize(directory)
			if err := os.WriteFile(filepath.Join(directory, ".git", "HEAD"), []byte(strings.Repeat("ab", 20)+"\n"), 0o644); err != nil {
				t.Fatal("unable to write HEAD:", err)
			}
			return filepath.Join(directory, ".git", "info", "exclude")
		}},
		"empty": {false, func(t *testing.T, directory string) string {
			if err := os.Mkdir(filepath.Join(directory, ".git"), 0o755); err != nil {
				t.Fatal("unable to create Git directory:", err)
			}
			return filepath.Join(directory, ".git", "info", "exclude")
		}},
		"invalid-head": {false, func(t *testing.T, directory string) string {
			h.initialize(directory)
			if err := os.WriteFile(filepath.Join(directory, ".git", "HEAD"), []byte("nonsense\n"), 0o644); err != nil {
				t.Fatal("unable to write HEAD:", err)
			}
			return filepath.Join(directory, ".git", "info", "exclude")
		}},
		"no-objects": {false, func(t *testing.T, directory string) string {
			h.initialize(directory)
			if err := os.RemoveAll(filepath.Join(directory, ".git", "objects")); err != nil {
				t.Fatal("unable to remove objects:", err)
			}
			return filepath.Join(directory, ".git", "info", "exclude")
		}},
		"dangling": {false, func(t *testing.T, directory string) string {
			gitFile(t, directory, "gitdir: ../.git/modules/missing\n")
			return ""
		}},
		"unaccepted": {false, func(t *testing.T, directory string) string {
			if err := os.MkdirAll(filepath.Join(modules, "unaccepted", "info"), 0o755); err != nil {
				t.Fatal("unable to create Git directory:", err)
			}
			gitFile(t, directory, "gitdir: ../.git/modules/unaccepted\n")
			return filepath.Join(modules, "unaccepted", "info", "exclude")
		}},
		"malformed": {false, func(t *testing.T, directory string) string {
			bare(t, "malformed")
			gitFile(t, directory, "gitdir:../.git/modules/malformed\n")
			return filepath.Join(modules, "malformed", "info", "exclude")
		}},
	}
	for name, v := range variants {
		directory := filepath.Join(root, name)
		writeTree(t, directory, map[string]string{"secret.env": "", "local.tmp": "", "plain": ""})
		if exclude := v.setup(t, directory); exclude != "" {
			if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
				t.Fatal("unable to create exclude file directory:", err)
			} else if err := os.WriteFile(exclude, []byte("*.tmp\n"), 0o644); err != nil {
				t.Fatal("unable to write exclude file:", err)
			}
		}
	}

	// Ask git which directories are nested repositories and which of the
	// contents of the others it ignores.
	output, _ := h.run(root, nil, "status", "--porcelain", "-z", "--ignored", "--untracked-files=all")
	listed := make(map[string]string)
	for _, line := range strings.Split(string(output), "\x00") {
		if len(line) > 3 {
			listed[line[3:]] = line[:2]
		}
	}

	// Compare.
	ignorer := NewIgnorer(root, nil)
	for name, v := range variants {
		nested := listed[name+"/"] == "??"
		if nested != v.accepted {
			t.Errorf("%s: git accepts Git entry: %v, expected: %v", name, nested, v.accepted)
			continue
		}
		for _, file := range []string{"secret.env", "local.tmp", "plain"} {
			path := name + "/" + file
			var expected bool
			if nested {
				expected = file == "local.tmp"
			} else if status, ok := listed[path]; !ok {
				t.Errorf("%s: not listed by git", path)
				continue
			} else {
				expected = status == "!!"
			}
			status, _ := ignorer.Ignore(path, false)
			if ignored := status == ignore.IgnoreStatusIgnored; ignored != expected {
				t.Errorf("%s: git ignores: %v, we ignore: %v", path, expected, ignored)
			}
		}
	}
}
