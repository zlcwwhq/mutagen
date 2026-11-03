package integration

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/filesystem/watching"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// gitSubmoduleFiles are the files of the first commit of a submodule's
// upstream repository in most tests. The submodule's own rules ignore log
// files, which the rules of the endpoints' roots don't.
var gitSubmoduleFiles = map[string]string{
	".gitignore": "*.log\n",
	"file":       "first",
	"other":      "other",
	"deleted":    "deleted",
}

// gitModeUpstream is a bare repository for a submodule, which is reachable
// using a file URL, along with a working repository from which its commits are
// pushed.
type gitModeUpstream struct {
	// url is the repository's URL.
	url string
	// path is the repository's path.
	path string
	// work is the working repository from which commits are pushed. It doesn't
	// have an origin.
	work string
	// first is the commit that the repository's main branch starts with, which
	// is also its default branch.
	first string
}

// newGitModeUpstream creates a bare repository for a submodule whose main
// branch has a single commit with the specified files.
func newGitModeUpstream(t *testing.T, files map[string]string) *gitModeUpstream {
	t.Helper()
	base, err := os.MkdirTemp("", "mgu-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	upstream := &gitModeUpstream{path: filepath.Join(base, "upstream.git"), work: filepath.Join(base, "work")}
	upstream.url = "file://" + filepath.ToSlash(upstream.path)
	if !strings.HasPrefix(filepath.ToSlash(upstream.path), "/") {
		upstream.url = "file:///" + filepath.ToSlash(upstream.path)
	}
	for _, directory := range []string{upstream.path, upstream.work} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	gitModeRun(t, upstream.path, "init", "--quiet", "--bare", "--initial-branch=main")
	gitModeRun(t, upstream.work, "init", "--quiet", "--initial-branch=main")
	upstream.first = gitModeCommitFiles(t, upstream.work, files, "first")
	upstream.push(t)
	return upstream
}

// push pushes the commit that the upstream's working repository has checked
// out to the upstream's main branch.
func (u *gitModeUpstream) push(t *testing.T) {
	t.Helper()
	gitModeRun(t, u.work, "push", "--quiet", u.url, "HEAD:refs/heads/main")
}

// gitModeCommitFiles creates a commit in a repository with files having the
// specified names, which may include directories, and contents, returning its
// identifier.
func gitModeCommitFiles(t *testing.T, directory string, files map[string]string, message string) string {
	t.Helper()
	names := []string{"add", "--"}
	for name, contents := range files {
		path := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		} else if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	gitModeRun(t, directory, names...)
	gitModeRun(t, directory, "commit", "--quiet", "--no-gpg-sign", "-m", message)
	return gitModeHead(t, directory)
}

// gitModeCommit creates a commit in a repository with a file having the
// specified name and contents, returning its identifier.
func gitModeCommit(t *testing.T, directory, name, contents string) string {
	t.Helper()
	return gitModeCommitFiles(t, directory, map[string]string{name: contents}, "commit "+name)
}

// gitModePush pushes the commit that's checked out in a repository to a branch
// of its origin.
func gitModePush(t *testing.T, directory, branch string) {
	t.Helper()
	gitModeRun(t, directory, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
}

// gitModeHead returns the commit that's checked out in a repository.
func gitModeHead(t *testing.T, directory string) string {
	t.Helper()
	return gitModeRun(t, directory, "rev-parse", "HEAD")
}

// gitModeHeadIs indicates whether or not a repository, which a session may be
// operating on, has the specified commit checked out. A repository that
// doesn't exist (yet) doesn't.
func gitModeHeadIs(directory, commit string) bool {
	head, err := gitModeTry(directory, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil && head == commit
}

// gitModeStatus returns the status of a repository's working tree, with a line
// for each file that differs from the index or that isn't tracked. It doesn't
// refresh the index, which a session could be updating.
func gitModeStatus(t *testing.T, directory string) string {
	t.Helper()
	output, err := gitModeOutput(directory, "--no-optional-locks", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		t.Fatalf("unable to determine status of %s: %v", directory, err)
	}
	return strings.TrimSuffix(output, "\n")
}

// waitForCondition waits for a condition to hold, without doing anything to
// make it hold, for at most the specified time, and returns whether it held,
// along with how long that took.
func waitForCondition(timeout time.Duration, condition func() bool) (bool, time.Duration) {
	start := time.Now()
	for {
		if condition() {
			return true, time.Since(start)
		} else if time.Since(start) > timeout {
			return false, time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// gitSubmoduleProblem returns a problem at a path, or at a path within it (such
// as a submodule's Git entry), whose message contains the specified text,
// failing the test if there isn't one.
func gitSubmoduleProblem(t *testing.T, problems []*core.Problem, path string, contents ...string) *core.Problem {
	t.Helper()
	for _, problem := range problems {
		if problem.Path != path && !strings.HasPrefix(problem.Path, path+"/") {
			continue
		}
		matches := true
		for _, content := range contents {
			matches = matches && strings.Contains(problem.Error, content)
		}
		if matches {
			return problem
		}
	}
	t.Fatalf("no problem at or within %q containing %q: %v", path, contents, problems)
	return nil
}

// verifyNoProblems verifies that a session's state has neither conflicts nor
// problems of any kind, warnings included.
func verifyNoProblems(t *testing.T, state *synchronization.State) {
	t.Helper()
	if len(state.Conflicts) != 0 {
		t.Errorf("unexpected conflicts: %v", state.Conflicts)
	}
	for _, problems := range []struct {
		name     string
		problems []*core.Problem
	}{
		{"alpha scan", state.AlphaState.ScanProblems},
		{"beta scan", state.BetaState.ScanProblems},
		{"alpha transition", state.AlphaState.TransitionProblems},
		{"beta transition", state.BetaState.TransitionProblems},
	} {
		if len(problems.problems) != 0 {
			t.Errorf("unexpected %s problems: %v", problems.name, problems.problems)
		}
	}
}

// verifyNoTemporaryContent verifies that there's no temporary content, such as
// that of a clone, beneath a directory, other than in Git directories.
func verifyNoTemporaryContent(t *testing.T, directory string) {
	t.Helper()
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		} else if entry.Name() == ".git" {
			return filepath.SkipDir
		} else if strings.HasPrefix(entry.Name(), filesystem.TemporaryNamePrefix) {
			t.Error("temporary content left behind:", path)
		}
		return nil
	})
	if err != nil {
		t.Error("unable to search for temporary content:", err)
	}
}

// modificationTimes returns the modification times of files in a directory,
// specified by their slash-separated relative paths.
func modificationTimes(t *testing.T, directory string, names []string) map[string]time.Time {
	t.Helper()
	times := make(map[string]time.Time, len(names))
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(directory, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		times[name] = info.ModTime()
	}
	return times
}

// verifySettled verifies that a session has settled without anything to report:
// a further flush takes a single synchronization cycle, whose scans see what
// the previous cycles did, and there are neither conflicts nor problems.
func (s *gitModeSession) verifySettled() {
	s.t.Helper()
	before := s.state().SuccessfulCycles
	state := s.flushAllowingProblems()
	if state.SuccessfulCycles != before+1 {
		s.t.Errorf("session hasn't settled: %d cycles after %d", state.SuccessfulCycles, before)
	}
	verifyNoProblems(s.t, state)
}

// flushUntil flushes a session until a condition holds, which it has to do
// within the specified time, and returns the state after the last flush. It's
// for what happens in the background, such as the fetching of commits, which
// flushes don't wait for.
func (s *gitModeSession) flushUntil(timeout time.Duration, description string, condition func() bool) *synchronization.State {
	s.t.Helper()
	start := time.Now()
	for {
		state := s.flushAllowingProblems()
		if condition() {
			return state
		} else if time.Since(start) > timeout {
			s.t.Fatalf("%s didn't happen within %s: %v, %v, %v", description, timeout,
				state.AlphaState.TransitionProblems, state.BetaState.TransitionProblems, state.Conflicts)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// gitSubmoduleFixture is a session whose alpha is a superproject with a real
// submodule named sub, whose upstream repository is a bare repository, along
// with a file named top. Beta starts without either.
type gitSubmoduleFixture struct {
	*gitModeSession
	// upstream is the submodule's upstream repository.
	upstream *gitModeUpstream
	// leader is alpha's directory for the submodule.
	leader string
	// follower is beta's directory for the submodule.
	follower string
}

// newGitSubmoduleFixture creates a session with a submodule on alpha, whose
// upstream repository has the files in gitSubmoduleFiles. The options are
// those for the transport, in addition to those provided by the test. The
// prepare function, if any, performs further setup before the session starts.
func newGitSubmoduleFixture(t *testing.T, transport gitSubmoduleTransport, options gitModeOptions, prepare func(*gitSubmoduleFixture)) *gitSubmoduleFixture {
	t.Helper()
	return newGitSubmoduleFixtureWithUpstream(t, transport, newGitModeUpstream(t, gitSubmoduleFiles), options, prepare)
}

// newGitSubmoduleFixtureWithUpstream creates a session with a submodule on
// alpha, as newGitSubmoduleFixture does, with an existing upstream repository,
// whose main branch the submodule checks out.
func newGitSubmoduleFixtureWithUpstream(t *testing.T, transport gitSubmoduleTransport, upstream *gitModeUpstream, options gitModeOptions, prepare func(*gitSubmoduleFixture)) *gitSubmoduleFixture {
	t.Helper()
	f := &gitSubmoduleFixture{upstream: upstream}
	if transport.configure != nil {
		transport.configure(&options)
	}
	if options.alpha == nil {
		options.alpha = map[string]string{"top": "top"}
	}
	options.prepare = func(session *gitModeSession) {
		f.gitModeSession = session
		f.leader, f.follower = filepath.Join(session.alpha, "sub"), filepath.Join(session.beta, "sub")
		gitModeRun(t, session.alpha, "init", "--quiet", "--initial-branch=main")
		gitModeRun(t, session.alpha, "submodule", "add", "--quiet", upstream.url, "sub")
		gitModeRun(t, session.alpha, "add", "--all")
		gitModeRun(t, session.alpha, "commit", "--quiet", "--no-gpg-sign", "-m", "add submodule")
		if prepare != nil {
			prepare(f)
		}
	}
	newGitModeSession(t, options)
	return f
}

// verifyCheckout verifies that a directory has a repository of its own, rather
// than being part of a superproject's repository, with the specified commit
// checked out on a detached HEAD, as clones made by sessions do.
func (f *gitSubmoduleFixture) verifyCheckout(directory, commit string) {
	f.t.Helper()
	if head := gitModeHead(f.t, directory); head != commit {
		f.t.Errorf("incorrect commit in %s: %s != %s", directory, head, commit)
	}
	if branch := gitModeRun(f.t, directory, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
		f.t.Errorf("HEAD isn't detached in %s: %s", directory, branch)
	}
	if top := gitModeRun(f.t, directory, "rev-parse", "--show-toplevel"); filepath.Base(top) != filepath.Base(directory) {
		f.t.Errorf("incorrect top level directory for %s: %s", directory, top)
	}
	if info, err := os.Lstat(filepath.Join(directory, ".git")); err != nil {
		f.t.Errorf("unable to query .git in %s: %v", directory, err)
	} else if !info.IsDir() {
		f.t.Errorf(".git in %s isn't a directory", directory)
	}
}

// verifyAlphaAt verifies that the submodule has the specified commit checked
// out on alpha, where it's still part of the superproject.
func (f *gitSubmoduleFixture) verifyAlphaAt(commit string) {
	f.t.Helper()
	if head := gitModeHead(f.t, f.leader); head != commit {
		f.t.Errorf("incorrect commit on alpha: %s != %s", head, commit)
	}
	if info, err := os.Lstat(filepath.Join(f.leader, ".git")); err != nil {
		f.t.Error("unable to query submodule's .git on alpha:", err)
	} else if !info.Mode().IsRegular() {
		f.t.Error("submodule on alpha no longer has a .git file")
	}
}

// verifySameStatus verifies that the repositories in the submodule's (or a
// nested submodule's) directories, specified relative to the submodule, have
// the same status on both endpoints, which is the expected status.
func (f *gitSubmoduleFixture) verifySameStatus(path, expected string) {
	f.t.Helper()
	alpha := gitModeStatus(f.t, filepath.Join(f.leader, filepath.FromSlash(path)))
	beta := gitModeStatus(f.t, filepath.Join(f.follower, filepath.FromSlash(path)))
	if alpha != expected {
		f.t.Errorf("incorrect status on alpha for %q: %q != %q", path, alpha, expected)
	}
	if beta != alpha {
		f.t.Errorf("status on beta for %q differs from alpha's: %q != %q", path, beta, alpha)
	}
}

// verifyRemote runs a command through the remote command line, if the transport
// supports that, verifying that the command succeeds and that the specified
// text is reported along with it.
func (f *gitSubmoduleFixture) verifyRemote(transport gitSubmoduleTransport, reported ...string) {
	f.t.Helper()
	if !transport.commands {
		return
	}

	// Synchronization can finish before the separate command channel connects.
	f.waitForRemoteCommands()

	code, output, diagnostics := runRemote(f.t, f.alpha, "--timeout", "30s", "cat", "top")
	if code != 0 || output != "top" {
		f.t.Errorf("command failed: %d: %q: %s", code, output, diagnostics)
	}
	for _, content := range reported {
		if !strings.Contains(diagnostics, content) {
			f.t.Errorf("command's diagnostics don't contain %q: %s", content, diagnostics)
		}
	}
}

// gitSubmoduleTransport describes how scenarios for submodules reach beta.
type gitSubmoduleTransport struct {
	// configure sets the options that are necessary to use the transport.
	configure func(*gitModeOptions)
	// commands indicates whether or not sessions support remote commands, in
	// which case scenarios verify what those report, and that they still run.
	commands bool
}

// configureOptions returns options with those of the transport applied.
func (t gitSubmoduleTransport) configureOptions(options gitModeOptions) gitModeOptions {
	if t.configure != nil {
		t.configure(&options)
	}
	return options
}

// gitSubmoduleScenarios are the scenarios for submodules that are run with each
// transport.
var gitSubmoduleScenarios = []struct {
	name string
	run  func(*testing.T, gitSubmoduleTransport)
}{
	{"populate empty root", func(t *testing.T, transport gitSubmoduleTransport) {
		testGitSubmodulePopulate(t, transport, false)
	}},
	{"populate missing root", func(t *testing.T, transport gitSubmoduleTransport) {
		testGitSubmodulePopulate(t, transport, true)
	}},
	{"uninitialized superproject", testGitSubmoduleUninitialized},
	{"nested submodules", testGitSubmoduleNested},
	{"follow", testGitSubmoduleFollow},
	{"unpushed commit", testGitSubmoduleUnpushedCommit},
	{"diverged commits", testGitSubmoduleDiverged},
	{"diverged commits resolved for alpha", testGitSubmoduleDivergedResolved},
	{"attach", testGitSubmoduleAttach},
	{"attach with unpushed commit", testGitSubmoduleAttachUnpushed},
	{"removal from alpha", func(t *testing.T, transport gitSubmoduleTransport) {
		testGitSubmoduleRemoval(t, transport, false)
	}},
	{"removal from beta", func(t *testing.T, transport gitSubmoduleTransport) {
		testGitSubmoduleRemoval(t, transport, true)
	}},
	{"deletion resolves change", func(t *testing.T, transport gitSubmoduleTransport) {
		testGitSubmoduleDeletionResolves(t, transport, false)
	}},
	{"deletion resolves diverged commits", func(t *testing.T, transport gitSubmoduleTransport) {
		testGitSubmoduleDeletionResolves(t, transport, true)
	}},
	{"one way/submodule only on beta", testGitSubmoduleOnlyOnBeta},
	{"one way/Git entry only on beta", testGitSubmoduleGitEntryOnlyOnBeta},
	{"one way/changes on beta", testGitSubmoduleOneWayChangesOnBeta},
	{"dangling .git file", testGitSubmoduleDanglingGitfile},
}

// testGitSubmodulePopulate tests that an endpoint without a submodule gets a
// clone with the commit that the other endpoint has checked out, and that a
// single flush also provides the other endpoint's uncommitted changes, without
// any conflicts or problems, leaving both with the same working tree and status,
// except for what the submodule's own rules ignore.
func testGitSubmodulePopulate(t *testing.T, transport gitSubmoduleTransport, betaMissing bool) {
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{betaMissing: betaMissing}, func(f *gitSubmoduleFixture) {
		f.write(f.leader, map[string]string{"file": "modified", "untracked": "untracked", "debug.log": "ignored"})
		f.remove(f.leader, "deleted")
	})
	f.flush()

	// Verify the content on beta, which is an independent repository at the
	// same commit, with the same changes.
	f.verifyCheckout(f.follower, f.upstream.first)
	f.expect(f.beta, map[string]string{
		"top":            "top",
		"sub/.gitignore": "*.log\n",
		"sub/file":       "modified",
		"sub/other":      "other",
		"sub/untracked":  "untracked",
	})
	f.expectMissing(f.beta, ".git", "sub/deleted", "sub/debug.log")
	if url := gitModeRun(t, f.follower, "config", "--get", "remote.origin.url"); url != f.upstream.url {
		t.Errorf("incorrect origin for clone: %s", url)
	}
	if contents, err := os.ReadFile(filepath.Join(f.beta, ".gitmodules")); err != nil {
		t.Error(".gitmodules wasn't synchronized:", err)
	} else if !strings.Contains(string(contents), f.upstream.url) {
		t.Errorf("incorrect .gitmodules contents: %s", contents)
	}
	f.verifySameStatus("", " D deleted\n M file\n?? untracked")
	verifyNoTemporaryContent(t, f.beta)

	// Alpha hasn't changed.
	f.verifyAlphaAt(f.upstream.first)
	f.expect(f.alpha, map[string]string{"sub/file": "modified", "sub/debug.log": "ignored"})
	f.expectMissing(f.alpha, "sub/deleted")

	// The session has settled, with nothing to report.
	before := f.state()
	verifyNoProblems(t, before)
	f.flush()
	if after := f.state(); after.SuccessfulCycles != before.SuccessfulCycles+1 {
		t.Errorf("session hasn't settled: %d cycles after %d", after.SuccessfulCycles, before.SuccessfulCycles)
	}

	// Changes continue to be synchronized in both directions.
	f.write(f.follower, map[string]string{"from-beta": "from beta"})
	f.write(f.leader, map[string]string{"other": "changed on alpha"})
	f.flush()
	f.expect(f.alpha, map[string]string{"sub/from-beta": "from beta"})
	f.expect(f.beta, map[string]string{"sub/other": "changed on alpha"})
	f.verifyRemote(transport)
}

// testGitSubmoduleUninitialized tests that a superproject cloned without its
// submodules, which leaves an empty directory for each, gets them populated,
// that .gitmodules is synchronized like any other file, and that the same
// applies the other way around, after git submodule deinit.
func testGitSubmoduleUninitialized(t *testing.T, transport gitSubmoduleTransport) {
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{}, func(f *gitSubmoduleFixture) {
		gitModeRun(t, f.beta, "clone", "--quiet", f.alpha, ".")
		if entries, err := os.ReadDir(f.follower); err != nil || len(entries) != 0 {
			t.Fatalf("clone without submodules didn't leave an empty directory: %v, %v", entries, err)
		}
		f.write(f.leader, map[string]string{"untracked": "untracked"})
	})
	f.flush()
	f.verifyCheckout(f.follower, f.upstream.first)
	f.expect(f.beta, map[string]string{"sub/file": "first", "sub/untracked": "untracked"})
	f.verifySameStatus("", "?? untracked")
	f.verifySettled()

	// Beta's superproject sees its submodule as alpha's does: at the commit
	// that it records, with content that isn't committed.
	if alpha, beta := gitModeStatus(t, f.alpha), gitModeStatus(t, f.beta); alpha != " M sub" || beta != alpha {
		t.Errorf("incorrect superproject status: %q on alpha, %q on beta", alpha, beta)
	}

	// .gitmodules is synchronized as a file.
	gitModeRun(t, f.alpha, "config", "--file", ".gitmodules", "submodule.sub.branch", "main")
	expected, err := os.ReadFile(filepath.Join(f.alpha, ".gitmodules"))
	if err != nil {
		t.Fatal(err)
	}
	f.flush()
	f.expect(f.beta, map[string]string{".gitmodules": string(expected)})

	// Deinitializing the submodule on alpha, which leaves an empty directory,
	// is undone by populating it from beta, which results in an independent
	// repository there too.
	gitModeRun(t, f.alpha, "submodule", "--quiet", "deinit", "--force", "--", "sub")
	if entries, err := os.ReadDir(f.leader); err != nil || len(entries) != 0 {
		t.Fatalf("submodule directory isn't empty: %v, %v", entries, err)
	}
	f.flush()
	f.verifyCheckout(f.leader, f.upstream.first)
	f.expect(f.alpha, map[string]string{"sub/file": "first", "sub/untracked": "untracked"})
	f.verifyCheckout(f.follower, f.upstream.first)
	f.verifySettled()
}

// testGitSubmoduleNested tests that nested submodules are populated, along with
// the other endpoint's changes in each of them, by a single flush.
func testGitSubmoduleNested(t *testing.T, transport gitSubmoduleTransport) {
	// Create the nested submodule's upstream repository and add it to the
	// submodule's.
	inner := newGitModeUpstream(t, map[string]string{"inner": "inner"})
	outer := newGitModeUpstream(t, gitSubmoduleFiles)
	gitModeRun(t, outer.work, "submodule", "add", "--quiet", inner.url, "nested")
	gitModeRun(t, outer.work, "commit", "--quiet", "--no-gpg-sign", "-m", "add nested submodule")
	outerCommit := gitModeHead(t, outer.work)
	outer.push(t)

	// Create the session, with changes in both submodules on alpha.
	f := newGitSubmoduleFixtureWithUpstream(t, transport, outer, gitModeOptions{}, func(f *gitSubmoduleFixture) {
		gitModeRun(t, f.alpha, "submodule", "--quiet", "update", "--init", "--recursive")
		f.write(f.leader, map[string]string{"file": "modified"})
		f.write(filepath.Join(f.leader, "nested"), map[string]string{"inner": "modified inner", "untracked": "untracked"})
	})
	f.flush()

	// Both are on beta, with the changes, after a single flush.
	f.verifyCheckout(f.follower, outerCommit)
	f.verifyCheckout(filepath.Join(f.follower, "nested"), inner.first)
	f.expect(f.beta, map[string]string{
		"sub/file":             "modified",
		"sub/nested/inner":     "modified inner",
		"sub/nested/untracked": "untracked",
	})
	f.verifySameStatus("nested", " M inner\n?? untracked")
	f.verifySameStatus("", " M file\n M nested")
	f.verifySettled()
	f.verifyRemote(transport)
}

// testGitSubmoduleFollow tests that commits are followed in either direction,
// by moving HEAD without touching files, which are synchronized like other
// changes, and without moving a branch that's checked out.
func testGitSubmoduleFollow(t *testing.T, transport gitSubmoduleTransport) {
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{}, nil)
	f.flush()
	f.verifyCheckout(f.follower, f.upstream.first)

	// Check out a branch on beta, and change a file there.
	gitModeRun(t, f.follower, "checkout", "--quiet", "-b", "work")
	f.write(f.follower, map[string]string{"other": "changed on beta"})

	// Create and push a commit on alpha. Its files arrive with the flush, and
	// HEAD follows once the commit has been fetched.
	second := gitModeCommitFiles(t, f.leader, map[string]string{"file": "second", "added": "added"}, "second")
	gitModePush(t, f.leader, "main")
	state := f.flushAllowingProblems()
	if len(state.Conflicts) != 0 {
		t.Errorf("unexpected conflicts: %v", state.Conflicts)
	}
	f.expect(f.beta, map[string]string{"sub/file": "second", "sub/added": "added", "sub/other": "changed on beta"})
	f.expect(f.alpha, map[string]string{"sub/other": "changed on beta"})
	f.flushUntil(30*time.Second, "following on beta", func() bool { return gitModeHeadIs(f.follower, second) })
	if branch := gitModeRun(t, f.follower, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
		t.Errorf("HEAD isn't detached on beta: %s", branch)
	}
	if branch := gitModeRun(t, f.follower, "rev-parse", "refs/heads/work"); branch != f.upstream.first {
		t.Errorf("branch moved on beta: %s", branch)
	}
	f.expect(f.beta, map[string]string{"sub/other": "changed on beta"})
	f.verifySameStatus("", " M other")
	f.verifySettled()

	// A commit on beta, which is pushed to another branch, is followed on
	// alpha, whose branch stays where it is.
	third := gitModeCommit(t, f.follower, "third", "third")
	gitModePush(t, f.follower, "work")
	f.flushUntil(30*time.Second, "following on alpha", func() bool { return gitModeHeadIs(f.leader, third) })
	f.expect(f.alpha, map[string]string{"sub/third": "third", "sub/other": "changed on beta"})
	if branch := gitModeRun(t, f.leader, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
		t.Errorf("HEAD isn't detached on alpha: %s", branch)
	}
	if branch := gitModeRun(t, f.leader, "rev-parse", "refs/heads/main"); branch != second {
		t.Errorf("branch moved on alpha: %s", branch)
	}
	f.verifyAlphaAt(third)
	f.verifySameStatus("", " M other")

	// Going back to an earlier commit is a change like any other, and it
	// doesn't require a fetch.
	gitModeRun(t, f.leader, "checkout", "--quiet", "--detach", second)
	f.flush()
	if head := gitModeHead(t, f.follower); head != second {
		t.Errorf("incorrect commit on beta: %s != %s", head, second)
	}
	f.expectMissing(f.beta, "sub/third")
	f.verifySameStatus("", " M other")
	f.verifySettled()
	f.verifyRemote(transport)
}

// testGitSubmoduleUnpushedCommit tests that the files of a commit that isn't
// available to the other endpoint are synchronized regardless, while HEAD stays
// behind with a warning, and that HEAD follows once the commit is pushed.
func testGitSubmoduleUnpushedCommit(t *testing.T, transport gitSubmoduleTransport) {
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{}, nil)
	f.flush()

	// The files of a commit that hasn't been pushed reach beta, and HEAD stays
	// where it is, which is reported as a warning.
	unpushed := gitModeCommitFiles(t, f.leader, map[string]string{"file": "second", "added": "added"}, "unpushed")
	for i := 0; i < 2; i++ {
		state := f.flushAllowingProblems()
		f.expect(f.beta, map[string]string{"sub/file": "second", "sub/added": "added"})
		if head := gitModeHead(t, f.follower); head != f.upstream.first {
			t.Errorf("incorrect commit on beta: %s != %s", head, f.upstream.first)
		}
		problem := gitSubmoduleProblem(t, state.BetaState.TransitionProblems, "sub", unpushed[:12])
		if !problem.Warning {
			t.Error("commit that isn't available isn't reported as a warning:", problem)
		}
		if len(state.BetaState.TransitionProblems) != 1 || len(state.AlphaState.TransitionProblems) != 0 {
			t.Errorf("unexpected problems: %v, %v", state.AlphaState.TransitionProblems, state.BetaState.TransitionProblems)
		}
		if len(state.Conflicts) != 0 {
			t.Errorf("unexpected conflicts: %v", state.Conflicts)
		}
		f.verifyAlphaAt(unpushed)
	}

	// Commands still run, with the warning.
	f.verifyRemote(transport, unpushed[:12])

	// Once the commit is pushed, HEAD follows, once the retry interval since
	// the last attempt to fetch it has passed, and beta's working tree is clean.
	gitModePush(t, f.leader, "main")
	start := time.Now()
	f.flushUntil(45*time.Second, "following after push", func() bool { return gitModeHeadIs(f.follower, unpushed) })
	t.Logf("HEAD followed %s after the push", time.Since(start).Round(100*time.Millisecond))
	f.flush()
	if status := gitModeStatus(t, f.follower); status != "" {
		t.Errorf("working tree on beta isn't clean: %q", status)
	}
	f.verifyRemote(transport)
}

// gitSubmoduleConflictListing is the part of the JSON output of remote
// --conflicts that describes conflicts.
type gitSubmoduleConflictListing struct {
	Edges []struct {
		Listing struct {
			Conflicts []struct {
				Path       string
				Resolvable bool
				Submodule  bool
				Take       []string
			}
		}
	}
}

// testGitSubmoduleDiverged tests that different new commits on both endpoints
// are a conflict for the whole submodule in the two-way-safe mode, which holds
// back all of its content, that remote lists it with a hint and can't resolve
// it, and that it's over once both have the same commit checked out.
func testGitSubmoduleDiverged(t *testing.T, transport gitSubmoduleTransport) {
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{}, nil)
	f.flush()

	// Create different commits, which both endpoints could obtain, and change
	// a file on alpha.
	alphaCommit := gitModeCommit(t, f.leader, "alpha", "alpha")
	gitModePush(t, f.leader, "alpha")
	betaCommit := gitModeCommit(t, f.follower, "beta", "beta")
	gitModePush(t, f.follower, "beta")
	f.write(f.leader, map[string]string{"other": "changed on alpha"})

	// There's a single conflict, and nothing in the submodule moves.
	for i := 0; i < 2; i++ {
		state := f.flushAllowingProblems()
		if len(state.Conflicts) != 1 || state.Conflicts[0].Root != "sub" {
			t.Fatalf("incorrect conflicts: %v", state.Conflicts)
		}
		if len(state.AlphaState.TransitionProblems) != 0 || len(state.BetaState.TransitionProblems) != 0 {
			t.Errorf("unexpected problems: %v, %v", state.AlphaState.TransitionProblems, state.BetaState.TransitionProblems)
		}
		f.verifyAlphaAt(alphaCommit)
		if head := gitModeHead(t, f.follower); head != betaCommit {
			t.Errorf("incorrect commit on beta: %s != %s", head, betaCommit)
		}
		f.expect(f.beta, map[string]string{"sub/other": "other"})
		f.expectMissing(f.beta, "sub/alpha")
		f.expectMissing(f.alpha, "sub/beta")
	}

	// It's listed, with a hint, and remote can't resolve it.
	var commands *conflictCommands
	if transport.commands {
		f.waitForRemoteCommands()
		commands = &conflictCommands{t: t, directory: f.alpha}
		var listing gitSubmoduleConflictListing
		if err := json.Unmarshal([]byte(commands.invoke("--conflicts", "--json")), &listing); err != nil {
			t.Fatal(err)
		} else if len(listing.Edges) != 1 || len(listing.Edges[0].Listing.Conflicts) != 1 {
			t.Fatalf("incorrect listing: %+v", listing)
		}
		conflict := listing.Edges[0].Listing.Conflicts[0]
		if conflict.Path != "sub" || !conflict.Submodule || conflict.Resolvable || len(conflict.Take) != 0 {
			t.Errorf("incorrect conflict: %+v", conflict)
		}
		code, output, diagnostics := runRemote(t, f.alpha, "--timeout", "20s", "--conflicts")
		if code != 0 || !strings.Contains(output, "sub") || !strings.Contains(output, "check out the same commit") {
			t.Errorf("incorrect listing: %d: %q: %s", code, output, diagnostics)
		}
		commands.fail("check out the same commit", "--conflict-checkout", "sub")
	}

	// Commands still run.
	f.verifyRemote(transport)

	// Check out alpha's commit on beta, after which the conflict is over, and
	// the change on alpha arrives.
	gitModeRun(t, f.follower, "fetch", "--quiet", "origin", "alpha")
	gitModeRun(t, f.follower, "checkout", "--quiet", "--detach", alphaCommit)
	f.flush()
	f.verifyAlphaAt(alphaCommit)
	if head := gitModeHead(t, f.follower); head != alphaCommit {
		t.Errorf("incorrect commit on beta: %s != %s", head, alphaCommit)
	}
	f.expect(f.beta, map[string]string{"sub/other": "changed on alpha", "sub/alpha": "alpha"})
	f.expectMissing(f.beta, "sub/beta")
	f.verifySameStatus("", " M other")
	if commands != nil {
		var listing gitSubmoduleConflictListing
		if err := json.Unmarshal([]byte(commands.invoke("--conflicts", "--json")), &listing); err != nil {
			t.Fatal(err)
		} else if len(listing.Edges) != 1 || len(listing.Edges[0].Listing.Conflicts) != 0 {
			t.Errorf("conflict remains: %+v", listing)
		}
	}

	// The session continues as usual.
	f.write(f.follower, map[string]string{"file": "changed on beta"})
	f.flush()
	f.expect(f.alpha, map[string]string{"sub/file": "changed on beta"})
}

// testGitSubmoduleDivergedResolved tests that different new commits on both
// endpoints in the two-way-resolved mode result in beta following alpha's
// commit, with files resolved path by path, as for any other content.
func testGitSubmoduleDivergedResolved(t *testing.T, transport gitSubmoduleTransport) {
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{mode: core.SynchronizationMode_SynchronizationModeTwoWayResolved}, nil)
	f.flush()
	alphaCommit := gitModeCommitFiles(t, f.leader, map[string]string{"alpha": "alpha", "file": "from alpha"}, "alpha")
	gitModePush(t, f.leader, "alpha")
	gitModeCommitFiles(t, f.follower, map[string]string{"beta": "beta", "file": "from beta"}, "beta")
	gitModePush(t, f.follower, "beta")
	state := f.flushUntil(30*time.Second, "following on beta", func() bool { return gitModeHeadIs(f.follower, alphaCommit) })
	if len(state.Conflicts) != 0 {
		t.Errorf("unexpected conflicts: %v", state.Conflicts)
	}
	f.verifyAlphaAt(alphaCommit)
	f.expect(f.beta, map[string]string{"sub/alpha": "alpha", "sub/file": "from alpha", "sub/beta": "beta"})
	f.expect(f.alpha, map[string]string{"sub/file": "from alpha", "sub/beta": "beta"})
	f.verifySameStatus("", "?? beta")
	f.verifySettled()
}

// testGitSubmoduleAttach tests that a directory with the files of a submodule,
// but without its .git, gets a repository attached, with the commit that the
// other endpoint has checked out, without touching any files.
func testGitSubmoduleAttach(t *testing.T, transport gitSubmoduleTransport) {
	names := []string{".gitignore", "file", "other", "deleted"}
	past := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{}, func(f *gitSubmoduleFixture) {
		f.write(f.follower, gitSubmoduleFiles)
		for _, name := range names {
			if err := os.Chtimes(filepath.Join(f.follower, name), past, past); err != nil {
				t.Fatal(err)
			}
		}
	})
	before := modificationTimes(t, f.follower, names)
	f.flush()

	// Verify the result.
	f.verifyCheckout(f.follower, f.upstream.first)
	if status := gitModeStatus(t, f.follower); status != "" {
		t.Errorf("working tree on beta isn't clean: %q", status)
	}
	for name, after := range modificationTimes(t, f.follower, names) {
		if !after.Equal(before[name]) {
			t.Errorf("modification time of %s changed: %v != %v", name, after, before[name])
		}
	}
	verifyNoTemporaryContent(t, f.beta)
	f.verifySettled()
	f.verifyAlphaAt(f.upstream.first)

	// The same happens if beta's repository is deleted later, which doesn't
	// delete alpha's either.
	if err := os.RemoveAll(filepath.Join(f.follower, ".git")); err != nil {
		t.Fatal(err)
	}
	f.flush()
	f.verifyCheckout(f.follower, f.upstream.first)
	if status := gitModeStatus(t, f.follower); status != "" {
		t.Errorf("working tree on beta isn't clean: %q", status)
	}
	f.verifyAlphaAt(f.upstream.first)
	f.verifySettled()
	f.verifyRemote(transport)
}

// testGitSubmoduleAttachUnpushed tests that a repository attached for a commit
// that isn't available has the commit of the default branch, with a warning,
// without touching any files, and that it follows the commit once it's pushed.
func testGitSubmoduleAttachUnpushed(t *testing.T, transport gitSubmoduleTransport) {
	files := map[string]string{"file": "second", "added": "added"}
	var unpushed string
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{}, func(f *gitSubmoduleFixture) {
		unpushed = gitModeCommitFiles(t, f.leader, files, "unpushed")
		f.write(f.follower, gitSubmoduleFiles)
		f.write(f.follower, files)
	})
	names := []string{"file", "added", "other"}
	before := modificationTimes(t, f.follower, names)
	state := f.flushAllowingProblems()
	problem := gitSubmoduleProblem(t, state.BetaState.TransitionProblems, "sub", unpushed[:12])
	if !problem.Warning {
		t.Error("commit that isn't available isn't reported as a warning:", problem)
	}
	if len(state.Conflicts) != 0 || len(state.AlphaState.TransitionProblems) != 0 {
		t.Errorf("unexpected problems: %v, %v", state.Conflicts, state.AlphaState.TransitionProblems)
	}
	f.verifyCheckout(f.follower, f.upstream.first)
	f.expect(f.beta, map[string]string{"sub/file": "second", "sub/added": "added"})
	for name, after := range modificationTimes(t, f.follower, names) {
		if !after.Equal(before[name]) {
			t.Errorf("modification time of %s changed: %v != %v", name, after, before[name])
		}
	}

	// Once the commit is pushed, HEAD follows, and beta's working tree is
	// clean.
	gitModePush(t, f.leader, "main")
	f.flushUntil(45*time.Second, "following after push", func() bool { return gitModeHeadIs(f.follower, unpushed) })
	if status := gitModeStatus(t, f.follower); status != "" {
		t.Errorf("working tree on beta isn't clean: %q", status)
	}
	f.verifyAlphaAt(unpushed)
	f.verifySettled()
}

// testGitSubmoduleRemoval tests that a submodule that's removed from an
// endpoint isn't removed from the other, which reports it, until that's done by
// hand.
func testGitSubmoduleRemoval(t *testing.T, transport gitSubmoduleTransport, removeFromBeta bool) {
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{}, nil)
	f.flush()
	removed, kept := f.leader, f.follower
	if removeFromBeta {
		removed, kept = f.follower, f.leader
	}
	removeDirectoryInUse(t, removed)

	// It's reported, but not removed, including in further cycles.
	for i := 0; i < 2; i++ {
		state := f.flushAllowingProblems()
		problems, others := state.BetaState.TransitionProblems, state.AlphaState.TransitionProblems
		if removeFromBeta {
			problems, others = others, problems
		}
		problem := gitSubmoduleProblem(t, problems, "sub", "isn't removed automatically")
		if problem.Warning {
			t.Error("refused removal reported as a warning:", problem)
		}
		if len(others) != 0 {
			t.Errorf("unexpected problems on the endpoint without the submodule: %v", others)
		}
		if len(state.Conflicts) != 0 {
			t.Errorf("unexpected conflicts: %v", state.Conflicts)
		}
		if head := gitModeHead(t, kept); head != f.upstream.first {
			t.Errorf("incorrect commit in submodule: %s != %s", head, f.upstream.first)
		}
		if contents, err := os.ReadFile(filepath.Join(kept, "file")); err != nil || string(contents) != "first" {
			t.Errorf("submodule's content changed: %q, %v", contents, err)
		}
		if _, err := os.Lstat(removed); !os.IsNotExist(err) {
			t.Errorf("submodule came back: %v", err)
		}
	}

	// Commands still run.
	f.verifyRemote(transport, "isn't removed automatically")

	// Removing it by hand ends that.
	removeDirectoryInUse(t, kept)
	f.flush()
	f.expectMissing(f.alpha, "sub")
	f.expectMissing(f.beta, "sub")
	f.verifySettled()
	f.verifyRemote(transport)
}

// testGitSubmoduleDeletionResolves tests that deleting a submodule on beta
// after alpha has changed it, either by changing its files or by checking out a
// commit that conflicts with beta's, results in a new clone on beta, with
// alpha's commit and changes, discarding beta's.
func testGitSubmoduleDeletionResolves(t *testing.T, transport gitSubmoduleTransport, diverged bool) {
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{}, nil)
	f.flush()
	alphaCommit := f.upstream.first
	if diverged {
		alphaCommit = gitModeCommit(t, f.leader, "alpha", "alpha")
		gitModePush(t, f.leader, "alpha")
		gitModeCommit(t, f.follower, "beta", "beta")
		gitModePush(t, f.follower, "beta")
	}
	f.write(f.leader, map[string]string{"other": "changed on alpha"})
	f.write(f.follower, map[string]string{"untracked": "discarded"})
	if diverged {
		state := f.flushAllowingProblems()
		if len(state.Conflicts) != 1 || state.Conflicts[0].Root != "sub" {
			t.Fatalf("incorrect conflicts: %v", state.Conflicts)
		}
	}

	// Delete the submodule on beta.
	removeDirectoryInUse(t, f.follower)
	f.flush()
	f.verifyCheckout(f.follower, alphaCommit)
	f.expect(f.beta, map[string]string{"sub/other": "changed on alpha", "sub/file": "first"})
	f.expectMissing(f.beta, "sub/beta", "sub/untracked")
	f.verifyAlphaAt(alphaCommit)
	f.expect(f.alpha, map[string]string{"sub/other": "changed on alpha"})
	f.expectMissing(f.alpha, "sub/beta", "sub/untracked")
	f.verifySameStatus("", " M other")
	f.verifySettled()
}

// gitModeOneWayModes are the one-way synchronization modes.
var gitModeOneWayModes = []core.SynchronizationMode{
	core.SynchronizationMode_SynchronizationModeOneWaySafe,
	core.SynchronizationMode_SynchronizationModeOneWayReplica,
}

// testGitSubmoduleOnlyOnBeta tests that a submodule that only beta has is left
// alone in one-way modes, with its uncommitted changes.
func testGitSubmoduleOnlyOnBeta(t *testing.T, transport gitSubmoduleTransport) {
	for _, mode := range gitModeOneWayModes {
		t.Run(mode.Description(), func(t *testing.T) {
			upstream := newGitModeUpstream(t, gitSubmoduleFiles)
			session := newGitModeSession(t, transport.configureOptions(gitModeOptions{
				mode:  mode,
				alpha: map[string]string{"top": "top"},
				prepare: func(s *gitModeSession) {
					gitModeRun(t, s.beta, "clone", "--quiet", upstream.url, "sub")
					s.write(filepath.Join(s.beta, "sub"), map[string]string{"other": "changed on beta", "untracked": "untracked"})
				},
			}))
			for i := 0; i < 2; i++ {
				state := session.flushAllowingProblems()
				session.expect(session.beta, map[string]string{"top": "top", "sub/other": "changed on beta", "sub/untracked": "untracked", "sub/file": "first"})
				if head := gitModeHead(t, filepath.Join(session.beta, "sub")); head != upstream.first {
					t.Errorf("incorrect commit on beta: %s", head)
				}
				session.expectMissing(session.alpha, "sub")
				if len(state.Conflicts) != 0 {
					t.Errorf("unexpected conflicts: %v", state.Conflicts)
				}

				// The one-way-replica mode would delete beta's content, which
				// is refused for a submodule. Report what it does, which isn't
				// prescribed.
				problems := state.BetaState.TransitionProblems
				if mode == core.SynchronizationMode_SynchronizationModeOneWaySafe {
					verifyNoProblems(t, state)
				} else if len(problems) > 0 {
					t.Logf("beta's problems in the one-way-replica mode: %v", problems)
				}
			}
		})
	}
}

// testGitSubmoduleGitEntryOnlyOnBeta tests that a repository on beta, in a
// directory that alpha has without one, is left alone in one-way modes, while
// the directory's files are synchronized like any others.
func testGitSubmoduleGitEntryOnlyOnBeta(t *testing.T, transport gitSubmoduleTransport) {
	for _, mode := range gitModeOneWayModes {
		t.Run(mode.Description(), func(t *testing.T) {
			upstream := newGitModeUpstream(t, gitSubmoduleFiles)
			alpha := map[string]string{"top": "top"}
			for name, contents := range gitSubmoduleFiles {
				alpha["sub/"+name] = contents
			}
			alpha["sub/added"] = "added"
			session := newGitModeSession(t, transport.configureOptions(gitModeOptions{
				mode:  mode,
				alpha: alpha,
				prepare: func(s *gitModeSession) {
					gitModeRun(t, s.beta, "clone", "--quiet", upstream.url, "sub")
				},
			}))
			session.flush()
			session.expect(session.beta, map[string]string{"sub/added": "added"})
			if head := gitModeHead(t, filepath.Join(session.beta, "sub")); head != upstream.first {
				t.Errorf("incorrect commit on beta: %s", head)
			}
			session.expectMissing(session.alpha, "sub/.git")
			verifyNoProblems(t, session.state())
		})
	}
}

// testGitSubmoduleOneWayChangesOnBeta tests that changes to a submodule on
// beta, including a new commit, never reach alpha in one-way modes, and that
// the one-way-replica mode overwrites them, like any other changes on beta,
// with HEAD following alpha's commit back.
func testGitSubmoduleOneWayChangesOnBeta(t *testing.T, transport gitSubmoduleTransport) {
	for _, mode := range gitModeOneWayModes {
		t.Run(mode.Description(), func(t *testing.T) {
			f := newGitSubmoduleFixture(t, transport, gitModeOptions{mode: mode}, nil)
			f.flush()
			f.verifyCheckout(f.follower, f.upstream.first)

			// Change a file on beta and create a commit there.
			f.write(f.follower, map[string]string{"other": "changed on beta"})
			betaCommit := gitModeCommit(t, f.follower, "beta", "beta")
			gitModePush(t, f.follower, "beta")
			f.flushAllowingProblems()
			f.flushAllowingProblems()

			// Alpha gets none of it.
			f.verifyAlphaAt(f.upstream.first)
			f.expect(f.alpha, map[string]string{"sub/other": "other"})
			f.expectMissing(f.alpha, "sub/beta")
			if status := gitModeStatus(t, f.leader); status != "" {
				t.Errorf("working tree on alpha isn't clean: %q", status)
			}

			// Beta keeps it, unless it's a replica. In the one-way-safe mode,
			// beta's modifications of what alpha has are conflicts, as usual,
			// and that includes its commit.
			state := f.state()
			if mode == core.SynchronizationMode_SynchronizationModeOneWayReplica {
				f.expect(f.beta, map[string]string{"sub/other": "other"})
				f.expectMissing(f.beta, "sub/beta")
				if head := gitModeHead(t, f.follower); head != f.upstream.first {
					t.Errorf("commit on beta wasn't replaced: %s instead of %s", head, f.upstream.first)
				}
				if status := gitModeStatus(t, f.follower); status != "" {
					t.Errorf("working tree on beta isn't clean: %q", status)
				}
				verifyNoProblems(t, state)
			} else {
				f.expect(f.beta, map[string]string{"sub/other": "changed on beta", "sub/beta": "beta"})
				if head := gitModeHead(t, f.follower); head != betaCommit {
					t.Errorf("commit on beta was replaced: %s instead of %s", head, betaCommit)
				}
				roots := make(map[string]bool)
				for _, conflict := range state.Conflicts {
					roots[conflict.Root] = true
				}
				if len(state.Conflicts) != 2 || !roots["sub/other"] || !roots["sub/.git"] {
					t.Errorf("incorrect conflicts: %v", state.Conflicts)
				}
				state.Conflicts = nil
				verifyNoProblems(t, state)

				// remote lists the commit's conflict as one that it can't
				// resolve, unlike the file's.
				if transport.commands {
					f.waitForRemoteCommands()
					commands := &conflictCommands{t: t, directory: f.alpha}
					var listing gitSubmoduleConflictListing
					if err := json.Unmarshal([]byte(commands.invoke("--conflicts", "--json")), &listing); err != nil {
						t.Fatal(err)
					} else if len(listing.Edges) != 1 || len(listing.Edges[0].Listing.Conflicts) != 2 {
						t.Fatalf("incorrect listing: %+v", listing)
					}
					for _, conflict := range listing.Edges[0].Listing.Conflicts {
						if submodule := conflict.Path == "sub/.git"; conflict.Submodule != submodule || conflict.Resolvable == submodule {
							t.Errorf("incorrect conflict: %+v", conflict)
						}
					}
					code, output, diagnostics := runRemote(t, f.alpha, "--timeout", "20s", "--conflicts")
					if code != 0 || !strings.Contains(output, "check out the same commit") {
						t.Errorf("incorrect listing: %d: %q: %s", code, output, diagnostics)
					}
					commands.fail("check out the same commit", "--conflict-checkout", "sub/.git")
				}
			}
		})
	}
}

// testGitSubmoduleDanglingGitfile tests what's left by a session that didn't
// use Git mode, which is a directory on the other endpoint with a copy of the
// .git file that doesn't point to anything: it's reported, it stays until it's
// been removed by hand, and then it's populated.
func testGitSubmoduleDanglingGitfile(t *testing.T, transport gitSubmoduleTransport) {
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{beta: map[string]string{
		"sub/.git":  "gitdir: ../.git/modules/sub\n",
		"sub/file":  "first",
		"sub/local": "local",
	}}, nil)
	state := f.flushAllowingProblems()
	gitModeProblem(t, state.BetaState.ScanProblems, "sub", "isn't a valid repository", "remove the directory")

	// Nothing has been touched, and what else there is has been synchronized.
	f.expect(f.beta, map[string]string{"sub/.git": "gitdir: ../.git/modules/sub\n", "sub/file": "first", "sub/local": "local", "top": "top"})
	f.expectMissing(f.alpha, "sub/local")
	f.verifyAlphaAt(f.upstream.first)
	f.verifyRemote(transport, "remove the directory")

	// Following the instructions gives the expected result.
	removeDirectoryInUse(t, f.follower)
	f.flush()
	f.verifyCheckout(f.follower, f.upstream.first)
	f.verifyAlphaAt(f.upstream.first)
	f.verifySettled()
}

// removeDirectoryInUse removes a directory that a running session is working in.
// On Windows, a directory can't be removed while it's the working directory of a
// process, such as a Git command that the session has started, so this keeps
// trying for a while.
func removeDirectoryInUse(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := os.RemoveAll(path)
		if err == nil {
			return
		} else if runtime.GOOS != "windows" || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// gitSubmoduleTransports are the transports with which the scenarios for
// submodules are run by TestGitModeSubmodules.
var gitSubmoduleTransports = []struct {
	name      string
	transport gitSubmoduleTransport
}{
	{"local", gitSubmoduleTransport{}},
	{"remote endpoint", gitSubmoduleTransport{
		commands: true,
		configure: func(options *gitModeOptions) {
			options.remoteBeta = true
			options.commands = synchronization.RemoteCommandMode_RemoteCommandModeBeta
		},
	}},
}

// TestGitModeSubmodules runs the scenarios for submodules between local
// endpoints and through the remote endpoint implementation, the latter of which
// also supports remote commands.
func TestGitModeSubmodules(t *testing.T) {
	isolateGitMode(t)
	for _, transport := range gitSubmoduleTransports {
		for _, scenario := range gitSubmoduleScenarios {
			t.Run(transport.name+"/"+scenario.name, func(t *testing.T) {
				if transport.transport.commands && runtime.GOOS == "windows" {
					t.Skip("commands require a POSIX shell")
				}
				scenario.run(t, transport.transport)
			})
		}
	}
}

// TestGitModeSubmoduleFetchWakesSession tests that a commit that a submodule
// has to follow, and that's fetched in the background, results in HEAD
// following without anything else happening, such as a flush. The commit
// doesn't change any files, so the fetch is the only thing that can wake the
// session, other than a flush. A session that isn't watching for changes only
// synchronizes when it's flushed, so what happens there is only reported, but
// a flush makes HEAD follow either way.
func TestGitModeSubmoduleFetchWakesSession(t *testing.T) {
	isolateGitMode(t)
	for _, watch := range []bool{true, false} {
		name := "no-watch"
		if watch {
			name = "watching"
		}
		t.Run(name, func(t *testing.T) {
			f := newGitSubmoduleFixture(t, gitSubmoduleTransport{}, gitModeOptions{watch: watch}, nil)
			if watch {
				if ok, _ := waitForCondition(time.Minute, func() bool { return gitModeHeadIs(f.follower, f.upstream.first) }); !ok {
					t.Fatal("submodule wasn't populated")
				}
				f.waitForIdle()
			} else {
				f.flush()
			}

			// Create and push an empty commit on alpha.
			gitModeRun(t, f.leader, "commit", "--quiet", "--no-gpg-sign", "--allow-empty", "-m", "empty")
			second := gitModeHead(t, f.leader)
			gitModePush(t, f.leader, "main")

			// Without watching, a flush makes beta try to follow, which can't
			// happen until the commit has been fetched.
			if !watch {
				state := f.flushAllowingProblems()
				if problem := gitSubmoduleProblem(t, state.BetaState.TransitionProblems, "sub", second[:12]); !problem.Warning {
					t.Error("commit that isn't available isn't reported as a warning:", problem)
				}
			}

			// Wait for HEAD to follow.
			followed, elapsed := waitForCondition(30*time.Second, func() bool { return gitModeHeadIs(f.follower, second) })
			if watch && !followed {
				t.Errorf("HEAD didn't follow within %s", elapsed.Round(time.Second))
			} else if followed {
				t.Logf("HEAD followed after %s", elapsed.Round(100*time.Millisecond))
			} else {
				t.Logf("HEAD didn't follow within %s without a flush (%d cycles)", elapsed.Round(time.Second), f.state().SuccessfulCycles)
			}

			// It follows after a flush in any case, with nothing to report.
			f.flushUntil(30*time.Second, "following after flush", func() bool { return gitModeHeadIs(f.follower, second) })
			f.flush()
			f.verifySettled()
		})
	}
}

// TestGitModeSubmoduleFollowsAfterPush tests that a submodule's HEAD follows a
// commit that's pushed after the session has tried to fetch it and failed, in a
// session that's watching for changes, without any further changes or flushes.
// The README says that the fetch is retried at growing intervals, starting at
// 10 seconds, while it fails.
func TestGitModeSubmoduleFollowsAfterPush(t *testing.T) {
	isolateGitMode(t)
	f := newGitSubmoduleFixture(t, gitSubmoduleTransport{}, gitModeOptions{watch: true}, nil)
	if ok, _ := waitForCondition(time.Minute, func() bool { return gitModeHeadIs(f.follower, f.upstream.first) }); !ok {
		t.Fatal("submodule wasn't populated")
	}
	f.waitForIdle()

	// Create a commit on alpha without pushing it, and wait for its files and
	// the warning to reach beta.
	unpushed := gitModeCommitFiles(t, f.leader, map[string]string{"file": "second"}, "unpushed")
	f.waitForState(func(state *synchronization.State) bool {
		for _, problem := range state.BetaState.TransitionProblems {
			if strings.HasPrefix(problem.Path, "sub") && strings.Contains(problem.Error, unpushed[:12]) {
				return true
			}
		}
		return false
	})
	if ok, _ := waitForCondition(30*time.Second, func() bool {
		contents, err := os.ReadFile(filepath.Join(f.follower, "file"))
		return err == nil && string(contents) == "second"
	}); !ok {
		t.Fatal("commit's files didn't arrive")
	}

	// Wait for the session to become idle, and then for longer than the first
	// retry interval, so that the attempts that the session's cycles requested
	// have failed, and then push the commit.
	f.waitForIdle()
	time.Sleep(11 * time.Second)
	cycles := f.state().SuccessfulCycles
	gitModePush(t, f.leader, "main")

	// HEAD follows within the next retry interval (20 seconds), plus some
	// leeway.
	followed, elapsed := waitForCondition(40*time.Second, func() bool { return gitModeHeadIs(f.follower, unpushed) })
	if !followed {
		t.Errorf("HEAD didn't follow within %s after the push (%d cycles after %d)", elapsed.Round(time.Second), f.state().SuccessfulCycles, cycles)
	} else {
		t.Logf("HEAD followed %s after the push", elapsed.Round(100*time.Millisecond))
	}
}

// testGitSubmoduleNoHotLoop tests that populating a submodule, following a
// commit, and attaching a repository don't result in further synchronization
// cycles, without any changes, in a session that's watching for changes.
func testGitSubmoduleNoHotLoop(t *testing.T, transport gitSubmoduleTransport, watchMode synchronization.WatchMode) {
	f := newGitSubmoduleFixture(t, transport, gitModeOptions{watch: true, watchMode: watchMode}, func(f *gitSubmoduleFixture) {
		f.write(f.leader, map[string]string{"file": "modified", "untracked": "untracked"})
	})
	waitForContents := func(path, expected string) {
		t.Helper()
		if ok, _ := waitForCondition(time.Minute, func() bool {
			contents, err := os.ReadFile(path)
			return err == nil && string(contents) == expected
		}); !ok {
			t.Fatalf("timed out waiting for %s", path)
		}
	}
	waitForHead := func(commit string) {
		t.Helper()
		if ok, _ := waitForCondition(time.Minute, func() bool { return gitModeHeadIs(f.follower, commit) }); !ok {
			t.Fatalf("timed out waiting for commit %s", commit)
		}
	}

	// Populate the submodule.
	waitForContents(filepath.Join(f.follower, "untracked"), "untracked")
	waitForHead(f.upstream.first)
	f.waitForIdle()

	// Follow a commit.
	second := gitModeCommit(t, f.leader, "second", "second")
	gitModePush(t, f.leader, "main")
	waitForContents(filepath.Join(f.follower, "second"), "second")
	waitForHead(second)
	f.waitForIdle()

	// Attach a repository.
	removeDirectoryInUse(t, filepath.Join(f.follower, ".git"))
	if ok, _ := waitForCondition(time.Minute, func() bool {
		info, err := os.Lstat(filepath.Join(f.follower, ".git"))
		return err == nil && info.IsDir() && gitModeHeadIs(f.follower, second)
	}); !ok {
		t.Fatal("timed out waiting for repository to be attached")
	}
	f.waitForIdle()
	verifyNoProblems(t, f.state())
	f.expect(f.beta, map[string]string{"sub/file": "modified", "sub/second": "second"})
}

// runGitSubmoduleNoHotLoop runs testGitSubmoduleNoHotLoop for each of the watch
// modes that are possible on this system.
func runGitSubmoduleNoHotLoop(t *testing.T, transport gitSubmoduleTransport) {
	for _, mode := range gitSubmoduleWatchModes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.mode == synchronization.WatchMode_WatchModePortable && mode.recursive != watching.RecursiveWatchingSupported {
				t.Skip("this kind of watching isn't what this system uses")
			}
			testGitSubmoduleNoHotLoop(t, transport, mode.mode)
		})
	}
}

// TestGitModeSubmoduleNoHotLoop tests that operations on submodules don't cause
// hot loops.
func TestGitModeSubmoduleNoHotLoop(t *testing.T) {
	isolateGitMode(t)
	runGitSubmoduleNoHotLoop(t, gitSubmoduleTransport{})
}
