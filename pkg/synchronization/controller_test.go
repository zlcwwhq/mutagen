package synchronization

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/encoding"
	"github.com/mutagen-io/mutagen/pkg/state"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/rsync"
)

// TODO: Implement tests for the remaining controller behavior.

const (
	// testingControllerTimeout is the maximum amount of time that controller
	// tests will wait for the synchronization loop.
	testingControllerTimeout = 30 * time.Second
	// testingFileRules is a name for a file that provides ignore rules.
	testingFileRules = ".gitignore"
)

// testingFile is a file entry with arbitrary content.
var testingFile = &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}

// testingEndpoint is an in-memory implementation of Endpoint, which reflects
// the essentials of Git mode: once its root has a rule file, certain entries in
// the root are untracked. Rule files at other locations have no effect on it.
type testingEndpoint struct {
	// polling receives a value each time the synchronization loop polls, which
	// means that it has finished performing synchronization cycles.
	polling chan struct{}
	// wake causes the current poll operation to return.
	wake chan struct{}
	// lock serializes access to the fields below.
	lock sync.Mutex
	// rootMissing indicates that the root doesn't exist, in which case contents
	// should be empty.
	rootMissing bool
	// contents are the contents of the root.
	contents map[string]*core.Entry
	// ruleTargets are the names of entries that are untracked if the root has a
	// rule file.
	ruleTargets []string
	// failing indicates that transitions for rule files in the root should
	// fail.
	failing bool
	// scans is the number of scans.
	scans int
	// fullScans is the number of scans that were requested to be full.
	fullScans int
	// staged are the paths that have been staged, sorted, in each of the
	// staging operations that have been performed. A synchronization cycle
	// performs at most one such operation on an endpoint, and only if it needs
	// to stage.
	staged [][]string
	// transitioned are the paths at which transitions were attempted, sorted,
	// in each of the transition operations that have been performed. A
	// synchronization cycle performs at most one such operation on an endpoint,
	// and only if it has transitions.
	transitioned [][]string
}

// newTestingEndpoint creates a new testing endpoint with the specified
// contents.
func newTestingEndpoint(contents map[string]*core.Entry, ruleTargets ...string) *testingEndpoint {
	if contents == nil {
		contents = make(map[string]*core.Entry)
	}
	return &testingEndpoint{
		polling:     make(chan struct{}, 1024),
		wake:        make(chan struct{}),
		contents:    contents,
		ruleTargets: ruleTargets,
	}
}

// Poll implements Endpoint.Poll.
func (e *testingEndpoint) Poll(ctx context.Context) error {
	e.polling <- struct{}{}
	select {
	case <-ctx.Done():
	case <-e.wake:
	}
	return nil
}

// Scan implements Endpoint.Scan.
func (e *testingEndpoint) Scan(_ context.Context, _ *core.Entry, full bool) (*core.Snapshot, error, bool) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.scans++
	if full {
		e.fullScans++
	}
	if e.rootMissing {
		return &core.Snapshot{}, nil, false
	}
	contents := make(map[string]*core.Entry, len(e.contents))
	for name, entry := range e.contents {
		contents[name] = entry
		if e.contents[testingFileRules] != nil && slices.Contains(e.ruleTargets, name) {
			contents[name] = &core.Entry{Kind: core.EntryKind_Untracked}
		}
	}
	return &core.Snapshot{
		Content: &core.Entry{Kind: core.EntryKind_Directory, Contents: contents},
	}, nil, false
}

// Stage implements Endpoint.Stage. It behaves as if all files were available.
func (e *testingEndpoint) Stage(paths []string, _ [][]byte) ([]string, []*rsync.Signature, rsync.Receiver, error) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.staged = append(e.staged, slices.Sorted(slices.Values(paths)))
	return nil, nil, nil, nil
}

// Supply implements Endpoint.Supply.
func (e *testingEndpoint) Supply(_ []string, _ []*rsync.Signature, _ rsync.Receiver) error {
	return nil
}

// Transition implements Endpoint.Transition.
func (e *testingEndpoint) Transition(_ context.Context, transitions []*core.Change) ([]*core.Entry, []*core.Problem, bool, error) {
	e.lock.Lock()
	defer e.lock.Unlock()
	results := make([]*core.Entry, len(transitions))
	var problems []*core.Problem
	var transitioned []string
	for t, transition := range transitions {
		transitioned = append(transitioned, transition.Path)
		if e.failing && transition.Path == testingFileRules {
			results[t] = transition.Old
			problems = append(problems, &core.Problem{Path: transition.Path, Error: "unable to write"})
			continue
		}
		results[t] = transition.New
		var root *core.Entry
		if !e.rootMissing {
			root = &core.Entry{Kind: core.EntryKind_Directory, Contents: e.contents}
		}
		root, err := core.Apply(root, []*core.Change{{Path: transition.Path, New: transition.New}})
		if err != nil {
			return nil, nil, false, err
		}
		e.rootMissing = root == nil
		e.contents = make(map[string]*core.Entry)
		for name, entry := range root.GetContents() {
			e.contents[name] = entry
		}
	}
	e.transitioned = append(e.transitioned, slices.Sorted(slices.Values(transitioned)))
	return results, problems, false, nil
}

// Shutdown implements Endpoint.Shutdown.
func (e *testingEndpoint) Shutdown() error {
	return nil
}

// update performs a modification of the endpoint's fields.
func (e *testingEndpoint) update(modify func()) {
	e.lock.Lock()
	defer e.lock.Unlock()
	modify()
}

// status returns the endpoint's statistics.
func (e *testingEndpoint) status() (scans, fullScans int, staged, transitioned [][]string) {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.scans, e.fullScans, slices.Clone(e.staged), slices.Clone(e.transitioned)
}

// testingLoop is a synchronization loop running for a test.
type testingLoop struct {
	// t is the test.
	t *testing.T
	// controller is the controller running the loop.
	controller *controller
	// alpha is the alpha endpoint.
	alpha *testingEndpoint
	// beta is the beta endpoint.
	beta *testingEndpoint
}

// startTestingLoop starts a synchronization loop between two testing endpoints,
// which is terminated when the test completes.
func startTestingLoop(t *testing.T, configuration *Configuration, alpha, beta *testingEndpoint) *testingLoop {
	t.Helper()

	// Create an empty archive.
	archivePath := filepath.Join(t.TempDir(), "archive")
	if err := encoding.MarshalAndSaveProtobuf(archivePath, &core.Archive{}); err != nil {
		t.Fatal("unable to create archive:", err)
	}

	// Create the controller.
	session := &Session{Identifier: "testing", Version: Version_Version1, Configuration: configuration}
	c := &controller{
		archivePath:              archivePath,
		stateLock:                state.NewTrackingLock(state.NewTracker()),
		session:                  session,
		mergedAlphaConfiguration: configuration,
		mergedBetaConfiguration:  configuration,
		state:                    &State{Session: session, AlphaState: &EndpointState{}, BetaState: &EndpointState{}},
		flushRequests:            make(chan chan error, 1),
	}

	// Run the synchronization loop until the test completes.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.synchronize(ctx, alpha, beta)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return &testingLoop{t, c, alpha, beta}
}

// waitForPolling waits until the loop finishes its synchronization cycles and
// begins polling.
func (l *testingLoop) waitForPolling() {
	l.t.Helper()
	select {
	case <-l.alpha.polling:
	case <-time.After(testingControllerTimeout):
		l.t.Fatal("timed out waiting for synchronization loop to start polling")
	}
}

// wakeFromPolling triggers another synchronization cycle in a loop that's
// polling.
func (l *testingLoop) wakeFromPolling() {
	l.t.Helper()
	select {
	case l.alpha.wake <- struct{}{}:
	case <-time.After(testingControllerTimeout):
		l.t.Fatal("timed out waiting to wake synchronization loop")
	}
}

// flush submits a flush request and waits for its response.
func (l *testingLoop) flush() {
	l.t.Helper()
	request := make(chan error, 1)
	select {
	case l.controller.flushRequests <- request:
	case <-time.After(testingControllerTimeout):
		l.t.Fatal("timed out sending flush request")
	}
	select {
	case err := <-request:
		if err != nil {
			l.t.Fatal("flush failed:", err)
		}
	case <-time.After(testingControllerTimeout):
		l.t.Fatal("timed out waiting for flush response")
	}
}

// cycles returns the number of successful synchronization cycles.
func (l *testingLoop) cycles() uint64 {
	l.controller.stateLock.Lock()
	defer l.controller.stateLock.UnlockWithoutNotify()
	return l.controller.state.SuccessfulCycles
}

// verifyCycles verifies the synchronization cycle count.
func (l *testingLoop) verifyCycles(cycles uint64) {
	l.t.Helper()
	if actual := l.cycles(); actual != cycles {
		l.t.Errorf("incorrect cycle count: %d != %d", actual, cycles)
	}
}

// verifyOperations verifies the paths in each of the staging and transition
// operations that have been performed on an endpoint.
func (l *testingLoop) verifyOperations(name string, endpoint *testingEndpoint, staged, transitioned [][]string) {
	l.t.Helper()
	_, _, actualStaged, actualTransitioned := endpoint.status()
	if !slices.EqualFunc(actualStaged, staged, slices.Equal[[]string]) {
		l.t.Errorf("incorrect paths staged on %s: %v != %v", name, actualStaged, staged)
	}
	if !slices.EqualFunc(actualTransitioned, transitioned, slices.Equal[[]string]) {
		l.t.Errorf("incorrect paths transitioned on %s: %v != %v", name, actualTransitioned, transitioned)
	}
}

// verify verifies the synchronization cycle count and the paths in each of the
// transition operations performed on each endpoint, as well as that the same
// paths have been staged (since staging is what transfers content). This only
// applies to tests where every transition creates a file.
func (l *testingLoop) verify(cycles uint64, alphaOperations, betaOperations [][]string) {
	l.t.Helper()
	l.verifyCycles(cycles)
	l.verifyOperations("alpha", l.alpha, alphaOperations, alphaOperations)
	l.verifyOperations("beta", l.beta, betaOperations, betaOperations)
}

// testingGitMode is a configuration with Git mode enabled.
var testingGitMode = &Configuration{GitMode: GitMode_GitModeEnabled}

// testingDirectory creates a directory entry containing files with the
// specified names.
func testingDirectory(names ...string) *core.Entry {
	contents := make(map[string]*core.Entry, len(names))
	for _, name := range names {
		contents[name] = testingFile
	}
	return &core.Entry{Kind: core.EntryKind_Directory, Contents: contents}
}

// TestSynchronizeRuleOnlyCycle tests that a cycle that delivers ignore rules to
// either endpoint transfers nothing else, in either direction, and that the
// transitions that it drops are recomputed immediately, without waiting for an
// event, and in light of the new rules.
func TestSynchronizeRuleOnlyCycle(t *testing.T) {
	rules := func() map[string]*core.Entry {
		return map[string]*core.Entry{testingFileRules: testingFile, "other": testingFile}
	}
	content := func() map[string]*core.Entry {
		return map[string]*core.Entry{"data": testingFile, "keep": testingFile}
	}

	t.Run("beta receives rules", func(t *testing.T) {
		alpha, beta := newTestingEndpoint(rules()), newTestingEndpoint(content(), "data")
		loop := startTestingLoop(t, testingGitMode, alpha, beta)
		loop.waitForPolling()
		loop.verify(2, [][]string{{"keep"}}, [][]string{{testingFileRules}, {"other"}})
	})

	t.Run("alpha receives rules", func(t *testing.T) {
		alpha, beta := newTestingEndpoint(content(), "data"), newTestingEndpoint(rules())
		loop := startTestingLoop(t, testingGitMode, alpha, beta)
		loop.waitForPolling()
		loop.verify(2, [][]string{{testingFileRules}, {"other"}}, [][]string{{"keep"}})
	})
}

// TestSynchronizeRuleOnlyCycleNothingDropped tests that a cycle that delivers
// ignore rules forces the next cycle even if it has nothing else to drop, since
// the new rules may change what's synchronized.
func TestSynchronizeRuleOnlyCycleNothingDropped(t *testing.T) {
	alpha := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile})
	beta := newTestingEndpoint(nil)
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()
	loop.verify(2, nil, [][]string{{testingFileRules}})
}

// TestSynchronizeRuleOnlyCycleProtectsDeletion tests that content that's
// missing on an endpoint with new rules isn't deleted on the opposite endpoint,
// which hasn't yet received those rules, in a mode that mirrors content.
func TestSynchronizeRuleOnlyCycleProtectsDeletion(t *testing.T) {
	alpha := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile})
	beta := newTestingEndpoint(map[string]*core.Entry{"data": testingFile}, "data")
	configuration := &Configuration{
		GitMode:             GitMode_GitModeEnabled,
		SynchronizationMode: core.SynchronizationMode_SynchronizationModeOneWayReplica,
	}
	loop := startTestingLoop(t, configuration, alpha, beta)
	loop.waitForPolling()
	loop.verify(2, nil, [][]string{{testingFileRules}})
	beta.update(func() {
		if beta.contents["data"] == nil {
			t.Error("content deleted")
		}
	})
}

// TestSynchronizeRuleOnlyCycleForRemoval tests that the removal of a rule file
// is delivered by itself, like any other change to the rules, and that a flush
// covers the content that it exposes.
func TestSynchronizeRuleOnlyCycleForRemoval(t *testing.T) {
	alpha := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile, "data": testingFile}, "data")
	beta := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile})
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()
	loop.verify(1, nil, nil)

	// Remove the rules, which exposes the content.
	alpha.update(func() { delete(alpha.contents, testingFileRules) })
	loop.flush()

	// The removal isn't staged, because it doesn't transfer anything.
	loop.verifyCycles(3)
	loop.verifyOperations("alpha", alpha, nil, nil)
	loop.verifyOperations("beta", beta, [][]string{{"data"}}, [][]string{{testingFileRules}, {"data"}})
}

// TestSynchronizeNoRuleOnlyCycleWithoutGitMode tests that behavior is unchanged
// if Git mode isn't enabled.
func TestSynchronizeNoRuleOnlyCycleWithoutGitMode(t *testing.T) {
	alpha := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile})
	beta := newTestingEndpoint(map[string]*core.Entry{"data": testingFile, "keep": testingFile}, "data")
	loop := startTestingLoop(t, &Configuration{}, alpha, beta)
	loop.waitForPolling()
	loop.verify(1, [][]string{{"data", "keep"}}, [][]string{{testingFileRules}})
}

// TestSynchronizeNoRuleOnlyCycleWithoutDeliveries tests that a cycle isn't
// limited if its changes merely involve rule files without delivering them.
func TestSynchronizeNoRuleOnlyCycleWithoutDeliveries(t *testing.T) {
	// Start with content that can then be changed in ways that aren't
	// deliveries.
	alpha := newTestingEndpoint(map[string]*core.Entry{
		"deleted":  testingDirectory(testingFileRules, "a"),
		"replaced": testingFile,
	})
	beta := newTestingEndpoint(nil)
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()
	loop.verifyCycles(2)
	beta.update(func() { beta.staged, beta.transitioned = nil, nil })

	// Perform the changes.
	alpha.update(func() {
		alpha.contents[testingFileRules] = &core.Entry{Kind: core.EntryKind_SymbolicLink, Target: "target"}
		alpha.contents["replaced"] = testingDirectory(testingFileRules, "a")
		delete(alpha.contents, "deleted")
	})
	loop.flush()

	// All of them have to take place in the same, single cycle.
	loop.verifyCycles(3)
	loop.verifyOperations("alpha", alpha, nil, nil)
	loop.verifyOperations(
		"beta", beta,
		[][]string{{"replaced/" + testingFileRules, "replaced/a"}},
		[][]string{{testingFileRules, "deleted", "replaced"}},
	)
}

// TestSynchronizeBothReceiveRules tests that both endpoints only receive rule
// files if both have rules to receive, including rule files in new directories,
// and that the remainder of those directories then follows.
func TestSynchronizeBothReceiveRules(t *testing.T) {
	alpha := newTestingEndpoint(map[string]*core.Entry{"first": testingDirectory(testingFileRules, "data")})
	beta := newTestingEndpoint(map[string]*core.Entry{"second": testingDirectory(testingFileRules, "data")})
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()
	loop.verifyCycles(2)
	loop.verifyOperations("alpha", alpha,
		[][]string{{"second/" + testingFileRules}, {"second/data"}},
		[][]string{{"second"}, {"second/data"}},
	)
	loop.verifyOperations("beta", beta,
		[][]string{{"first/" + testingFileRules}, {"first/data"}},
		[][]string{{"first"}, {"first/data"}},
	)
}

// TestSynchronizeRootCreationDeliversRulesFirst tests that the creation of a
// root is limited to rule files, with the remainder following in the next cycle
// without any conflicts.
func TestSynchronizeRootCreationDeliversRulesFirst(t *testing.T) {
	alpha := newTestingEndpoint(map[string]*core.Entry{
		testingFileRules: testingFile,
		"other":          testingFile,
		"sub":            testingDirectory(testingFileRules, "data"),
	})
	beta := newTestingEndpoint(nil)
	beta.rootMissing = true
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()
	loop.verifyCycles(2)
	loop.verifyOperations("alpha", alpha, nil, nil)
	loop.verifyOperations("beta", beta,
		[][]string{{testingFileRules, "sub/" + testingFileRules}, {"other", "sub/data"}},
		[][]string{{""}, {"other", "sub/data"}},
	)
	if conflicts := loop.controller.remoteReport().Conflicts; len(conflicts) != 0 {
		t.Errorf("unexpected conflicts: %v", conflicts)
	}
	beta.update(func() {
		if len(beta.contents) != 3 || len(beta.contents["sub"].GetContents()) != 2 {
			t.Errorf("incomplete contents: %v", beta.contents)
		}
	})
}

// TestSynchronizeRuleOnlyFailsClosed tests that a rule file that can't be
// delivered blocks every other transition, on both endpoints, for as long as it
// can't be delivered, even when events occur, that the failure stays visible,
// and that it doesn't result in a loop of synchronization cycles.
func TestSynchronizeRuleOnlyFailsClosed(t *testing.T) {
	alpha := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile, "other": testingFile})
	beta := newTestingEndpoint(map[string]*core.Entry{"data": testingFile, "keep": testingFile}, "data")
	beta.failing = true
	loop := startTestingLoop(t, testingGitMode, alpha, beta)

	// verifyProblem verifies that the failure is visible.
	verifyProblem := func() {
		t.Helper()
		loop.controller.stateLock.Lock()
		problems := loop.controller.state.BetaState.TransitionProblems
		loop.controller.stateLock.UnlockWithoutNotify()
		if len(problems) != 1 || problems[0].Path != testingFileRules {
			t.Errorf("incorrect transition problems: %v", problems)
		}
	}

	// repeated creates an expected list of operations that only have rules.
	repeated := func(count int) [][]string {
		return slices.Repeat([][]string{{testingFileRules}}, count)
	}

	// The initial cycle only delivers rules, which forces one more cycle. That
	// cycle does the same because the delivery failed, but it mustn't force
	// another.
	loop.waitForPolling()
	loop.verify(2, nil, repeated(2))
	if scans, _, _, _ := alpha.status(); scans != 2 {
		t.Errorf("incorrect scan count while delivery is failing: %d", scans)
	}
	verifyProblem()

	// An event may trigger more cycles, but they have to stop as well, and the
	// other transitions have to remain blocked.
	loop.wakeFromPolling()
	loop.waitForPolling()
	loop.verify(4, nil, repeated(4))
	verifyProblem()

	// A flush is still answered, after its second cycle.
	loop.flush()
	loop.verify(6, nil, repeated(6))
	verifyProblem()
	loop.waitForPolling()

	// If delivery starts to work, then the next event completes synchronization
	// and the failure goes away. The content that the rules ignore stays put.
	beta.update(func() { beta.failing = false })
	loop.wakeFromPolling()
	loop.waitForPolling()
	loop.verify(8, [][]string{{"keep"}}, append(repeated(7), []string{"other"}))
	loop.controller.stateLock.Lock()
	problems := loop.controller.state.BetaState.TransitionProblems
	loop.controller.stateLock.UnlockWithoutNotify()
	if len(problems) != 0 {
		t.Errorf("transition problems remain after recovery: %v", problems)
	}
}

// TestSynchronizeFlushHeldForRuleOnlyCycle tests that a flush isn't answered by
// a synchronization cycle that only delivered rules, but rather by the
// immediately following one, which has performed the remaining transitions.
func TestSynchronizeFlushHeldForRuleOnlyCycle(t *testing.T) {
	alpha, beta := newTestingEndpoint(nil), newTestingEndpoint(nil, "data")
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()
	loop.verify(1, nil, nil)

	// Create content that requires a rule-only cycle and flush.
	alpha.update(func() { alpha.contents[testingFileRules] = testingFile })
	beta.update(func() {
		beta.contents["data"] = testingFile
		beta.contents["keep"] = testingFile
	})
	loop.flush()

	// The flush must have seen the second cycle and the transitions in it.
	// Since the response is sent at the end of the cycle, this is reliable.
	loop.verify(3, [][]string{{"keep"}}, [][]string{{testingFileRules}})

	// Both cycles of the flush must have forced full scans on both endpoints.
	if _, fullScans, _, _ := alpha.status(); fullScans != 2 {
		t.Errorf("incorrect alpha full scan count: %d", fullScans)
	}
	if _, fullScans, _, _ := beta.status(); fullScans != 2 {
		t.Errorf("incorrect beta full scan count: %d", fullScans)
	}

	// No more cycles occur on their own, and the next flush only takes one.
	loop.waitForPolling()
	loop.flush()
	loop.verify(4, [][]string{{"keep"}}, [][]string{{testingFileRules}})
}

// TestSynchronizeFlushHeldWithNothingDropped tests that a flush is held by a
// cycle that delivers rules even if it has nothing else to drop.
func TestSynchronizeFlushHeldWithNothingDropped(t *testing.T) {
	alpha, beta := newTestingEndpoint(nil), newTestingEndpoint(nil)
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()
	loop.verify(1, nil, nil)

	alpha.update(func() { alpha.contents[testingFileRules] = testingFile })
	loop.flush()
	loop.verify(3, nil, [][]string{{testingFileRules}})
}

// TestSynchronizeFlushAnsweredDespiteContinuedRuleOnlyCycles tests that a flush
// is held for only one synchronization cycle, so that it can't wait
// indefinitely if rules can't be delivered.
func TestSynchronizeFlushAnsweredDespiteContinuedRuleOnlyCycles(t *testing.T) {
	alpha, beta := newTestingEndpoint(nil), newTestingEndpoint(nil, "data")
	beta.failing = true
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()

	// Create content that requires a rule-only cycle and flush.
	alpha.update(func() { alpha.contents[testingFileRules] = testingFile })
	beta.update(func() {
		beta.contents["data"] = testingFile
		beta.contents["keep"] = testingFile
	})
	loop.flush()
	loop.verifyCycles(3)

	// The loop must go back to polling, and not lose subsequent flushes, each
	// of which are handled in the same way. Nothing can be transferred to
	// alpha.
	loop.waitForPolling()
	loop.flush()
	loop.verifyCycles(5)
	loop.waitForPolling()
	loop.verifyOperations("alpha", alpha, nil, nil)
}

// TestSynchronizeFlushNotHeldWithoutDeliveries tests that a flush is answered
// by its first synchronization cycle if it doesn't deliver rules.
func TestSynchronizeFlushNotHeldWithoutDeliveries(t *testing.T) {
	for _, configuration := range []*Configuration{{}, testingGitMode} {
		alpha, beta := newTestingEndpoint(nil), newTestingEndpoint(nil)
		loop := startTestingLoop(t, configuration, alpha, beta)
		loop.waitForPolling()

		// A cycle that transfers content in both directions isn't special.
		alpha.update(func() { alpha.contents["first"] = testingFile })
		beta.update(func() { beta.contents["second"] = testingFile })
		loop.flush()
		loop.verify(2, [][]string{{"second"}}, [][]string{{"first"}})
	}
}
