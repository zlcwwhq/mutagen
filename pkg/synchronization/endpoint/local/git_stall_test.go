//go:build !windows

package local

import (
	"context"
	"fmt"
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
	transport.setCommand(t, "sleep 600")
	transport.url = "ext::" + transport.script
	return transport
}

// setCommand sets the command that the transport runs after recording an
// attempt.
func (h *hangingTransport) setCommand(t *testing.T, command string) {
	t.Helper()
	contents := "#!/bin/sh\n" +
		"echo attempt >> " + h.attempts + "\n" +
		"echo $$ > " + h.process + "\n" +
		"exec " + command + "\n"
	if err := os.WriteFile(h.script, []byte(contents), 0700); err != nil {
		t.Fatal("unable to create transport:", err)
	}
}

// recover makes the transport serve a bare repository.
func (h *hangingTransport) recover(t *testing.T, bare string) {
	t.Helper()
	h.setCommand(t, "git upload-pack "+bare)
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

// useLargeRepositoryTemplate gives the repositories that Git creates from now on
// a large number of files, so that a clone that's stopped takes a while to remove
// what it has created, rather than the moment that it would otherwise take.
func useLargeRepositoryTemplate(t *testing.T) {
	t.Helper()
	template := realTemporaryDirectory(t)
	files := make(map[string]string, 1000)
	for f := 0; f < 1000; f++ {
		files[fmt.Sprintf("filler/file-%d", f)] = "filler"
	}
	writeTree(t, template, files)
	t.Setenv("GIT_TEMPLATE_DIR", template)
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

// TestEndpointGitModeNestedStall tests that a nested submodule that shows no
// progress stops the update of the others, whose URLs are unknown and thus
// avoided as a group, that the group is avoided for later updates, that the
// submodule that contains them still ends up in place, and that the stall leaves
// nothing behind. A clone that's killed would leave a .git file with an absolute
// path into the temporary clone, which stops working once that's moved into
// place and which breaks every later checkout, and a repository without commits,
// which Git won't clone again. The same goes for a clone that's still removing
// what it created when the temporary clone is moved, since it does so using
// absolute paths and doesn't share the output of the command that started it, so
// this makes that take a while.
func TestEndpointGitModeNestedStall(t *testing.T) {
	// Create an upstream that records two nested submodules, the first of which
	// never responds. Git doesn't let us add that one in the usual way, because
	// that would have to contact it.
	nested := newGitUpstream(t, "nested")
	upstream := newGitUpstream(t, "upstream")
	transport := newHangingTransport(t)
	for path, url := range map[string]string{"a-hanging": transport.url, "b-sibling": nested.url} {
		mustGit(t, upstream.work, "config", "--file", ".gitmodules", "submodule."+path+".path", path)
		mustGit(t, upstream.work, "config", "--file", ".gitmodules", "submodule."+path+".url", url)
		mustGit(t, upstream.work, "update-index", "--add", "--cacheinfo", "160000,"+nested.head+","+path)
	}
	mustGit(t, upstream.work, "add", "--", ".gitmodules")
	upstream.commitAll("add submodules")

	// The clone goes ahead, but the update stops at the first nested submodule.
	// The stall timeout has to leave room for the large template to be copied.
	useLargeRepositoryTemplate(t)
	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	provider := useShortStallDurations(e, 2*time.Second, time.Hour)
	ctx := context.Background()
	first := upstream.entry(upstream.head)
	results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: first})
	if results[0] != first {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "unable to update nested submodules", "a-hanging:", "no progress")
	transport.verifyExited(t)
	verifyAbsent(t, root, "sub/b-sibling/file")
	if count := transport.count(t); count != 1 {
		t.Errorf("incorrect number of attempts: %d", count)
	}

	// Nothing is left of the clone that stalled.
	directory := filepath.Join(root, "sub")
	if entries, err := os.ReadDir(filepath.Join(directory, "a-hanging")); err != nil || len(entries) != 0 {
		t.Errorf("stalled clone left content behind: %v, %v", entries, err)
	}
	verifyAbsent(t, root, "sub/.git/modules/a-hanging")

	// Trying again doesn't contact the group, and doesn't take any time.
	start := time.Now()
	err := provider.UpdateNested(ctx, directory, first.Url)
	if err == nil || !strings.Contains(err.Error(), "a-hanging:") || !strings.Contains(err.Error(), "not contacting") {
		t.Error("incorrect error during backoff:", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Error("update was delayed:", elapsed)
	}
	if count := transport.count(t); count != 1 {
		t.Errorf("URL was contacted during backoff: %d", count)
	}

	// The submodule's next commit can be checked out in the meantime, which
	// wouldn't be possible if the stalled clone had broken the repository.
	second := upstream.entry(upstream.commit("second", "second"))
	upstream.push()
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: first, New: second})
	if results[0] != second {
		t.Error("incorrect result:", results[0])
	}
	verifySingleProblem(t, problems, "sub", "unable to update nested submodules", "a-hanging:", "not contacting")
	verifyCheckoutCommit(t, directory, upstream.head)

	// Once the URL works and the backoff has ended, the next commit brings all
	// of the nested submodules in, since the clone that stalled isn't in the
	// way. Nothing should stall now, even on a busy system.
	transport.recover(t, nested.bare)
	provider.stallTimeout = time.Minute
	provider.backoffsLock.Lock()
	clear(provider.backoffs)
	provider.backoffsLock.Unlock()
	third := upstream.entry(upstream.commit("third", "third"))
	upstream.push()
	results, problems = transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: second, New: third})
	if results[0] != third {
		t.Error("incorrect result:", results[0])
	} else if len(problems) != 0 {
		t.Error("unexpected problems:", problems)
	}
	verifyCheckoutCommit(t, filepath.Join(directory, "a-hanging"), nested.head)
	verifyCheckoutCommit(t, filepath.Join(directory, "b-sibling"), nested.head)
}

// verifyCheckoutCommit verifies that a repository has a commit checked out.
func verifyCheckoutCommit(t *testing.T, directory, commit string) {
	t.Helper()
	if head := mustGit(t, directory, "rev-parse", "HEAD"); head != commit {
		t.Errorf("incorrect commit in %s: %s != %s", directory, head, commit)
	}
}

// TestEndpointGitModeNestedSlowCheckout tests that a nested submodule's checkout
// that takes longer than the stall timeout isn't taken for a stall, as long as
// it's making progress. Stopping it would leave a partial working tree in the
// way of checking the commit out again.
func TestEndpointGitModeNestedSlowCheckout(t *testing.T) {
	// Create a nested submodule whose files will be slow to check out.
	nested := newGitUpstream(t, "nested")
	const count = 30
	files := map[string]string{".gitattributes": "file-* filter=slow\n"}
	for f := 0; f < count; f++ {
		files[fmt.Sprintf("file-%d", f)] = "content"
	}
	writeTree(t, nested.work, files)
	mustGit(t, nested.work, "add", "--all")
	nested.commitAll("add files")
	upstream := newGitUpstream(t, "upstream")
	upstream.addSubmodule(nested, "nested", "")

	// Have each file take a while, using configuration that a real repository
	// would have to get from elsewhere. Only the total exceeds the timeout.
	configuration := filepath.Join(realTemporaryDirectory(t), "configuration")
	if err := os.WriteFile(configuration, []byte("[filter \"slow\"]\n\tsmudge = \"sleep 0.1; cat\"\n"), 0600); err != nil {
		t.Fatal("unable to write configuration:", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", configuration)

	// Git waits two seconds before it first reports progress. That's nothing
	// compared to the default stall timeout, but it's longer than this test's.
	t.Setenv("GIT_PROGRESS_DELAY", "0")

	// Clone the submodule.
	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	useShortStallDurations(e, time.Second, time.Hour)
	_, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub", New: upstream.entry(upstream.head)})
	if len(problems) != 0 {
		t.Fatal("unexpected problems:", problems)
	}
	verifyCheckoutCommit(t, filepath.Join(root, "sub", "nested"), nested.head)
	verifyContents(t, root, fmt.Sprintf("sub/nested/file-%d", count-1), "content")
}
