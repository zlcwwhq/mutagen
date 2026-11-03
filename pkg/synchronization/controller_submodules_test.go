package synchronization

import (
	"slices"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// testingLink is a GitLink entry for testing.
var testingLink = &core.Entry{
	Kind:   core.EntryKind_GitLink,
	Digest: []byte{0xc0},
	Url:    "https://example.com/submodule.git",
}

// testingClonedFile is a file entry with the content that a clone has, as
// opposed to the uncommitted changes that testingFile represents.
var testingClonedFile = &core.Entry{Kind: core.EntryKind_File, Digest: []byte{2}}

// testingSeededLink creates a copy of testingLink that carries a seed.
func testingSeededLink(seed string) *core.Entry {
	result := testingLink.Copy(core.EntryCopyBehaviorSlim)
	result.Seed = seed
	return result
}

// testingSubmodule creates a submodule: a directory with the specified GitLink
// entry as its Git entry and the specified other contents.
func testingSubmodule(link *core.Entry, contents map[string]*core.Entry) *core.Entry {
	result := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{core.GitLinkName: link}}
	for name, entry := range contents {
		result.Contents[name] = entry
	}
	return result
}

// TestPopulatesSubmodules tests populatesSubmodules.
func TestPopulatesSubmodules(t *testing.T) {
	// Define entries.
	seeded := func(seed string) *core.Entry {
		return testingSubmodule(testingSeededLink(seed), map[string]*core.Entry{"file": testingFile})
	}
	ancestor := testingDirectory()
	ancestor.Contents["absorbed"] = seeded("absorbed")
	ancestor.Contents["unseeded"] = testingSubmodule(testingLink, nil)

	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// ancestor is the ancestor.
		ancestor *core.Entry
		// results are the transition results for each endpoint.
		results [][]*core.Entry
		// expected is the expected result.
		expected bool
	}{
		{"no results", ancestor, nil, false},
		{"empty results", ancestor, [][]*core.Entry{nil, {}}, false},
		{"results without seeds", ancestor, [][]*core.Entry{{testingFile, nil, testingSubmodule(testingLink, nil), testingLink}}, false},
		{"absorbed seed", ancestor, [][]*core.Entry{{seeded("absorbed")}}, false},
		{"absorbed seed at another path", ancestor, [][]*core.Entry{{nil}, {testingDirectory(), seeded("absorbed")}}, false},
		{"new seed", ancestor, [][]*core.Entry{{seeded("new")}}, true},
		{"new seed on beta", ancestor, [][]*core.Entry{{testingFile}, {nil, seeded("new")}}, true},
		{"new seed with absorbed seed", ancestor, [][]*core.Entry{{seeded("absorbed"), seeded("new")}}, true},
		{
			"nested new seed",
			ancestor,
			[][]*core.Entry{{{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{
				"directory": {Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"sub": seeded("new")}},
			}}}},
			true,
		},
		{"new seed on GitLink entry", ancestor, [][]*core.Entry{{testingSeededLink("new")}}, true},
		{"new seed without ancestor", nil, [][]*core.Entry{{seeded("new")}}, true},
		{"no seeds without ancestor", nil, [][]*core.Entry{{testingFile}}, false},
	}

	// Process test cases.
	for _, test := range tests {
		if result := populatesSubmodules(test.ancestor, test.results...); result != test.expected {
			t.Errorf("%s: incorrect result: %t != %t", test.description, result, test.expected)
		}
	}
}

// TestSynchronizeFlushHeldForPopulations tests that a flush isn't answered by a
// synchronization cycle that populated submodules, which only provides their
// clones, but rather by the first cycle that doesn't, by which point the
// content that the other endpoint has in them, and submodules nested in them,
// have followed. It also tests that populated submodules' contents aren't
// staged, and that the scans receive the seeds that the ancestor carries.
func TestSynchronizeFlushHeldForPopulations(t *testing.T) {
	// Start with nothing to synchronize, but with clones for beta to populate
	// submodules with, which differ from alpha's content.
	alpha, beta := newTestingEndpoint(nil), newTestingEndpoint(nil)
	beta.populations = map[string]*core.Entry{
		"sub": testingSubmodule(testingSeededLink("first"), map[string]*core.Entry{
			"x":      testingClonedFile,
			"nested": testingDirectory(),
		}),
		"sub/nested": testingSubmodule(testingSeededLink("second"), map[string]*core.Entry{
			"y": testingClonedFile,
		}),
	}
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()
	loop.verify(1, nil, nil)

	// Create a submodule on alpha, with a nested submodule, and uncommitted
	// changes in both, and flush.
	alpha.update(func() {
		alpha.contents["sub"] = testingSubmodule(testingLink, map[string]*core.Entry{
			"x":      testingFile,
			"nested": testingSubmodule(testingLink, map[string]*core.Entry{"y": testingFile}),
		})
	})
	loop.flush()

	// The flush must have waited for three cycles: one that populated the
	// submodule, one that populated the nested submodule and transferred the
	// submodule's changes, and one that transferred the nested submodule's
	// changes. Only the changes are staged.
	loop.verifyCycles(4)
	loop.verifyOperations("alpha", alpha, nil, nil)
	loop.verifyOperations("beta", beta,
		[][]string{{"sub/x"}, {"sub/nested/y"}},
		[][]string{{"sub"}, {"sub/nested", "sub/x"}, {"sub/nested/y"}},
	)
	beta.update(func() {
		sub := beta.contents["sub"]
		if !sub.Contents["x"].Equal(testingFile, true) || !sub.Contents["nested"].Contents["y"].Equal(testingFile, true) {
			t.Errorf("changes not synchronized: %v", sub)
		}
	})
	loop.controller.stateLock.Lock()
	conflicts := loop.controller.state.Conflicts
	loop.controller.stateLock.UnlockWithoutNotify()
	if len(conflicts) != 0 {
		t.Errorf("unexpected conflicts: %v", conflicts)
	}

	// Every cycle of the flush must have forced full scans, which received the
	// seeds as the ancestor absorbed them, as must the ancestor itself.
	for name, endpoint := range map[string]*testingEndpoint{"alpha": alpha, "beta": beta} {
		_, fullScans, _, _ := endpoint.status()
		if fullScans != 3 {
			t.Errorf("incorrect %s full scan count: %d", name, fullScans)
		}
		endpoint.update(func() {
			expected := [][]string{{}, {}, {"first"}, {"first", "second"}}
			if !slices.EqualFunc(endpoint.scanSeeds, expected, slices.Equal[[]string]) {
				t.Errorf("incorrect seeds received by %s: %v != %v", name, endpoint.scanSeeds, expected)
			}
		})
	}
	seeds := loop.ancestor().GitLinkSeeds()
	slices.Sort(seeds)
	if !slices.Equal(seeds, []string{"first", "second"}) {
		t.Errorf("incorrect seeds in ancestor: %v", seeds)
	}

	// No more cycles occur on their own.
	loop.waitForPolling()
	loop.verifyCycles(4)
}

// TestSynchronizeFlushHeldForRulesAfterPopulation tests that a flush is held
// for a cycle that only delivers rules after a cycle that populated
// submodules, even if an earlier cycle of the same flush only delivered rules
// too. The population makes progress, so it's a new opportunity to deliver
// rules, which the submodule's uncommitted rule files require, and without
// which the rest of the submodule's content wouldn't follow before the flush
// is answered.
func TestSynchronizeFlushHeldForRulesAfterPopulation(t *testing.T) {
	// Start with nothing to synchronize, but with a clone for beta to populate
	// a submodule with, which lacks the submodule's rule file.
	alpha, beta := newTestingEndpoint(nil), newTestingEndpoint(nil)
	beta.populations = map[string]*core.Entry{
		"sub": testingSubmodule(testingSeededLink("seed"), map[string]*core.Entry{"x": testingClonedFile}),
	}
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()
	loop.verify(1, nil, nil)

	// Create a rule file on alpha, along with a submodule that has an
	// uncommitted rule file and an uncommitted change, and flush.
	alpha.update(func() {
		alpha.contents[testingFileRules] = testingFile
		alpha.contents["sub"] = testingSubmodule(testingLink, map[string]*core.Entry{
			testingFileRules: testingFile,
			"x":              testingFile,
		})
	})
	loop.flush()

	// The flush must have waited for the rule file to be delivered, for the
	// submodule to be populated, for the submodule's rule file to be
	// delivered, and only then for the submodule's change. A cycle that only
	// delivers rules forces another cycle even without a flush, so the cycles
	// that the flush held are identified by their full scans, once the loop
	// has settled.
	loop.waitForPolling()
	loop.verifyCycles(5)
	loop.verifyOperations("beta", beta,
		[][]string{{testingFileRules}, {"sub/" + testingFileRules}, {"sub/x"}},
		[][]string{{testingFileRules}, {"sub"}, {"sub/" + testingFileRules}, {"sub/x"}},
	)
	for name, endpoint := range map[string]*testingEndpoint{"alpha": alpha, "beta": beta} {
		if _, fullScans, _, _ := endpoint.status(); fullScans != 4 {
			t.Errorf("incorrect %s full scan count: %d", name, fullScans)
		}
	}
}

// TestSynchronizeFlushNotHeldForFailedPopulation tests that a population that
// fails doesn't hold a flush, which is then answered with the failure, and
// doesn't cause any further cycles.
func TestSynchronizeFlushNotHeldForFailedPopulation(t *testing.T) {
	alpha, beta := newTestingEndpoint(nil), newTestingEndpoint(nil)
	beta.populations = map[string]*core.Entry{"sub": nil}
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()
	alpha.update(func() {
		alpha.contents["sub"] = testingSubmodule(testingLink, map[string]*core.Entry{"x": testingFile})
	})
	loop.flush()
	loop.verifyCycles(2)
	loop.verifyOperations("beta", beta, nil, [][]string{{"sub"}})
	loop.waitForPolling()
	loop.verifyCycles(2)
}

// TestSynchronizeAbsorbsReportedPopulation tests that a population that an
// endpoint reports in its snapshot, because the session stopped before saving
// the ancestor that the population yielded, is used as the ancestor's content
// for the submodule, so that the other endpoint's uncommitted changes in it
// propagate without conflicts, and without the deletion of a tracked file
// being undone.
func TestSynchronizeAbsorbsReportedPopulation(t *testing.T) {
	// Beta has a clean clone, while alpha has modified one file and deleted
	// another, and the ancestor doesn't have the submodule at all.
	clone := testingSubmodule(testingSeededLink("seed"), map[string]*core.Entry{
		"x": testingClonedFile,
		"y": testingClonedFile,
		"z": testingClonedFile,
	})
	alpha := newTestingEndpoint(map[string]*core.Entry{
		"sub": testingSubmodule(testingLink, map[string]*core.Entry{
			"x": testingFile,
			"z": testingClonedFile,
		}),
	})
	beta := newTestingEndpoint(map[string]*core.Entry{"sub": clone})
	beta.seeds = map[string]*core.Entry{"sub": clone}
	loop := startTestingLoop(t, testingGitMode, alpha, beta)
	loop.waitForPolling()

	// Verify that alpha's changes reached beta, and nothing else happened.
	loop.verifyCycles(1)
	loop.verifyOperations("alpha", alpha, nil, nil)
	loop.verifyOperations("beta", beta, [][]string{{"sub/x"}}, [][]string{{"sub/x", "sub/y"}})
	loop.controller.stateLock.Lock()
	conflicts := loop.controller.state.Conflicts
	loop.controller.stateLock.UnlockWithoutNotify()
	if len(conflicts) != 0 {
		t.Errorf("unexpected conflicts: %v", conflicts)
	}
	beta.update(func() {
		sub := beta.contents["sub"]
		if len(sub.Contents) != 3 || !sub.Contents["x"].Equal(testingFile, true) || sub.Contents["y"] != nil {
			t.Errorf("incorrect content on beta: %v", sub)
		}
	})

	// Verify that the ancestor absorbed the population.
	ancestor := loop.ancestor()
	if sub := ancestor.GetContents()["sub"]; sub == nil {
		t.Fatal("submodule missing from ancestor")
	} else if seed := sub.Contents[core.GitLinkName].GetSeed(); seed != "seed" {
		t.Errorf("incorrect seed in ancestor: %q", seed)
	} else if len(sub.Contents) != 3 || !sub.Contents["x"].Equal(testingFile, true) || sub.Contents["y"] != nil {
		t.Errorf("incorrect submodule content in ancestor: %v", sub)
	}
}

// TestSynchronizeSavesAbsorbedPopulation tests that a population that an
// endpoint reports in its snapshot is saved to the ancestor even if nothing
// else changes, so that the endpoint can discard its record of the population,
// which it does once a scan receives the population's seed.
func TestSynchronizeSavesAbsorbedPopulation(t *testing.T) {
	// The ancestor has the same content as both endpoints, but from an earlier
	// population, as it would if the submodule had been removed with git
	// submodule deinit on beta and populated again.
	contents := map[string]*core.Entry{"x": testingFile}
	ancestor := testingDirectory()
	ancestor.Contents["sub"] = testingSubmodule(testingSeededLink("old"), contents)
	population := testingSubmodule(testingSeededLink("new"), contents)
	alpha := newTestingEndpoint(map[string]*core.Entry{"sub": testingSubmodule(testingLink, contents)})
	beta := newTestingEndpoint(map[string]*core.Entry{"sub": population})
	beta.seeds = map[string]*core.Entry{"sub": population}
	loop := startTestingLoopWithAncestor(t, testingGitMode, ancestor, alpha, beta)
	loop.waitForPolling()

	// Verify that nothing was transitioned, but that the ancestor was saved
	// with the new seed.
	loop.verify(1, nil, nil)
	if seed := loop.ancestor().GetContents()["sub"].GetContents()[core.GitLinkName].GetSeed(); seed != "new" {
		t.Errorf("incorrect seed in saved ancestor: %q", seed)
	}

	// Verify that the next cycle's scans receive the new seed, after which beta
	// no longer reports the population.
	loop.wakeFromPolling()
	loop.waitForPolling()
	loop.verify(2, nil, nil)
	for name, endpoint := range map[string]*testingEndpoint{"alpha": alpha, "beta": beta} {
		endpoint.update(func() {
			expected := [][]string{{"old"}, {"new"}}
			if !slices.EqualFunc(endpoint.scanSeeds, expected, slices.Equal[[]string]) {
				t.Errorf("incorrect seeds received by %s: %v != %v", name, endpoint.scanSeeds, expected)
			}
		})
	}
	beta.update(func() {
		if len(beta.seeds) != 0 {
			t.Errorf("population still reported: %v", beta.seeds)
		}
	})
}
