//go:build !windows

package local

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// hangingTransport is a remote that never responds, because it runs a command
// as its transport (using Git's ext transport) that only records that it has
// been run and that then waits. It behaves the way a connection to an
// unreachable host does, including that the transport is a separate process.
type hangingTransport struct {
	// url is the URL.
	url string
	// script is the path to the transport's script.
	script string
	// attempts is the path to the file that contains a line for each attempt to
	// contact the remote.
	attempts string
	// process is the path to the file that contains the most recent attempt's
	// process identifier.
	process string
}

// newHangingTransport creates a remote that never responds and enables its use.
func newHangingTransport(t *testing.T) *hangingTransport {
	t.Helper()
	isolateGit(t)
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext:file")
	base := realTemporaryDirectory(t)
	transport := &hangingTransport{
		attempts: filepath.Join(base, "attempts"),
		process:  filepath.Join(base, "process"),
	}
	transport.script = filepath.Join(base, "transport.sh")
	// The process identifier is recorded before the attempt, since waiting
	// for an attempt is what tells tests that they can stop the transport.
	contents := "#!/bin/sh\n" +
		"echo $$ > " + transport.process + "\n" +
		"echo attempt >> " + transport.attempts + "\n" +
		"exec sleep 600\n"
	if err := os.WriteFile(transport.script, []byte(contents), 0700); err != nil {
		t.Fatal("unable to create transport:", err)
	}
	transport.url = "ext::" + transport.script
	return transport
}

// count returns the number of attempts to contact the remote.
func (h *hangingTransport) count(t *testing.T) int {
	t.Helper()
	contents, err := os.ReadFile(h.attempts)
	if os.IsNotExist(err) {
		return 0
	} else if err != nil {
		t.Fatal("unable to read attempts:", err)
	}
	return strings.Count(string(contents), "\n")
}

// waitForAttempts waits until the remote has been contacted the specified
// number of times.
func (h *hangingTransport) waitForAttempts(t *testing.T, count int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for h.count(t) < count {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for transport")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// verifyExited verifies that the most recent transport process has exited,
// which isn't immediate, since it depends on its being reaped by its parent.
// The process is only killed, by its identifier, if it's still running at the
// deadline.
func (h *hangingTransport) verifyExited(t *testing.T) {
	t.Helper()
	contents, err := os.ReadFile(h.process)
	if err != nil {
		t.Fatal("unable to read process identifier:", err)
	}
	process, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil {
		t.Fatal("invalid process identifier:", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(process, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			t.Error("transport process is still running")
			syscall.Kill(process, syscall.SIGKILL)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// gitTransportStallTimeout is the stall timeout for tests that count attempts
// to contact a transport. Git has to get as far as running the transport within
// this time, since an attempt that's stopped earlier isn't recorded, and that
// takes a while on a busy system.
const gitTransportStallTimeout = 2 * time.Second

// useShortStallDurations makes stall detection use the specified durations for
// an endpoint. Git waits two seconds before it first reports progress, which is
// nothing compared to the default stall timeout, but which would be a stall for
// the timeouts used here, so it's told to report it right away.
func useShortStallDurations(t *testing.T, e *endpoint, timeout, backoff time.Duration) *gitProvider {
	t.Helper()
	t.Setenv("GIT_PROGRESS_DELAY", "0")
	provider := e.gitProvider
	provider.stallTimeout = timeout
	provider.stallBackoff = backoff
	return provider
}

// TestEndpointGitModePopulateStallBackoff tests that a clone that shows no
// progress is stopped without any effect on existing content, that it's
// throttled like any other failure, that its URL is then avoided for a while
// (without any delay), and that it's tried again after that.
func TestEndpointGitModePopulateStallBackoff(t *testing.T) {
	transport := newHangingTransport(t)
	root := newGitRoot(t, map[string]string{"sub/file": "precious"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	provider := useShortStallDurations(t, e, gitTransportStallTimeout, 3*time.Second)
	old := entryAt(scanEndpoint(t, e, true), "sub")
	link := &core.Entry{Kind: core.EntryKind_GitLink, Digest: make([]byte, 20), Url: transport.url}
	change := &core.Change{Path: "sub", Old: old, New: submodule(link, nil)}
	key := gitRetryKey{url: transport.url, commit: strings.Repeat("00", 20)}
	ctx := context.Background()

	// verifyUnchanged verifies that the transition had no effect.
	verifyUnchanged := func(results []*core.Entry) {
		t.Helper()
		if results[0] != old {
			t.Error("incorrect result:", results[0])
		}
		verifyContents(t, root, "sub/file", "precious")
		verifyAbsent(t, root, "sub/.git")
	}

	// The first attempt stalls, which is the end of the transport and of the
	// clone. The transport would otherwise take much longer than the limit used
	// here.
	start := time.Now()
	results, problems := transitionEndpoint(t, ctx, e, change)
	if elapsed := time.Since(start); elapsed < gitTransportStallTimeout {
		t.Errorf("clone stopped before stall timeout: %s", elapsed)
	} else if elapsed > time.Minute {
		t.Errorf("clone stopped long after stall timeout: %s", elapsed)
	}
	verifyUnchanged(results)
	verifySingleFailure(t, problems, "sub", "no progress", transport.url)
	if count := transport.count(t); count != 1 {
		t.Errorf("incorrect number of attempts: %d", count)
	}
	transport.verifyExited(t)
	if provider.backoffRemaining(transport.url) <= 0 {
		t.Error("URL not avoided following stall")
	}

	// The next attempt is throttled, and so it doesn't even get as far as the
	// backoff.
	results, problems = transitionEndpoint(t, ctx, e, change)
	verifyUnchanged(results)
	verifySingleFailure(t, problems, "sub", "no progress", "tried again in")

	// Once the throttling has ended, the backoff still keeps attempts from
	// reaching the URL, and the problem says what's happening.
	provider.cloneRetries.expire(key)
	results, problems = transitionEndpoint(t, ctx, e, change)
	verifyUnchanged(results)
	verifySingleFailure(t, problems, "sub", "not contacting", transport.url, "tried again")
	if count := transport.count(t); count != 1 {
		t.Errorf("URL was contacted while being avoided: %d", count)
	}

	// Once both have ended, it's tried again.
	for provider.backoffRemaining(transport.url) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	provider.cloneRetries.expire(key)
	results, problems = transitionEndpoint(t, ctx, e, change)
	verifyUnchanged(results)
	verifySingleFailure(t, problems, "sub", "no progress")
	if count := transport.count(t); count != 2 {
		t.Errorf("URL not contacted after backoff: %d", count)
	}
	transport.verifyExited(t)
}

// TestEndpointGitModePopulateProgressIsNotStall tests that output keeps a
// command from being treated as stalled, so that a large clone can take as long
// as it needs to, by running a transport that takes much longer than the stall
// timeout to make a clone fail, but that always reports what it's doing.
func TestEndpointGitModePopulateProgressIsNotStall(t *testing.T) {
	isolateGit(t)
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext:file")
	base := realTemporaryDirectory(t)
	script := filepath.Join(base, "transport.sh")
	contents := "#!/bin/sh\n" +
		"for i in 1 2 3 4; do echo progress >&2; sleep 1; done\n"
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal("unable to create transport:", err)
	}
	url := "ext::" + script
	root := newGitRoot(t, map[string]string{"kept": "kept"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	provider := useShortStallDurations(t, e, 2500*time.Millisecond, time.Hour)
	link := &core.Entry{Kind: core.EntryKind_GitLink, Digest: make([]byte, 20), Url: url}

	// The clone fails, because this isn't really a transport, but not because
	// of the stall timeout, which is shorter than the transport's lifetime.
	start := time.Now()
	results, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub", New: submodule(link, nil)})
	if elapsed := time.Since(start); elapsed < 3*time.Second {
		t.Errorf("transport stopped early: %s", elapsed)
	}
	if results[0] != nil {
		t.Error("incorrect result:", results[0])
	}
	verifySingleFailure(t, problems, "sub", "unable to clone")
	if strings.Contains(problems[0].Error, "no progress") {
		t.Error("command with progress treated as stalled:", problems[0].Error)
	}
	if provider.backoffRemaining(url) > 0 {
		t.Error("URL is being avoided without a stall")
	}
	verifyAbsent(t, root, "sub")
}

// TestEndpointGitModePopulateCancellation tests that cancellation during a
// clone stops it, including its transport, removes the temporary clone, and
// leaves existing content in place. The clone would not otherwise end.
// Cancellation isn't a failure of the clone, so it's neither throttled nor a
// reason to avoid the URL.
func TestEndpointGitModePopulateCancellation(t *testing.T) {
	transport := newHangingTransport(t)
	root := newGitRoot(t, map[string]string{"sub/file": "precious"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	old := entryAt(scanEndpoint(t, e, true), "sub")
	link := &core.Entry{Kind: core.EntryKind_GitLink, Digest: make([]byte, 20), Url: transport.url}

	// Start the transition.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		results  []*core.Entry
		problems []*core.Problem
		err      error
	}
	done := make(chan result, 1)
	go func() {
		results, problems, _, err := e.Transition(ctx, []*core.Change{{Path: "sub", Old: old, New: submodule(link, nil)}})
		done <- result{results, problems, err}
	}()

	// Wait until the clone has reached the transport, and verify that it's
	// happening in a temporary location next to the existing content.
	transport.waitForAttempts(t, 1)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal("unable to read root:", err)
	}
	var temporary bool
	for _, entry := range entries {
		temporary = temporary || strings.HasPrefix(entry.Name(), filesystem.TemporaryNamePrefix)
	}
	if !temporary {
		t.Error("no temporary directory for clone:", entries)
	}

	// Cancel it.
	cancel()
	var outcome result
	select {
	case outcome = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for clone to stop")
	}
	if outcome.err != nil {
		t.Fatal("transition failed:", outcome.err)
	}

	// Verify the results.
	if outcome.results[0] != old {
		t.Error("incorrect result:", outcome.results[0])
	}
	verifySingleFailure(t, outcome.problems, "sub", "cancelled")
	verifyNoTemporaryContent(t, root)
	verifyContents(t, root, "sub/file", "precious")
	transport.verifyExited(t)
	if e.gitProvider.backoffRemaining(transport.url) > 0 {
		t.Error("URL is being avoided after cancellation")
	}
	if state := e.gitProvider.cloneRetries.state(gitRetryKey{url: transport.url, commit: strings.Repeat("00", 20)}); state != nil {
		t.Errorf("cancellation recorded as failure: %+v", state)
	}
}

// TestEndpointGitModeFetchStall tests that a background fetch whose origin
// stalls doesn't hold up transitions, that the origin is then avoided while
// the URL that the other endpoint has is used instead, and that the session is
// woken once the commit has been fetched.
func TestEndpointGitModeFetchStall(t *testing.T) {
	// Create a submodule whose origin never responds, and a commit that's
	// available from the URL that the other endpoint has.
	transport := newHangingTransport(t)
	upstream := newGitUpstream(t, "upstream")
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext:file")
	clone := newSubmoduleClone(t, upstream)
	mustGit(t, clone.directory, "config", "remote.origin.url", transport.url)
	second := upstream.commit("second", "second")
	upstream.push()
	const stallTimeout = 5 * time.Second
	e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
	provider := useShortStallDurations(t, e, stallTimeout, time.Hour)
	drainPollSignal(e)
	target := upstream.entry(second)

	// The first attempt to follow the commit requests the fetch and returns.
	old, result, problems := follow(t, e, target, nil)
	if result != old {
		t.Error("incorrect result:", result)
	}
	verifySingleWarning(t, problems, "sub/.git", "being fetched")

	// While the fetch is stuck on the origin, transitions still complete
	// promptly.
	transport.waitForAttempts(t, 1)
	start := time.Now()
	_, result, problems = follow(t, e, target, nil)
	if elapsed := time.Since(start); elapsed >= stallTimeout {
		t.Errorf("transition held up by fetch: %s", elapsed)
	}
	if result.Equal(target, true) {
		t.Error("commit fetched despite stall")
	}
	verifySingleWarning(t, problems, "sub/.git", "being fetched")

	// The fetch stops once it's stalled, after which the other URL is used,
	// which succeeds and wakes the session, and the origin is avoided.
	waitForPollSignal(t, e)
	transport.verifyExited(t)
	if provider.backoffRemaining(transport.url) <= 0 {
		t.Error("URL not avoided following stall")
	} else if provider.backoffRemaining(upstream.url) > 0 {
		t.Error("URL that didn't stall is being avoided")
	}
	_, result, problems = follow(t, e, target, nil)
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if result != target {
		t.Error("incorrect result:", result)
	}
	verifyDetachedAt(t, clone.directory, second)

	// The next fetch skips the origin entirely.
	third := upstream.commit("third", "third")
	upstream.push()
	start = time.Now()
	follow(t, e, upstream.entry(third), nil)
	waitForPollSignal(t, e)
	if elapsed := time.Since(start); elapsed >= stallTimeout {
		t.Errorf("fetch waited for origin that's being avoided: %s", elapsed)
	}
	if count := transport.count(t); count != 1 {
		t.Errorf("URL was contacted while being avoided: %d", count)
	}
	_, _, problems = follow(t, e, upstream.entry(third), nil)
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	}
	verifyDetachedAt(t, clone.directory, third)
}

// TestEndpointGitModeFollowPartialClone tests that following a commit that a
// partial clone lacks doesn't make Git fetch it from within the transition,
// which Git otherwise does on demand from the promisor remote, without the
// stall detection and throttling of the background fetch, holding up the
// synchronization cycle. The commit is fetched in the background instead,
// after which HEAD follows.
func TestEndpointGitModeFollowPartialClone(t *testing.T) {
	// Create a partial clone, whose origin (its promisor remote) never
	// responds, and a commit that's available from the URL that the other
	// endpoint has.
	transport := newHangingTransport(t)
	upstream := newGitUpstream(t, "upstream")
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext:file")
	mustGit(t, upstream.bare, "--git-dir=.", "config", "uploadpack.allowFilter", "true")
	root := newGitRoot(t, nil)
	directory := filepath.Join(root, "sub")
	mustGit(t, root, "clone", "--quiet", "--filter=blob:none", "--", upstream.url, directory)
	mustGit(t, directory, "config", "remote.origin.url", transport.url)
	second := upstream.commit("second", "second")
	upstream.push()
	const stallTimeout = 5 * time.Second
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	useShortStallDurations(t, e, stallTimeout, time.Hour)
	drainPollSignal(e)
	target := upstream.entry(second)

	// The first attempt to follow the commit requests the fetch and returns
	// promptly. The transition is bounded in case Git fetches lazily.
	old := gitLinkAt(t, scanEndpoint(t, e, true), "sub")
	ctx, cancel := context.WithTimeout(context.Background(), 2*stallTimeout)
	defer cancel()
	start := time.Now()
	results, problems, _, err := e.Transition(ctx, []*core.Change{{Path: "sub/.git", Old: old, New: target}})
	if err != nil {
		t.Fatal("transition failed:", err)
	} else if elapsed := time.Since(start); elapsed >= stallTimeout {
		t.Errorf("transition held up by the network: %s", elapsed)
	} else if results[0] != old {
		t.Error("incorrect result:", results[0])
	}
	verifySingleWarning(t, problems, "sub/.git", "being fetched")

	// The background fetch stalls on the origin, which is thus contacted only
	// once, and then fetches the commit from the other URL, after which HEAD
	// follows.
	waitForPollSignal(t, e)
	transport.verifyExited(t)
	_, result, problems := follow(t, e, target, nil)
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if result != target {
		t.Error("incorrect result:", result)
	}
	verifyDetachedAt(t, directory, second)
	if count := transport.count(t); count != 1 {
		t.Errorf("incorrect number of attempts to contact the origin: %d", count)
	}
}

// TestEndpointGitModeFetchShutdown tests that shutting an endpoint down stops
// a background fetch, including its transport, without waiting for it to
// stall, and without trying anything else.
func TestEndpointGitModeFetchShutdown(t *testing.T) {
	transport := newHangingTransport(t)
	upstream := newGitUpstream(t, "upstream")
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext:file")
	clone := newSubmoduleClone(t, upstream)
	mustGit(t, clone.directory, "config", "remote.origin.url", transport.url)
	second := upstream.commit("second", "second")
	upstream.push()
	e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
	useShortStallDurations(t, e, time.Hour, time.Hour)
	_, _, problems := follow(t, e, upstream.entry(second), nil)
	verifySingleWarning(t, problems, "sub/.git", "being fetched")
	transport.waitForAttempts(t, 1)

	// Shut the endpoint down.
	done := make(chan struct{})
	go func() {
		e.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown held up by fetch")
	}
	transport.verifyExited(t)
	if _, err := runGit(clone.directory, "cat-file", "-e", second+"^{commit}"); err == nil {
		t.Error("other URL used after shutdown")
	}
}
