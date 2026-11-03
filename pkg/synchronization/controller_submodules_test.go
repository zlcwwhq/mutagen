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
