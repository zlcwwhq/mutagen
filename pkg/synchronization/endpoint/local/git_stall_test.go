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
	script := filepath.Join(base, "transport.sh")
	contents := "#!/bin/sh\n" +
		"echo attempt >> " + transport.attempts + "\n" +
		"echo $$ > " + transport.process + "\n" +
		"exec sleep 600\n"
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal("unable to create transport:", err)
	}
	transport.url = "ext::" + script
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

// verifyExited verifies that the most recent transport process has exited,
// which isn't immediate, since it depends on its being reaped by its parent.
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
// an endpoint.
func useShortStallDurations(e *endpoint, timeout, backoff time.Duration) *gitProvider {
	provider := e.gitProvider.(*gitProvider)
	provider.stallTimeout = timeout
	provider.stallBackoff = backoff
	return provider
}

// TestEndpointGitModeCloneStallBackoff tests that a clone that shows no
// progress is stopped without any effect on existing content, that its URL is
// avoided for a while (without any delay), and that it's tried again after
// that.
func TestEndpointGitModeCloneStallBackoff(t *testing.T) {
	transport := newHangingTransport(t)
	root := newGitRoot(t, map[string]string{"sub/file": "precious"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	provider := useShortStallDurations(e, 250*time.Millisecond, 3*time.Second)
	old := entryAt(scanEndpoint(t, e, true), "sub")
	target := &core.Entry{Kind: core.EntryKind_GitLink, Digest: make([]byte, 20), Url: transport.url}
	change := &core.Change{Path: "sub", Old: old, New: target}
	ctx := context.Background()

	// verifyUnchanged verifies that the transition had no effect.
	verifyUnchanged := func(results []*core.Entry) {
		t.Helper()
		if results[0] != old {
			t.Error("incorrect result:", results[0])
		}
		if contents, err := os.ReadFile(filepath.Join(root, "sub", "file")); err != nil || string(contents) != "precious" {
			t.Errorf("content was lost: %q, %v", contents, err)
		}
	}

	// The first attempt stalls, which is the end of the transport and of the
	// clone. The transport would otherwise take much longer than the limit used
	// here.
	start := time.Now()
	results, problems := transitionEndpoint(t, ctx, e, change)
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("clone stopped before stall timeout: %s", elapsed)
	} else if elapsed > time.Minute {
		t.Errorf("clone stopped long after stall timeout: %s", elapsed)
	}
	verifyUnchanged(results)
	verifySingleProblem(t, problems, "sub", "no progress", transport.url)
	if count := transport.count(t); count != 1 {
		t.Errorf("incorrect number of attempts: %d", count)
	}
	transport.verifyExited(t)

	// While the URL is being avoided, attempts don't reach it. The problem says
	// what's happening.
	results, problems = transitionEndpoint(t, ctx, e, change)
	verifyUnchanged(results)
	verifySingleProblem(t, problems, "sub", "not contacting", transport.url, "tried again")
	if count := transport.count(t); count != 1 {
		t.Errorf("URL was contacted while being avoided: %d", count)
	}

	// Once the duration passes, it's tried again.
	for provider.backoffRemaining(transport.url) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	results, problems = transitionEndpoint(t, ctx, e, change)
	verifyUnchanged(results)
	verifySingleProblem(t, problems, "sub", "no progress")
	if count := transport.count(t); count != 2 {
		t.Errorf("URL not contacted after backoff: %d", count)
	}
}

// TestEndpointGitModeFetchStallBackoff tests stall detection for a fetch by an
// existing submodule, whose origin is the one that stalls, which doesn't
// prevent the use of another URL, and that nothing else about the origin is
// affected.
func TestEndpointGitModeFetchStallBackoff(t *testing.T) {
	transport := newHangingTransport(t)
	upstream := newGitUpstream(t, "upstream")
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext:file")
	root := newGitRoot(t, nil)
	directory := filepath.Join(root, "sub")
	if _, err := runGit(root, "clone", "--quiet", upstream.url, directory); err != nil {
		t.Fatal("unable to clone:", err)
	}
	mustGit(t, directory, "config", "remote.origin.url", transport.url)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	provider := useShortStallDurations(e, 250*time.Millisecond, time.Minute)
	old := entryAt(scanEndpoint(t, e, true), "sub")
	if old == nil || old.Kind != core.EntryKind_GitLink {
		t.Fatalf("incorrect entry: %v", old)
	}
	ctx := context.Background()

	// A commit that's available from the other URL can be obtained, even though
	// the origin stalls.
	upstream.commit("second", "second")
	upstream.push()
	second := upstream.entry(upstream.head)
	results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: old, New: second})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if results[0] != second {
		t.Error("incorrect result:", results[0])
	}
	verifyCheckout(t, directory, upstream.head)
	if count := transport.count(t); count != 1 {
		t.Errorf("incorrect number of attempts: %d", count)
	}
	transport.verifyExited(t)
	if provider.backoffRemaining(transport.url) <= 0 {
		t.Error("URL not avoided following stall")
	}
	if provider.backoffRemaining(upstream.url) > 0 {
		t.Error("URL that didn't stall is being avoided")
	}

	// The next fetch skips the origin.
	upstream.commit("third", "third")
	upstream.push()
	third := upstream.entry(upstream.head)
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: second, New: third})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if results[0] != third {
		t.Error("incorrect result:", results[0])
	}
	if count := transport.count(t); count != 1 {
		t.Errorf("URL was contacted while being avoided: %d", count)
	}

	// If there's nothing else to try, then the message says so.
	unavailable := upstream.commit("fourth", "fourth")
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: third, New: upstream.entry(unavailable)})
	if results[0] != third {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "not contacting "+transport.url, "isn't available from "+upstream.url)
}

// TestEndpointGitModeNoBackoffWithoutStall tests that failures other than
// stalls don't result in URLs being avoided, because a push should take effect
// at the next synchronization.
func TestEndpointGitModeNoBackoffWithoutStall(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	unpushed := upstream.commit("second", "second")
	root := newGitRoot(t, map[string]string{"kept": "kept"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	provider := useShortStallDurations(e, time.Minute, time.Hour)
	target := upstream.entry(unpushed)
	ctx := context.Background()

	// A commit that's not there is a failure, but not one that gets remembered.
	results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: target})
	if results[0] != nil {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "isn't available from "+upstream.url)
	if provider.backoffRemaining(upstream.url) > 0 {
		t.Error("URL is being avoided after a commit wasn't found")
	}

	// A repository that isn't there yet is the same.
	url := fileURL(filepath.Join(filepath.Dir(upstream.bare), "missing.git"))
	missing := &core.Entry{Kind: core.EntryKind_GitLink, Digest: target.Digest, Url: url}
	_, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: missing})
	verifySingleProblem(t, problems, "sub", "unable to clone "+url)
	if provider.backoffRemaining(url) > 0 {
		t.Error("URL is being avoided after a clone failed")
	}

	// A push takes effect at the next attempt.
	upstream.push()
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: target})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	} else if results[0] != target {
		t.Error("incorrect result:", results[0])
	}
	verifyCheckout(t, filepath.Join(root, "sub"), unpushed)
}

// TestEndpointGitModeCloneProgressIsNotStall tests that output keeps a command
// from being treated as stalled, so that a large clone can take as long as it
// needs to, by running a transport that takes much longer than the stall
// timeout to make a clone fail, but that always reports what it's doing.
func TestEndpointGitModeCloneProgressIsNotStall(t *testing.T) {
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
	provider := useShortStallDurations(e, 2500*time.Millisecond, time.Hour)
	target := &core.Entry{Kind: core.EntryKind_GitLink, Digest: make([]byte, 20), Url: url}

	// The clone fails, because this isn't really a transport, but not because
	// of the stall timeout, which is shorter than the transport's lifetime.
	start := time.Now()
	results, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub", New: target})
	if elapsed := time.Since(start); elapsed < 3*time.Second {
		t.Errorf("transport stopped early: %s", elapsed)
	}
	if results[0] != nil {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "unable to clone")
	if strings.Contains(problems[0].Error, "no progress") {
		t.Error("command with progress treated as stalled:", problems[0].Error)
	}
	if provider.backoffRemaining(url) > 0 {
		t.Error("URL is being avoided without a stall")
	}
}

// TestEndpointGitModeCloneCancellation tests that cancellation during a clone
// stops it, including its transport, removes the temporary clone, and leaves
// existing content in place. The clone would not otherwise end.
func TestEndpointGitModeCloneCancellation(t *testing.T) {
	transport := newHangingTransport(t)
	root := newGitRoot(t, map[string]string{"sub/file": "precious"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	old := entryAt(scanEndpoint(t, e, true), "sub")
	target := &core.Entry{Kind: core.EntryKind_GitLink, Digest: make([]byte, 20), Url: transport.url}

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
		results, problems, _, err := e.Transition(ctx, []*core.Change{{Path: "sub", Old: old, New: target}})
		done <- result{results, problems, err}
	}()

	// Wait until the clone has reached the transport, and verify that it's
	// happening in a temporary location next to the existing content.
	deadline := time.Now().Add(30 * time.Second)
	for transport.count(t) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for transport")
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	verifySingleProblem(t, outcome.problems, "sub", "cancelled")
	verifyNoTemporaryContent(t, root)
	if contents, err := os.ReadFile(filepath.Join(root, "sub", "file")); err != nil || string(contents) != "precious" {
		t.Errorf("content was lost: %q, %v", contents, err)
	}
	transport.verifyExited(t)

	// Cancellation isn't a stall.
	if e.gitProvider.(*gitProvider).backoffRemaining(transport.url) > 0 {
		t.Error("URL is being avoided after cancellation")
	}
}
