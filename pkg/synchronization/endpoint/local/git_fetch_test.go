package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGitProviderFetchCommitKeepsReferences tests that fetching a commit adds
// nothing but objects to a repository, even when the commit can't be requested
// by its identifier, so that what the remote's configuration names is fetched
// instead, and even if the repository's configuration would fetch or prune
// tags or prune its remote-tracking references, or if the remote's HEAD
// reference is missing.
func TestGitProviderFetchCommitKeepsReferences(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	ctx := context.Background()

	// Create a branch on the server that's going to be deleted, and clone the
	// repository with every branch.
	mustGit(t, upstream.work, "push", "--quiet", "origin", "HEAD:refs/heads/gone")
	directory := filepath.Join(realTemporaryDirectory(t), "clone")
	mustGit(t, filepath.Dir(directory), "clone", "--quiet", "--no-tags", upstream.url, directory)

	// Give the clone a tag that only it has and configuration that prunes, and
	// change the server's branches and tags in ways that a fetch with the
	// remote's configured refspecs would reflect. The clone also loses the
	// remote's HEAD reference, which such a fetch creates (since Git 2.48).
	mustGit(t, directory, "remote", "set-head", "origin", "--delete")
	mustGit(t, directory, "tag", "local-only")
	mustGit(t, directory, "config", "fetch.prune", "true")
	mustGit(t, directory, "config", "fetch.pruneTags", "true")
	mustGit(t, directory, "config", "remote.origin.tagOpt", "--tags")
	mustGit(t, upstream.work, "push", "--quiet", "origin", ":refs/heads/gone")
	mustGit(t, upstream.work, "checkout", "--quiet", "-b", "feature")
	feature := upstream.commit("feature", "feature")
	mustGit(t, upstream.work, "tag", "server-only")
	mustGit(t, upstream.work, "push", "--quiet", "origin", "feature", "server-only")
	references := mustGit(t, directory, "for-each-ref")

	// Fetch a commit that the server doesn't have, which fails to be requested
	// by its identifier, so that the remote's configured refspecs are fetched,
	// and which then still isn't available.
	provider := &gitProvider{
		stallTimeout: time.Minute,
		stallBackoff: time.Minute,
		backoffs:     make(map[string]time.Time),
	}
	missing := "0123456789abcdef0123456789abcdef01234567"
	if err := provider.fetchCommit(ctx, directory, "origin", upstream.url, missing); err == nil {
		t.Fatal("missing commit fetched")
	}

	// The objects of the server's branches have been fetched, but no reference
	// has changed, and FETCH_HEAD hasn't been written.
	if !provider.hasCommit(ctx, directory, feature) {
		t.Error("commit of a branch on the server not fetched")
	}
	if after := mustGit(t, directory, "for-each-ref"); after != references {
		t.Errorf("references changed:\n%s\nbecame\n%s", references, after)
	}
	if _, err := os.Lstat(filepath.Join(directory, ".git", "FETCH_HEAD")); err == nil {
		t.Error("FETCH_HEAD written")
	} else if !os.IsNotExist(err) {
		t.Fatal("unable to check for FETCH_HEAD:", err)
	}
}

// TestGitProviderFetchMissingLocalOrigin tests that fetching a commit from
// origins that are a local path and a file URL, which Git mode doesn't fetch
// from by default, fails with an error that says which origins it fetches
// from, once, although both attempts fail for that reason.
func TestGitProviderFetchMissingLocalOrigin(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	directory := filepath.Join(realTemporaryDirectory(t), "clone")
	mustGit(t, filepath.Dir(directory), "clone", "--quiet", upstream.url, directory)
	mustGit(t, directory, "remote", "set-url", "origin", upstream.bare)
	commit := upstream.commit("second", "second")
	upstream.push()

	// Restore the default list of transports, which the upstream needed to
	// be set up.
	t.Setenv("GIT_ALLOW_PROTOCOL", "")
	if err := os.Unsetenv("GIT_ALLOW_PROTOCOL"); err != nil {
		t.Fatal("unable to unset GIT_ALLOW_PROTOCOL:", err)
	}

	// Fetch the commit from the clone's origin and the file URL.
	provider := &gitProvider{
		stallTimeout: time.Minute,
		stallBackoff: time.Minute,
		backoffs:     make(map[string]time.Time),
	}
	err := provider.fetchMissing(context.Background(), directory, upstream.url, commit)
	if err == nil {
		t.Fatal("commit fetched")
	}
	message := err.Error()
	if count := strings.Count(message, "transport 'file' not allowed"); count != 2 {
		t.Errorf("both refusals not reported: %s", message)
	}
	if count := strings.Count(message, "only clones and fetches submodules from https, http, ssh, and git URLs"); count != 1 {
		t.Errorf("refusals not explained once: %s", message)
	}
	if provider.hasCommit(context.Background(), directory, commit) {
		t.Error("commit fetched")
	}
}

// TestGitFetcherRequestAfterCancellation tests that a request whose fetch is
// in progress, and that's been cancelled, is attempted again if it fails, as
// long as it's been requested again since the cancellation.
func TestGitFetcherRequestAfterCancellation(t *testing.T) {
	fetcher := newGitFetcher(&gitProvider{}, func() {})
	request := gitFetchRequest{directory: "sub", key: gitRetryKey{url: "https://example.org/repository.git", commit: "commit"}}

	// Start the request's fetch, cancel the request, and request it again.
	fetcher.request(request.directory, request.key.url, request.key.commit)
	if started, _, ok := fetcher.next(); !ok || started != request {
		t.Fatalf("incorrect request started: %+v, %t", started, ok)
	}
	fetcher.cancel(request.directory)
	fetcher.request(request.directory, request.key.url, request.key.commit)

	// If the fetch fails, then the request is pending again.
	fetcher.finish(request, errors.New("not pushed yet"))
	if pending, ok := fetcher.pending[request.directory]; !ok || pending != request {
		t.Errorf("request not kept after failure: %+v, %t", pending, ok)
	}

	// Without the second request, it isn't.
	fetcher.retries.succeeded(request.key)
	if started, _, ok := fetcher.next(); !ok || started != request {
		t.Fatalf("incorrect request started: %+v, %t", started, ok)
	}
	fetcher.cancel(request.directory)
	fetcher.finish(request, errors.New("not pushed yet"))
	if pending, ok := fetcher.pending[request.directory]; ok {
		t.Errorf("cancelled request kept: %+v", pending)
	}
}
