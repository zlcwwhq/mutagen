package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
