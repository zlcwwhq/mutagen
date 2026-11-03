//go:build !windows

package local

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
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
