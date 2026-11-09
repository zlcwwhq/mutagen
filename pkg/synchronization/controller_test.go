package synchronization

import (
	"context"
	"maps"
	"os"
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
	// excluded are the names of entries that are untracked regardless, as if
	// the endpoint's own exclude file, which isn't synchronized, ignored them.
	excluded []string
	// warnings are the warnings to include in snapshots.
	warnings []*core.Problem
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
	// populations maps paths to the results of the transitions that populate
	// submodules at them, which are what clones contain, rather than what was
	// requested. A nil result is a failed population, which yields the old
	// content. Transitions for other paths yield what was requested.
	populations map[string]*core.Entry
	// seeds are the populations that the endpoint reports in its snapshots
	// (see core.Snapshot.Seeds). Like an endpoint's records of populations,
	// each is discarded by the first scan that receives its seed.
	seeds map[string]*core.Entry
	// scanSeeds are the seeds that each scan has received, sorted.
	scanSeeds [][]string
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
func (e *testingEndpoint) Scan(_ context.Context, _ *core.Entry, submodules map[string]*core.Entry, full bool) (*core.Snapshot, error, bool) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.scans++
	if full {
		e.fullScans++
	}
	var seeds []string
	for _, link := range submodules {
		if link.Seed != "" {
			seeds = append(seeds, link.Seed)
		}
	}
	e.scanSeeds = append(e.scanSeeds, slices.Sorted(slices.Values(seeds)))
	for path, population := range e.seeds {
		if slices.Contains(seeds, population.Contents[core.GitLinkName].Seed) {
			delete(e.seeds, path)
		}
	}
	if e.rootMissing {
		return &core.Snapshot{Warnings: e.warnings}, nil, false
	}
	contents := make(map[string]*core.Entry, len(e.contents))
	for name, entry := range e.contents {
		contents[name] = entry
		if e.contents[testingFileRules] != nil && slices.Contains(e.ruleTargets, name) {
			contents[name] = &core.Entry{Kind: core.EntryKind_Untracked}
		} else if slices.Contains(e.excluded, name) {
			contents[name] = &core.Entry{Kind: core.EntryKind_Untracked}
		}
	}
	var reported map[string]*core.Entry
	if len(e.seeds) > 0 {
		reported = maps.Clone(e.seeds)
	}
	return &core.Snapshot{
		Content:  &core.Entry{Kind: core.EntryKind_Directory, Contents: contents},
		Warnings: e.warnings,
		Seeds:    reported,
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
		if population, ok := e.populations[transition.Path]; ok && transition.New.IsSubmodule() {
			results[t] = population
			if population == nil {
				results[t] = transition.Old
			}
		}
		var root *core.Entry
		if !e.rootMissing {
			root = &core.Entry{Kind: core.EntryKind_Directory, Contents: e.contents}
		}
		root, err := core.Apply(root, []*core.Change{{Path: transition.Path, New: results[t]}})
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
	return startTestingLoopWithAncestor(t, configuration, nil, alpha, beta)
}

// startTestingLoopWithAncestor is a variant of startTestingLoop for a session
// whose archive already has an ancestor.
func startTestingLoopWithAncestor(t *testing.T, configuration *Configuration, ancestor *core.Entry, alpha, beta *testingEndpoint) *testingLoop {
	t.Helper()
	return startTestingLoopWithRecord(t, configuration, ancestor, nil, alpha, beta)
}

// startTestingLoopWithRecord is a variant of startTestingLoopWithAncestor for a
// session that has already saved a replica record, unless it's nil.
func startTestingLoopWithRecord(t *testing.T, configuration *Configuration, ancestor *core.Entry, record *ReplicaRecord, alpha, beta *testingEndpoint) *testingLoop {
	t.Helper()

	// Create the archive and the replica record.
	directory := t.TempDir()
	archivePath := filepath.Join(directory, "archive")
	if err := encoding.MarshalAndSaveProtobuf(archivePath, &core.Archive{Content: ancestor}); err != nil {
		t.Fatal("unable to create archive:", err)
	}
	replicaRecordPath := filepath.Join(directory, "replica")
	if record != nil {
		if err := encoding.MarshalAndSaveProtobuf(replicaRecordPath, record); err != nil {
			t.Fatal("unable to create replica record:", err)
		}
	}

	// Create the controller.
	session := &Session{Identifier: "testing", Version: Version_Version1, Configuration: configuration}
	c := &controller{
		archivePath:              archivePath,
		replicaRecordPath:        replicaRecordPath,
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

// ancestor loads the ancestor that the loop has saved.
func (l *testingLoop) ancestor() *core.Entry {
	l.t.Helper()
	archive := &core.Archive{}
	if err := encoding.LoadAndUnmarshalProtobuf(l.controller.archivePath, archive); err != nil {
		l.t.Fatal("unable to load archive:", err)
	}
	return archive.Content
}

// replicaRecord loads the replica record that the loop has saved, which is
// empty if it hasn't saved one.
func (l *testingLoop) replicaRecord() *ReplicaRecord {
	l.t.Helper()
	record, err := loadReplicaRecord(l.controller.replicaRecordPath)
	if err != nil {
		l.t.Fatal("unable to load replica record:", err)
	}
	return record
}

// betaTransitionProblems returns beta's transition problems from the loop's
// last synchronization cycle.
func (l *testingLoop) betaTransitionProblems() []*core.Problem {
	l.controller.stateLock.Lock()
	defer l.controller.stateLock.UnlockWithoutNotify()
	return slices.Clone(l.controller.state.BetaState.TransitionProblems)
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

// testingReplica is a configuration with Git mode enabled in the
// one-way-replica mode.
var testingReplica = &Configuration{
	GitMode:             GitMode_GitModeEnabled,
	SynchronizationMode: core.SynchronizationMode_SynchronizationModeOneWayReplica,
}

// testingOtherFile is a file entry with content other than testingFile's.
var testingOtherFile = &core.Entry{Kind: core.EntryKind_File, Digest: []byte{2}}

// verifyExposedContentKept verifies that beta has the specified content at a
// path that a change of rules has exposed, that the replica record says so, and
// that beta reports it as a problem.
func (l *testingLoop) verifyExposedContentKept(path string, content *core.Entry) {
	l.t.Helper()
	l.beta.update(func() {
		if !l.beta.contents[path].Equal(content, true) {
			l.t.Errorf("exposed content at %s not kept: %v", path, l.beta.contents[path])
		}
	})
	if record := l.replicaRecord(); !slices.Equal(record.Exposed, []string{path}) {
		l.t.Errorf("incorrect exposed paths: %v", record.Exposed)
	}
	problems := l.betaTransitionProblems()
	if len(problems) != 1 || problems[0].Path != path || problems[0].Warning || problems[0].Error != exposedContentProblem {
		l.t.Errorf("incorrect problems for exposed content: %v", problems)
	}
}

// TestSynchronizeReplicaKeepsExposedContent tests that, in the one-way-replica
// mode, content that beta's rules ignored isn't removed or overwritten once a
// change of rules exposes it, whether the session itself removes the rules from
// beta or delivers their removal from alpha, and that beta reports it.
func TestSynchronizeReplicaKeepsExposedContent(t *testing.T) {
	t.Run("rules only on beta", func(t *testing.T) {
		// The rule file only exists on beta, so the session removes it in a
		// cycle that only does that, after which the content that it ignored is
		// tracked, but missing from alpha.
		alpha := newTestingEndpoint(map[string]*core.Entry{"src": testingFile})
		beta := newTestingEndpoint(map[string]*core.Entry{
			"src":            testingFile,
			testingFileRules: testingFile,
			"data":           testingDirectory("big"),
		}, "data")
		loop := startTestingLoopWithAncestor(t, testingReplica, testingDirectory("src"), alpha, beta)
		loop.waitForPolling()
		loop.verifyCycles(2)
		loop.verifyOperations("beta", beta, nil, [][]string{{testingFileRules}})
		loop.verifyExposedContentKept("data", testingDirectory("big"))

		// Nothing changes in further cycles, and the ancestor doesn't record
		// the content.
		loop.flush()
		loop.verifyOperations("beta", beta, nil, [][]string{{testingFileRules}})
		loop.verifyExposedContentKept("data", testingDirectory("big"))
		if loop.ancestor().Contents["data"] != nil {
			t.Error("ancestor records exposed content")
		}
	})

	t.Run("rule removed on alpha", func(t *testing.T) {
		// Both endpoints ignore beta's content, until alpha's rule is removed,
		// which the session delivers to beta.
		alpha := newTestingEndpoint(map[string]*core.Entry{"src": testingFile, testingFileRules: testingFile})
		beta := newTestingEndpoint(map[string]*core.Entry{
			"src":            testingFile,
			testingFileRules: testingFile,
			"data":           testingDirectory("big"),
		}, "data")
		loop := startTestingLoopWithAncestor(t, testingReplica, testingDirectory("src", testingFileRules), alpha, beta)
		loop.waitForPolling()
		if record := loop.replicaRecord(); !slices.Equal(record.Untracked, []string{"data"}) || len(record.Exposed) != 0 {
			t.Errorf("incorrect replica record: %v", record)
		}
		alpha.update(func() { delete(alpha.contents, testingFileRules) })
		loop.flush()
		loop.verifyOperations("beta", beta, nil, [][]string{{testingFileRules}})
		loop.verifyExposedContentKept("data", testingDirectory("big"))
	})

	t.Run("different content on alpha", func(t *testing.T) {
		// Both endpoints ignore their own content at the same path, until
		// alpha's rule is removed, which would overwrite beta's.
		alpha := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile, "data": testingOtherFile}, "data")
		beta := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile, "data": testingFile}, "data")
		loop := startTestingLoopWithAncestor(t, testingReplica, testingDirectory(testingFileRules), alpha, beta)
		loop.waitForPolling()
		alpha.update(func() { delete(alpha.contents, testingFileRules) })
		loop.flush()
		loop.verifyOperations("beta", beta, nil, [][]string{{testingFileRules}})
		loop.verifyExposedContentKept("data", testingFile)
	})

	t.Run("identical content on alpha", func(t *testing.T) {
		// Content that's identical to alpha's isn't exposed, but synchronized.
		alpha := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile, "data": testingFile}, "data")
		beta := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile, "data": testingFile}, "data")
		loop := startTestingLoopWithAncestor(t, testingReplica, testingDirectory(testingFileRules), alpha, beta)
		loop.waitForPolling()
		alpha.update(func() { delete(alpha.contents, testingFileRules) })
		loop.flush()
		if record := loop.replicaRecord(); len(record.Untracked) != 0 || len(record.Exposed) != 0 {
			t.Errorf("incorrect replica record: %v", record)
		}
		if problems := loop.betaTransitionProblems(); len(problems) != 0 {
			t.Error("unexpected problems:", problems)
		}
		if !loop.ancestor().Contents["data"].Equal(testingFile, true) {
			t.Error("ancestor doesn't record synchronized content")
		}
	})
}

// TestSynchronizeReplicaExposedContentAfterRestart tests that the protection of
// exposed content survives a restart of the synchronization loop, both between
// the cycle in which the content was untracked and the one in which it's
// exposed, and after it's been exposed, while content that beta has created at
// other paths is still removed.
func TestSynchronizeReplicaExposedContentAfterRestart(t *testing.T) {
	records := map[string]*ReplicaRecord{
		"untracked": {Untracked: []string{"data"}},
		"exposed":   {Exposed: []string{"data"}},
	}
	for name, record := range records {
		t.Run(name, func(t *testing.T) {
			alpha := newTestingEndpoint(map[string]*core.Entry{"src": testingFile})
			beta := newTestingEndpoint(map[string]*core.Entry{
				"src":   testingFile,
				"data":  testingDirectory("big"),
				"other": testingFile,
			})
			loop := startTestingLoopWithRecord(t, testingReplica, testingDirectory("src"), record, alpha, beta)
			loop.waitForPolling()
			loop.verifyOperations("beta", beta, nil, [][]string{{"other"}})
			loop.verifyExposedContentKept("data", testingDirectory("big"))
			beta.update(func() {
				if beta.contents["other"] != nil {
					t.Error("content created on beta not removed")
				}
			})
		})
	}
}

// TestSynchronizeReplicaExposedContentReleased tests that exposed content is no
// longer protected once it's gone from beta, identical to alpha's, or ignored
// again, and that beta stops reporting it.
func TestSynchronizeReplicaExposedContentReleased(t *testing.T) {
	tests := []struct {
		// name is the name of the test.
		name string
		// release releases the exposed content.
		release func(alpha, beta *testingEndpoint)
		// untracked are the expected untracked paths in the record.
		untracked []string
		// data is the expected content at the exposed path on beta afterward.
		data *core.Entry
	}{
		{"deleted", func(_, beta *testingEndpoint) { delete(beta.contents, "data") }, nil, nil},
		{"identical", func(alpha, _ *testingEndpoint) { alpha.contents["data"] = testingDirectory("big") }, nil, testingDirectory("big")},
		{"ignored again", func(_, beta *testingEndpoint) { beta.excluded = []string{"data"} }, []string{"data"}, testingDirectory("big")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alpha := newTestingEndpoint(map[string]*core.Entry{"src": testingFile})
			beta := newTestingEndpoint(map[string]*core.Entry{"src": testingFile, "data": testingDirectory("big")})
			record := &ReplicaRecord{Exposed: []string{"data"}}
			loop := startTestingLoopWithRecord(t, testingReplica, testingDirectory("src"), record, alpha, beta)
			loop.waitForPolling()
			loop.verifyExposedContentKept("data", testingDirectory("big"))

			// Release the content.
			alpha.update(func() {
				beta.update(func() { test.release(alpha, beta) })
			})
			loop.flush()
			if record := loop.replicaRecord(); !slices.Equal(record.Untracked, test.untracked) || len(record.Exposed) != 0 {
				t.Errorf("incorrect replica record: %v", record)
			}
			if problems := loop.betaTransitionProblems(); len(problems) != 0 {
				t.Error("unexpected problems:", problems)
			}
			beta.update(func() {
				if !beta.contents["data"].Equal(test.data, true) {
					t.Error("incorrect content on beta:", beta.contents["data"])
				}
			})

			// Content that beta creates there from now on is removed, unless
			// it's ignored.
			if test.untracked == nil {
				beta.update(func() { beta.contents["data"] = testingDirectory("new") })
				loop.flush()
				beta.update(func() {
					if !beta.contents["data"].Equal(test.data, true) {
						t.Error("content created on beta not replaced:", beta.contents["data"])
					}
				})
			}
		})
	}
}

// TestSynchronizeNoReplicaRecordInOtherModes tests that sessions in other modes
// don't keep a replica record, and that exposed content is synchronized as
// before.
func TestSynchronizeNoReplicaRecordInOtherModes(t *testing.T) {
	alpha := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile})
	beta := newTestingEndpoint(map[string]*core.Entry{testingFileRules: testingFile, "data": testingFile}, "data")
	loop := startTestingLoopWithAncestor(t, testingGitMode, testingDirectory(testingFileRules), alpha, beta)
	loop.waitForPolling()
	alpha.update(func() { delete(alpha.contents, testingFileRules) })
	loop.flush()
	alpha.update(func() {
		if !alpha.contents["data"].Equal(testingFile, true) {
			t.Error("exposed content not synchronized:", alpha.contents["data"])
		}
	})
	if _, err := os.Lstat(loop.controller.replicaRecordPath); !os.IsNotExist(err) {
		t.Error("replica record saved:", err)
	}
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

// TestSynchronizeWarnings tests that snapshot warnings are reported as scan
// problems, along with the problems in the snapshot's content, that they're
// sorted after those problems and truncated like them, that they remain
// identifiable as warnings, and that they don't outlive the snapshot.
func TestSynchronizeWarnings(t *testing.T) {
	alpha := newTestingEndpoint(map[string]*core.Entry{"problematic": {Kind: core.EntryKind_Problematic, Problem: "unreadable"}})
	alpha.warnings = []*core.Problem{{Path: "warned", Error: "warning", Warning: true}, {Path: "a", Error: "another warning", Warning: true}}
	beta := newTestingEndpoint(map[string]*core.Entry{"file": testingFile})
	beta.warnings = []*core.Problem{{Path: "file", Error: "beta warning", Warning: true}}
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()

	// Verify the state.
	loop.controller.stateLock.Lock()
	alphaProblems := core.CopyProblems(loop.controller.state.AlphaState.ScanProblems)
	betaProblems := core.CopyProblems(loop.controller.state.BetaState.ScanProblems)
	loop.controller.stateLock.UnlockWithoutNotify()
	core.SortProblems(alphaProblems)
	if len(alphaProblems) != 3 ||
		alphaProblems[0].Path != "problematic" || alphaProblems[1].Path != "a" || alphaProblems[2].Path != "warned" {
		t.Errorf("incorrect alpha scan problems: %v", alphaProblems)
	}
	if len(betaProblems) != 1 || betaProblems[0].Error != "beta warning" {
		t.Errorf("incorrect beta scan problems: %v", betaProblems)
	}

	// Verify that the report sorts them, without affecting the snapshots, and
	// that it distinguishes them from the problem that isn't a warning.
	report := loop.controller.remoteReport()
	var paths []string
	var warnings []bool
	for _, problem := range report.Problems {
		paths = append(paths, problem.Path)
		warnings = append(warnings, problem.Warning)
	}
	if !slices.Equal(paths, []string{"problematic", "a", "warned", "file"}) {
		t.Errorf("incorrect report paths: %v", paths)
	}
	if !slices.Equal(warnings, []bool{false, true, true, true}) {
		t.Errorf("incorrect report warning indications: %v", warnings)
	}
	if alpha.warnings[0].Path != "warned" {
		t.Error("snapshot warnings modified")
	}

	// Verify that the conflict listing distinguishes them as well.
	listing, err := loop.controller.remoteConflicts("sha256")
	if err != nil {
		t.Fatal("unable to list conflicts:", err)
	}
	listed := make(map[string]bool)
	for _, problem := range listing.Problems {
		listed[problem.Path] = problem.Warning
	}
	if len(listed) != 4 || !listed["a"] || listed["problematic"] || !listed["warned"] || !listed["file"] {
		t.Errorf("incorrect listing warning indications: %v", listed)
	}

	// Verify that warnings are truncated in reports.
	alpha.update(func() {
		alpha.warnings = nil
		for i := 0; i <= maximumListScanProblems; i++ {
			alpha.warnings = append(alpha.warnings, &core.Problem{Path: "w" + string(rune('a'+i)), Error: "warning", Warning: true})
		}
	})
	loop.flush()
	report = loop.controller.remoteReport()
	if report.ExcludedProblems != 2 {
		t.Errorf("incorrect excluded problem count: %d", report.ExcludedProblems)
	}

	// Verify that warnings disappear with the next snapshot.
	alpha.update(func() { alpha.warnings = nil })
	loop.waitForPolling()
	loop.flush()
	loop.controller.stateLock.Lock()
	alphaProblems = core.CopyProblems(loop.controller.state.AlphaState.ScanProblems)
	loop.controller.stateLock.UnlockWithoutNotify()
	if len(alphaProblems) != 1 || alphaProblems[0].Path != "problematic" {
		t.Errorf("incorrect alpha scan problems after warnings removed: %v", alphaProblems)
	}
}
