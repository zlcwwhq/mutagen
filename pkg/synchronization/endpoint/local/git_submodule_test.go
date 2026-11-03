package local

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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
		verifyProblematic(t, root, "isn't a valid repository", "(not the directory)")
	})
	t.Run("dangling Git file", func(t *testing.T) {
		root := newRoot(t)
		writeTree(t, root, map[string]string{"sub/.git": "gitdir: ../.git/modules/sub\n"})
		verifyProblematic(t, root, "isn't a valid repository", "(not the directory)")
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
		verifyProblematic(t, root, "working tree", "(not the directory)")
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
		verifyProblematic(t, root, "damaged", "(not the directory)")
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
