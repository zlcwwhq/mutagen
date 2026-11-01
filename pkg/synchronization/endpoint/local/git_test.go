package local

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
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

		// The current commit and references of each kind of repository are.
		{".git/HEAD", true},
		{"sub/.git/HEAD", true},
		{"a/b/.git/HEAD", true},
		{".git/modules/x/HEAD", true},
		{".git/modules/x/y/HEAD", true},
		{".git/modules/x/modules/y/HEAD", true},
		{".git/worktrees/w/HEAD", true},
		{".git/packed-refs", true},
		{"sub/.git/packed-refs", true},
		{".git/modules/x/y/packed-refs", true},
		{".git/refs", true},
		{".git/refs/heads", true},
		{".git/refs/heads/main", true},
		{".git/refs/heads/feature/main", true},
		{".git/refs/remotes/origin/main", true},
		{".git/refs/tags/v1", true},
		{"sub/.git/refs/heads/main", true},
		{".git/modules/x/refs/heads/main", true},
		{".git/modules/x/y/refs/heads/main", true},
		{".GIT/HEAD", true},
		{".Git/refs/heads/main", true},

		// A branch can have a name that resembles anything.
		{".git/refs/heads/logs/main", true},
		{".git/refs/heads/index", true},
		{".git/refs/heads/modules/x", true},

		// So can a submodule.
		{".git/modules/logs/HEAD", true},
		{".git/modules/logs/refs/heads/main", true},
		{".git/modules/refs/HEAD", true},
		{".git/modules/refs/index", false},
		{".git/modules/logs/logs/HEAD", false},
		{".git/modules/modules/HEAD", true},
		{".git/modules/modules/index", false},

		// So do the exclude files of each kind of repository.
		{".git/info/exclude", true},
		{"sub/.git/info/exclude", true},
		{".git/modules/x/info/exclude", true},
		{".git/modules/x/y/info/exclude", true},

		// Everything else in a Git directory isn't relevant.
		{".git", false},
		{"sub/.git", false},
		{".GIT", false},
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

		// That includes reference logs, despite their resembling references.
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

// gitUpstream is a bare repository for tests to clone and fetch from using a
// file URL, along with a working copy for creating commits in it.
type gitUpstream struct {
	// t is the test.
	t *testing.T
	// bare is the path to the bare repository.
	bare string
	// url is the URL of the bare repository.
	url string
	// work is the path to the working copy.
	work string
	// head is the hexadecimal identifier of the commit most recently created.
	head string
}

// isolateGit sets up the environment of Git commands for a test, enabling the
// file transport and insulating them from the user's configuration, such as
// commit signing.
func isolateGit(t *testing.T) {
	t.Helper()
	requireGit(t)
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.org")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.org")
}

// mustGit runs a Git command that must succeed and returns its trimmed output.
func mustGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	output, err := runGit(directory, args...)
	if err != nil {
		t.Fatalf("unable to run Git with arguments %v: %v", args, err)
	}
	return output
}

// fileURL returns the file URL for a path.
func fileURL(path string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "file://" + path
}

// newGitUpstream creates an upstream repository with a single commit that has
// been pushed. The name distinguishes it from other upstreams in the same test.
func newGitUpstream(t *testing.T, name string) *gitUpstream {
	t.Helper()
	isolateGit(t)
	base := filepath.Join(realTemporaryDirectory(t), name)
	u := &gitUpstream{
		t:    t,
		bare: filepath.Join(base, "upstream.git"),
		work: filepath.Join(base, "work"),
	}
	u.url = fileURL(u.bare)
	initializeRepository(t, u.bare, "--bare", "--initial-branch=main")
	initializeRepository(t, u.work, "--initial-branch=main")
	mustGit(t, u.work, "remote", "add", "origin", u.url)
	u.commit("file", "first")
	u.push()
	return u
}

// commit creates a commit in the upstream's working copy that hasn't been
// pushed and returns its identifier.
func (u *gitUpstream) commit(name, contents string) string {
	u.t.Helper()
	writeTree(u.t, u.work, map[string]string{name: contents})
	mustGit(u.t, u.work, "add", "--", name)
	mustGit(u.t, u.work, "commit", "--quiet", "--no-gpg-sign", "-m", "commit "+name)
	u.head = mustGit(u.t, u.work, "rev-parse", "HEAD")
	return u.head
}

// push pushes the upstream's working copy.
func (u *gitUpstream) push() {
	u.t.Helper()
	mustGit(u.t, u.work, "push", "--quiet", "origin", "HEAD:refs/heads/main")
}

// entry returns the entry for a commit of the upstream.
func (u *gitUpstream) entry(commit string) *core.Entry {
	u.t.Helper()
	digest, err := hex.DecodeString(commit)
	if err != nil {
		u.t.Fatal("invalid commit:", err)
	}
	return &core.Entry{Kind: core.EntryKind_GitLink, Digest: digest, Url: u.url}
}

// transitionEndpoint performs a scan, which is what enables an endpoint to
// perform a transition, followed by the specified transitions, and verifies
// that no temporary content is left in the root.
func transitionEndpoint(t *testing.T, ctx context.Context, e *endpoint, changes ...*core.Change) ([]*core.Entry, []*core.Problem) {
	t.Helper()
	scanEndpoint(t, e, true)
	results, problems, _, err := e.Transition(ctx, changes)
	if err != nil {
		t.Fatal("transition failed:", err)
	}
	verifyNoTemporaryContent(t, e.root)
	return results, problems
}

// verifyNoTemporaryContent verifies that a root doesn't have temporary content.
func verifyNoTemporaryContent(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		} else if strings.HasPrefix(entry.Name(), filesystem.TemporaryNamePrefix) {
			t.Error("temporary content left behind:", path)
		}
		return nil
	})
	if err != nil {
		t.Error("unable to walk root:", err)
	}
}

// verifyCheckout verifies that a repository has a commit checked out on a
// detached HEAD, and that its working tree has the commit's content.
func verifyCheckout(t *testing.T, directory, commit string) {
	t.Helper()
	if head := mustGit(t, directory, "rev-parse", "HEAD"); head != commit {
		t.Errorf("incorrect commit checked out in %s: %s != %s", directory, head, commit)
	}
	if _, err := runGit(directory, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		t.Errorf("HEAD isn't detached in %s", directory)
	}
	if status := mustGit(t, directory, "status", "--porcelain"); status != "" {
		t.Errorf("working tree isn't clean in %s: %s", directory, status)
	}
}

// verifySingleProblem verifies that a list of problems consists of one problem,
// for the specified path, whose message contains the specified strings.
func verifySingleProblem(t *testing.T, problems []*core.Problem, path string, contents ...string) {
	t.Helper()
	if len(problems) != 1 {
		t.Fatalf("incorrect number of problems: %v", problems)
	}
	if problems[0].Path != path {
		t.Errorf("incorrect problem path: %q != %q", problems[0].Path, path)
	}
	for _, content := range contents {
		if !strings.Contains(problems[0].Error, content) {
			t.Errorf("problem doesn't contain %q: %s", content, problems[0].Error)
		}
	}
}

// newGitRoot creates a root for an endpoint with the specified files.
func newGitRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := filepath.Join(realTemporaryDirectory(t), "root")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal("unable to create root:", err)
	}
	writeTree(t, root, files)
	return root
}

// newGitWithOrigin creates a repository in a new directory within a root that
// has a single commit and, if the URL isn't empty, an origin.
func newGitWithOrigin(t *testing.T, root, name, url string) string {
	t.Helper()
	directory := filepath.Join(root, name)
	initializeRepository(t, directory)
	writeTree(t, directory, map[string]string{"file": "first"})
	mustGit(t, directory, "add", "--", "file")
	mustGit(t, directory, "commit", "--quiet", "--no-gpg-sign", "-m", "first")
	if url != "" {
		mustGit(t, directory, "config", "remote.origin.url", url)
	}
	return directory
}

// TestGitProviderProbe tests the classification of directories with .git
// entries.
func TestGitProviderProbe(t *testing.T) {
	isolateGit(t)
	provider := newGitProvider(nil)
	base := realTemporaryDirectory(t)
	ctx := context.Background()

	// A repository with a commit and an origin is a submodule.
	t.Run("submodule", func(t *testing.T) {
		directory := newGitWithOrigin(t, base, "submodule", "https://example.org/repository.git")
		commit, url, err := provider.Probe(ctx, directory)
		if err != nil {
			t.Fatal("probe failed:", err)
		} else if url != "https://example.org/repository.git" {
			t.Error("incorrect URL:", url)
		} else if hex.EncodeToString(commit) != mustGit(t, directory, "rev-parse", "HEAD") {
			t.Errorf("incorrect commit: %x", commit)
		} else if len(commit) != 20 {
			t.Error("incorrect commit length:", len(commit))
		}
	})

	// That includes repositories that use SHA-256.
	t.Run("SHA-256", func(t *testing.T) {
		directory := filepath.Join(base, "sha256")
		if _, err := runGit(base, "init", "--quiet", "--object-format=sha256", directory); err != nil {
			t.Skip("SHA-256 repositories aren't supported:", err)
		}
		writeTree(t, directory, map[string]string{"file": "first"})
		mustGit(t, directory, "add", "--", "file")
		mustGit(t, directory, "commit", "--quiet", "--no-gpg-sign", "-m", "first")
		mustGit(t, directory, "config", "remote.origin.url", "https://example.org/repository.git")
		commit, url, err := provider.Probe(ctx, directory)
		if err != nil {
			t.Fatal("probe failed:", err)
		} else if url != "https://example.org/repository.git" {
			t.Error("incorrect URL:", url)
		} else if hex.EncodeToString(commit) != mustGit(t, directory, "rev-parse", "HEAD") {
			t.Errorf("incorrect commit: %x", commit)
		} else if len(commit) != 32 {
			t.Error("incorrect commit length:", len(commit))
		}
	})

	// A URL isn't subject to this system's rewrites, since the other endpoint
	// has to be able to apply its own.
	t.Run("rewritten URL", func(t *testing.T) {
		directory := newGitWithOrigin(t, base, "rewritten", "https://example.org/repository.git")
		mustGit(t, directory, "config", "url.https://mirror.example.org/.insteadOf", "https://example.org/")
		if _, url, err := provider.Probe(ctx, directory); err != nil {
			t.Fatal("probe failed:", err)
		} else if url != "https://example.org/repository.git" {
			t.Error("rewritten URL reported:", url)
		}
	})

	// A repository without an origin or commits is an ordinary directory.
	t.Run("no origin", func(t *testing.T) {
		directory := newGitWithOrigin(t, base, "no-origin", "")
		if commit, url, err := provider.Probe(ctx, directory); err != nil {
			t.Fatal("probe failed:", err)
		} else if commit != nil || url != "" {
			t.Errorf("repository without origin treated as submodule: %x %s", commit, url)
		}
	})
	t.Run("no commits", func(t *testing.T) {
		directory := filepath.Join(base, "no-commits")
		initializeRepository(t, directory)
		mustGit(t, directory, "config", "remote.origin.url", "https://example.org/repository.git")
		if commit, url, err := provider.Probe(ctx, directory); err != nil {
			t.Fatal("probe failed:", err)
		} else if commit != nil || url != "" {
			t.Errorf("repository without commits treated as submodule: %x %s", commit, url)
		}
	})

	// Everything else is a problem with an actionable message.
	verifyProblem := func(t *testing.T, directory string, contents ...string) {
		t.Helper()
		commit, url, err := provider.Probe(ctx, directory)
		if err == nil {
			t.Fatalf("probe succeeded: %x %s", commit, url)
		}
		for _, content := range contents {
			if !strings.Contains(err.Error(), content) {
				t.Errorf("error doesn't contain %q: %v", content, err)
			}
		}
	}
	t.Run("empty .git directory", func(t *testing.T) {
		directory := filepath.Join(base, "empty-git")
		if err := os.MkdirAll(filepath.Join(directory, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
		verifyProblem(t, directory, "isn't a valid repository", "remove the directory")
	})
	t.Run("dangling gitfile", func(t *testing.T) {
		directory := filepath.Join(base, "dangling")
		writeTree(t, directory, map[string]string{".git": "gitdir: ../.git/modules/dangling\n"})
		verifyProblem(t, directory, "isn't a valid repository", "remove the directory")
	})
	t.Run("uppercase", func(t *testing.T) {
		directory := filepath.Join(base, "uppercase")
		if err := os.MkdirAll(filepath.Join(directory, ".GIT"), 0700); err != nil {
			t.Fatal(err)
		}
		verifyProblem(t, directory, "remove the directory")
	})
	t.Run("bare repository", func(t *testing.T) {
		directory := filepath.Join(base, "bare")
		initializeRepository(t, filepath.Join(directory, ".git"), "--bare")
		verifyProblem(t, directory, "working tree", "remove the directory")
	})
	t.Run("working tree elsewhere", func(t *testing.T) {
		directory := newGitWithOrigin(t, base, "elsewhere", "https://example.org/repository.git")
		mustGit(t, directory, "config", "core.worktree", base)
		verifyProblem(t, directory, "working tree")
	})
	t.Run("missing commit", func(t *testing.T) {
		directory := newGitWithOrigin(t, base, "damaged", "https://example.org/repository.git")
		if err := os.RemoveAll(filepath.Join(directory, ".git", "objects")); err != nil {
			t.Fatal(err)
		} else if err := os.MkdirAll(filepath.Join(directory, ".git", "objects"), 0700); err != nil {
			t.Fatal(err)
		}
		verifyProblem(t, directory, "damaged", "remove the directory")
	})
	t.Run("relative origin", func(t *testing.T) {
		for i, url := range []string{"../relative.git", "./relative.git", "relative.git"} {
			verifyProblem(t, newGitWithOrigin(t, base, fmt.Sprintf("relative-%d", i), url), "is relative", "set-url")
		}
	})
	t.Run("origin starting with dash", func(t *testing.T) {
		verifyProblem(t, newGitWithOrigin(t, base, "dash", "--upload-pack=touch"), "starts with a dash", "set-url")
	})
	t.Run("unusable origin without credentials", func(t *testing.T) {
		directory := newGitWithOrigin(t, base, "dash-credentials", "-https://user:secret@example.org/repository.git")
		_, _, err := provider.Probe(ctx, directory)
		if err == nil {
			t.Fatal("probe succeeded")
		} else if strings.Contains(err.Error(), "secret") {
			t.Error("error contains credentials:", err)
		}
	})
	t.Run("different owner", func(t *testing.T) {
		directory := newGitWithOrigin(t, base, "owner", "https://example.org/repository.git")
		t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
		if _, _, err := provider.Probe(ctx, directory); err == nil {
			t.Skip("Git doesn't check ownership")
		}
		verifyProblem(t, directory, "safe.directory", directory)

		// The check isn't ours to override, but what the user decides is
		// respected.
		configuration := filepath.Join(t.TempDir(), "config")
		writeFiles(t, filepath.Dir(configuration), map[string]string{"config": "[safe]\n\tdirectory = *\n"})
		t.Setenv("GIT_CONFIG_GLOBAL", configuration)
		if _, _, err := provider.Probe(ctx, directory); err != nil {
			t.Error("safe.directory not respected:", err)
		}
	})
	t.Run("without Git", func(t *testing.T) {
		directory := newGitWithOrigin(t, base, "without-git", "https://example.org/repository.git")
		t.Setenv("PATH", t.TempDir())
		verifyProblem(t, directory, "git isn't installed")
	})
}

// TestEndpointGitModeCredentialsNotReported tests that credentials in the
// origin of a submodule don't reach the snapshot or any messages, including
// when using that origin fails, which requires that they still be there in its
// configuration.
func TestEndpointGitModeCredentialsNotReported(t *testing.T) {
	isolateGit(t)
	t.Setenv("GIT_ALLOW_PROTOCOL", "https:file")

	// Create a submodule whose origin has credentials and can't be reached.
	origin := "https://user:secret-token@127.0.0.1:1/repository.git"
	root := newGitRoot(t, nil)
	directory := newGitWithOrigin(t, root, "sub", origin)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)

	// Verify the snapshot, including its serialized form, which is what goes to
	// the other endpoint and ends up in the session's archive.
	snapshot := scanEndpoint(t, e, true)
	entry := entryAt(snapshot, "sub")
	if entry == nil || entry.Kind != core.EntryKind_GitLink {
		t.Fatalf("incorrect entry: %v", entry)
	}
	if entry.Url != "https://127.0.0.1:1/repository.git" {
		t.Errorf("incorrect URL: %s", entry.Url)
	}
	if serialized, err := proto.Marshal(snapshot); err != nil {
		t.Error("unable to marshal snapshot:", err)
	} else if bytes.Contains(serialized, []byte("secret-token")) {
		t.Error("serialized snapshot contains credentials")
	}

	// The repository's own configuration is left alone, so it can still be
	// used.
	if url := mustGit(t, directory, "config", "--get", "remote.origin.url"); url != origin {
		t.Errorf("origin was changed: %s", url)
	}

	// Verify the problem from trying to fetch from it, which will fail.
	missing := &core.Entry{Kind: core.EntryKind_GitLink, Digest: bytes.Repeat([]byte{1}, 20), Url: entry.Url}
	results, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub", Old: entry, New: missing})
	if !results[0].Equal(entry, true) {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "127.0.0.1")
	if strings.Contains(problems[0].Error, "secret-token") {
		t.Error("problem contains credentials:", problems[0].Error)
	}
}

// TestEndpointGitModeFirstClone tests that a transition obtains a submodule
// that's absent, and that the scan that follows finds it in the same state.
func TestEndpointGitModeFirstClone(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	root := newGitRoot(t, map[string]string{"kept": "kept"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)

	// Perform the transition.
	target := upstream.entry(upstream.head)
	results, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub", New: target})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if results[0] != target {
		t.Error("incorrect result:", results[0])
	}

	// Verify the result on disk.
	directory := filepath.Join(root, "sub")
	verifyCheckout(t, directory, upstream.head)
	if contents, err := os.ReadFile(filepath.Join(directory, "file")); err != nil || string(contents) != "first" {
		t.Errorf("incorrect file contents: %q, %v", contents, err)
	}
	if info, err := os.Lstat(filepath.Join(directory, ".git")); err != nil || !info.IsDir() {
		t.Errorf("clone doesn't have .git directory: %v, %v", info, err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Lstat(directory); err != nil {
			t.Error("unable to query clone:", err)
		} else if info.Mode().Perm() != os.FileMode(e.defaultDirectoryMode&filesystem.ModePermissionsMask) {
			t.Errorf("incorrect directory permissions: %v", info.Mode().Perm())
		}
	}

	// Verify that the next scan agrees, without counting what's in the
	// submodule.
	snapshot := scanEndpoint(t, e, true)
	entry := entryAt(snapshot, "sub")
	if !entry.Equal(target, true) || entry.Url != upstream.url {
		t.Errorf("incorrect entry: %v != %v", entry, target)
	}
	if snapshot.Directories != 1 || snapshot.Files != 1 {
		t.Errorf("incorrect statistics: %d directories, %d files", snapshot.Directories, snapshot.Files)
	}
	if len(snapshot.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", snapshot.Warnings)
	}
}

// TestEndpointGitModeCloneOfUnreachableCommit tests a clone of a repository
// that doesn't have the commit on any branch, but which can provide it when
// it's asked for by its identifier.
func TestEndpointGitModeCloneOfUnreachableCommit(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	unreachable := upstream.commit("second", "second")
	mustGit(t, upstream.work, "push", "--quiet", "origin", "HEAD:refs/heads/unusual")
	mustGit(t, upstream.bare, "update-ref", "refs/hidden/unusual", unreachable)
	mustGit(t, upstream.bare, "update-ref", "-d", "refs/heads/unusual")

	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	results, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub", New: upstream.entry(unreachable)})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if results[0] == nil {
		t.Fatal("submodule not created")
	}
	verifyCheckout(t, filepath.Join(root, "sub"), unreachable)
}

// TestEndpointGitModeUpdateFollowsPushedCommits tests that a pushed commit is
// followed, and that one that hasn't been pushed leaves the submodule where it
// is with a message that says what to do, until it's pushed, which takes effect
// at the next attempt.
func TestEndpointGitModeUpdateFollowsPushedCommits(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	ctx := context.Background()

	// Obtain the submodule.
	first := upstream.entry(upstream.head)
	if _, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: first}); len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	}
	directory := filepath.Join(root, "sub")

	// Follow a pushed commit.
	upstream.commit("second", "second")
	upstream.push()
	second := upstream.entry(upstream.head)
	results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: first, New: second})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if results[0] != second {
		t.Error("incorrect result:", results[0])
	}
	verifyCheckout(t, directory, upstream.head)
	if _, err := os.Stat(filepath.Join(directory, "second")); err != nil {
		t.Error("commit's content is missing:", err)
	}

	// A commit that hasn't been pushed isn't available.
	unpushed := upstream.commit("third", "third")
	third := upstream.entry(unpushed)
	head := mustGit(t, directory, "rev-parse", "HEAD")
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: second, New: third})
	if results[0] != second {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", gitShortCommit(unpushed), "isn't available from "+upstream.url, "push it")
	if current := mustGit(t, directory, "rev-parse", "HEAD"); current != head {
		t.Errorf("submodule moved: %s != %s", current, head)
	}

	// Fetching must not have written any of the files that wake a session.
	if _, err := os.Stat(filepath.Join(directory, ".git", "FETCH_HEAD")); !os.IsNotExist(err) {
		t.Error("FETCH_HEAD was written:", err)
	}

	// Once it's pushed, the next attempt succeeds, without having to wait.
	upstream.push()
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: second, New: third})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if results[0] != third {
		t.Error("incorrect result:", results[0])
	}
	verifyCheckout(t, directory, unpushed)

	// A transition to what's already checked out doesn't require anything.
	if err := os.RemoveAll(upstream.bare); err != nil {
		t.Fatal(err)
	}
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: second, New: third})
	if len(problems) != 0 || results[0] != third {
		t.Error("incorrect results for transition to current commit:", results, problems)
	}
}

// TestEndpointGitModeUpdateChecks tests the conditions that prevent a submodule
// from moving to another commit.
func TestEndpointGitModeUpdateChecks(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	first := upstream.head
	second := upstream.commit("second", "second")
	upstream.push()
	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	ctx := context.Background()
	if _, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: upstream.entry(first)}); len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	}
	directory := filepath.Join(root, "sub")

	// A commit made since the scan isn't discarded.
	local := upstream.entry(second)
	writeTree(t, directory, map[string]string{"local": "local"})
	mustGit(t, directory, "add", "--", "local")
	mustGit(t, directory, "commit", "--quiet", "--no-gpg-sign", "-m", "local")
	head := mustGit(t, directory, "rev-parse", "HEAD")
	old := upstream.entry(first)
	results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: old, New: local})
	if results[0] != old {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "changed since the scan")
	if current := mustGit(t, directory, "rev-parse", "HEAD"); current != head {
		t.Errorf("submodule moved: %s != %s", current, head)
	}
	mustGit(t, directory, "reset", "--quiet", "--hard", first)

	// Conflicting local changes are for Git to refuse.
	writeTree(t, directory, map[string]string{"second": "conflicting"})
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: old, New: local})
	if results[0] != old {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "unable to check out", "second")
	if contents, err := os.ReadFile(filepath.Join(directory, "second")); err != nil || string(contents) != "conflicting" {
		t.Errorf("local change was lost: %q, %v", contents, err)
	}
	if current := mustGit(t, directory, "rev-parse", "HEAD"); current != first {
		t.Errorf("submodule moved: %s != %s", current, first)
	}

	// Changes that don't conflict come along.
	if err := os.Remove(filepath.Join(directory, "second")); err != nil {
		t.Fatal(err)
	}
	writeTree(t, directory, map[string]string{"untracked": "untracked"})
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: old, New: local})
	if len(problems) != 0 || results[0] != local {
		t.Error("incorrect results with non-conflicting changes:", results, problems)
	}
	if contents, err := os.ReadFile(filepath.Join(directory, "untracked")); err != nil || string(contents) != "untracked" {
		t.Errorf("local change was lost: %q, %v", contents, err)
	}
}

// TestEndpointGitModeUpdateFromOtherURL tests that a commit that's missing from
// a repository's own origin can be fetched from the URL that the other endpoint
// reports, and that the origin is tried first.
func TestEndpointGitModeUpdateFromOtherURL(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	ctx := context.Background()
	first := upstream.entry(upstream.head)
	if _, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: first}); len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	}
	directory := filepath.Join(root, "sub")

	// Create a commit that's only in a second repository, which the other
	// endpoint reaches, with this endpoint's origin not having it.
	fork := newGitUpstream(t, "fork")
	mustGit(t, fork.work, "fetch", "--quiet", upstream.url, "main")
	mustGit(t, fork.work, "reset", "--quiet", "--hard", "FETCH_HEAD")
	fork.commit("forked", "forked")
	mustGit(t, fork.work, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/main")
	target := fork.entry(fork.head)

	// The origin doesn't have it, but the other URL does.
	results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: first, New: target})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if results[0] != target {
		t.Error("incorrect result:", results[0])
	}
	verifyCheckout(t, directory, fork.head)
	if url := mustGit(t, directory, "config", "--get", "remote.origin.url"); url != upstream.url {
		t.Errorf("origin was changed: %s", url)
	}

	// A commit that's in neither results in a problem that mentions both.
	unavailable := fork.commit("unavailable", "unavailable")
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: target, New: fork.entry(unavailable)})
	if results[0] != target {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "isn't available from "+upstream.url, "isn't available from "+fork.url)

	// A URL that the other endpoint has provided that's not usable is reported
	// and isn't provided to Git, even though we have another one that's tried
	// first.
	missing := &core.Entry{Kind: core.EntryKind_GitLink, Digest: bytes.Repeat([]byte{2}, 20), Url: "--upload-pack=touch"}
	_, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: target, New: missing})
	verifySingleProblem(t, problems, "sub", "isn't available from "+upstream.url, "starts with a dash")
}

// TestEndpointGitModeFailedCloneKeepsContent tests that content that a
// submodule is supposed to replace stays in place if the submodule can't be
// obtained.
func TestEndpointGitModeFailedCloneKeepsContent(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	files := map[string]string{"sub/file": "precious", "sub/deep/file": "also precious", "kept": "kept"}
	unavailable := upstream.commit("unavailable", "unavailable")

	verify := func(t *testing.T, target *core.Entry, contents ...string) {
		t.Helper()
		root := newGitRoot(t, files)
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		ctx := context.Background()
		old := entryAt(scanEndpoint(t, e, true), "sub")
		results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: old, New: target})
		if results[0] != old {
			t.Error("incorrect result:", results[0])
		}
		verifySingleProblem(t, problems, "sub", contents...)
		for name, expected := range files {
			if actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name))); err != nil || string(actual) != expected {
				t.Errorf("content was lost at %s: %q, %v", name, actual, err)
			}
		}
		if _, err := os.Lstat(filepath.Join(root, "sub", ".git")); !os.IsNotExist(err) {
			t.Error("clone left in place:", err)
		}
		if entries, err := os.ReadDir(root); err != nil {
			t.Error("unable to read root:", err)
		} else if len(entries) != 2 {
			t.Errorf("unexpected content in root: %v", entries)
		}
	}

	// In all of these cases, the repository doesn't make it, for different
	// reasons.
	t.Run("repository doesn't exist", func(t *testing.T) {
		url := fileURL(filepath.Join(filepath.Dir(upstream.bare), "missing.git"))
		verify(t, &core.Entry{Kind: core.EntryKind_GitLink, Digest: upstream.entry(unavailable).Digest, Url: url}, "unable to clone "+url)
	})
	t.Run("commit doesn't exist", func(t *testing.T) {
		verify(t, upstream.entry(unavailable), gitShortCommit(unavailable), "isn't available from "+upstream.url, "push it")
	})
	t.Run("protocol isn't allowed", func(t *testing.T) {
		t.Setenv("GIT_ALLOW_PROTOCOL", "https")
		verify(t, upstream.entry(unavailable), "unable to clone "+upstream.url)
	})
	t.Run("URL isn't usable", func(t *testing.T) {
		verify(t, &core.Entry{Kind: core.EntryKind_GitLink, Digest: upstream.entry(unavailable).Digest, Url: "--upload-pack=touch"}, "starts with a dash")
	})
	t.Run("Git isn't available", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		verify(t, upstream.entry(unavailable), "unable to clone")
	})
}

// TestEndpointGitModeCloneReplacesContent tests that a successful clone
// replaces what's there, including an empty directory and a file.
func TestEndpointGitModeCloneReplacesContent(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	ctx := context.Background()
	for _, test := range []struct {
		name string
		make func(t *testing.T, root string)
	}{
		{"empty directory", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "sub"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, root string) {
			writeTree(t, root, map[string]string{"sub/replaced": "replaced", "sub/deep/replaced": "replaced"})
		}},
		{"file", func(t *testing.T, root string) {
			writeTree(t, root, map[string]string{"sub": "replaced"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newGitRoot(t, nil)
			test.make(t, root)
			e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
			old := entryAt(scanEndpoint(t, e, true), "sub")
			target := upstream.entry(upstream.head)
			results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: old, New: target})
			if len(problems) != 0 {
				t.Fatal("unexpected problems:", problems)
			} else if results[0] != target {
				t.Error("incorrect result:", results[0])
			}
			verifyCheckout(t, filepath.Join(root, "sub"), upstream.head)
			if _, err := os.Lstat(filepath.Join(root, "sub", "replaced")); !os.IsNotExist(err) {
				t.Error("replaced content remains:", err)
			}
		})
	}

	// Content that was modified since the scan is caught by the removal,
	// despite the clone having succeeded, and the clone doesn't go anywhere.
	t.Run("modified content", func(t *testing.T) {
		root := newGitRoot(t, map[string]string{"sub/file": "original", "sub/kept": "kept"})
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		old := entryAt(scanEndpoint(t, e, true), "sub")
		scanEndpoint(t, e, true)
		writeTree(t, root, map[string]string{"sub/file": "modified"})
		results, problems, _, err := e.Transition(ctx, []*core.Change{{Path: "sub", Old: old, New: upstream.entry(upstream.head)}})
		if err != nil {
			t.Fatal("transition failed:", err)
		}
		verifyNoTemporaryContent(t, root)
		if results[0] == nil || results[0].Kind != core.EntryKind_Directory {
			t.Fatalf("incorrect result: %v", results[0])
		}
		if len(problems) == 0 {
			t.Error("no problem reported")
		}
		if contents, err := os.ReadFile(filepath.Join(root, "sub", "file")); err != nil || string(contents) != "modified" {
			t.Errorf("modification was lost: %q, %v", contents, err)
		}
		if _, err := os.Lstat(filepath.Join(root, "sub", ".git")); !os.IsNotExist(err) {
			t.Error("clone was moved into place:", err)
		}
	})
}

// TestEndpointGitModeInvalidRepository tests that an invalid .git entry in a
// directory is reported and that it can't lead to the use of an enclosing
// repository, whether for scanning, warnings, or transitions.
func TestEndpointGitModeInvalidRepository(t *testing.T) {
	isolateGit(t)
	files := map[string]string{"sub/.git": "gitdir: ../.git/modules/sub\n", "sub/file": "content"}

	// Create a superproject that contains the directory, with something for
	// each operation to find if it were to reach this repository.
	root := newGitRoot(t, files)
	initializeRepository(t, root)
	mustGit(t, root, "config", "remote.origin.url", "https://example.org/superproject.git")
	writeTree(t, root, map[string]string{"tracked": "tracked"})
	mustGit(t, root, "add", "--", "tracked")
	mustGit(t, root, "commit", "--quiet", "--no-gpg-sign", "-m", "commit")
	writeTree(t, root, map[string]string{"tracked": "modified"})
	head := mustGit(t, root, "rev-parse", "HEAD")

	verify := func(t *testing.T) {
		t.Helper()
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		snapshot := scanEndpoint(t, e, true)

		// The directory is problematic, and it hasn't been taken for the
		// enclosing repository. If it had been, then it would be reported as a
		// submodule, with a warning, since that repository has a change.
		entry := entryAt(snapshot, "sub")
		if entry == nil || entry.Kind != core.EntryKind_Problematic {
			t.Fatalf("incorrect entry: %v", entry)
		} else if !strings.Contains(entry.Problem, "remove the directory") {
			t.Errorf("message doesn't say what to do: %s", entry.Problem)
		}
		if len(snapshot.Warnings) != 0 {
			t.Errorf("unexpected warnings: %v", snapshot.Warnings)
		}

		// A transition for this directory must not affect the superproject
		// either.
		digest, err := hex.DecodeString(head)
		if err != nil {
			t.Fatal(err)
		}
		old := &core.Entry{Kind: core.EntryKind_GitLink, Digest: digest, Url: "https://example.org/superproject.git"}
		target := &core.Entry{Kind: core.EntryKind_GitLink, Digest: bytes.Repeat([]byte{1}, 20), Url: old.Url}
		results, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub", Old: old, New: target})
		if results[0] != old {
			t.Error("incorrect result:", results[0])
		}
		verifySingleProblem(t, problems, "sub", "unable to determine submodule's commit")

		// Everything is as it was.
		if current := mustGit(t, root, "rev-parse", "HEAD"); current != head {
			t.Errorf("superproject moved: %s != %s", current, head)
		}
		if status := mustGit(t, root, "status", "--porcelain", "--", "tracked"); status != "M tracked" {
			t.Errorf("superproject's working tree changed: %q", status)
		}
		for name, expected := range files {
			if actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name))); err != nil || string(actual) != expected {
				t.Errorf("content changed at %s: %q, %v", name, actual, err)
			}
		}
	}

	t.Run("dangling gitfile", verify)
	if err := os.Remove(filepath.Join(root, "sub", ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub", ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	delete(files, "sub/.git")
	t.Run("empty directory", verify)
}

// TestEndpointGitModeGitfileSubmodule tests that a submodule created by Git
// within a superproject, whose .git is a file that points into the
// superproject's Git directory, is treated the same way, and that it's updated
// in place.
func TestEndpointGitModeGitfileSubmodule(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	root := newGitRoot(t, nil)
	initializeRepository(t, root)
	mustGit(t, root, "submodule", "add", "--quiet", upstream.url, "third_party/sub")
	mustGit(t, root, "commit", "--quiet", "--no-gpg-sign", "-m", "add submodule")
	directory := filepath.Join(root, "third_party", "sub")
	if info, err := os.Lstat(filepath.Join(directory, ".git")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("submodule doesn't use a .git file: %v, %v", info, err)
	}

	// Verify that it's a submodule with the correct URL.
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	snapshot := scanEndpoint(t, e, true)
	old := entryAt(snapshot, "third_party/sub")
	if !old.Equal(upstream.entry(upstream.head), true) || old.Url != upstream.url {
		t.Fatalf("incorrect entry: %v", old)
	}
	if status := entryAt(snapshot, ".gitmodules"); status == nil || status.Kind != core.EntryKind_File {
		t.Errorf(".gitmodules isn't synchronized as a file: %v", status)
	}

	// Update it in place.
	upstream.commit("second", "second")
	upstream.push()
	target := upstream.entry(upstream.head)
	results, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "third_party/sub", Old: old, New: target})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if results[0] != target {
		t.Error("incorrect result:", results[0])
	}
	verifyCheckout(t, directory, upstream.head)
	if info, err := os.Lstat(filepath.Join(directory, ".git")); err != nil || !info.Mode().IsRegular() {
		t.Errorf("submodule no longer uses a .git file: %v, %v", info, err)
	}
	if !entryAt(scanEndpoint(t, e, true), "third_party/sub").Equal(target, true) {
		t.Error("next scan doesn't agree")
	}

	// Verify that an accelerated scan notices a change of HEAD that was made
	// outside of the submodule's directory, without re-check paths that would
	// indicate it.
	e.watchMode = reifiedWatchModeRecursive
	e.accelerate = true
	e.recheckPaths = make(map[string]bool)
	mustGit(t, directory, "checkout", "--quiet", "--detach", "HEAD~1")
	if entry := entryAt(scanEndpoint(t, e, false), "third_party/sub"); entry == nil || entry.Equal(target, true) {
		t.Errorf("accelerated scan returned a stale entry: %v", entry)
	}
}

// TestEndpointGitModePollDetectsCommit tests that a change of commit in a
// submodule wakes a session that uses poll-based watching, because each of its
// scans probes the submodule, despite the absence of any non-recursive watch
// for it, and that nothing else about the submodule does, such as a fetch.
func TestEndpointGitModePollDetectsCommit(t *testing.T) {
	for _, test := range []struct {
		name string
		mode synchronization.WatchMode
	}{
		{"poll", synchronization.WatchMode_WatchModeForcePoll},
		{"portable", synchronization.WatchMode_WatchModePortable},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := newGitUpstream(t, "upstream")
			upstream.commit("second", "second")
			upstream.push()
			root := newGitRoot(t, nil)
			directory := filepath.Join(root, "sub")
			if _, err := runGit(root, "clone", "--quiet", upstream.url, directory); err != nil {
				t.Fatal("unable to clone:", err)
			}

			// Wait for the baseline scan.
			e := newGitModeEndpoint(t, root, test.mode)
			deadline := time.Now().Add(30 * time.Second)
			for {
				e.lockScanLock(context.Background())
				accelerate := e.accelerate
				e.unlockScanLock()
				if accelerate {
					break
				} else if time.Now().After(deadline) {
					t.Fatal("timed out waiting for baseline scan")
				}
				time.Sleep(10 * time.Millisecond)
			}
			initial := entryAt(scanEndpoint(t, e, false), "sub")
			if initial == nil || initial.Kind != core.EntryKind_GitLink {
				t.Fatalf("incorrect initial entry: %v", initial)
			}

			// Verify that a fetch doesn't result in a wake up.
			mustGit(t, directory, "fetch", "--quiet", "--no-write-fetch-head", "origin")
			quiet, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
			defer cancel()
			if err := e.Poll(quiet); err != nil {
				t.Fatal("polling failed:", err)
			} else if quiet.Err() == nil {
				t.Error("session woken by a fetch")
			}

			// Change the commit.
			mustGit(t, directory, "checkout", "--quiet", "--detach", "HEAD~1")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := e.Poll(ctx); err != nil {
				t.Fatal("polling failed:", err)
			} else if ctx.Err() != nil {
				t.Fatal("timed out waiting for commit change to be detected")
			}
			if entry := entryAt(scanEndpoint(t, e, false), "sub"); entry == nil || entry.Equal(initial, true) {
				t.Errorf("stale entry: %v", entry)
			}
		})
	}
}

// TestEndpointGitModeNestedSubmodules tests that submodules nested in a
// submodule are updated along with it, and that failure to do that isn't
// failure of the transition, since the commit that's being synchronized is in
// place.
func TestEndpointGitModeNestedSubmodules(t *testing.T) {
	nested := newGitUpstream(t, "nested")
	upstream := newGitUpstream(t, "upstream")
	mustGit(t, upstream.work, "submodule", "add", "--quiet", nested.url, "nested")
	mustGit(t, upstream.work, "commit", "--quiet", "--no-gpg-sign", "-m", "add submodule")
	upstream.head = mustGit(t, upstream.work, "rev-parse", "HEAD")
	upstream.push()

	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	ctx := context.Background()

	// A clone populates nested submodules, with the gitfiles remaining valid
	// after the clone is moved from its temporary location.
	first := upstream.entry(upstream.head)
	if _, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: first}); len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	}
	directory := filepath.Join(root, "sub")
	nestedDirectory := filepath.Join(directory, "nested")
	if contents, err := os.ReadFile(filepath.Join(nestedDirectory, "file")); err != nil || string(contents) != "first" {
		t.Errorf("nested submodule not populated: %q, %v", contents, err)
	}
	if head := mustGit(t, nestedDirectory, "rev-parse", "HEAD"); head != nested.head {
		t.Errorf("incorrect commit in nested submodule: %s != %s", head, nested.head)
	}
	if status := mustGit(t, directory, "status", "--porcelain"); status != "" {
		t.Errorf("clone isn't clean: %s", status)
	}

	// Nested submodules aren't compared separately, so they don't appear.
	snapshot := scanEndpoint(t, e, true)
	if entry := entryAt(snapshot, "sub"); !entry.Equal(first, true) {
		t.Errorf("incorrect entry: %v", entry)
	}
	if paths := snapshot.Content.GitLinkPaths(); !slices.Equal(paths, []string{"sub"}) {
		t.Errorf("incorrect submodules: %v", paths)
	}

	// An update brings nested submodules along.
	nested.commit("nested-second", "nested second")
	nested.push()
	mustGit(t, filepath.Join(upstream.work, "nested"), "pull", "--quiet", "origin", "main")
	mustGit(t, upstream.work, "add", "--", "nested")
	mustGit(t, upstream.work, "commit", "--quiet", "--no-gpg-sign", "-m", "update submodule")
	upstream.head = mustGit(t, upstream.work, "rev-parse", "HEAD")
	upstream.push()
	second := upstream.entry(upstream.head)
	results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: first, New: second})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if results[0] != second {
		t.Error("incorrect result:", results[0])
	}
	if head := mustGit(t, nestedDirectory, "rev-parse", "HEAD"); head != nested.head {
		t.Errorf("nested submodule didn't follow: %s != %s", head, nested.head)
	}
	if _, err := os.Stat(filepath.Join(nestedDirectory, "nested-second")); err != nil {
		t.Error("nested submodule's content is missing:", err)
	}
}

// TestEndpointGitModeNestedSubmoduleFailure tests that a submodule whose nested
// submodule can't be obtained still ends up in place, along with a problem,
// whether it's being cloned or updated.
func TestEndpointGitModeNestedSubmoduleFailure(t *testing.T) {
	nested := newGitUpstream(t, "nested")
	upstream := newGitUpstream(t, "upstream")
	mustGit(t, upstream.work, "submodule", "add", "--quiet", nested.url, "nested")
	mustGit(t, upstream.work, "commit", "--quiet", "--no-gpg-sign", "-m", "add submodule")
	upstream.head = mustGit(t, upstream.work, "rev-parse", "HEAD")
	upstream.push()
	ctx := context.Background()

	// Make the nested submodule's repository unavailable.
	if err := os.RemoveAll(nested.bare); err != nil {
		t.Fatal(err)
	}

	// A clone that replaces content goes ahead.
	root := newGitRoot(t, map[string]string{"sub/replaced": "replaced"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	old := entryAt(scanEndpoint(t, e, true), "sub")
	first := upstream.entry(upstream.head)
	results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: old, New: first})
	if results[0] != first {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "unable to update nested submodules")
	directory := filepath.Join(root, "sub")
	if head := mustGit(t, directory, "rev-parse", "HEAD"); head != upstream.head {
		t.Errorf("incorrect commit: %s != %s", head, upstream.head)
	}
	if _, err := os.Lstat(filepath.Join(directory, "replaced")); !os.IsNotExist(err) {
		t.Error("replaced content remains:", err)
	}

	// An update goes ahead as well.
	upstream.commit("second", "second")
	upstream.push()
	second := upstream.entry(upstream.head)
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: first, New: second})
	if results[0] != second {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "unable to update nested submodules")
	if head := mustGit(t, directory, "rev-parse", "HEAD"); head != upstream.head {
		t.Errorf("incorrect commit: %s != %s", head, upstream.head)
	}
}

// TestGitWarningsDirtySubmodule tests warnings about uncommitted changes in
// submodules, and that the check doesn't change anything on disk, which would
// result in events that wake the session.
func TestGitWarningsDirtySubmodule(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	root := newGitRoot(t, nil)
	directory := filepath.Join(root, "deep", "sub")
	if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "clone", "--quiet", upstream.url, directory); err != nil {
		t.Fatal("unable to clone:", err)
	}
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)

	// A clean submodule doesn't get a warning.
	if paths := warningPaths(scanEndpoint(t, e, true)); len(paths) != 0 {
		t.Errorf("unexpected warnings: %v", paths)
	}

	// Modify the file's metadata but not its contents. The status of this
	// repository doesn't change, but Git will need to examine the file, and it
	// would normally save what it finds in the index.
	index := filepath.Join(directory, ".git", "index")
	file := filepath.Join(directory, "file")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(file, past, past); err != nil {
		t.Fatal(err)
	}
	indexBefore, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	indexInfoBefore, err := os.Lstat(index)
	if err != nil {
		t.Fatal(err)
	}
	if paths := warningPaths(scanEndpoint(t, e, true)); len(paths) != 0 {
		t.Errorf("unexpected warnings for unchanged content: %v", paths)
	}
	indexAfter, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	indexInfoAfter, err := os.Lstat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) || !indexInfoBefore.ModTime().Equal(indexInfoAfter.ModTime()) {
		t.Error("checking status modified the index")
	}

	// Verify that the premise of the above is correct, which is that this is
	// due to the lack of optional locks. If Git were not to update its index at
	// all, then the test above would prove nothing.
	control := git.Command(context.Background(), directory, "status", "--porcelain")
	control.Args = slices.DeleteFunc(control.Args, func(argument string) bool { return argument == "--no-optional-locks" })
	if err := control.Run(); err != nil {
		t.Fatal("unable to run status:", err)
	}
	if indexControl, err := os.ReadFile(index); err != nil {
		t.Fatal(err)
	} else if bytes.Equal(indexBefore, indexControl) {
		t.Error("status didn't update index even without --no-optional-locks")
	}

	// Each kind of uncommitted change gets a warning at the submodule's path.
	verifyWarned := func() {
		t.Helper()
		snapshot := scanEndpoint(t, e, true)
		if len(snapshot.Warnings) != 1 {
			t.Fatalf("incorrect warnings: %v", snapshot.Warnings)
		}
		warning := snapshot.Warnings[0]
		if warning.Path != "deep/sub" {
			t.Errorf("incorrect warning path: %s", warning.Path)
		} else if !warning.Warning {
			t.Error("not indicated as a warning")
		} else if !strings.Contains(warning.Error, "uncommitted changes") || !strings.Contains(warning.Error, "commit and push") {
			t.Errorf("incorrect warning message: %s", warning.Error)
		}
		if err := snapshot.EnsureValid(); err != nil {
			t.Error("snapshot invalid:", err)
		}
		if len(e.snapshot.Warnings) != 0 {
			t.Error("warnings stored in the endpoint's snapshot")
		}
	}
	writeTree(t, directory, map[string]string{"untracked": "untracked"})
	verifyWarned()
	if err := os.Remove(filepath.Join(directory, "untracked")); err != nil {
		t.Fatal(err)
	}
	writeTree(t, directory, map[string]string{"file": "modified"})
	verifyWarned()
	mustGit(t, directory, "add", "--", "file")
	verifyWarned()

	// Changes that have been committed aren't uncommitted changes, even if they
	// aren't pushed, which is reported by the other endpoint if it can't fetch
	// them.
	mustGit(t, directory, "commit", "--quiet", "--no-gpg-sign", "-m", "local")
	if paths := warningPaths(scanEndpoint(t, e, true)); len(paths) != 0 {
		t.Errorf("unexpected warnings after commit: %v", paths)
	}

	// Ignored content isn't a change.
	writeTree(t, directory, map[string]string{".git/info/exclude": "*.ignored\n", "local.ignored": "ignored"})
	if paths := warningPaths(scanEndpoint(t, e, true)); len(paths) != 0 {
		t.Errorf("unexpected warnings for ignored content: %v", paths)
	}

	// A failure doesn't produce a warning, because it's not known to be true.
	if err := os.Chmod(filepath.Join(directory, ".git", "HEAD"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := scanEndpoint(t, e, true)
	if err := os.RemoveAll(filepath.Join(directory, ".git")); err != nil {
		t.Fatal(err)
	}
	if warnings := e.gitWarnings(context.Background(), snapshot); len(warnings) != 0 {
		t.Errorf("unexpected warnings after failure: %v", warnings)
	}

	// Cancellation doesn't produce warnings either.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if warnings := e.gitWarnings(ctx, snapshot); len(warnings) != 0 {
		t.Errorf("unexpected warnings after cancellation: %v", warnings)
	}
}

// TestGitWarningsDirtySubmoduleLimit tests that warnings about uncommitted
// changes in submodules are limited in number.
func TestGitWarningsDirtySubmoduleLimit(t *testing.T) {
	isolateGit(t)
	root := newGitRoot(t, nil)
	count := maximumDirtySubmoduleWarnings + 5
	for i := 0; i < count; i++ {
		directory := newGitWithOrigin(t, root, fmt.Sprintf("sub%02d", i), "https://example.org/repository.git")
		writeTree(t, directory, map[string]string{"file": "modified"})
	}
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	snapshot := scanEndpoint(t, e, true)
	if len(snapshot.Warnings) != maximumDirtySubmoduleWarnings+1 {
		t.Fatalf("incorrect warning count: %d", len(snapshot.Warnings))
	}
	if err := snapshot.EnsureValid(); err != nil {
		t.Error("snapshot invalid:", err)
	}
	summary := snapshot.Warnings[maximumDirtySubmoduleWarnings]
	if !strings.Contains(summary.Error, "this and 4 more submodules") || !summary.Warning || summary.Path == "" {
		t.Errorf("incorrect summary: %v", summary)
	}
}
