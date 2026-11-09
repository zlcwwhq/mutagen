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
// resetting the index would discard, such as a merge with conflicts, and that it
// follows once the operation is over.
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
	err := provider.Follow(ctx, directory, upstream.entry(local), upstream.entry(second))
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
	err = provider.Follow(ctx, directory, upstream.entry(local), upstream.entry(second))
	if err == nil || !strings.Contains(err.Error(), "the index has conflicts") {
		t.Error("HEAD followed with conflicts in the index:", err)
	}

	// Once the conflicts are resolved, HEAD follows.
	mustGit(t, directory, "reset", "--quiet", "--hard")
	if err := provider.Follow(ctx, directory, upstream.entry(local), upstream.entry(second)); err != nil {
		t.Fatal("HEAD didn't follow:", err)
	} else if head := mustGit(t, directory, "rev-parse", "HEAD"); head != second {
		t.Error("HEAD didn't follow:", head)
	}
}

// TestEndpointGitModeFollowDuringCommit tests that HEAD doesn't follow while a
// commit is in progress in a submodule, which holds the index's lock while its
// message is being edited, and that the commit is then made on the branch that
// was checked out, rather than on a detached HEAD, which a follow that
// detached HEAD before failing to move the index would have left behind.
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

// installFailingIndexGit puts a wrapper in front of Git for the duration of a
// test that makes the commands that move a repository's index fail, as they
// do if another command takes the index's lock after a follow has detached
// HEAD. Before failing, the wrapper runs a shell command (if it isn't empty) in
// the repository, with the real Git as $git.
func installFailingIndexGit(t *testing.T, command string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := realTemporaryDirectory(t)
	script := "#!/bin/sh\n" +
		"git='" + real + "'\n" +
		"for argument in \"$@\"; do\n" +
		"  if [ \"$argument\" = reset ] || [ \"$argument\" = read-tree ]; then\n" +
		"    " + command + "\n" +
		"    echo 'fatal: index unavailable' >&2\n" +
		"    exit 128\n" +
		"  fi\n" +
		"done\n" +
		"exec \"$git\" \"$@\"\n"
	writeTree(t, wrapper, map[string]string{"git": script})
	if err := os.Chmod(filepath.Join(wrapper, "git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestEndpointGitModeFollowReattachesHead tests that if moving the index fails
// after HEAD has been detached, then HEAD is reattached to the branch that it
// referred to, unless the branch has moved in the mean time.
func TestEndpointGitModeFollowReattachesHead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper requires a POSIX shell")
	}

	// HEAD is reattached to its branch.
	t.Run("reattached", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commit("second", "second")
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		installFailingIndexGit(t, "")
		old, result, problems := follow(t, e, upstream.entry(second), nil)
		if result != old {
			t.Error("incorrect result:", result)
		}
		verifySingleWarning(t, problems, "sub/.git", gitShortCommit(second), "index unavailable")
		if branch, err := runGit(clone.directory, "symbolic-ref", "--quiet", "HEAD"); err != nil || branch != "refs/heads/main" {
			t.Errorf("HEAD wasn't reattached: %q, %v", branch, err)
		} else if head := mustGit(t, clone.directory, "rev-parse", "HEAD"); head != clone.first {
			t.Errorf("HEAD moved: %s", head)
		}
	})

	// If the branch has moved, then reattaching HEAD would change the commit
	// that's checked out, so HEAD stays detached, and the warning says so.
	t.Run("branch moved", func(t *testing.T) {
		upstream := newGitUpstream(t, "upstream")
		clone := newSubmoduleClone(t, upstream)
		second := upstream.commit("second", "second")
		upstream.push()
		mustGit(t, clone.directory, "fetch", "--quiet", "origin")
		e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
		installFailingIndexGit(t, "\"$git\" update-ref refs/heads/main "+second)
		old, result, problems := follow(t, e, upstream.entry(second), nil)
		if result != old {
			t.Error("incorrect result:", result)
		}
		verifySingleWarning(t, problems, "sub/.git", gitShortCommit(second), "left detached", "branch has moved")
		verifyDetachedAt(t, clone.directory, clone.first)
	})
}
