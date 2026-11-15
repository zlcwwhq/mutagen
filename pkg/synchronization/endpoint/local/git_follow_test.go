package local

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
)

// TestGitProviderFollowDuringOperation tests that HEAD doesn't follow a commit
// while an operation is in progress in a submodule whose state moving HEAD and
// the index would disrupt, such as a merge with conflicts, and that it follows
// once the operation is over.
func TestGitProviderFollowDuringOperation(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	first := upstream.head
	second := upstream.commit("file", "second")
	upstream.push()
	ctx := context.Background()
	provider := &gitProvider{
		stallTimeout: time.Minute,
		stallBackoff: time.Minute,
		backoffs:     make(map[string]time.Time),
	}
	provider.fetcher = newGitFetcher(provider, func() {})

	// Create a commit of a local branch that conflicts with the second commit,
	// and start merging the second commit into it.
	directory := cloneUpstream(t, upstream, realTemporaryDirectory(t), "sub")
	mustGit(t, directory, "checkout", "--quiet", "-b", "local", first)
	writeTree(t, directory, map[string]string{"file": "local"})
	mustGit(t, directory, "commit", "--quiet", "--no-gpg-sign", "-am", "local")
	local := mustGit(t, directory, "rev-parse", "HEAD")
	if _, err := runGit(directory, "merge", "--quiet", "--no-gpg-sign", second); err == nil {
		t.Fatal("merge didn't conflict")
	}
	mergeHead := filepath.Join(directory, ".git", "MERGE_HEAD")
	if _, err := os.Lstat(mergeHead); err != nil {
		t.Fatal("merge not in progress:", err)
	}

	// Verify that HEAD doesn't follow, and that the merge is intact.
	_, err := provider.Follow(ctx, directory, upstream.entry(local), upstream.entry(second))
	if err == nil || !strings.Contains(err.Error(), "a merge is in progress") {
		t.Error("HEAD followed during merge:", err)
	}
	if head := mustGit(t, directory, "rev-parse", "HEAD"); head != local {
		t.Error("HEAD moved during merge:", head)
	} else if _, err := os.Lstat(mergeHead); err != nil {
		t.Error("merge state lost:", err)
	} else if unmerged := mustGit(t, directory, "ls-files", "--unmerged"); unmerged == "" {
		t.Error("conflicts lost")
	}

	// The same applies to conflicts in the index without an operation in
	// progress, such as those left by applying a stash.
	if err := os.Remove(mergeHead); err != nil {
		t.Fatal(err)
	}
	_, err = provider.Follow(ctx, directory, upstream.entry(local), upstream.entry(second))
	if err == nil || !strings.Contains(err.Error(), "the index has conflicts") {
		t.Error("HEAD followed with conflicts in the index:", err)
	}

	// Once the conflicts are resolved, HEAD follows.
	mustGit(t, directory, "reset", "--quiet", "--hard")
	if _, err := provider.Follow(ctx, directory, upstream.entry(local), upstream.entry(second)); err != nil {
		t.Fatal("HEAD didn't follow:", err)
	} else if head := mustGit(t, directory, "rev-parse", "HEAD"); head != second {
		t.Error("HEAD didn't follow:", head)
	}
}

// TestEndpointGitModeFollowDuringCommit tests that HEAD doesn't follow while a
// commit is in progress in a submodule, which holds the index's lock while its
// message is being edited, and that the commit is then made on the branch that
// was checked out, rather than on a detached HEAD.
func TestEndpointGitModeFollowDuringCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("editor requires a POSIX shell")
	}
	upstream := newGitUpstream(t, "upstream")
	clone := newSubmoduleClone(t, upstream)
	second := upstream.commit("second", "second")
	upstream.push()
	mustGit(t, clone.directory, "fetch", "--quiet", "origin")
	e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)

	// Start a commit whose editor waits for a marker file.
	base := realTemporaryDirectory(t)
	marker := filepath.Join(base, "done")
	editor := filepath.Join(base, "editor")
	writeTree(t, base, map[string]string{"editor": "#!/bin/sh\n" +
		"while [ ! -f '" + marker + "' ]; do sleep 0.05; done\n" +
		"echo 'user commit' > \"$1\"\n"})
	if err := os.Chmod(editor, 0700); err != nil {
		t.Fatal(err)
	}
	writeTree(t, clone.root, map[string]string{"sub/file": "user change"})
	commit := exec.Command("git", "commit", "-a", "--no-gpg-sign")
	commit.Dir = clone.directory
	commit.Env = append(os.Environ(), "GIT_EDITOR="+editor)
	var output bytes.Buffer
	commit.Stdout, commit.Stderr = &output, &output
	if err := commit.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished {
			writeTree(t, base, map[string]string{"done": ""})
			commit.Wait()
		}
	}()
	lock := filepath.Join(clone.directory, ".git", "index.lock")
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, err := os.Lstat(lock); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("commit didn't lock the index")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Try to follow while the commit is in progress, which must leave HEAD
	// alone.
	old, result, problems := follow(t, e, upstream.entry(second), nil)
	if result != old {
		t.Error("incorrect result:", result)
	}
	verifySingleWarning(t, problems, "sub/.git", gitShortCommit(second), "index.lock")
	if branch, err := runGit(clone.directory, "symbolic-ref", "--quiet", "HEAD"); err != nil || branch != "refs/heads/main" {
		t.Errorf("HEAD doesn't refer to the branch: %q, %v", branch, err)
	}

	// Finish the commit, which must have moved the branch.
	writeTree(t, base, map[string]string{"done": ""})
	finished = true
	if err := commit.Wait(); err != nil {
		t.Fatalf("commit failed: %v: %s", err, output.String())
	}
	if branch := mustGit(t, clone.directory, "rev-parse", "refs/heads/main"); branch == clone.first {
		t.Errorf("commit wasn't made on the branch: %s", strings.TrimSpace(output.String()))
	} else if head := mustGit(t, clone.directory, "rev-parse", "HEAD"); head != branch {
		t.Errorf("HEAD isn't at the commit: %s != %s", head, branch)
	}
}

// installGitWrapper puts a wrapper in front of Git for the duration of a test,
// which, once the returned function has armed it, runs a shell command in the
// repository, with the real Git as $git, before each Git command whose
// arguments include the trigger. The shell command can make the Git command
// fail by exiting, and it can disarm the wrapper by removing "$armed".
func installGitWrapper(t *testing.T, trigger, command string) func() {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := realTemporaryDirectory(t)
	script := "#!/bin/sh\n" +
		"git='" + real + "'\n" +
		"armed='" + filepath.Join(wrapper, "armed") + "'\n" +
		"for argument in \"$@\"; do\n" +
		"  if [ \"$argument\" = '" + trigger + "' ] && [ -f \"$armed\" ]; then\n" +
		"    " + command + "\n" +
		"  fi\n" +
		"done\n" +
		"exec \"$git\" \"$@\"\n"
	writeTree(t, wrapper, map[string]string{"git": script})
	if err := os.Chmod(filepath.Join(wrapper, "git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() {
		writeTree(t, wrapper, map[string]string{"armed": ""})
	}
}

// TestEndpointGitModeFollowRaces tests that a follow doesn't change anything
// if another Git command takes the index's lock or moves HEAD after the
// follow has checked for them, since the follow holds the lock while it moves
// HEAD, and moves it only if it's still where it was.
func TestEndpointGitModeFollowRaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper requires a POSIX shell")
	}

	// verifyAttached verifies that HEAD still refers to the branch, at the
	// specified commit, and that the follow didn't leave its lock behind.
	verifyAttached := func(t *testing.T, clone *submoduleClone, commit string) {
		t.Helper()
		if branch, err := runGit(clone.directory, "symbolic-ref", "--quiet", "HEAD"); err != nil || branch != "refs/heads/main" {
			t.Errorf("HEAD doesn't refer to the branch: %q, %v", branch, err)
		} else if head := mustGit(t, clone.directory, "rev-parse", "HEAD"); head != commit {
			t.Errorf("incorrect commit: %s != %s", head, commit)
		}
	}

	// A command, such as a commit, that takes the index's lock once the
	// follow has checked that no command is using the index, but before the
	// follow takes the lock, keeps HEAD where it is, and its lock is left
	// alone. The follow succeeds once the lock is gone.
	t.Run("lock taken", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commit("second", "second")
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		lock := filepath.Join(clone.directory, ".git", "index.lock")
		arm := installGitWrapper(t, "index", "rm -f \"$armed\"; : > '"+lock+"'")
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		old, result, problems := follow(t, e, upstream.entry(second), arm)
		if result != old {
			t.Error("incorrect result:", result)
		}
		verifySingleWarning(t, problems, "sub/.git", gitShortCommit(second), "index.lock exists", lock)
		verifyAttached(t, clone, clone.first)
		if _, err := os.Lstat(lock); err != nil {
			t.Fatal("other command's lock removed:", err)
		} else if err := os.Remove(lock); err != nil {
			t.Fatal(err)
		}
		_, result, problems = follow(t, e, upstream.entry(second), nil)
		if len(problems) != 0 {
			t.Fatal("unexpected problems:", problems)
		} else if !result.Equal(upstream.entry(second), true) {
			t.Error("incorrect result:", result)
		}
		verifyDetachedAt(t, clone.directory, second)
		if branch := mustGit(t, clone.directory, "rev-parse", "refs/heads/main"); branch != clone.first {
			t.Errorf("branch moved: %s", branch)
		}
	})

	// A command that moves the branch that HEAD refers to while the follow
	// computes the index makes moving HEAD fail, and the follow leaves HEAD
	// and the index alone.
	t.Run("branch moved", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commit("second", "second")
		third := upstream.commit("third", "third")
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		index, err := os.ReadFile(filepath.Join(clone.directory, ".git", "index"))
		if err != nil {
			t.Fatal(err)
		}
		arm := installGitWrapper(t, "read-tree", "rm -f \"$armed\"; \"$git\" update-ref refs/heads/main "+second)
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		old, result, problems := follow(t, e, upstream.entry(third), arm)
		if result != old {
			t.Error("incorrect result:", result)
		}
		verifySingleWarning(t, problems, "sub/.git", gitShortCommit(third), "HEAD couldn't be moved")
		verifyAttached(t, clone, second)
		if current, err := os.ReadFile(filepath.Join(clone.directory, ".git", "index")); err != nil || !bytes.Equal(current, index) {
			t.Errorf("index changed: %v", err)
		}
		verifyAbsent(t, clone.root, "sub/.git/index.lock")
	})

	// If the index can't be computed, then nothing changes.
	t.Run("index failure", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commit("second", "second")
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		arm := installGitWrapper(t, "read-tree", "echo 'fatal: index unavailable' >&2; exit 128")
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		old, result, problems := follow(t, e, upstream.entry(second), arm)
		if result != old {
			t.Error("incorrect result:", result)
		}
		verifySingleWarning(t, problems, "sub/.git", gitShortCommit(second), "index couldn't be computed", "index unavailable")
		verifyAttached(t, clone, clone.first)
		verifyAbsent(t, clone.root, "sub/.git/index.lock")
	})
}
