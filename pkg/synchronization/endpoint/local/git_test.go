package local

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/git"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
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

// TestShallowestUntracked tests shallowestUntracked.
func TestShallowestUntracked(t *testing.T) {
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	untracked := &core.Entry{Kind: core.EntryKind_Untracked}
	problematic := &core.Entry{Kind: core.EntryKind_Problematic, Problem: "problem"}
	root := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{
		"file":        file,
		"ignored":     untracked,
		"problematic": problematic,
		"directory": {Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{
			"file":    file,
			"ignored": untracked,
		}},
		"phantom": {Kind: core.EntryKind_PhantomDirectory, Contents: map[string]*core.Entry{
			"ignored": untracked,
		}},
	}}
	tests := []struct {
		path     string
		expected string
	}{
		{"file", ""},
		{"missing", ""},
		{"file/below", ""},
		{"problematic", ""},
		{"problematic/below", ""},
		{"ignored", "ignored"},
		{"ignored/below", "ignored"},
		{"ignored/below/further", "ignored"},
		{"directory", ""},
		{"directory/file", ""},
		{"directory/missing", ""},
		{"directory/missing/below", ""},
		{"directory/ignored", "directory/ignored"},
		{"directory/ignored/below", "directory/ignored"},
		{"phantom/ignored", "phantom/ignored"},
		{"phantom/ignored/below", "phantom/ignored"},
	}
	for _, test := range tests {
		if result := shallowestUntracked(root, test.path); result != test.expected {
			t.Errorf("incorrect result for %q: %q != %q", test.path, result, test.expected)
		}
	}
}

// newTrackedIgnoredRepository creates a repository in a new directory, with
// .gitignore rules for *.o files and build directories, in which the specified
// files are tracked. Files that match the rules have to be added forcefully.
func newTrackedIgnoredRepository(t *testing.T, tracked []string, untracked ...string) string {
	t.Helper()
	root := filepath.Join(realTemporaryDirectory(t), "root")
	initializeRepository(t, root)
	files := map[string]string{".gitignore": "*.o\nbuild/\n"}
	for _, file := range slices.Concat(tracked, untracked) {
		files[file] = "content of " + file
	}
	writeTree(t, root, files)
	for _, file := range tracked {
		if _, err := runGit(root, "add", "--force", "--", file); err != nil {
			t.Fatal("unable to add file to repository:", err)
		}
	}
	return root
}

// warningPaths returns the paths of warnings in a snapshot.
func warningPaths(snapshot *core.Snapshot) []string {
	var paths []string
	for _, warning := range snapshot.Warnings {
		paths = append(paths, warning.Path)
	}
	return paths
}

// TestGitWarningsTrackedIgnored tests warnings about tracked files that match
// rules, with one warning for each such file or ignored directory containing
// such files.
func TestGitWarningsTrackedIgnored(t *testing.T) {
	requireGit(t)
	root := newTrackedIgnoredRepository(t,
		[]string{"main.c", "tracked.o", "with space.o", "sub/nested.o", "build/out/first", "build/out/second", "sub/build/other"},
		"untracked.o", "build/untracked",
	)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	snapshot := scanEndpoint(t, e, true)

	// Verify which warnings are reported, in what order.
	expected := []string{"build", "sub/build", "sub/nested.o", "tracked.o", "with space.o"}
	if paths := warningPaths(snapshot); !slices.Equal(paths, expected) {
		t.Fatalf("incorrect warning paths: %v != %v", paths, expected)
	}

	// Verify the advice, which differs for directories and for files.
	for _, warning := range snapshot.Warnings {
		if !strings.Contains(warning.Error, "tracked by Git") || !strings.Contains(warning.Error, "matched by .gitignore") {
			t.Errorf("warning for %q doesn't describe the problem: %s", warning.Path, warning.Error)
		}
		if !strings.Contains(warning.Error, `"!`+warning.Path+`" to ignore.paths`) {
			t.Errorf("warning for %q doesn't have correct advice: %s", warning.Path, warning.Error)
		}
		isDirectory := strings.HasSuffix(warning.Path, "build")
		if mentionsDirectory := strings.Contains(warning.Error, "whole directory"); mentionsDirectory != isDirectory {
			t.Errorf("warning for %q has incorrect form: %s", warning.Path, warning.Error)
		}
		if !warning.Warning {
			t.Errorf("warning for %q not indicated as such", warning.Path)
		}
	}

	// Verify that warnings are only attached to the returned snapshot, since
	// the stored one acts as a baseline and is compared by poll-based watching.
	if len(e.snapshot.Warnings) != 0 {
		t.Error("warnings stored in the endpoint's snapshot")
	}
	if snapshot == e.snapshot || snapshot.Content != e.snapshot.Content {
		t.Error("warnings not attached to shallow copy of the endpoint's snapshot")
	}
	if !snapshot.Equal(e.snapshot) {
		t.Error("warnings affect snapshot equality")
	}
	if snapshot.Files != e.snapshot.Files || snapshot.Directories != e.snapshot.Directories {
		t.Error("statistics not retained")
	}

	// Verify that warnings are computed for scans that reuse the last snapshot,
	// which are the common case for poll-based watching.
	e.accelerate = true
	if paths := warningPaths(scanEndpoint(t, e, false)); !slices.Equal(paths, expected) {
		t.Errorf("incorrect warning paths for reused snapshot: %v != %v", paths, expected)
	}
	e.accelerate = false

	// Verify that warnings go away when the file does.
	if err := os.Remove(filepath.Join(root, "tracked.o")); err != nil {
		t.Fatal("unable to remove file:", err)
	}
	expected = []string{"build", "sub/build", "sub/nested.o", "with space.o"}
	if paths := warningPaths(scanEndpoint(t, e, true)); !slices.Equal(paths, expected) {
		t.Errorf("incorrect warning paths after removal: %v != %v", paths, expected)
	}

	// Verify that warnings are computed with exclude files.
	writeTree(t, root, map[string]string{".git/info/exclude": "main.c\n"})
	expected = []string{"build", "main.c", "sub/build", "sub/nested.o", "with space.o"}
	if paths := warningPaths(scanEndpoint(t, e, true)); !slices.Equal(paths, expected) {
		t.Errorf("incorrect warning paths with exclude file: %v != %v", paths, expected)
	}

	// Verify that listing doesn't fail if there's no exclude file, which Git
	// would otherwise treat as a fatal error.
	if err := os.Remove(filepath.Join(root, ".git", "info", "exclude")); err != nil {
		t.Fatal("unable to remove exclude file:", err)
	}
	expected = []string{"build", "sub/build", "sub/nested.o", "with space.o"}
	if paths := warningPaths(scanEndpoint(t, e, true)); !slices.Equal(paths, expected) {
		t.Errorf("incorrect warning paths without exclude file: %v != %v", paths, expected)
	}

	// Verify that warnings don't change the repository or its contents, so that
	// they can't wake the session.
	if status, err := runGit(root, "status", "--porcelain", "--", "main.c"); err != nil {
		t.Error("unable to query repository status:", err)
	} else if status != "A  main.c" {
		t.Errorf("repository modified: %s", status)
	}
}

// TestGitWarningsSessionIgnores tests that the session's ignores, which are
// what the warnings suggest, make warnings go away.
func TestGitWarningsSessionIgnores(t *testing.T) {
	requireGit(t)
	tracked := []string{"tracked.o", "other.o", "build/file", "sub/build/file"}
	tests := []struct {
		description string
		ignores     []string
		expected    []string
	}{
		{"no ignores", nil, []string{"build", "other.o", "sub/build", "tracked.o"}},
		{"unignored file", []string{"!tracked.o"}, []string{"build", "other.o", "sub/build"}},
		{"unignored directory by name", []string{"!build"}, []string{"other.o", "tracked.o"}},
		{"unignored directory by path", []string{"!sub/build"}, []string{"build", "other.o", "tracked.o"}},
		{"unignored everything", []string{"!*.o", "!build"}, nil},
		{"also ignored by the session", []string{"tracked.o"}, []string{"build", "other.o", "sub/build", "tracked.o"}},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			root := newTrackedIgnoredRepository(t, tracked)
			e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch, test.ignores...)
			if paths := warningPaths(scanEndpoint(t, e, true)); !slices.Equal(paths, test.expected) {
				t.Errorf("incorrect warning paths: %v != %v", paths, test.expected)
			}
		})
	}
}

// TestGitWarningsLimit tests that warnings are limited in number, that they're
// consistent across scans, and that the summary doesn't use the empty path,
// which remote treats as overlapping with everything.
func TestGitWarningsLimit(t *testing.T) {
	requireGit(t)
	var tracked []string
	for i := 0; i < maximumTrackedIgnoredWarnings+5; i++ {
		tracked = append(tracked, fmt.Sprintf("file%03d.o", i))
	}
	root := newTrackedIgnoredRepository(t, tracked)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	snapshot := scanEndpoint(t, e, true)
	if len(snapshot.Warnings) != maximumTrackedIgnoredWarnings+1 {
		t.Fatalf("incorrect warning count: %d", len(snapshot.Warnings))
	}
	if err := snapshot.EnsureValid(); err != nil {
		t.Error("snapshot invalid:", err)
	}
	for i, warning := range snapshot.Warnings {
		if warning.Path != tracked[i] {
			t.Errorf("incorrect path for warning %d: %s != %s", i, warning.Path, tracked[i])
		}
	}
	summary := snapshot.Warnings[maximumTrackedIgnoredWarnings]
	if !strings.Contains(summary.Error, "this and 4 more paths") {
		t.Errorf("incorrect summary: %s", summary.Error)
	}
	if !summary.Warning {
		t.Error("summary not indicated as a warning")
	}
	if paths := warningPaths(scanEndpoint(t, e, true)); !slices.Equal(paths, warningPaths(snapshot)) {
		t.Errorf("warnings differ between scans: %v != %v", paths, warningPaths(snapshot))
	}

	// A summary that would only cover a single path isn't used in its place.
	root = newTrackedIgnoredRepository(t, tracked[:maximumTrackedIgnoredWarnings+1])
	e = newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	snapshot = scanEndpoint(t, e, true)
	if len(snapshot.Warnings) != maximumTrackedIgnoredWarnings+1 {
		t.Fatalf("incorrect warning count at limit: %d", len(snapshot.Warnings))
	}
	if last := snapshot.Warnings[maximumTrackedIgnoredWarnings]; strings.Contains(last.Error, "more paths") {
		t.Errorf("unnecessary summary: %s", last.Error)
	}
}

// TestGitWarningsOrder tests that warnings are sorted in the order in which
// problems are reported, which isn't the order in which Git lists paths, so
// that a summary remains the last of them.
func TestGitWarningsOrder(t *testing.T) {
	requireGit(t)
	root := newTrackedIgnoredRepository(t, []string{"a-b.o", "a/c.o"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	expected := []string{"a/c.o", "a-b.o"}
	if paths := warningPaths(scanEndpoint(t, e, true)); !slices.Equal(paths, expected) {
		t.Errorf("incorrect warning order: %v != %v", paths, expected)
	}
}

// TestGitWarningsUnavailable tests that there are no warnings, and no errors,
// if there's no repository to check, or no way to check it.
func TestGitWarningsUnavailable(t *testing.T) {
	requireGit(t)
	tracked := []string{"tracked.o"}

	t.Run("without repository", func(t *testing.T) {
		root := filepath.Join(realTemporaryDirectory(t), "root")
		writeTree(t, root, map[string]string{".gitignore": "*.o\n", "tracked.o": "content"})
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		if snapshot := scanEndpoint(t, e, true); len(snapshot.Warnings) != 0 || snapshot != e.snapshot {
			t.Errorf("unexpected warnings: %v", snapshot.Warnings)
		}
	})

	t.Run("within a repository", func(t *testing.T) {
		// The check applies only to repositories at the root.
		root := newTrackedIgnoredRepository(t, []string{"sub/tracked.o"})
		e := newGitModeEndpoint(t, filepath.Join(root, "sub"), synchronization.WatchMode_WatchModeNoWatch)
		if snapshot := scanEndpoint(t, e, true); len(snapshot.Warnings) != 0 {
			t.Errorf("unexpected warnings: %v", snapshot.Warnings)
		}
	})

	t.Run("invalid repository", func(t *testing.T) {
		// An invalid .git entry must not lead to the enclosing repository being
		// used, which has tracked files that match the rules.
		enclosing := newTrackedIgnoredRepository(t, []string{"root/tracked.o"})
		root := filepath.Join(enclosing, "root")
		if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
			t.Fatal("unable to create invalid repository:", err)
		}
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		if snapshot := scanEndpoint(t, e, true); len(snapshot.Warnings) != 0 {
			t.Errorf("unexpected warnings: %v", snapshot.Warnings)
		}
	})

	t.Run("missing root", func(t *testing.T) {
		e := newGitModeEndpoint(t, filepath.Join(realTemporaryDirectory(t), "missing"), synchronization.WatchMode_WatchModeNoWatch)
		if snapshot := scanEndpoint(t, e, true); snapshot.Content != nil || len(snapshot.Warnings) != 0 {
			t.Errorf("unexpected snapshot for missing root: %v", snapshot)
		}
	})

	t.Run("file root", func(t *testing.T) {
		root := filepath.Join(realTemporaryDirectory(t), "root")
		writeFiles(t, filepath.Dir(root), map[string]string{"root": "content"})
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		if snapshot := scanEndpoint(t, e, true); len(snapshot.Warnings) != 0 {
			t.Errorf("unexpected warnings: %v", snapshot.Warnings)
		}
	})

	t.Run("without Git", func(t *testing.T) {
		root := newTrackedIgnoredRepository(t, tracked)
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		t.Setenv("PATH", t.TempDir())
		snapshot := scanEndpoint(t, e, true)
		if len(snapshot.Warnings) != 0 {
			t.Errorf("unexpected warnings: %v", snapshot.Warnings)
		}
		if entry := entryAt(snapshot, "tracked.o"); entry == nil || entry.Kind != core.EntryKind_Untracked {
			t.Errorf("rules not applied: %v", entry)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		root := newTrackedIgnoredRepository(t, tracked)
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		snapshot := scanEndpoint(t, e, true)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if warnings := e.gitWarnings(ctx, snapshot); len(warnings) != 0 {
			t.Errorf("unexpected warnings: %v", warnings)
		}
	})
}

// TestGitWarningsLinkedWorktree tests that warnings use the exclude file of a
// repository whose Git directory is elsewhere, which is shared across
// worktrees.
func TestGitWarningsLinkedWorktree(t *testing.T) {
	requireGit(t)

	// Create a repository with a commit, and a linked worktree of it.
	main := newTrackedIgnoredRepository(t, []string{"main.c", "tracked.o"})
	if _, err := runGit(main, "add", ".gitignore"); err != nil {
		t.Fatal("unable to add file to repository:", err)
	}
	if _, err := runGit(main, "-c", "user.name=test", "-c", "user.email=test@example.org", "commit", "--quiet", "--no-gpg-sign", "-m", "commit"); err != nil {
		t.Fatal("unable to commit:", err)
	}
	worktree := filepath.Join(filepath.Dir(main), "worktree")
	if _, err := runGit(main, "worktree", "add", "--quiet", worktree); err != nil {
		t.Fatal("unable to create worktree:", err)
	}

	// Verify warnings, including with an exclude file, which is only found in
	// the main repository.
	e := newGitModeEndpoint(t, worktree, synchronization.WatchMode_WatchModeNoWatch)
	if paths := warningPaths(scanEndpoint(t, e, true)); !slices.Equal(paths, []string{"tracked.o"}) {
		t.Errorf("incorrect warning paths: %v", paths)
	}
	writeTree(t, main, map[string]string{".git/info/exclude": "main.c\n"})
	if paths := warningPaths(scanEndpoint(t, e, true)); !slices.Equal(paths, []string{"main.c", "tracked.o"}) {
		t.Errorf("incorrect warning paths with exclude file: %v", paths)
	}
}

// TestGitWarningsUnusualNames tests that warnings handle names that Git and
// snapshots represent differently. Warnings are transmitted using Protocol
// Buffers, which require valid UTF-8 and would cause a scan failure otherwise.
func TestGitWarningsUnusualNames(t *testing.T) {
	requireGit(t)
	if runtime.GOOS != "linux" {
		t.Skip("filesystem may not preserve names")
	}

	// Names that aren't valid UTF-8 can't be matched, but the ignored
	// directories above them can.
	invalid := "invalid\xff.o"
	root := newTrackedIgnoredRepository(t, []string{invalid, "build/" + invalid, "line\nbreak.o"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	snapshot := scanEndpoint(t, e, true)
	if paths := warningPaths(snapshot); !slices.Equal(paths, []string{"build", "line\nbreak.o"}) {
		t.Errorf("incorrect warning paths: %q", paths)
	}
	if _, err := proto.Marshal(snapshot); err != nil {
		t.Error("unable to marshal snapshot:", err)
	}

	// A decomposed name in the index has to be found using the composed name
	// that a snapshot uses on a filesystem that decomposes names.
	decomposed, composed := "é.o", "é.o"
	root = newTrackedIgnoredRepository(t, []string{decomposed})
	e = newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	warnings := e.gitWarnings(context.Background(), &core.Snapshot{
		DecomposesUnicode: true,
		Content: &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{
			composed: {Kind: core.EntryKind_Untracked},
		}},
	})
	if len(warnings) != 1 || warnings[0].Path != composed || strings.Contains(warnings[0].Error, "whole directory") {
		t.Errorf("incorrect warnings for decomposed name: %v", warnings)
	}
}
