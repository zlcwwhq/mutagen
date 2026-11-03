package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem/watching"
	"github.com/mutagen-io/mutagen/pkg/integration/protocols/netpipe"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// gitModeSynchronizationModes are the synchronization modes tested in Git mode.
var gitModeSynchronizationModes = []core.SynchronizationMode{
	core.SynchronizationMode_SynchronizationModeTwoWaySafe,
	core.SynchronizationMode_SynchronizationModeTwoWayResolved,
	core.SynchronizationMode_SynchronizationModeOneWaySafe,
	core.SynchronizationMode_SynchronizationModeOneWayReplica,
}

// gitModeOptions are the options for a Git mode session.
type gitModeOptions struct {
	// mode is the synchronization mode, or the default if unset.
	mode core.SynchronizationMode
	// ignores are the session's ignores.
	ignores []string
	// watch indicates that the session should watch for changes. Otherwise, it
	// only performs synchronization when flushed.
	watch bool
	// watchMode is the watch mode to use if the session should watch for
	// changes, or the default if unset.
	watchMode synchronization.WatchMode
	// remoteBeta indicates that beta should be reached through the remote
	// endpoint implementation.
	remoteBeta bool
	// betaEndpoint specifies how beta should be reached, if set, returning its
	// URL and its root on this system. It takes precedence over remoteBeta.
	betaEndpoint func() (*url.URL, string)
	// betaMissing indicates that beta's root shouldn't exist. Beta must not
	// have any initial contents.
	betaMissing bool
	// alpha are the initial contents of alpha.
	alpha map[string]string
	// beta are the initial contents of beta.
	beta map[string]string
	// prepare performs any further setup of the roots, before the session
	// starts.
	prepare func(*gitModeSession)
	// commands is the session's remote command mode.
	commands synchronization.RemoteCommandMode
	// manager is the session manager, or synchronizationManager if nil.
	manager *synchronization.Manager
}

// gitModeSession is a session in Git mode for an integration test.
type gitModeSession struct {
	// t is the test.
	t *testing.T
	// manager is the session manager.
	manager *synchronization.Manager
	// alpha is the root of alpha.
	alpha string
	// beta is the root of beta.
	beta string
	// selection selects the session.
	selection *selection.Selection
}

// newGitModeSession creates a session in Git mode with the specified options,
// waits for its synchronization loop to start, and terminates it when the test
// completes. Temporary directories for the roots come from os.MkdirTemp rather
// than t.TempDir because socket paths are created in them, and test names would
// make them too long on some platforms.
func newGitModeSession(t *testing.T, options gitModeOptions) *gitModeSession {
	t.Helper()

	// Create the roots and their initial content.
	base, err := os.MkdirTemp("", "mgm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	session := &gitModeSession{
		t:       t,
		manager: options.manager,
		alpha:   filepath.Join(base, "alpha"),
		beta:    filepath.Join(base, "beta"),
	}
	if session.manager == nil {
		session.manager = synchronizationManager
	}
	betaURL := &url.URL{Path: session.beta}
	if options.remoteBeta {
		betaURL.Protocol = netpipe.Protocol_Netpipe
	}
	if options.betaEndpoint != nil {
		betaURL, session.beta = options.betaEndpoint()
	}
	for _, root := range []string{session.alpha, session.beta} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	session.write(session.alpha, options.alpha)
	session.write(session.beta, options.beta)
	if options.betaMissing {
		if err := os.Remove(session.beta); err != nil {
			t.Fatal(err)
		}
	}
	if options.prepare != nil {
		options.prepare(session)
	}

	// Create the session.
	configuration := &synchronization.Configuration{
		GitMode:              synchronization.GitMode_GitModeEnabled,
		SynchronizationMode:  options.mode,
		Ignores:              options.ignores,
		WatchPollingInterval: 1,
		RemoteCommandMode:    options.commands,
	}
	if options.watch {
		configuration.WatchMode = options.watchMode
	} else {
		configuration.WatchMode = synchronization.WatchMode_WatchModeNoWatch
	}
	ctx := context.Background()
	id, err := session.manager.Create(
		ctx,
		&url.URL{Path: session.alpha}, betaURL,
		configuration, &synchronization.Configuration{}, &synchronization.Configuration{},
		"", nil, nil, false, "",
	)
	if err != nil {
		t.Fatal("unable to create session:", err)
	}
	session.selection = &selection.Selection{Specifications: []string{id}}
	t.Cleanup(func() { session.manager.Terminate(ctx, session.selection, "") })
	logSessionOnFailure(t, id)

	// Wait for the synchronization loop to start, since a session only accepts
	// flush requests once it has.
	session.waitForState(func(state *synchronization.State) bool {
		return state.Status >= synchronization.Status_Watching
	})
	return session
}

// waitForState waits until a condition is satisfied by the session's state.
func (s *gitModeSession) waitForState(condition func(*synchronization.State) bool) *synchronization.State {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var previousStateIndex uint64
	for {
		stateIndex, states, err := s.manager.List(ctx, s.selection, previousStateIndex)
		if err != nil {
			s.t.Fatal("unable to wait for session state:", err)
		} else if len(states) != 1 {
			s.t.Fatal("invalid number of session states returned")
		} else if condition(states[0]) {
			return states[0]
		}
		previousStateIndex = stateIndex
	}
}

// state returns the session's current state.
func (s *gitModeSession) state() *synchronization.State {
	s.t.Helper()
	return s.waitForState(func(*synchronization.State) bool { return true })
}

// flushAllowingProblems performs a flush, which must succeed, and returns the
// session's state, verifying only that the session hasn't reported an error.
func (s *gitModeSession) flushAllowingProblems() *synchronization.State {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.manager.Flush(ctx, s.selection, "", false); err != nil {
		s.t.Fatal("unable to flush session:", err)
	}
	state := s.state()
	if state.LastError != "" {
		s.t.Error("session reported error:", state.LastError)
	}
	return state
}

// flush performs a flush, which must succeed, and verifies that there are no
// conflicts or transition problems, none of which any of these tests expect.
func (s *gitModeSession) flush() {
	s.t.Helper()
	state := s.flushAllowingProblems()
	if len(state.Conflicts) > 0 {
		s.t.Errorf("unexpected conflicts: %v", state.Conflicts)
	}
	if len(state.AlphaState.TransitionProblems) > 0 {
		s.t.Errorf("unexpected alpha transition problems: %v", state.AlphaState.TransitionProblems)
	}
	if len(state.BetaState.TransitionProblems) > 0 {
		s.t.Errorf("unexpected beta transition problems: %v", state.BetaState.TransitionProblems)
	}
}

// write writes files, specified as a map from their slash-separated paths
// relative to a root to their contents, creating parent directories as needed.
func (s *gitModeSession) write(root string, files map[string]string) {
	s.t.Helper()
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			s.t.Fatal(err)
		} else if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			s.t.Fatal(err)
		}
	}
}

// remove removes a file with the specified relative path from a root.
func (s *gitModeSession) remove(root, name string) {
	s.t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
		s.t.Fatal(err)
	}
}

// expect verifies the contents of files in a root, specified as a map from
// their slash-separated relative paths to their expected contents.
func (s *gitModeSession) expect(root string, files map[string]string) {
	s.t.Helper()
	for name, expected := range files {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			s.t.Errorf("unable to read %s: %v", name, err)
		} else if string(contents) != expected {
			s.t.Errorf("incorrect contents for %s: %q != %q", name, contents, expected)
		}
	}
}

// expectMissing verifies that the specified paths don't exist in a root.
func (s *gitModeSession) expectMissing(root string, names ...string) {
	s.t.Helper()
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); err == nil {
			s.t.Errorf("%s unexpectedly exists", name)
		} else if !os.IsNotExist(err) {
			s.t.Errorf("unable to query %s: %v", name, err)
		}
	}
}

// waitForRemoteCommands waits for beta to be advertised as available for remote
// commands, whose channel connects separately from synchronization.
func (s *gitModeSession) waitForRemoteCommands() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		available := false
		connection, _, err := (&remote.Selection{Workspace: true}).Submit(ctx, s.alpha, &remote.Submission{Operation: remote.OperationNodes})
		if err == nil {
			if graph, err := remote.ListNodes(ctx, connection); err == nil {
				for _, node := range graph.Nodes {
					available = available || node.Name == "beta" && node.Available
				}
			}
		}
		if available {
			return
		} else if ctx.Err() != nil {
			s.t.Fatal("remote command channel did not connect:", ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForIdle waits for a session that's watching for changes to stop
// performing synchronization cycles, which takes longer than the polling
// interval. There's no way to observe idleness directly, so this watches the
// number of cycles, and there's only a limited number of them to allow before
// deciding that the session isn't going to become idle. It then verifies that
// the session stays that way.
func (s *gitModeSession) waitForIdle() {
	s.t.Helper()
	idleWindow := 2500 * time.Millisecond
	cycles := s.state().SuccessfulCycles
	for attempts, settled := 0, false; !settled; attempts++ {
		if attempts == 4 {
			s.t.Fatalf("session isn't becoming idle: %d cycles after %d", s.state().SuccessfulCycles, cycles)
		}
		time.Sleep(idleWindow)
		current := s.state().SuccessfulCycles
		settled = current == cycles
		cycles = current
	}
	time.Sleep(idleWindow)
	if current := s.state().SuccessfulCycles; current != cycles {
		s.t.Errorf("session woken while idle: %d cycles after %d", current, cycles)
	}
}

// gitModeRuleSides describes which endpoint a rule is added to for tests, along
// with the opposite endpoint.
var gitModeRuleSides = []struct {
	name string
	// roots returns the root that gets the rule and the opposite root.
	roots func(*gitModeSession) (string, string)
	// alphaGetsRule indicates whether or not alpha gets the rule.
	alphaGetsRule bool
}{
	{"alpha", func(s *gitModeSession) (string, string) { return s.alpha, s.beta }, true},
	{"beta", func(s *gitModeSession) (string, string) { return s.beta, s.alpha }, false},
}

// TestGitModeRuleDeletesNothing tests that adding a rule to one endpoint
// doesn't delete content on the other, as a naive three-way merge would, in any
// mode and in either direction, and that ignored content is left alone from
// then on.
func TestGitModeRuleDeletesNothing(t *testing.T) {
	ignored := map[string]string{"data/file": "data", "pkg/build/output": "output", "secret.txt": "secret"}
	for _, mode := range gitModeSynchronizationModes {
		for _, side := range gitModeRuleSides {
			t.Run(mode.Description()+"/"+side.name, func(t *testing.T) {
				// Create a session and let it synchronize everything.
				alphaContent := map[string]string{"keep": "keep", "pkg/source": "source"}
				for name, contents := range ignored {
					alphaContent[name] = contents
				}
				session := newGitModeSession(t, gitModeOptions{mode: mode, alpha: alphaContent})
				session.flush()
				session.expect(session.beta, alphaContent)
				unidirectional := mode == core.SynchronizationMode_SynchronizationModeOneWaySafe ||
					mode == core.SynchronizationMode_SynchronizationModeOneWayReplica
				propagates := side.alphaGetsRule || !unidirectional

				// Add a rule to one endpoint. In each case, the ignored content
				// has to remain on both endpoints, including after the rule is
				// (or would be) delivered to the opposite endpoint.
				ruleRoot, oppositeRoot := side.roots(session)
				session.write(ruleRoot, map[string]string{".gitignore": "data/\nbuild/\nsecret.txt\n"})
				for i := 0; i < 2; i++ {
					session.flush()
					session.expect(ruleRoot, ignored)
					session.expect(oppositeRoot, ignored)
				}
				if propagates {
					session.expect(oppositeRoot, map[string]string{".gitignore": "data/\nbuild/\nsecret.txt\n"})
				}

				// Everything else continues to be synchronized.
				session.write(session.alpha, map[string]string{"pkg/source": "changed source", "new": "new"})
				session.flush()
				session.expect(session.beta, map[string]string{"pkg/source": "changed source", "new": "new"})

				// Bidirectionally, changes to ignored content stay where they
				// are. Unidirectionally, the session's own mirroring may have
				// removed the rule from beta, so that content is tracked again
				// there.
				if propagates {
					session.write(oppositeRoot, map[string]string{"data/file": "changed", "data/added": "added"})
					session.flush()
					session.expect(ruleRoot, map[string]string{"data/file": "data"})
					session.expectMissing(ruleRoot, "data/added")
					session.expect(oppositeRoot, map[string]string{"data/file": "changed", "data/added": "added"})
				}
			})
		}
	}
}

// TestGitModeRuleRemoval tests that content starts being synchronized again
// once a rule is removed, in either direction.
func TestGitModeRuleRemoval(t *testing.T) {
	for _, side := range gitModeRuleSides {
		t.Run(side.name, func(t *testing.T) {
			// Synchronize content using a rule.
			session := newGitModeSession(t, gitModeOptions{alpha: map[string]string{
				"keep":      "keep",
				"data/file": "data",
			}})
			session.flush()
			ruleRoot, oppositeRoot := side.roots(session)
			session.write(ruleRoot, map[string]string{".gitignore": "data/\n"})
			session.flush()
			session.expect(oppositeRoot, map[string]string{".gitignore": "data/\n"})

			// Create new ignored content on each endpoint, which stays there.
			session.write(ruleRoot, map[string]string{"data/from-rule-side": "first"})
			session.write(oppositeRoot, map[string]string{"data/from-opposite-side": "second"})
			session.flush()
			session.expectMissing(ruleRoot, "data/from-opposite-side")
			session.expectMissing(oppositeRoot, "data/from-rule-side")

			// Remove the rule from one endpoint. A single flush delivers the
			// removal and then synchronizes the content that it exposes. That
			// content is the same on both endpoints isn't a conflict.
			session.remove(ruleRoot, ".gitignore")
			session.flush()
			session.expectMissing(oppositeRoot, ".gitignore")
			both := map[string]string{
				"data/file":               "data",
				"data/from-rule-side":     "first",
				"data/from-opposite-side": "second",
			}
			session.expect(ruleRoot, both)
			session.expect(oppositeRoot, both)

			// Everything is tracked again, including deletions.
			session.remove(ruleRoot, "data/file")
			session.flush()
			session.expectMissing(oppositeRoot, "data/file")
		})
	}
}

// TestGitModeRelaxedRule tests that a single flush synchronizes the content
// that a relaxed rule exposes, in either direction, while what's still ignored
// stays where it is.
func TestGitModeRelaxedRule(t *testing.T) {
	for _, side := range gitModeRuleSides {
		t.Run(side.name, func(t *testing.T) {
			// Create ignored content on both endpoints.
			session := newGitModeSession(t, gitModeOptions{alpha: map[string]string{
				".gitignore": "data/\n*.bin\n",
				"keep":       "keep",
			}})
			session.flush()
			ruleRoot, oppositeRoot := side.roots(session)
			session.write(ruleRoot, map[string]string{"data/from-rule-side": "first", "weights.bin": "weights"})
			session.write(oppositeRoot, map[string]string{"data/from-opposite-side": "second"})
			session.flush()
			session.expectMissing(ruleRoot, "data/from-opposite-side")
			session.expectMissing(oppositeRoot, "data/from-rule-side", "weights.bin")

			// Relax the rule on one endpoint.
			session.write(ruleRoot, map[string]string{".gitignore": "*.bin\n"})
			session.flush()
			both := map[string]string{
				".gitignore":              "*.bin\n",
				"data/from-rule-side":     "first",
				"data/from-opposite-side": "second",
			}
			session.expect(ruleRoot, both)
			session.expect(oppositeRoot, both)
			session.expectMissing(oppositeRoot, "weights.bin")
		})
	}
}

// TestGitModeRelaxedRuleExposesRules tests a relaxed rule that exposes another
// rule file, which has to be delivered in the cycle after the one that delivers
// the relaxed rule. Nothing can be lost, and the rules take effect first, but
// since a flush is only held for a single cycle, what they don't ignore may
// need another flush in a session that isn't watching for changes.
func TestGitModeRelaxedRuleExposesRules(t *testing.T) {
	session := newGitModeSession(t, gitModeOptions{alpha: map[string]string{
		".gitignore": "sub/\n",
		"keep":       "keep",
	}})
	session.flush()
	session.write(session.alpha, map[string]string{
		"sub/.gitignore": "*.bin\n",
		"sub/file":       "file",
		"sub/weights":    "weights",
	})
	session.write(session.beta, map[string]string{"sub/weights.bin": "beta weights"})
	session.flush()
	session.expectMissing(session.beta, "sub/.gitignore", "sub/file")
	session.expectMissing(session.alpha, "sub/weights.bin")

	// Remove the rule that was hiding the nested rule file. Once that has been
	// delivered, the nested one has to arrive before anything else can move.
	session.remove(session.alpha, ".gitignore")
	session.flush()
	session.expectMissing(session.beta, ".gitignore")
	session.expect(session.beta, map[string]string{"sub/.gitignore": "*.bin\n", "sub/weights.bin": "beta weights"})
	session.expectMissing(session.alpha, "sub/weights.bin")

	// Everything has settled after another flush.
	session.flush()
	session.expect(session.beta, map[string]string{"sub/file": "file", "sub/weights": "weights", "sub/weights.bin": "beta weights"})
	session.expectMissing(session.alpha, "sub/weights.bin")
}

// TestGitModePreexistingContentNotMirrored tests that content that's already on
// beta isn't deleted because it's missing from alpha, when alpha has a rule
// that ignores it, in the mode that deletes whatever is missing from alpha.
func TestGitModePreexistingContentNotMirrored(t *testing.T) {
	tests := []struct {
		// name is the name of the test.
		name string
		// remoteBeta indicates that beta should be reached through the remote
		// endpoint implementation.
		remoteBeta bool
		// alpha are the initial contents of alpha.
		alpha map[string]string
		// beta are the initial contents of beta.
		beta map[string]string
		// surviving are the expected contents of beta, in addition to alpha's.
		surviving map[string]string
		// removed are the paths on beta that are expected to be removed.
		removed []string
	}{
		{
			name:      "root rule",
			alpha:     map[string]string{".gitignore": "data/\n", "keep": "keep"},
			beta:      map[string]string{"data/file": "data", "data/deep/file": "deep", "other": "other"},
			surviving: map[string]string{"data/file": "data", "data/deep/file": "deep"},
			removed:   []string{"other"},
		},
		{
			name:       "root rule through remote endpoint",
			remoteBeta: true,
			alpha:      map[string]string{".gitignore": "data/\n", "keep": "keep"},
			beta:       map[string]string{"data/file": "data", "data/deep/file": "deep", "other": "other"},
			surviving:  map[string]string{"data/file": "data", "data/deep/file": "deep"},
			removed:    []string{"other"},
		},
		{
			name:      "nested rule",
			alpha:     map[string]string{"sub/.gitignore": "data/\n", "sub/keep": "keep"},
			beta:      map[string]string{"sub/data/file": "data", "sub/other": "other"},
			surviving: map[string]string{"sub/data/file": "data"},
			removed:   []string{"sub/other"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := newGitModeSession(t, gitModeOptions{
				mode:       core.SynchronizationMode_SynchronizationModeOneWayReplica,
				remoteBeta: test.remoteBeta,
				alpha:      test.alpha,
				beta:       test.beta,
			})
			session.flush()
			session.expect(session.beta, test.alpha)
			session.expect(session.beta, test.surviving)
			session.expectMissing(session.beta, test.removed...)
			for name := range test.surviving {
				session.expectMissing(session.alpha, name)
			}

			// The content also survives subsequent cycles.
			session.flush()
			session.expect(session.beta, test.surviving)
		})
	}
}

// TestGitModeRuleAddedWithDeletion tests that adding a rule to an endpoint
// along with deleting what it ignores doesn't delete that content on the
// opposite endpoint, in any mode where the opposite endpoint would otherwise
// mirror the deletion.
func TestGitModeRuleAddedWithDeletion(t *testing.T) {
	for _, mode := range gitModeSynchronizationModes {
		for _, side := range gitModeRuleSides {
			unidirectional := mode == core.SynchronizationMode_SynchronizationModeOneWaySafe ||
				mode == core.SynchronizationMode_SynchronizationModeOneWayReplica
			if unidirectional && !side.alphaGetsRule {
				continue
			}
			t.Run(mode.Description()+"/"+side.name, func(t *testing.T) {
				// Create a session and let it synchronize everything.
				session := newGitModeSession(t, gitModeOptions{mode: mode, alpha: map[string]string{
					"keep":      "keep",
					"data/file": "data",
				}})
				session.flush()
				session.expect(session.beta, map[string]string{"data/file": "data"})

				// Delete the content and add the rule, both before the next
				// cycle.
				ruleRoot, oppositeRoot := side.roots(session)
				if err := os.RemoveAll(filepath.Join(ruleRoot, "data")); err != nil {
					t.Fatal(err)
				}
				session.write(ruleRoot, map[string]string{".gitignore": "data/\n"})
				for i := 0; i < 2; i++ {
					session.flush()
					session.expect(oppositeRoot, map[string]string{".gitignore": "data/\n", "data/file": "data", "keep": "keep"})
					session.expectMissing(ruleRoot, "data")
				}
			})
		}
	}
}

// TestGitModeBothDirectionsAtOnce tests that nothing is transferred if each
// endpoint gets a new rule ignoring content that exists only on the other, even
// though the rules and the content are found in the same synchronization cycle.
func TestGitModeBothDirectionsAtOnce(t *testing.T) {
	session := newGitModeSession(t, gitModeOptions{alpha: map[string]string{
		"one/keep": "keep",
		"two/keep": "keep",
	}})
	session.flush()
	session.write(session.alpha, map[string]string{"one/.gitignore": "data/\n", "two/data/file": "from alpha"})
	session.write(session.beta, map[string]string{"two/.gitignore": "data/\n", "one/data/file": "from beta"})
	session.flush()
	for _, root := range []string{session.alpha, session.beta} {
		session.expect(root, map[string]string{"one/.gitignore": "data/\n", "two/.gitignore": "data/\n"})
	}
	session.expect(session.alpha, map[string]string{"two/data/file": "from alpha"})
	session.expect(session.beta, map[string]string{"one/data/file": "from beta"})
	session.expectMissing(session.alpha, "one/data")
	session.expectMissing(session.beta, "two/data")
}

// TestGitModeFirstSyncNestedRules tests a first synchronization into an empty
// endpoint of content with nested rule files, which are delivered ahead of
// everything else: everything has to arrive, apart from what the rules ignore,
// and the session has to settle without conflicts or problems, in any mode, and
// whether or not the endpoint's root exists.
func TestGitModeFirstSyncNestedRules(t *testing.T) {
	synchronized := map[string]string{
		".gitignore":               "*.tmp\n",
		"top":                      "top",
		"sub/.gitignore":           "*.log\n",
		"sub/file":                 "file",
		"sub/deep/.gitignore":      "!keep.log\n",
		"sub/deep/file":            "deep file",
		"sub/deep/keep.log":        "kept",
		"plain/deep/file":          "plain",
		"only/rules/.gitignore":    "*.x\n",
		"only/rules/deep/content":  "content",
		"only/rules/deep/more/one": "one",
	}
	ignored := map[string]string{
		"skip.tmp":          "ignored",
		"sub/skip.log":      "ignored",
		"sub/deep/skip.tmp": "ignored",
		"only/rules/skip.x": "ignored",
	}
	endpoints := []struct {
		// name is the name of the endpoint configuration.
		name string
		// betaMissing indicates that beta's root shouldn't exist.
		betaMissing bool
		// remoteBeta indicates that beta should be reached through the remote
		// endpoint implementation.
		remoteBeta bool
	}{
		{name: "empty root"},
		{name: "missing root", betaMissing: true},
		{name: "empty root through remote endpoint", remoteBeta: true},
		{name: "missing root through remote endpoint", betaMissing: true, remoteBeta: true},
	}
	for _, mode := range gitModeSynchronizationModes {
		for _, endpoint := range endpoints {
			t.Run(mode.Description()+"/"+endpoint.name, func(t *testing.T) {
				// Create the session with content on alpha alone.
				session := newGitModeSession(t, gitModeOptions{
					mode:        mode,
					remoteBeta:  endpoint.remoteBeta,
					betaMissing: endpoint.betaMissing,
					alpha:       synchronized,
				})
				session.write(session.alpha, ignored)
				empty := filepath.Join("plain", "empty", "directory")
				if err := os.MkdirAll(filepath.Join(session.alpha, empty), 0700); err != nil {
					t.Fatal(err)
				}

				// A single flush transfers everything that isn't ignored.
				session.flush()
				session.expect(session.alpha, synchronized)
				session.expect(session.alpha, ignored)
				session.expect(session.beta, synchronized)
				for name := range ignored {
					session.expectMissing(session.beta, name)
				}
				if info, err := os.Lstat(filepath.Join(session.beta, empty)); err != nil {
					t.Error("unable to query empty directory:", err)
				} else if !info.IsDir() {
					t.Error("empty directory isn't a directory")
				}

				// The session has settled. It took one cycle to deliver the
				// rules and one for everything else, and the next flush takes
				// a single cycle, since there's nothing to do.
				state := session.state()
				if state.SuccessfulCycles != 2 {
					t.Errorf("incorrect cycle count: %d", state.SuccessfulCycles)
				}
				session.flush()
				state = session.state()
				if state.SuccessfulCycles != 3 {
					t.Errorf("incorrect cycle count after settling: %d", state.SuccessfulCycles)
				}
				if len(state.AlphaState.ScanProblems) > 0 {
					t.Errorf("unexpected alpha scan problems: %v", state.AlphaState.ScanProblems)
				}
				if len(state.BetaState.ScanProblems) > 0 {
					t.Errorf("unexpected beta scan problems: %v", state.BetaState.ScanProblems)
				}
			})
		}
	}
}

// TestGitModeNestedRules tests that .gitignore files apply within their own
// directories, and that they take precedence over those above them.
func TestGitModeNestedRules(t *testing.T) {
	session := newGitModeSession(t, gitModeOptions{alpha: map[string]string{
		".gitignore":          "*.tmp\n",
		"top.tmp":             "ignored",
		"top.log":             "synchronized",
		"local":               "synchronized",
		"other/nested.log":    "synchronized",
		"other/nested.tmp":    "ignored",
		"sub/.gitignore":      "*.log\n/local\n!important.tmp\n",
		"sub/a.log":           "ignored",
		"sub/deep/b.log":      "ignored",
		"sub/deep/local":      "synchronized",
		"sub/local":           "ignored",
		"sub/kept":            "synchronized",
		"sub/important.tmp":   "synchronized",
		"sub/unimportant.tmp": "ignored",
	}})
	session.flush()
	session.expect(session.beta, map[string]string{
		".gitignore":        "*.tmp\n",
		"top.log":           "synchronized",
		"local":             "synchronized",
		"other/nested.log":  "synchronized",
		"sub/.gitignore":    "*.log\n/local\n!important.tmp\n",
		"sub/deep/local":    "synchronized",
		"sub/kept":          "synchronized",
		"sub/important.tmp": "synchronized",
	})
	session.expectMissing(session.beta,
		"top.tmp", "other/nested.tmp", "sub/a.log", "sub/deep/b.log", "sub/local", "sub/unimportant.tmp",
	)

	// A rule file arriving in a new directory on the opposite endpoint protects
	// the content there that's already ignored.
	session.write(session.beta, map[string]string{"second/data.bin": "beta data", "second/kept": "beta kept"})
	session.write(session.alpha, map[string]string{"second/.gitignore": "*.bin\n", "second/kept": "beta kept"})
	session.flush()
	session.expect(session.beta, map[string]string{"second/.gitignore": "*.bin\n", "second/data.bin": "beta data"})
	session.expectMissing(session.alpha, "second/data.bin")
}

// TestGitModeSessionIgnoresOverride tests that the session's ignores override
// rule files, both to synchronize content that a rule would ignore and to
// ignore content that a rule wouldn't.
func TestGitModeSessionIgnoresOverride(t *testing.T) {
	session := newGitModeSession(t, gitModeOptions{
		ignores: []string{"!.env", "!secrets", "local"},
		alpha: map[string]string{
			".gitignore":      ".env\nother.env\nsecrets/\ndata/\n!local\n",
			".env":            "unignored",
			"other.env":       "ignored",
			"secrets/key":     "unignored",
			"data/file":       "ignored",
			"local":           "ignored by session",
			"sub/.env":        "unignored",
			"sub/secrets/key": "unignored",
		},
	})
	session.flush()
	session.expect(session.beta, map[string]string{
		".env":            "unignored",
		"secrets/key":     "unignored",
		"sub/.env":        "unignored",
		"sub/secrets/key": "unignored",
	})
	session.expectMissing(session.beta, "other.env", "data", "local")

	// Content that's been explicitly unignored is tracked as usual.
	session.remove(session.beta, ".env")
	session.flush()
	session.expectMissing(session.alpha, ".env")
}

// TestGitModeGitEntriesNotSynchronized tests that entries named .git, whether
// they're directories (as in repositories) or files (as in submodules and
// linked worktrees), are never synchronized, even if the session tries to allow
// it, and that endpoints' own entries don't interfere with each other.
func TestGitModeGitEntriesNotSynchronized(t *testing.T) {
	session := newGitModeSession(t, gitModeOptions{
		ignores: []string{"!.git"},
		alpha: map[string]string{
			".git/config":        "alpha config",
			"sub/.git":           "gitdir: ../.git/modules/sub\n",
			"sub/file":           "file",
			"directory/.GIT/any": "alpha upper",
			"directory/file":     "file",
		},
		beta: map[string]string{
			".git/config":        "beta config",
			"other/.git/objects": "beta objects",
			"other/file":         "file",
		},
	})
	session.flush()
	session.expect(session.beta, map[string]string{
		"sub/file":           "file",
		"directory/file":     "file",
		".git/config":        "beta config",
		"other/.git/objects": "beta objects",
	})
	session.expectMissing(session.beta, "sub/.git", "directory/.GIT")
	session.expect(session.alpha, map[string]string{
		"other/file":         "file",
		".git/config":        "alpha config",
		"sub/.git":           "gitdir: ../.git/modules/sub\n",
		"directory/.GIT/any": "alpha upper",
	})
	session.expectMissing(session.alpha, "other/.git")

	// Changes to them don't result in any changes on the opposite endpoint.
	session.write(session.alpha, map[string]string{".git/config": "changed", "sub/.git": "changed"})
	session.remove(session.alpha, "directory/.GIT/any")
	session.flush()
	session.expect(session.beta, map[string]string{".git/config": "beta config"})
	session.expectMissing(session.beta, "sub/.git")
}

// TestGitModeNestedRepositoryWithoutOrigin tests that nested repositories that
// can't be fetched, since they don't have an origin or any commits, are
// synchronized as ordinary directories, without their Git directories.
func TestGitModeNestedRepositoryWithoutOrigin(t *testing.T) {
	isolateGitMode(t)
	session := newGitModeSession(t, gitModeOptions{
		alpha: map[string]string{"local/file": "file", "new/file": "file"},
		prepare: func(session *gitModeSession) {
			local, created := filepath.Join(session.alpha, "local"), filepath.Join(session.alpha, "new")
			gitModeRun(t, local, "init", "--quiet")
			gitModeRun(t, local, "add", "--", "file")
			gitModeRun(t, local, "commit", "--quiet", "--no-gpg-sign", "-m", "commit")
			gitModeRun(t, created, "init", "--quiet")
			gitModeRun(t, created, "config", "remote.origin.url", "https://example.org/repository.git")
		},
	})
	session.flush()
	session.expect(session.beta, map[string]string{"local/file": "file", "new/file": "file"})
	session.expectMissing(session.beta, "local/.git", "new/.git")
	state := session.state()
	if len(state.AlphaState.ScanProblems) != 0 || len(state.BetaState.ScanProblems) != 0 {
		t.Errorf("unexpected problems: %v, %v", state.AlphaState.ScanProblems, state.BetaState.ScanProblems)
	}
}

// TestGitModePreexistingContentNotPulled tests that content that already exists
// on an endpoint, and that the opposite endpoint's rules would ignore, isn't
// transferred to the endpoint with those rules before they've been delivered.
// Since nothing else happens in the session, this also verifies that a single
// flush covers the cycle that delivers the rules and the next cycle, which
// performs the transitions that it dropped.
func TestGitModePreexistingContentNotPulled(t *testing.T) {
	rules := map[string]string{".gitignore": "data/\n*.bin\n"}
	withRules := func(contents map[string]string) map[string]string {
		for name, content := range rules {
			contents[name] = content
		}
		return contents
	}
	withData := map[string]string{"data/file": "data", "sub/weights.bin": "weights", "unignored": "unignored"}

	t.Run("beta has content", func(t *testing.T) {
		session := newGitModeSession(t, gitModeOptions{alpha: withRules(map[string]string{}), beta: withData})
		session.flush()
		session.expect(session.beta, rules)
		session.expect(session.beta, map[string]string{"data/file": "data", "sub/weights.bin": "weights"})
		session.expect(session.alpha, map[string]string{"unignored": "unignored"})
		session.expectMissing(session.alpha, "data", "sub/weights.bin")
	})

	t.Run("alpha has content", func(t *testing.T) {
		session := newGitModeSession(t, gitModeOptions{alpha: withData, beta: withRules(map[string]string{})})
		session.flush()
		session.expect(session.alpha, rules)
		session.expect(session.alpha, map[string]string{"data/file": "data", "sub/weights.bin": "weights"})
		session.expect(session.beta, map[string]string{"unignored": "unignored"})
		session.expectMissing(session.beta, "data", "sub/weights.bin")
	})

	t.Run("beta has content through remote endpoint", func(t *testing.T) {
		session := newGitModeSession(t, gitModeOptions{remoteBeta: true, alpha: withRules(map[string]string{}), beta: withData})
		session.flush()
		session.expect(session.beta, rules)
		session.expect(session.alpha, map[string]string{"unignored": "unignored"})
		session.expectMissing(session.alpha, "data", "sub/weights.bin")
	})

	t.Run("rule added after session starts", func(t *testing.T) {
		session := newGitModeSession(t, gitModeOptions{})
		session.flush()
		session.write(session.beta, withData)
		session.write(session.alpha, rules)
		session.flush()
		session.expect(session.beta, rules)
		session.expect(session.alpha, map[string]string{"unignored": "unignored"})
		session.expectMissing(session.alpha, "data", "sub/weights.bin")
	})
}

// TestGitModeWatching tests that a session that's watching for changes, rather
// than one driven by flushes, delivers rules and leaves content in place.
func TestGitModeWatching(t *testing.T) {
	isolateGitMode(t)
	session := newGitModeSession(t, gitModeOptions{
		watch: true,
		alpha: map[string]string{
			"keep":                           "keep",
			"data/file":                      "data",
			".git/index":                     "index",
			"sub/.mutagen-temporary-x/index": "index",
		},
		// Use a repository for which the directory is synchronized as usual, so
		// that its .git directory is as close as possible to a change.
		prepare: func(session *gitModeSession) {
			gitModeRun(t, filepath.Join(session.alpha, "sub"), "init", "--quiet")
		},
	})
	waitForFile := func(path, expected string) {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for {
			if contents, err := os.ReadFile(path); err == nil && string(contents) == expected {
				return
			} else if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %q, %v", path, contents, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForFile(filepath.Join(session.beta, "data", "file"), "data")

	// Add a rule, and wait for it to be delivered. Content that the rule
	// ignores must survive that, and the cycles that follow it.
	session.write(session.alpha, map[string]string{".gitignore": "data/\n"})
	waitForFile(filepath.Join(session.beta, ".gitignore"), "data/\n")
	session.flush()
	session.expect(session.alpha, map[string]string{"data/file": "data"})
	session.expect(session.beta, map[string]string{"data/file": "data"})

	// Changes to ignored content must not be propagated, but others must.
	session.write(session.beta, map[string]string{"data/file": "changed", "later": "later"})
	waitForFile(filepath.Join(session.alpha, "later"), "later")
	session.expect(session.alpha, map[string]string{"data/file": "data"})

	// Wait for the session to become idle, which takes longer than the polling
	// interval. We can't observe idleness directly, so we watch the number of
	// synchronization cycles.
	idleWindow := 2500 * time.Millisecond
	cycles := session.state().SuccessfulCycles
	for settled := false; !settled; {
		time.Sleep(idleWindow)
		current := session.state().SuccessfulCycles
		settled = current == cycles
		cycles = current
	}

	// Activity in Git and temporary directories, which already exist at this
	// point, must not wake the session. With recursive watching, this is due to
	// filtering, and with poll-based watching, it's because it isn't scanned.
	session.write(session.alpha, map[string]string{
		".git/index":                            "changed",
		".git/index.lock":                       "lock",
		".git/FETCH_HEAD":                       "fetched",
		".git/objects/ab/cdef":                  "object",
		".git/logs/HEAD":                        "log",
		"sub/.git/index":                        "changed",
		"sub/.git/modules/x/index":              "index",
		"sub/.mutagen-temporary-x/index":        "changed",
		"sub/.mutagen-temporary-x/objects/file": "object",
	})
	time.Sleep(idleWindow)
	if current := session.state().SuccessfulCycles; current != cycles {
		t.Errorf("session woken by repository activity: %d cycles after %d", current, cycles)
	}
	session.expectMissing(session.beta, ".git", "sub/.git", "sub/.mutagen-temporary-x")
}

// TestGitModeRootSafety tests that the check for an emptied root still works if
// the root contains a repository, and that a rule that hides everything in the
// root doesn't look like the root has been emptied.
func TestGitModeRootSafety(t *testing.T) {
	content := map[string]string{"first": "first", "second": "second"}

	t.Run("emptied except for repository", func(t *testing.T) {
		session := newGitModeSession(t, gitModeOptions{alpha: map[string]string{
			".git/config": "config",
			"first":       "first",
			"second":      "second",
		}})
		session.flush()
		session.expect(session.beta, content)
		session.remove(session.alpha, "first")
		session.remove(session.alpha, "second")

		// The flush fails because synchronization halts.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := synchronizationManager.Flush(ctx, session.selection, "", false); err == nil {
			t.Error("flush succeeded unexpectedly")
		}
		session.waitForState(func(state *synchronization.State) bool {
			return state.Status == synchronization.Status_HaltedOnRootEmptied
		})
		session.expect(session.beta, content)
	})

	t.Run("hidden by rule", func(t *testing.T) {
		session := newGitModeSession(t, gitModeOptions{alpha: content})
		session.flush()
		session.expect(session.beta, content)
		session.write(session.alpha, map[string]string{".gitignore": "*\n"})
		session.flush()
		session.expect(session.alpha, content)
		session.expect(session.beta, content)
		if status := session.state().Status; status == synchronization.Status_HaltedOnRootEmptied {
			t.Error("session halted on root being hidden by rule")
		}
	})
}

// runGitInRepository runs Git in a directory for test setup purposes, without
// being affected by any environment variables that may be set for a repository
// in which the test itself is being run, such as those set by Git hooks.
func runGitInRepository(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("unable to run Git with arguments %v: %v: %s", args, err, output)
	}
}

// problemErrors returns a map from problem paths to their messages.
func problemErrors(problems []*core.Problem) map[string]string {
	errors := make(map[string]string, len(problems))
	for _, problem := range problems {
		errors[problem.Path] = problem.Error
	}
	return errors
}

// isolateGitMode sets up the environment of the Git commands run by a test's
// endpoints, including those of agents (which is why it uses the environment),
// enabling the file transport and insulating them from the user's
// configuration. It skips the test if Git isn't available.
func isolateGitMode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is not available:", err)
	}
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// gitModeRun runs Git in a directory for test setup purposes, which must
// succeed, and returns its output without surrounding whitespace. The command
// isn't affected by the environment variables that may be set for a repository
// in which the test is being run or by the user's configuration, such as for
// signing commits.
func gitModeRun(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env,
		"GIT_ALLOW_PROTOCOL=file", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.org",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.org",
	)
	var errorOutput strings.Builder
	command.Stderr = &errorOutput
	output, err := command.Output()
	if err != nil {
		t.Fatalf("unable to run Git with arguments %v in %s: %v: %s", args, directory, err, errorOutput.String())
	}
	return strings.TrimSpace(string(output))
}

// gitModeProblem returns the problem at the specified path, failing the test if
// there isn't one, and verifies that its message contains the specified text.
func gitModeProblem(t *testing.T, problems []*core.Problem, path string, contents ...string) *core.Problem {
	t.Helper()
	for _, problem := range problems {
		if problem.Path == path {
			for _, content := range contents {
				if !strings.Contains(problem.Error, content) {
					t.Errorf("problem doesn't contain %q: %s", content, problem.Error)
				}
			}
			return problem
		}
	}
	t.Fatalf("no problem at %q: %v", path, problems)
	return nil
}

// gitSubmoduleWatchModes describes the watch modes for tests with sessions that
// are watching for changes. Each endpoint picks the kind of watching that's
// available on its system, so the portable mode is either recursive or
// non-recursive watching, the other of which can only be skipped.
var gitSubmoduleWatchModes = []struct {
	// name is the name of the mode.
	name string
	// mode is the watch mode for a session.
	mode synchronization.WatchMode
	// recursive indicates whether or not recursive watching is required, or,
	// otherwise, that its absence is required, if the mode is the portable
	// mode.
	recursive bool
}{
	{name: "poll", mode: synchronization.WatchMode_WatchModeForcePoll},
	{name: "non-recursive", mode: synchronization.WatchMode_WatchModePortable},
	{name: "recursive", mode: synchronization.WatchMode_WatchModePortable, recursive: true},
}

// excludeOnBeta returns a prepare hook for a session that makes beta's root a
// repository whose .git/info/exclude has the specified rules. That file isn't
// synchronized, so these rules only ever apply on beta.
func excludeOnBeta(rules string) func(*gitModeSession) {
	return func(s *gitModeSession) {
		runGitInRepository(s.t, s.beta, "init", "--quiet")
		s.write(s.beta, map[string]string{".git/info/exclude": rules})
	}
}

// verifyExclusionWarning verifies that a session's only problem is a warning on
// beta about the specified path, which its rules exclude.
func verifyExclusionWarning(t *testing.T, state *synchronization.State, path string) {
	t.Helper()
	if len(state.Conflicts) != 0 {
		t.Errorf("unexpected conflicts: %v", state.Conflicts)
	}
	if len(state.AlphaState.TransitionProblems) != 0 {
		t.Errorf("unexpected alpha transition problems: %v", state.AlphaState.TransitionProblems)
	}
	if len(state.AlphaState.ScanProblems) != 0 || len(state.BetaState.ScanProblems) != 0 {
		t.Errorf("unexpected scan problems: %v, %v", state.AlphaState.ScanProblems, state.BetaState.ScanProblems)
	}
	if len(state.BetaState.TransitionProblems) != 1 {
		t.Fatalf("incorrect beta transition problems: %v", state.BetaState.TransitionProblems)
	}
	problem := gitModeProblem(t, state.BetaState.TransitionProblems, path, "ignored by this endpoint's own rules", `"!`+path+`"`)
	if !problem.Warning {
		t.Error("exclusion not reported as a warning:", problem)
	}
}

// TestGitModeExcludedRuleFile tests that a rule file for a directory that the
// other endpoint excludes doesn't hold up the session, which it would otherwise
// do in every cycle, since cycles with rules to deliver only do that. It's
// delivered, after which the directory is simply untracked on that endpoint.
func TestGitModeExcludedRuleFile(t *testing.T) {
	isolateGitMode(t)
	for _, mode := range gitModeSynchronizationModes {
		t.Run(mode.Description(), func(t *testing.T) {
			session := newGitModeSession(t, gitModeOptions{
				mode:    mode,
				alpha:   map[string]string{"outputs/.gitignore": "*.tmp\n", "outputs/data": "data", "public": "public"},
				prepare: excludeOnBeta("outputs/\n"),
			})

			// The first flush delivers everything, including what the rule file
			// was holding up, without anything to report. Nothing changes after
			// that, and nothing else is written to the excluded directory.
			for i := 0; i < 3; i++ {
				session.flush()
				session.expect(session.beta, map[string]string{"outputs/.gitignore": "*.tmp\n", "public": "public"})
				session.expectMissing(session.beta, "outputs/data")
				session.expect(session.alpha, map[string]string{"outputs/.gitignore": "*.tmp\n", "outputs/data": "data", "public": "public"})
				state := session.state()
				if len(state.AlphaState.ScanProblems) != 0 || len(state.BetaState.ScanProblems) != 0 {
					t.Errorf("unexpected scan problems: %v, %v", state.AlphaState.ScanProblems, state.BetaState.ScanProblems)
				}
			}

			// Other changes continue to be synchronized.
			session.write(session.alpha, map[string]string{"later": "later"})
			session.flush()
			session.expect(session.beta, map[string]string{"later": "later"})
		})
	}
}

// TestGitModeExcludedSibling tests that a path that the receiving endpoint
// excludes doesn't keep the rest of a new directory from arriving, and that
// it's reported as a warning, by every cycle, on the path itself.
func TestGitModeExcludedSibling(t *testing.T) {
	isolateGitMode(t)
	for _, mode := range gitModeSynchronizationModes {
		t.Run(mode.Description(), func(t *testing.T) {
			alphaContent := map[string]string{"pkg/a.go": "a", "pkg/b.go": "b", "pkg/debug.log": "log"}
			session := newGitModeSession(t, gitModeOptions{mode: mode, alpha: alphaContent, prepare: excludeOnBeta("*.log\n")})
			for i := 0; i < 3; i++ {
				verifyExclusionWarning(t, session.flushAllowingProblems(), "pkg/debug.log")
				session.expect(session.beta, map[string]string{"pkg/a.go": "a", "pkg/b.go": "b"})
				session.expectMissing(session.beta, "pkg/debug.log")
				session.expect(session.alpha, alphaContent)
			}

			// Other changes in the directory continue to be synchronized.
			session.write(session.alpha, map[string]string{"pkg/c.go": "c", "pkg/a.go": "changed"})
			verifyExclusionWarning(t, session.flushAllowingProblems(), "pkg/debug.log")
			session.expect(session.beta, map[string]string{"pkg/a.go": "changed", "pkg/c.go": "c"})

			// The warning goes away with the path that it's about.
			session.remove(session.alpha, "pkg/debug.log")
			session.flush()
		})
	}
}

// TestGitModeExclusionSkew tests the treatment of a file that only one endpoint
// excludes: it isn't copied to that endpoint, for which there's a warning if it
// doesn't have the file, and there's nothing to report if it does.
func TestGitModeExclusionSkew(t *testing.T) {
	isolateGitMode(t)
	for _, mode := range gitModeSynchronizationModes {
		t.Run(mode.Description(), func(t *testing.T) {
			t.Run("absent", func(t *testing.T) {
				session := newGitModeSession(t, gitModeOptions{
					mode:    mode,
					alpha:   map[string]string{"secret.env": "secret", "public": "public"},
					prepare: excludeOnBeta("secret.env\n"),
				})
				for i := 0; i < 2; i++ {
					verifyExclusionWarning(t, session.flushAllowingProblems(), "secret.env")
					session.expect(session.alpha, map[string]string{"secret.env": "secret"})
					session.expect(session.beta, map[string]string{"public": "public"})
					session.expectMissing(session.beta, "secret.env")
				}
			})
			t.Run("present", func(t *testing.T) {
				session := newGitModeSession(t, gitModeOptions{
					mode:    mode,
					alpha:   map[string]string{"secret.env": "secret", "public": "public"},
					beta:    map[string]string{"secret.env": "other"},
					prepare: excludeOnBeta("secret.env\n"),
				})
				for i := 0; i < 2; i++ {
					session.flush()
					state := session.state()
					if len(state.AlphaState.ScanProblems) != 0 || len(state.BetaState.ScanProblems) != 0 {
						t.Errorf("unexpected scan problems: %v, %v", state.AlphaState.ScanProblems, state.BetaState.ScanProblems)
					}
					session.expect(session.alpha, map[string]string{"secret.env": "secret"})
					session.expect(session.beta, map[string]string{"public": "public", "secret.env": "other"})
				}
			})
		})
	}
}

// TestGitModeExclusionNoHotLoop tests that a refused path and a delivered rule
// file don't result in further synchronization cycles, without any changes, in
// a session that's watching for changes, which is noticed by cycles being
// counted.
func TestGitModeExclusionNoHotLoop(t *testing.T) {
	isolateGitMode(t)
	for _, mode := range gitSubmoduleWatchModes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.mode == synchronization.WatchMode_WatchModePortable && mode.recursive != watching.RecursiveWatchingSupported {
				t.Skip("this kind of watching isn't what this system uses")
			}
			session := newGitModeSession(t, gitModeOptions{
				watch:     true,
				watchMode: mode.mode,
				alpha:     map[string]string{"outputs/.gitignore": "*.tmp\n", "pkg/a.go": "a", "pkg/debug.log": "log"},
				prepare:   excludeOnBeta("outputs/\n*.log\n"),
			})

			// The session wakes up on its own. Wait for all of the paths to
			// have been handled, then for it to stop.
			session.waitForState(func(state *synchronization.State) bool {
				return len(state.BetaState.TransitionProblems) == 1 && state.BetaState.TransitionProblems[0].Path == "pkg/debug.log"
			})
			session.waitForIdle()
			verifyExclusionWarning(t, session.state(), "pkg/debug.log")
			session.expect(session.beta, map[string]string{"outputs/.gitignore": "*.tmp\n", "pkg/a.go": "a"})
			session.expectMissing(session.beta, "pkg/debug.log")

			// It isn't stuck, though.
			session.write(session.alpha, map[string]string{"pkg/b.go": "b"})
			session.flushAllowingProblems()
			session.expect(session.beta, map[string]string{"pkg/b.go": "b"})
		})
	}
}

// TestGitModeExclusionThroughRemote tests the same behavior between an endpoint
// and an agent, and that the remote command line reports a warning without
// preventing commands, whatever endpoint it's reported by.
func TestGitModeExclusionThroughRemote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("commands require a POSIX shell")
	}
	isolateGitMode(t)
	session := newGitModeSession(t, gitModeOptions{
		remoteBeta: true,
		commands:   synchronization.RemoteCommandMode_RemoteCommandModeBeta,
		alpha:      map[string]string{"pkg/a.go": "a", "pkg/debug.log": "log"},
		prepare:    excludeOnBeta("*.log\n"),
	})
	session.waitForRemoteCommands()
	code, output, diagnostics := runRemote(t, session.alpha, "--timeout", "30s", "cat", "pkg/a.go")
	if code != 0 || output != "a" {
		t.Errorf("command failed: %d: %q: %s", code, output, diagnostics)
	}
	for _, content := range []string{"remote: warning:", "pkg/debug.log (remote): ignored by this endpoint's own rules"} {
		if !strings.Contains(diagnostics, content) {
			t.Errorf("command's diagnostics don't contain %q: %s", content, diagnostics)
		}
	}
	session.expectMissing(session.beta, "pkg/debug.log")
}

// TestGitModeTrackedIgnoredWarnings tests that files that are tracked by Git
// but matched by rules result in scan problems on the endpoint with the
// repository, without preventing synchronization, including for remote
// endpoints.
func TestGitModeTrackedIgnoredWarnings(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is not available:", err)
	}
	files := map[string]string{
		".gitignore":      "*.o\nbuild/\n",
		"main.c":          "main",
		"tracked.o":       "tracked",
		"untracked.o":     "untracked",
		"build/out/first": "first",
	}
	setup := func(t *testing.T, session *gitModeSession, root string) {
		t.Helper()
		runGitInRepository(t, root, "init", "--quiet")
		for _, name := range []string{"main.c", "tracked.o", "build/out/first"} {
			runGitInRepository(t, root, "add", "--force", "--", name)
		}
	}
	verify := func(t *testing.T, session *gitModeSession, repository, other string, repositoryProblems, otherProblems []*core.Problem) {
		t.Helper()

		// Verify the problems.
		errors := problemErrors(repositoryProblems)
		if len(errors) != 2 {
			t.Errorf("incorrect number of scan problems: %v", repositoryProblems)
		}
		if message := errors["tracked.o"]; !strings.Contains(message, "tracked by Git") || !strings.Contains(message, `"!tracked.o"`) {
			t.Errorf("incorrect problem for file: %q", message)
		}
		if message := errors["build"]; !strings.Contains(message, "tracked by Git") || !strings.Contains(message, `"!build"`) || !strings.Contains(message, "whole directory") {
			t.Errorf("incorrect problem for directory: %q", message)
		}
		if len(otherProblems) != 0 {
			t.Errorf("unexpected scan problems on endpoint without repository: %v", otherProblems)
		}

		// Verify that synchronization proceeded otherwise.
		session.expect(other, map[string]string{".gitignore": "*.o\nbuild/\n", "main.c": "main"})
		session.expectMissing(other, "tracked.o", "untracked.o", "build", ".git")
	}

	t.Run("alpha", func(t *testing.T) {
		session := newGitModeSession(t, gitModeOptions{alpha: files})
		setup(t, session, session.alpha)
		session.flush()
		state := session.state()
		verify(t, session, session.alpha, session.beta, state.AlphaState.ScanProblems, state.BetaState.ScanProblems)
	})

	t.Run("beta", func(t *testing.T) {
		session := newGitModeSession(t, gitModeOptions{beta: files})
		setup(t, session, session.beta)
		session.flush()
		state := session.state()
		verify(t, session, session.beta, session.alpha, state.BetaState.ScanProblems, state.AlphaState.ScanProblems)
	})

	t.Run("beta through remote endpoint", func(t *testing.T) {
		session := newGitModeSession(t, gitModeOptions{remoteBeta: true, beta: files})
		setup(t, session, session.beta)
		session.flush()
		state := session.state()
		verify(t, session, session.beta, session.alpha, state.BetaState.ScanProblems, state.AlphaState.ScanProblems)
	})

	t.Run("resolved using suggestions", func(t *testing.T) {
		session := newGitModeSession(t, gitModeOptions{ignores: []string{"!tracked.o", "!build"}, alpha: files})
		setup(t, session, session.alpha)
		session.flush()
		state := session.state()
		if len(state.AlphaState.ScanProblems) != 0 {
			t.Errorf("unexpected scan problems: %v", state.AlphaState.ScanProblems)
		}
		session.expect(session.beta, map[string]string{"tracked.o": "tracked", "build/out/first": "first"})
		session.expectMissing(session.beta, "untracked.o")
	})

	t.Run("problems are limited", func(t *testing.T) {
		many := map[string]string{".gitignore": "*.o\n"}
		var names []string
		for i := 0; i < 30; i++ {
			name := fmt.Sprintf("file%02d.o", i)
			many[name] = "content"
			names = append(names, name)
		}
		session := newGitModeSession(t, gitModeOptions{alpha: many})
		runGitInRepository(t, session.alpha, "init", "--quiet")
		runGitInRepository(t, session.alpha, append([]string{"add", "--force", "--"}, names...)...)
		session.flush()

		// Sessions report a limited number of problems as a matter of course.
		state := session.state()
		reported := uint64(len(state.AlphaState.ScanProblems)) + state.AlphaState.ExcludedScanProblems
		if len(state.AlphaState.ScanProblems) == 0 || reported > 30 {
			t.Errorf("incorrect scan problem count: %d", reported)
		}
		for _, problem := range state.AlphaState.ScanProblems {
			if problem.Path == "" {
				t.Errorf("problem without path: %v", problem)
			}
		}
	})
}

// TestGitModeSymbolicLinkReplaced tests that a session in a one-way replica mode
// replaces a symbolic link with the directory that alpha has at its path, on the
// first flush, without reading rules through the link, which can't work if it's
// a cycle, and which would otherwise apply the rules at the link's target.
func TestGitModeSymbolicLinkReplaced(t *testing.T) {
	isolateGitMode(t)
	for _, test := range []struct {
		// name is the name of the test.
		name string
		// target is the target of the symbolic link.
		target string
		// content is the content that alpha has beneath the path.
		content map[string]string
	}{
		{"cycle", "pkg", map[string]string{"pkg/file.txt": "content"}},
		{"cycle with rules", "pkg", map[string]string{"pkg/.gitignore": "*.tmp\n", "pkg/file.txt": "content"}},
		{"rules at target", "other", map[string]string{"pkg/file.txt": "content"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rules := map[string]string{"other/.gitignore": "file.txt\n"}
			alpha := map[string]string{"other/.gitignore": "file.txt\n"}
			for path, contents := range test.content {
				alpha[path] = contents
			}
			session := newGitModeSession(t, gitModeOptions{
				mode:  core.SynchronizationMode_SynchronizationModeOneWayReplica,
				alpha: alpha,
				beta:  rules,
				prepare: func(s *gitModeSession) {
					if err := os.Symlink(test.target, filepath.Join(s.beta, "pkg")); err != nil {
						s.t.Skip("unable to create symbolic link:", err)
					}
				},
			})
			session.flush()
			session.flush()
			session.expect(session.beta, test.content)
			if info, err := os.Lstat(filepath.Join(session.beta, "pkg")); err != nil || !info.IsDir() {
				t.Errorf("symbolic link not replaced: %v, %v", info, err)
			}
			session.expectMissing(session.beta, "other/file.txt")
		})
	}
}

// TestGitModeRemoteWarnings tests how remote reports what isn't synchronized by
// design, which in a listing of conflicts doesn't say anything about conflicts.
func TestGitModeRemoteWarnings(t *testing.T) {
	// Have beta's own rules exclude a path that alpha has, which beta doesn't.
	session := newGitModeSession(t, gitModeOptions{
		commands: synchronization.RemoteCommandMode_RemoteCommandModeBeta,
		alpha:    map[string]string{"keep": "keep", "excluded": "excluded"},
		beta:     map[string]string{".git/info/exclude": "excluded\n"},
	})
	session.waitForRemoteCommands()
	session.flushAllowingProblems()
	session.expect(session.beta, map[string]string{"keep": "keep"})
	session.expectMissing(session.beta, "excluded")

	code, output, diagnostic := runRemote(t, session.alpha, "--conflicts")
	if code != 0 || output != "No conflicts.\n" {
		t.Errorf("incorrect listing: %d: %q: %s", code, output, diagnostic)
	}
	if !strings.Contains(diagnostic, "remote: warning: this content isn't synchronized, by design:\n") ||
		!strings.Contains(diagnostic, "excluded (remote): ignored by this endpoint's own rules") {
		t.Errorf("warning not reported as being by design: %q", diagnostic)
	} else if strings.Contains(diagnostic, "couldn't be synchronized") {
		t.Errorf("warning reported as a failure: %q", diagnostic)
	}
}
