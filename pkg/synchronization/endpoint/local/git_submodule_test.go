package local

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
)

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
