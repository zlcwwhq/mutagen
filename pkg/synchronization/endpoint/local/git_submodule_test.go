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

	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// commitFiles creates a commit in the upstream's working copy with the
// specified files, which are added forcefully, so that files that match
// .gitignore rules can be tracked, and returns its identifier. It isn't pushed.
func (u *gitUpstream) commitFiles(message string, files map[string]string) string {
	u.t.Helper()
	writeTree(u.t, u.work, files)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	mustGit(u.t, u.work, append([]string{"add", "--force", "--"}, names...)...)
	mustGit(u.t, u.work, "commit", "--quiet", "--no-gpg-sign", "-m", message)
	u.head = mustGit(u.t, u.work, "rev-parse", "HEAD")
	return u.head
}

// cloneUpstream clones an upstream into a new directory at the specified path
// within a root, with its default branch checked out, the way a user would
// obtain a submodule, and returns the directory.
func cloneUpstream(t *testing.T, u *gitUpstream, root, path string) string {
	t.Helper()
	directory := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
		t.Fatal("unable to create parent directory:", err)
	}
	mustGit(t, filepath.Dir(directory), "clone", "--quiet", "--", u.url, directory)
	return directory
}

// gitLinkAt returns the GitLink entry of the submodule at the specified path
// in a snapshot, failing the test if there isn't one.
func gitLinkAt(t *testing.T, snapshot *core.Snapshot, path string) *core.Entry {
	t.Helper()
	link := entryAt(snapshot, path+"/"+core.GitLinkName)
	if link == nil || link.Kind != core.EntryKind_GitLink {
		t.Fatalf("no GitLink entry for %s: %v (directory: %v)", path, link, entryAt(snapshot, path))
	}
	return link
}

// verifySingleWarning verifies that a list of problems consists of one
// warning, for the specified path, whose message contains the specified
// strings.
func verifySingleWarning(t *testing.T, problems []*core.Problem, path string, contents ...string) {
	t.Helper()
	verifySingleProblem(t, problems, path, contents...)
	if !problems[0].Warning {
		t.Error("problem not reported as a warning:", problems[0])
	}
}

// verifySingleFailure verifies that a list of problems consists of one problem
// that isn't a warning, for the specified path, whose message contains the
// specified strings.
func verifySingleFailure(t *testing.T, problems []*core.Problem, path string, contents ...string) {
	t.Helper()
	verifySingleProblem(t, problems, path, contents...)
	if problems[0].Warning {
		t.Error("failure reported as a warning:", problems[0])
	}
}

// verifyDetachedAt verifies that a repository has a detached HEAD at the
// specified commit, without considering its working tree.
func verifyDetachedAt(t *testing.T, directory, commit string) {
	t.Helper()
	if head := mustGit(t, directory, "rev-parse", "HEAD"); head != commit {
		t.Errorf("incorrect HEAD in %s: %s != %s", directory, head, commit)
	}
	if _, err := runGit(directory, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		t.Errorf("HEAD isn't detached in %s", directory)
	}
}

// useIntervals sets the retry intervals of a throttle.
func (r *gitRetries) useIntervals(minimum, maximum time.Duration) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.minimumInterval, r.maximumInterval = minimum, maximum
}

// state returns a copy of the failure history of a key, or nil if there's
// none.
func (r *gitRetries) state(key gitRetryKey) *gitRetryState {
	r.lock.Lock()
	defer r.lock.Unlock()
	if state := r.states[key]; state != nil {
		result := *state
		return &result
	}
	return nil
}

// backdate sets the modification times of files within a root to a time in
// the past, which is returned, so that any later write is noticeable.
func backdate(t *testing.T, root string, paths ...string) time.Time {
	t.Helper()
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, path := range paths {
		if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(path)), past, past); err != nil {
			t.Fatal("unable to set modification time:", err)
		}
	}
	return past
}

// verifyModificationTime verifies the modification time of a file within a
// root.
func verifyModificationTime(t *testing.T, root, path string, expected time.Time) {
	t.Helper()
	if info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
		t.Errorf("unable to query %s: %v", path, err)
	} else if !info.ModTime().Equal(expected) {
		t.Errorf("%s was modified: %v != %v", path, info.ModTime(), expected)
	}
}

// TestGitProviderProbe tests the classification of directories with .git
// entries through scans: a submodule has a GitLink entry for its Git entry,
// with the rest of its directory scanned as usual, a repository that can't be
// fetched elsewhere is an ordinary directory, and anything else makes the
// whole directory problematic, with a message that says what to do.
func TestGitProviderProbe(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")

	// scan creates an endpoint for a root and scans it, verifying that the
	// snapshot is valid and that its serialized form, which is what goes to
	// the other endpoint, doesn't contain credentials.
	scan := func(t *testing.T, root string) *core.Snapshot {
		t.Helper()
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		snapshot := scanEndpoint(t, e, true)
		if err := snapshot.EnsureValid(); err != nil {
			t.Fatal("invalid snapshot:", err)
		} else if serialized, err := proto.Marshal(snapshot); err != nil {
			t.Fatal("unable to marshal snapshot:", err)
		} else if bytes.Contains(serialized, []byte("secret")) {
			t.Error("serialized snapshot contains credentials")
		}
		return snapshot
	}

	// A clone with a commit and an origin is a submodule, whose URL lacks
	// credentials, and which doesn't carry a seed without a record of a
	// population. Its files are scanned, and its Git entry doesn't count as
	// content.
	t.Run("submodule", func(t *testing.T) {
		root := newGitRoot(t, map[string]string{"kept": "kept"})
		directory := cloneUpstream(t, upstream, root, "sub")
		origin := "https://user:secret@example.org/repository.git"
		mustGit(t, directory, "config", "remote.origin.url", origin)
		snapshot := scan(t, root)
		link := gitLinkAt(t, snapshot, "sub")
		if hex.EncodeToString(link.Digest) != upstream.head {
			t.Errorf("incorrect commit: %x != %s", link.Digest, upstream.head)
		} else if link.Url != "https://example.org/repository.git" {
			t.Error("incorrect URL:", link.Url)
		} else if link.Seed != "" || link.AutoCRLF != "" || link.Eol != "" {
			t.Errorf("unexpected fields: %q %q %q", link.Seed, link.AutoCRLF, link.Eol)
		}
		verifyKinds(t, snapshot, map[string]core.EntryKind{
			"sub":      core.EntryKind_Directory,
			"sub/file": core.EntryKind_File,
		})
		if snapshot.Directories != 2 || snapshot.Files != 2 {
			t.Errorf("incorrect statistics: %d directories, %d files", snapshot.Directories, snapshot.Files)
		}
		if url := mustGit(t, directory, "config", "--get", "remote.origin.url"); url != origin {
			t.Error("origin was changed:", url)
		}
	})

	// The line ending settings that apply to the repository are carried over.
	t.Run("line ending settings", func(t *testing.T) {
		root := newGitRoot(t, nil)
		directory := cloneUpstream(t, upstream, root, "sub")
		mustGit(t, directory, "config", "core.autocrlf", "input")
		mustGit(t, directory, "config", "core.eol", "crlf")
		link := gitLinkAt(t, scan(t, root), "sub")
		if link.AutoCRLF != "input" || link.Eol != "crlf" {
			t.Errorf("incorrect line ending settings: %q %q", link.AutoCRLF, link.Eol)
		}
	})

	// A submodule whose Git entry is a file that refers to the Git directory
	// of the enclosing repository, which is what Git creates, works the same
	// way. Nested submodules are found too.
	t.Run("Git file", func(t *testing.T) {
		nested := newGitUpstream(t, "nested")
		root := newGitRoot(t, nil)
		initializeRepository(t, root)
		mustGit(t, root, "submodule", "--quiet", "add", "--", upstream.url, "sub")
		mustGit(t, root, "submodule", "--quiet", "add", "--", nested.url, "a/b")
		if info, err := os.Lstat(filepath.Join(root, "sub", ".git")); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("submodule doesn't have a Git file: %v, %v", info, err)
		}
		snapshot := scan(t, root)
		if link := gitLinkAt(t, snapshot, "sub"); hex.EncodeToString(link.Digest) != upstream.head || link.Url != upstream.url {
			t.Errorf("incorrect GitLink entry: %v", link)
		}
		if link := gitLinkAt(t, snapshot, "a/b"); hex.EncodeToString(link.Digest) != nested.head || link.Url != nested.url {
			t.Errorf("incorrect nested GitLink entry: %v", link)
		}
		verifyKinds(t, snapshot, map[string]core.EntryKind{
			".git":        core.EntryKind_Untracked,
			".gitmodules": core.EntryKind_File,
			"sub/file":    core.EntryKind_File,
			"a/b/file":    core.EntryKind_File,
		})
		if paths := snapshot.Content.GitLinkPaths(); !slices.Equal(paths, []string{"a/b/.git", "sub/.git"}) {
			t.Error("incorrect GitLink paths:", paths)
		}
	})

	// A repository without an origin or commits is an ordinary directory, whose
	// Git entry is untracked.
	t.Run("no origin", func(t *testing.T) {
		root := newGitRoot(t, nil)
		newGitWithOrigin(t, root, "sub", "")
		verifyKinds(t, scan(t, root), map[string]core.EntryKind{
			"sub":      core.EntryKind_Directory,
			"sub/.git": core.EntryKind_Untracked,
			"sub/file": core.EntryKind_File,
		})
	})
	t.Run("no commits", func(t *testing.T) {
		root := newGitRoot(t, map[string]string{"sub/file": "file"})
		initializeRepository(t, filepath.Join(root, "sub"))
		mustGit(t, filepath.Join(root, "sub"), "config", "remote.origin.url", upstream.url)
		verifyKinds(t, scan(t, root), map[string]core.EntryKind{
			"sub":      core.EntryKind_Directory,
			"sub/.git": core.EntryKind_Untracked,
			"sub/file": core.EntryKind_File,
		})
	})

	// Everything else makes the directory problematic as a whole, so that
	// none of its content is synchronized.
	verifyProblematic := func(t *testing.T, root string, contents ...string) {
		t.Helper()
		snapshot := scan(t, root)
		entry := entryAt(snapshot, "sub")
		if entry == nil || entry.Kind != core.EntryKind_Problematic {
			t.Fatalf("directory not problematic: %v", entry)
		}
		for _, content := range contents {
			if !strings.Contains(entry.Problem, content) {
				t.Errorf("problem doesn't contain %q: %s", content, entry.Problem)
			}
		}
		if entryAt(snapshot, "kept") == nil {
			t.Error("unrelated content not scanned")
		}
	}
	newRoot := func(t *testing.T) string {
		return newGitRoot(t, map[string]string{"kept": "kept", "sub/file": "file"})
	}
	t.Run("empty Git directory", func(t *testing.T) {
		root := newRoot(t)
		if err := os.Mkdir(filepath.Join(root, "sub", ".git"), 0700); err != nil {
			t.Fatal(err)
		}
		verifyProblematic(t, root, "isn't a valid repository", "remove the directory")
	})
	t.Run("dangling Git file", func(t *testing.T) {
		root := newRoot(t)
		writeTree(t, root, map[string]string{"sub/.git": "gitdir: ../.git/modules/sub\n"})
		verifyProblematic(t, root, "isn't a valid repository", "remove the directory")
	})
	// A differently cased Git entry that Git can't use, as it can't on a
	// case-sensitive filesystem, leaves an ordinary directory, whose Git entry
	// isn't synchronized.
	t.Run("differently cased Git entry", func(t *testing.T) {
		root := newRoot(t)
		if err := os.Mkdir(filepath.Join(root, "sub", ".GIT"), 0700); err != nil {
			t.Fatal(err)
		} else if _, err := os.Lstat(filepath.Join(root, "sub", ".git")); err == nil {
			t.Skip("filesystem is case-insensitive")
		}
		verifyKinds(t, scan(t, root), map[string]core.EntryKind{
			"sub":      core.EntryKind_Directory,
			"sub/.GIT": core.EntryKind_Untracked,
			"sub/file": core.EntryKind_File,
		})
	})
	t.Run("bare repository", func(t *testing.T) {
		root := newRoot(t)
		initializeRepository(t, filepath.Join(root, "sub", ".git"), "--bare")
		verifyProblematic(t, root, "working tree", "remove the directory")
	})
	t.Run("working tree elsewhere", func(t *testing.T) {
		root := newRoot(t)
		directory := newGitWithOrigin(t, root, "sub", upstream.url)
		mustGit(t, directory, "config", "core.worktree", root)
		verifyProblematic(t, root, "working tree")
	})
	t.Run("missing commit", func(t *testing.T) {
		root := newRoot(t)
		directory := newGitWithOrigin(t, root, "sub", upstream.url)
		objects := filepath.Join(directory, ".git", "objects")
		if err := os.RemoveAll(objects); err != nil {
			t.Fatal(err)
		} else if err := os.Mkdir(objects, 0700); err != nil {
			t.Fatal(err)
		}
		verifyProblematic(t, root, "damaged", "remove the directory")
	})
	t.Run("relative origin", func(t *testing.T) {
		root := newRoot(t)
		newGitWithOrigin(t, root, "sub", "../relative.git")
		verifyProblematic(t, root, "is relative", "set-url")
	})
	t.Run("origin starting with dash", func(t *testing.T) {
		root := newRoot(t)
		newGitWithOrigin(t, root, "sub", "-https://user:secret@example.org/repository.git")
		verifyProblematic(t, root, "starts with a dash", "set-url")
	})
	t.Run("different owner", func(t *testing.T) {
		root := newRoot(t)
		directory := newGitWithOrigin(t, root, "sub", upstream.url)
		t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
		if _, err := runGit(directory, "rev-parse", "HEAD"); err == nil {
			t.Skip("Git doesn't check ownership")
		}
		verifyProblematic(t, root, "safe.directory", directory)

		// The check isn't ours to override, but what the user decides is
		// respected.
		configuration := filepath.Join(realTemporaryDirectory(t), "config")
		writeTree(t, filepath.Dir(configuration), map[string]string{"config": "[safe]\n\tdirectory = *\n"})
		t.Setenv("GIT_CONFIG_GLOBAL", configuration)
		gitLinkAt(t, scan(t, root), "sub")
	})
	t.Run("without Git", func(t *testing.T) {
		root := newRoot(t)
		newGitWithOrigin(t, root, "sub", upstream.url)
		t.Setenv("PATH", t.TempDir())
		verifyProblematic(t, root, "git isn't installed")
	})
}

// TestEndpointGitModeAttach tests that a transition that creates the Git entry
// of an existing directory attaches a repository with the commit checked out on
// a detached HEAD, without touching the directory's other contents.
func TestEndpointGitModeAttach(t *testing.T) {
	// Create an upstream and a directory with its files, as synchronized from
	// the other endpoint, along with files that the commit doesn't track, one
	// of which its rules ignore, and a file that differs from the commit.
	upstream := newGitUpstream(t, "upstream")
	head := upstream.commitFiles("more files", map[string]string{
		".gitignore":    "*.log\n",
		"directory/fix": "committed",
		"modified":      "committed",
	})
	upstream.push()
	files := map[string]string{
		"sub/.gitignore":    "*.log\n",
		"sub/file":          "first",
		"sub/directory/fix": "committed",
		"sub/modified":      "modified",
		"sub/untracked":     "untracked",
		"sub/ignored.log":   "ignored",
	}
	root := newGitRoot(t, files)
	var paths []string
	for path := range files {
		paths = append(paths, path)
	}
	past := backdate(t, root, paths...)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)

	// Attach the repository. The target's seed records a population on the
	// other endpoint, so it isn't part of the result.
	target := upstream.entry(head)
	target.Seed = "seed"
	results, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub/.git", New: target})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	}
	if link := results[0]; link == nil || !link.Equal(target, true) || link.Url != upstream.url || link.Seed != "" {
		t.Errorf("incorrect result: %v", link)
	}

	// Verify the repository, whose working tree is the directory, and whose
	// status reflects the files that differ.
	directory := filepath.Join(root, "sub")
	if info, err := os.Lstat(filepath.Join(directory, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("Git directory not attached: %v, %v", info, err)
	}
	verifyDetachedAt(t, directory, head)
	if toplevel := mustGit(t, directory, "rev-parse", "--show-toplevel"); filepath.Clean(filepath.FromSlash(toplevel)) != directory {
		t.Errorf("incorrect working tree: %s", toplevel)
	}
	if status := mustGit(t, directory, "status", "--porcelain"); status != "M modified\n?? untracked" {
		t.Errorf("incorrect status: %q", status)
	}
	if staged := mustGit(t, directory, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("index doesn't match commit: %q", staged)
	}

	// Nothing else was touched.
	for path, contents := range files {
		verifyContents(t, root, path, contents)
		verifyModificationTime(t, root, path, past)
	}

	// Once the files equal the commit's, the status is clean.
	writeTree(t, root, map[string]string{"sub/modified": "committed"})
	if err := os.Remove(filepath.Join(directory, "untracked")); err != nil {
		t.Fatal(err)
	}
	verifyCheckout(t, directory, head)

	// The next scan has the submodule.
	if link := gitLinkAt(t, scanEndpoint(t, e, true), "sub"); hex.EncodeToString(link.Digest) != head {
		t.Errorf("incorrect commit: %x", link.Digest)
	}
}

// TestEndpointGitModeAttachFailure tests that a repository that can't be
// attached leaves the directory without a Git entry, and without anything
// else.
func TestEndpointGitModeAttachFailure(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	root := newGitRoot(t, map[string]string{"sub/file": "first"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	url := fileURL(filepath.Join(filepath.Dir(upstream.bare), "missing.git"))
	target := &core.Entry{Kind: core.EntryKind_GitLink, Digest: upstream.entry(upstream.head).Digest, Url: url}
	results, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub/.git", New: target})
	if results[0] != nil {
		t.Error("incorrect result:", results[0])
	}
	verifySingleFailure(t, problems, "sub/.git", "unable to clone "+url)
	verifyAbsent(t, root, "sub/.git")
	verifyContents(t, root, "sub/file", "first")
	if entries, err := os.ReadDir(filepath.Join(root, "sub")); err != nil || len(entries) != 1 {
		t.Errorf("directory has unexpected content: %v, %v", entries, err)
	}
}

// TestEndpointGitModeAttachFallback tests that attaching a commit that isn't
// available from the origin attaches the default branch's commit instead, with
// a warning, without touching the working tree.
func TestEndpointGitModeAttachFallback(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	pushed := upstream.head
	unpushed := upstream.commit("second", "second")
	root := newGitRoot(t, map[string]string{"sub/file": "local"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	results, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub/.git", New: upstream.entry(unpushed)})
	verifySingleWarning(t, problems, "sub/.git", "default branch", gitShortCommit(pushed), gitShortCommit(unpushed), upstream.url)
	if results[0] == nil || hex.EncodeToString(results[0].Digest) != pushed || results[0].Seed != "" {
		t.Errorf("incorrect GitLink entry: %v", results[0])
	}
	verifyDetachedAt(t, filepath.Join(root, "sub"), pushed)
	verifyContents(t, root, "sub/file", "local")
	verifyAbsent(t, root, "sub/second")
}

// submoduleClone is a submodule that a user has cloned, with an endpoint for
// its synchronization root.
type submoduleClone struct {
	// upstream is the submodule's upstream.
	upstream *gitUpstream
	// root is the synchronization root.
	root string
	// directory is the submodule's directory, at sub within the root.
	directory string
	// first is the commit that's checked out.
	first string
}

// newSubmoduleClone clones an upstream at sub within a new root.
func newSubmoduleClone(t *testing.T, upstream *gitUpstream) *submoduleClone {
	t.Helper()
	root := newGitRoot(t, nil)
	return &submoduleClone{
		upstream:  upstream,
		root:      root,
		directory: cloneUpstream(t, upstream, root, "sub"),
		first:     upstream.head,
	}
}

// follow scans an endpoint, calls a function (if it isn't nil), and then moves
// the submodule at sub to the specified commit, returning the scanned GitLink
// entry, the result, and the problems.
func follow(t *testing.T, e *endpoint, target *core.Entry, beforeTransition func()) (*core.Entry, *core.Entry, []*core.Problem) {
	t.Helper()
	old := gitLinkAt(t, scanEndpoint(t, e, true), "sub")
	if beforeTransition != nil {
		beforeTransition()
	}
	results, problems, _, err := e.Transition(context.Background(), []*core.Change{{Path: "sub/.git", Old: old, New: target}})
	if err != nil {
		t.Fatal("transition failed:", err)
	}
	verifyNoTemporaryContent(t, e.root)
	return old, results[0], problems
}

// drainPollSignal discards any pending signal from an endpoint's polling
// coalescer.
func drainPollSignal(e *endpoint) {
	select {
	case <-e.pollSignal.Signals():
	default:
	}
}

// waitForPollSignal waits for a signal from an endpoint's polling coalescer.
func waitForPollSignal(t *testing.T, e *endpoint) {
	t.Helper()
	select {
	case <-e.pollSignal.Signals():
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for poll signal")
	}
}

// TestEndpointGitModeFollow tests that a transition between GitLink entries
// moves the repository's HEAD and index to the target commit, without touching
// the working tree or the branch that was checked out, and that it's only a
// warning if it can't.
func TestEndpointGitModeFollow(t *testing.T) {
	// The working tree is left alone, including modifications, files that the
	// rules ignore, and the files of the target commit, which are synchronized
	// separately. The branch stays where it was.
	t.Run("working tree", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		upstream.commitFiles("rules", map[string]string{".gitignore": "*.log\n"})
		upstream.push()
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commitFiles("second", map[string]string{"file": "second", "added": "added"})
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		files := map[string]string{"sub/file": "modified", "sub/ignored.log": "ignored", "sub/untracked": "untracked"}
		writeTree(t, clone.root, files)
		past := backdate(t, clone.root, "sub/file", "sub/ignored.log", "sub/untracked", "sub/.gitignore")
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		target := upstream.entry(second)
		old, result, problems := follow(t, e, target, nil)
		if len(problems) != 0 {
			t.Fatal("unexpected problems:", problems)
		} else if result != target {
			t.Error("incorrect result:", result)
		} else if hex.EncodeToString(old.Digest) != clone.first {
			t.Errorf("incorrect scanned commit: %x", old.Digest)
		}
		verifyDetachedAt(t, clone.directory, second)
		if branch := mustGit(t, clone.directory, "rev-parse", "refs/heads/main"); branch != clone.first {
			t.Errorf("branch moved: %s", branch)
		}
		if staged := mustGit(t, clone.directory, "diff", "--cached", "--name-only"); staged != "" {
			t.Errorf("index doesn't match commit: %q", staged)
		}
		for path, contents := range files {
			verifyContents(t, clone.root, path, contents)
			verifyModificationTime(t, clone.root, path, past)
		}
		verifyModificationTime(t, clone.root, "sub/.gitignore", past)
		verifyAbsent(t, clone.root, "sub/added")
		if link := gitLinkAt(t, scanEndpoint(t, e, true), "sub"); hex.EncodeToString(link.Digest) != second {
			t.Errorf("incorrect commit after follow: %x", link.Digest)
		}

		// Moving to the commit that's already checked out does nothing.
		_, result, problems = follow(t, e, target, nil)
		if len(problems) != 0 || result != target {
			t.Errorf("incorrect outcome: %v, %v", result, problems)
		}
	})

	// Nested repositories aren't touched, even if the configuration asks for
	// commands to recurse into submodules.
	t.Run("nested repository", func(t *testing.T) {
		nested := newGitUpstream(t, "nested")
		nestedFirst := nested.head
		upstream := newGitUpstream(t, "upstream")
		mustGit(t, upstream.work, "config", "--file", ".gitmodules", "submodule.nested.path", "nested")
		mustGit(t, upstream.work, "config", "--file", ".gitmodules", "submodule.nested.url", nested.url)
		mustGit(t, upstream.work, "add", "--", ".gitmodules")
		mustGit(t, upstream.work, "update-index", "--add", "--cacheinfo", "160000,"+nestedFirst+",nested")
		mustGit(t, upstream.work, "commit", "--quiet", "--no-gpg-sign", "-m", "add nested")
		upstream.push()
		clone := newSubmoduleClone(t, upstream)
		nestedDirectory := cloneUpstream(t, nested, clone.root, "sub/nested")
		mustGit(t, nestedDirectory, "checkout", "--quiet", "--detach", nestedFirst)

		// Move the nested submodule in the upstream.
		nestedSecond := nested.commit("second", "second")
		nested.push()
		mustGit(t, upstream.work, "update-index", "--cacheinfo", "160000,"+nestedSecond+",nested")
		mustGit(t, upstream.work, "commit", "--quiet", "--no-gpg-sign", "-m", "update nested")
		second := mustGit(t, upstream.work, "rev-parse", "HEAD")
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		mustGit(t, clone.directory, "submodule", "--quiet", "init")
		mustGit(t, clone.directory, "config", "submodule.recurse", "true")
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		_, result, problems := follow(t, e, upstream.entry(second), nil)
		if len(problems) != 0 {
			t.Fatal("unexpected problems:", problems)
		} else if !result.Equal(upstream.entry(second), true) {
			t.Error("incorrect result:", result)
		}
		verifyDetachedAt(t, clone.directory, second)
		if head := mustGit(t, nestedDirectory, "rev-parse", "HEAD"); head != nestedFirst {
			t.Errorf("nested repository moved: %s", head)
		}
		if status := mustGit(t, nestedDirectory, "status", "--porcelain"); status != "" {
			t.Errorf("nested repository modified: %q", status)
		}
		verifyAbsent(t, clone.root, "sub/nested/second")

		// The nested repository is a submodule of its own, which would be
		// moved separately.
		if link := gitLinkAt(t, scanEndpoint(t, e, true), "sub/nested"); hex.EncodeToString(link.Digest) != nestedFirst {
			t.Errorf("incorrect nested commit: %x", link.Digest)
		}
	})

	// In a sparse checkout, files outside of its patterns that have been
	// written into the working tree stay there, which a checkout wouldn't
	// leave them.
	t.Run("sparse checkout", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		upstream.commitFiles("sparse", map[string]string{"in/a": "a", "out/b": "b"})
		upstream.push()
		clone := newSubmoduleClone(t, upstream)
		mustGit(t, clone.directory, "sparse-checkout", "set", "in")
		verifyAbsent(t, clone.root, "sub/out/b")
		writeTree(t, clone.root, map[string]string{"sub/out/b": "b"})
		second := upstream.commitFiles("second", map[string]string{"in/a": "changed", "out/b": "changed"})
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		_, _, problems := follow(t, e, upstream.entry(second), nil)
		if len(problems) != 0 {
			t.Fatal("unexpected problems:", problems)
		}
		verifyDetachedAt(t, clone.directory, second)
		verifyContents(t, clone.root, "sub/out/b", "b")
		verifyContents(t, clone.root, "sub/in/a", "a")
	})

	// A required clean filter that fails doesn't stop HEAD from following,
	// since the index isn't refreshed. The clone's files are given times that
	// precede the index, since Git compares the contents of files that are
	// "racily clean" (whose times are those of the index) whenever it writes
	// the index, refresh or not, which would run the filter (see the next
	// test).
	t.Run("failing clean filter", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		upstream.commitFiles("filtered", map[string]string{".gitattributes": "* filter=x\n", "data": "first"})
		upstream.push()
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commitFiles("second", map[string]string{"data": "second"})
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		backdate(t, clone.root, "sub/.gitattributes", "sub/data", "sub/file")
		mustGit(t, clone.directory, "update-index", "--refresh")
		writeTree(t, clone.root, map[string]string{"sub/file": "modified"})
		useFailingCleanFilter(t)
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		_, _, problems := follow(t, e, upstream.entry(second), nil)
		if len(problems) != 0 {
			t.Fatal("unexpected problems:", problems)
		}
		verifyDetachedAt(t, clone.directory, second)
		verifyContents(t, clone.root, "sub/data", "first")
		verifyContents(t, clone.root, "sub/file", "modified")

		// A refresh would have run the filter, and failed.
		if _, err := runGit(clone.directory, "update-index", "-q", "--refresh"); err == nil {
			t.Error("refresh succeeded despite failing filter, so the test is ineffective")
		}
	})

	// Right after a checkout, when the files are racily clean, which is what a
	// clone that the endpoint populates looks like, writing the index runs the
	// filter even without a refresh, so the reset fails, and the index is
	// replaced with the commit's tree instead.
	t.Run("failing clean filter with racily clean files", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		upstream.commitFiles("filtered", map[string]string{".gitattributes": "* filter=x\n", "data": "first"})
		upstream.push()
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commitFiles("second", map[string]string{"data": "second"})
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		useFailingCleanFilter(t)
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		_, _, problems := follow(t, e, upstream.entry(second), nil)
		if len(problems) != 0 {
			t.Fatal("unexpected problems:", problems)
		}
		verifyDetachedAt(t, clone.directory, second)
		verifyContents(t, clone.root, "sub/data", "first")
		if tree := mustGit(t, clone.directory, "write-tree"); tree != mustGit(t, clone.directory, "rev-parse", second+"^{tree}") {
			t.Error("index doesn't match the commit")
		}
	})

	// HEAD that's changed since the scan is left alone, with a warning.
	t.Run("changed since scan", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commit("second", "second")
		third := upstream.commit("third", "third")
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		old, result, problems := follow(t, e, upstream.entry(third), func() {
			mustGit(t, clone.directory, "update-ref", "--no-deref", "HEAD", second)
		})
		if result != old {
			t.Error("incorrect result:", result)
		}
		verifySingleWarning(t, problems, "sub/.git", "changed since the scan", gitShortCommit(third))
		verifyDetachedAt(t, clone.directory, second)
	})

	// A lock on the index stops the index and HEAD from moving, but HEAD is
	// detached at the commit where it was, which is consistent with the index.
	// The next attempt succeeds once the lock is gone.
	t.Run("locked index", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commit("second", "second")
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		lock := filepath.Join(clone.directory, ".git", "index.lock")
		writeTree(t, filepath.Dir(lock), map[string]string{"index.lock": ""})
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		old, result, problems := follow(t, e, upstream.entry(second), nil)
		if result != old {
			t.Error("incorrect result:", result)
		}
		verifySingleWarning(t, problems, "sub/.git", gitShortCommit(second), "index.lock")
		verifyDetachedAt(t, clone.directory, clone.first)
		if err := os.Remove(lock); err != nil {
			t.Fatal(err)
		}
		if staged := mustGit(t, clone.directory, "diff", "--cached", "--name-only"); staged != "" {
			t.Errorf("index doesn't match HEAD: %q", staged)
		}
		_, _, problems = follow(t, e, upstream.entry(second), nil)
		if len(problems) != 0 {
			t.Fatal("unexpected problems:", problems)
		}
		verifyDetachedAt(t, clone.directory, second)
	})

	// Versions of Git before 2.36 don't know --no-refresh, which they reject
	// as invalid usage, in which case the reset is performed without it.
	t.Run("Git without --no-refresh", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("wrapper requires a POSIX shell")
		}
		upstream := newGitUpstream(t, "upstream")
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commit("second", "second")
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")

		// Put a wrapper in front of Git that behaves like such a version.
		real, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		wrapper := realTemporaryDirectory(t)
		rejections := filepath.Join(wrapper, "rejections")
		script := "#!/bin/sh\n" +
			"for argument in \"$@\"; do\n" +
			"  if [ \"$argument\" = --no-refresh ]; then\n" +
			"    echo rejected >> '" + rejections + "'\n" +
			"    echo \"error: unknown option \\`no-refresh'\" >&2\n" +
			"    for line in 1 2 3 4 5 6 7 8 9 10 11 12; do echo \"usage line $line\" >&2; done\n" +
			"    exit 129\n" +
			"  fi\n" +
			"done\n" +
			"exec '" + real + "' \"$@\"\n"
		writeTree(t, wrapper, map[string]string{"git": script})
		if err := os.Chmod(filepath.Join(wrapper, "git"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))

		// Follow.
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		_, _, problems := follow(t, e, upstream.entry(second), nil)
		if len(problems) != 0 {
			t.Fatal("unexpected problems:", problems)
		}
		verifyDetachedAt(t, clone.directory, second)
		if contents, err := os.ReadFile(rejections); err != nil || string(contents) != "rejected\n" {
			t.Errorf("wrapper didn't reject the option once: %q, %v", contents, err)
		}
	})
}

// useFailingCleanFilter configures Git with a filter named x that's required
// and whose clean command fails.
func useFailingCleanFilter(t *testing.T) {
	t.Helper()
	configuration := filepath.Join(realTemporaryDirectory(t), "config")
	writeTree(t, filepath.Dir(configuration), map[string]string{
		"config": "[filter \"x\"]\n\trequired = true\n\tclean = false\n",
	})
	t.Setenv("GIT_CONFIG_GLOBAL", configuration)
}

// TestEndpointGitModeFollowFetch tests that a commit that isn't available
// locally is fetched in the background, which wakes the session, so that the
// next attempt to follow it succeeds, and that a commit that can't be fetched
// isn't fetched again until the retry interval has passed.
func TestEndpointGitModeFollowFetch(t *testing.T) {
	t.Run("pushed", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commit("second", "second")
		upstream.push()
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		drainPollSignal(e)
		target := upstream.entry(second)
		old, result, problems := follow(t, e, target, nil)
		if result != old {
			t.Error("incorrect result:", result)
		}
		verifySingleWarning(t, problems, "sub/.git", gitShortCommit(second), "isn't available on this endpoint yet", "being fetched", "synchronized regardless")
		if head := mustGit(t, clone.directory, "symbolic-ref", "HEAD"); head != "refs/heads/main" {
			t.Error("HEAD changed:", head)
		}
		waitForPollSignal(t, e)
		mustGit(t, clone.directory, "cat-file", "-e", second+"^{commit}")
		if branch := mustGit(t, clone.directory, "rev-parse", "refs/heads/main"); branch != clone.first {
			t.Errorf("fetch moved branch: %s", branch)
		}
		verifyAbsent(t, clone.root, "sub/.git/FETCH_HEAD")
		_, result, problems = follow(t, e, target, nil)
		if len(problems) != 0 {
			t.Fatal("unexpected problems:", problems)
		} else if result != target {
			t.Error("incorrect result:", result)
		}
		verifyDetachedAt(t, clone.directory, second)
	})

	t.Run("unpushed", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		clone := newSubmoduleClone(t, upstream)
		unpushed := upstream.commit("second", "second")
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		fetcher := e.gitProvider.fetcher
		key := gitRetryKey{url: upstream.url, commit: unpushed}
		target := upstream.entry(unpushed)
		_, _, problems := follow(t, e, target, nil)
		verifySingleWarning(t, problems, "sub/.git", "being fetched")
		deadline := time.Now().Add(30 * time.Second)
		for fetcher.retries.state(key) == nil {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for fetch to fail")
			}
			time.Sleep(10 * time.Millisecond)
		}
		failure := fetcher.retries.state(key)

		// The next attempt to follow reports the failure and when the next
		// fetch happens, which isn't right away.
		_, _, problems = follow(t, e, target, nil)
		verifySingleWarning(t, problems, "sub/.git", "couldn't be fetched", "isn't available from "+upstream.url, "fetched again in")
		time.Sleep(100 * time.Millisecond)
		if state := fetcher.retries.state(key); state == nil || !state.last.Equal(failure.last) {
			t.Errorf("fetch attempted again within retry interval: %+v", state)
		}
		select {
		case <-e.pollSignal.Signals():
			t.Error("session woken without fetch")
		default:
		}
	})
}

// TestGitRetries tests the throttling of attempts after failures.
func TestGitRetries(t *testing.T) {
	retries := newGitRetries()
	key := gitRetryKey{url: "https://example.org/repository.git", commit: "0123"}
	if remaining, last := retries.wait(key); remaining != 0 || last != nil {
		t.Fatalf("throttled without failure: %s, %v", remaining, last)
	}

	// Each consecutive failure doubles the interval, up to the maximum.
	failure := fmt.Errorf("failure")
	var intervals []time.Duration
	for i := 0; i < 7; i++ {
		retries.failed(key, failure)
		if remaining, last := retries.wait(key); last != failure || remaining <= 0 {
			t.Fatalf("incorrect throttling after failure %d: %s, %v", i, remaining, last)
		}
		intervals = append(intervals, retries.state(key).interval)
	}
	expected := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	if !slices.Equal(intervals, expected) {
		t.Errorf("incorrect intervals: %v != %v", intervals, expected)
	}

	// Keys are independent of each other.
	if remaining, _ := retries.wait(gitRetryKey{url: key.url, commit: "4567"}); remaining != 0 {
		t.Error("other commit throttled")
	}

	// A success ends the throttling.
	retries.succeeded(key)
	if remaining, last := retries.wait(key); remaining != 0 || last != nil {
		t.Errorf("throttled after success: %s, %v", remaining, last)
	}
	retries.failed(key, failure)
	if state := retries.state(key); state.interval != gitMinimumRetryInterval {
		t.Errorf("interval not reset by success: %s", state.interval)
	}
}

// TestGitFetcherDefersRequests tests that a fetch that fails isn't attempted
// again without a request, which keeps an idle session off the network, that a
// request within the retry interval is deferred until the interval has passed
// rather than dropped, and that a successful fetch wakes the session.
func TestGitFetcherDefersRequests(t *testing.T) {
	const interval = 300 * time.Millisecond
	upstream := newGitUpstream(t, "upstream")
	clone := newSubmoduleClone(t, upstream)
	unpushed := upstream.commit("second", "second")
	e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
	fetcher := e.gitProvider.fetcher
	fetcher.retries.useIntervals(interval, time.Minute)
	key := gitRetryKey{url: upstream.url, commit: unpushed}
	drainPollSignal(e)

	// waitForFailures waits until the specified number of consecutive failures
	// have been recorded, which can be determined from the interval.
	waitForFailures := func(count int) *gitRetryState {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			if state := fetcher.retries.state(key); state != nil && state.interval == interval<<(count-1) {
				return state
			} else if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for failure %d: %+v", count, state)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// The first request is attempted right away, and it fails.
	if status := fetcher.request(clone.directory, upstream.url, unpushed); status != "it's being fetched" {
		t.Error("incorrect status for first request:", status)
	}
	first := waitForFailures(1)

	// Without another request, the fetch is attempted again once the interval
	// has passed, since pushing the commit wouldn't lead to another request.
	second := waitForFailures(2)
	if elapsed := second.last.Sub(first.last); elapsed < first.interval {
		t.Errorf("fetch attempted again after %s, within interval of %s", elapsed, first.interval)
	}

	// A request within the interval doesn't lead to an earlier attempt. Once
	// the interval has passed, the fetch succeeds, since the commit has been
	// pushed meanwhile, and the session is woken.
	upstream.push()
	if status := fetcher.request(clone.directory, upstream.url, unpushed); !strings.Contains(status, "fetched again in") {
		t.Error("incorrect status for request within interval:", status)
	}
	waitForPollSignal(t, e)
	if elapsed := time.Since(second.last); elapsed < second.interval {
		t.Errorf("deferred request attempted after %s, within interval of %s", elapsed, second.interval)
	}
	mustGit(t, clone.directory, "cat-file", "-e", unpushed+"^{commit}")
	if state := fetcher.retries.state(key); state != nil {
		t.Errorf("retry state retained after success: %+v", state)
	}

	// A request that's cancelled isn't attempted again.
	another := upstream.commit("third", "third")
	anotherKey := gitRetryKey{url: upstream.url, commit: another}
	fetcher.request(clone.directory, upstream.url, another)
	deadline := time.Now().Add(30 * time.Second)
	for fetcher.retries.state(anotherKey) == nil {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for failure")
		}
		time.Sleep(10 * time.Millisecond)
	}
	fetcher.cancel(clone.directory)
	cancelled := fetcher.retries.state(anotherKey)
	time.Sleep(3 * interval)
	if state := fetcher.retries.state(anotherKey); !state.last.Equal(cancelled.last) {
		t.Error("cancelled request attempted again")
	}

	// A request for a repository that's gone is dropped without fetching.
	if err := os.RemoveAll(clone.directory); err != nil {
		t.Fatal(err)
	}
	fetcher.request(clone.directory, upstream.url, another)
	time.Sleep(3 * interval)
	fetcher.lock.Lock()
	pending := len(fetcher.pending)
	fetcher.lock.Unlock()
	if pending != 0 {
		t.Error("request for missing repository retained")
	}
}

// TestGitWarningsTrackedIgnoredSubmodule tests that files that a submodule
// tracks, but that its own rules match, are reported at their paths within the
// root, along with those of the repository at the root, and that the root's
// rules don't apply to the submodule's files.
func TestGitWarningsTrackedIgnoredSubmodule(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	upstream.commitFiles("ignored", map[string]string{
		".gitignore": "*.o\nbuild/\n",
		"tracked.o":  "object",
		"build/out":  "output",
		"main.c":     "source",
	})
	upstream.push()
	root := newTrackedIgnoredRepository(t, []string{"root.o"})
	writeTree(t, root, map[string]string{".gitignore": "*.o\nbuild/\n*.c\n"})
	cloneUpstream(t, upstream, root, "sub")

	// Verify the warnings and their advice.
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	snapshot := scanEndpoint(t, e, true)
	expected := []string{"root.o", "sub/build", "sub/tracked.o"}
	if paths := warningPaths(snapshot); !slices.Equal(paths, expected) {
		t.Fatalf("incorrect warning paths: %v != %v", paths, expected)
	}
	for _, warning := range snapshot.Warnings {
		if !warning.Warning || !strings.Contains(warning.Error, `"!`+warning.Path+`" to ignore.paths`) {
			t.Errorf("incorrect warning for %s: %s", warning.Path, warning.Error)
		}
	}
	verifyKinds(t, snapshot, map[string]core.EntryKind{
		"sub/tracked.o": core.EntryKind_Untracked,
		"sub/build":     core.EntryKind_Untracked,
		"sub/main.c":    core.EntryKind_File,
	})

	// The advice works.
	e = newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch, "!sub/tracked.o")
	snapshot = scanEndpoint(t, e, true)
	if paths := warningPaths(snapshot); !slices.Equal(paths, []string{"root.o", "sub/build"}) {
		t.Errorf("incorrect warning paths with ignore: %v", paths)
	}
	verifyKinds(t, snapshot, map[string]core.EntryKind{"sub/tracked.o": core.EntryKind_File})
}
