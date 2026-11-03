package core

import (
	"testing"
)

// TestReconcileGitModeSubmodules tests ReconcileGitMode with submodules, whose
// working trees are reconciled path by path like any other directory, and whose
// Git entries are leaves that identify their commits, with a few exceptions:
// diverged commits in the two-way-safe mode, a Git entry that's missing on one
// side, and an empty directory opposite a submodule.
func TestReconcileGitModeSubmodules(t *testing.T) {
	// Define helper variables. The ancestor and endpoints in these test cases
	// are all directories, as is the case for a synchronization root, with the
	// submodule (if any) at sub. The commit that the ancestor records is tGL0.
	type contents = map[string]*Entry
	root := func(entry *Entry) *Entry {
		return nested("sub", entry)
	}
	submodule := func(link *Entry, files contents) *Entry {
		return root(testingSubmodule(link, files))
	}
	plain := func(files contents) *Entry {
		return root(&Entry{Contents: files})
	}
	base := contents{"a": tF1, "b": tF1}
	otherURL := &Entry{Kind: EntryKind_GitLink, Digest: tGL0.Digest, Url: "https://example.com/other.git"}

	// Define test cases.
	var tests = []struct {
		// description is a human readable description of the test case.
		description string
		// modes are the synchronization modes for which the test case should
		// apply.
		modes []SynchronizationMode
		// ancestor is the root ancestor entry.
		ancestor *Entry
		// alpha is the root alpha entry.
		alpha *Entry
		// beta is the root beta entry.
		beta *Entry
		// expectedAncestorChanges are the expected ancestor changes.
		expectedAncestorChanges []*Change
		// expectedAlphaChanges are the expected alpha changes.
		expectedAlphaChanges []*Change
		// expectedBetaChanges are the expected beta changes.
		expectedBetaChanges []*Change
		// expectedConflicts are the expected conflicts.
		expectedConflicts []*Conflict
	}{
		// Changes to the working tree with the same commit on both sides are
		// reconciled like those to any other directory.
		{
			description:         "file modified on alpha",
			modes:               allModes,
			ancestor:            submodule(tGL0, base),
			alpha:               submodule(tGL0, contents{"a": tF2, "b": tF1}),
			beta:                submodule(tGL0, base),
			expectedBetaChanges: []*Change{{Path: "sub/a", Old: tF1, New: tF2}},
		},
		{
			description:          "file modified on beta",
			modes:                twoWayModes,
			ancestor:             submodule(tGL0, base),
			alpha:                submodule(tGL0, base),
			beta:                 submodule(tGL0, contents{"a": tF1, "b": tF2}),
			expectedAlphaChanges: []*Change{{Path: "sub/b", Old: tF1, New: tF2}},
		},
		{
			description: "file modified on beta",
			modes:       oneWaySafeMode,
			ancestor:    submodule(tGL0, base),
			alpha:       submodule(tGL0, base),
			beta:        submodule(tGL0, contents{"a": tF1, "b": tF2}),
			expectedConflicts: []*Conflict{{
				Root:         "sub/b",
				AlphaChanges: []*Change{{Path: "sub/b", Old: tF1, New: tF1}},
				BetaChanges:  []*Change{{Path: "sub/b", Old: tF1, New: tF2}},
			}},
		},
		{
			description:         "file modified on beta",
			modes:               oneWayReplicaMode,
			ancestor:            submodule(tGL0, base),
			alpha:               submodule(tGL0, base),
			beta:                submodule(tGL0, contents{"a": tF1, "b": tF2}),
			expectedBetaChanges: []*Change{{Path: "sub/b", Old: tF2, New: tF1}},
		},
		{
			description:          "modification undone on alpha with another file modified on beta",
			modes:                twoWayModes,
			ancestor:             submodule(tGL0, contents{"x": tF2, "y": tF1}),
			alpha:                submodule(tGL0, contents{"x": tF1, "y": tF1}),
			beta:                 submodule(tGL0, contents{"x": tF2, "y": tF3}),
			expectedAlphaChanges: []*Change{{Path: "sub/y", Old: tF1, New: tF3}},
			expectedBetaChanges:  []*Change{{Path: "sub/x", Old: tF2, New: tF1}},
		},
		{
			description:          "different files modified on each side",
			modes:                twoWayModes,
			ancestor:             submodule(tGL0, base),
			alpha:                submodule(tGL0, contents{"a": tF2, "b": tF1}),
			beta:                 submodule(tGL0, contents{"a": tF1, "b": tF3, "c": tF1}),
			expectedAlphaChanges: []*Change{{Path: "sub/b", Old: tF1, New: tF3}, {Path: "sub/c", New: tF1}},
			expectedBetaChanges:  []*Change{{Path: "sub/a", Old: tF1, New: tF2}},
		},

		// A commit that moves on one side only is a change to the Git entry,
		// which propagates as such, and nothing else.
		{
			description:         "commit moved on alpha",
			modes:               allModes,
			ancestor:            submodule(tGL0, base),
			alpha:               submodule(tGL1, base),
			beta:                submodule(tGL0, base),
			expectedBetaChanges: []*Change{{Path: "sub/.git", Old: tGL0, New: tGL1}},
		},
		{
			description:          "commit moved on beta",
			modes:                twoWayModes,
			ancestor:             submodule(tGL0, base),
			alpha:                submodule(tGL0, base),
			beta:                 submodule(tGL1, base),
			expectedAlphaChanges: []*Change{{Path: "sub/.git", Old: tGL0, New: tGL1}},
		},
		{
			description: "commit moved on beta",
			modes:       oneWaySafeMode,
			ancestor:    submodule(tGL0, base),
			alpha:       submodule(tGL0, base),
			beta:        submodule(tGL1, base),
			expectedConflicts: []*Conflict{{
				Root:         "sub/.git",
				AlphaChanges: []*Change{{Path: "sub/.git", Old: tGL0, New: tGL0}},
				BetaChanges:  []*Change{{Path: "sub/.git", Old: tGL0, New: tGL1}},
			}},
		},
		{
			description:         "commit moved on beta",
			modes:               oneWayReplicaMode,
			ancestor:            submodule(tGL0, base),
			alpha:               submodule(tGL0, base),
			beta:                submodule(tGL1, base),
			expectedBetaChanges: []*Change{{Path: "sub/.git", Old: tGL1, New: tGL0}},
		},
		{
			description:          "commit switched on alpha with other files modified on beta",
			modes:                twoWayModes,
			ancestor:             submodule(tGL0, base),
			alpha:                submodule(tGL1, contents{"a": tF2, "b": tF1}),
			beta:                 submodule(tGL0, contents{"a": tF1, "b": tF3}),
			expectedAlphaChanges: []*Change{{Path: "sub/b", Old: tF1, New: tF3}},
			expectedBetaChanges: []*Change{
				{Path: "sub/.git", Old: tGL0, New: tGL1},
				{Path: "sub/a", Old: tF1, New: tF2},
			},
		},
		{
			description: "commit switched on alpha with other files modified on beta",
			modes:       oneWaySafeMode,
			ancestor:    submodule(tGL0, base),
			alpha:       submodule(tGL1, contents{"a": tF2, "b": tF1}),
			beta:        submodule(tGL0, contents{"a": tF1, "b": tF3}),
			expectedBetaChanges: []*Change{
				{Path: "sub/.git", Old: tGL0, New: tGL1},
				{Path: "sub/a", Old: tF1, New: tF2},
			},
			expectedConflicts: []*Conflict{{
				Root:         "sub/b",
				AlphaChanges: []*Change{{Path: "sub/b", Old: tF1, New: tF1}},
				BetaChanges:  []*Change{{Path: "sub/b", Old: tF1, New: tF3}},
			}},
		},
		{
			description: "commit switched on alpha with other files modified on beta",
			modes:       oneWayReplicaMode,
			ancestor:    submodule(tGL0, base),
			alpha:       submodule(tGL1, contents{"a": tF2, "b": tF1}),
			beta:        submodule(tGL0, contents{"a": tF1, "b": tF3}),
			expectedBetaChanges: []*Change{
				{Path: "sub/.git", Old: tGL0, New: tGL1},
				{Path: "sub/a", Old: tF1, New: tF2},
				{Path: "sub/b", Old: tF3, New: tF1},
			},
		},
		{
			description: "both sides moved to the same commit",
			modes:       allModes,
			ancestor:    submodule(tGL0, base),
			alpha:       submodule(tGL1, contents{"a": tF2, "b": tF1}),
			beta:        submodule(tGL1, contents{"a": tF2, "b": tF1}),
			expectedAncestorChanges: []*Change{
				{Path: "sub/.git", New: tGL1},
				{Path: "sub/a", New: tF2},
			},
		},
		{
			description: "same commit with different URLs",
			modes:       allModes,
			ancestor:    submodule(tGL0, base),
			alpha:       submodule(tGL0, base),
			beta:        submodule(otherURL, base),
		},
		{
			description: "same commit with different seeds",
			modes:       allModes,
			ancestor:    submodule(testingSeeded(tGL0, "ancestor"), base),
			alpha:       submodule(tGL0, base),
			beta:        submodule(testingSeeded(tGL0, "beta"), base),
		},

		// Commits that diverge are a conflict for the whole submodule in the
		// two-way-safe mode, without anything inside of it being reconciled.
		// Other modes resolve them in alpha's favor, path by path.
		{
			description: "diverged commits",
			modes:       twoWaySafeMode,
			ancestor:    submodule(tGL0, base),
			alpha:       submodule(tGL1, contents{"a": tF2, "b": tF1, "c": tF1}),
			beta:        submodule(tGL2, contents{"a": tF1, "b": tF3}),
			expectedConflicts: []*Conflict{{
				Root:         "sub",
				AlphaChanges: []*Change{{Path: "sub/.git", Old: tGL0, New: tGL1}},
				BetaChanges:  []*Change{{Path: "sub/.git", Old: tGL0, New: tGL2}},
			}},
		},
		{
			description:          "diverged commits",
			modes:                twoWayResolvedMode,
			ancestor:             submodule(tGL0, base),
			alpha:                submodule(tGL1, contents{"a": tF2, "b": tF1, "c": tF1}),
			beta:                 submodule(tGL2, contents{"a": tF1, "b": tF3}),
			expectedAlphaChanges: []*Change{{Path: "sub/b", Old: tF1, New: tF3}},
			expectedBetaChanges: []*Change{
				{Path: "sub/.git", Old: tGL2, New: tGL1},
				{Path: "sub/a", Old: tF1, New: tF2},
				{Path: "sub/c", New: tF1},
			},
		},
		{
			description: "diverged commits",
			modes:       oneWaySafeMode,
			ancestor:    submodule(tGL0, base),
			alpha:       submodule(tGL1, contents{"a": tF2, "b": tF1, "c": tF1}),
			beta:        submodule(tGL2, contents{"a": tF1, "b": tF3}),
			expectedBetaChanges: []*Change{
				{Path: "sub/a", Old: tF1, New: tF2},
				{Path: "sub/c", New: tF1},
			},
			expectedConflicts: []*Conflict{
				{
					Root:         "sub/.git",
					AlphaChanges: []*Change{{Path: "sub/.git", Old: tGL0, New: tGL1}},
					BetaChanges:  []*Change{{Path: "sub/.git", Old: tGL0, New: tGL2}},
				},
				{
					Root:         "sub/b",
					AlphaChanges: []*Change{{Path: "sub/b", Old: tF1, New: tF1}},
					BetaChanges:  []*Change{{Path: "sub/b", Old: tF1, New: tF3}},
				},
			},
		},
		{
			description: "diverged commits",
			modes:       oneWayReplicaMode,
			ancestor:    submodule(tGL0, base),
			alpha:       submodule(tGL1, contents{"a": tF2, "b": tF1, "c": tF1}),
			beta:        submodule(tGL2, contents{"a": tF1, "b": tF3}),
			expectedBetaChanges: []*Change{
				{Path: "sub/.git", Old: tGL2, New: tGL1},
				{Path: "sub/a", Old: tF1, New: tF2},
				{Path: "sub/b", Old: tF3, New: tF1},
				{Path: "sub/c", New: tF1},
			},
		},
		{
			description: "different commits without an ancestor",
			modes:       twoWaySafeMode,
			ancestor:    tD0,
			alpha:       submodule(tGL1, contents{"a": tF2}),
			beta:        submodule(tGL2, contents{"a": tF1}),
			expectedConflicts: []*Conflict{{
				Root:         "sub",
				AlphaChanges: []*Change{{Path: "sub/.git", New: tGL1}},
				BetaChanges:  []*Change{{Path: "sub/.git", New: tGL2}},
			}},
		},
		{
			description:             "different commits without an ancestor",
			modes:                   twoWayResolvedMode,
			ancestor:                tD0,
			alpha:                   submodule(tGL1, contents{"a": tF1}),
			beta:                    submodule(tGL2, contents{"a": tF1}),
			expectedAncestorChanges: []*Change{{Path: "sub", New: tD0}, {Path: "sub/a", New: tF1}},
			expectedBetaChanges:     []*Change{{Path: "sub/.git", Old: tGL2, New: tGL1}},
		},
		{
			description: "diverged commits of nested submodule",
			modes:       twoWaySafeMode,
			ancestor:    submodule(tGL0, contents{"a": tF1, "nested": testingSubmodule(tGL0, base)}),
			alpha:       submodule(tGL0, contents{"a": tF2, "nested": testingSubmodule(tGL1, contents{"a": tF2, "b": tF1})}),
			beta:        submodule(tGL0, contents{"a": tF1, "nested": testingSubmodule(tGL2, base)}),
			expectedBetaChanges: []*Change{
				{Path: "sub/a", Old: tF1, New: tF2},
			},
			expectedConflicts: []*Conflict{{
				Root:         "sub/nested",
				AlphaChanges: []*Change{{Path: "sub/nested/.git", Old: tGL0, New: tGL1}},
				BetaChanges:  []*Change{{Path: "sub/nested/.git", Old: tGL0, New: tGL2}},
			}},
		},
		{
			description: "commit moved on beta with files modified on both sides",
			modes:       twoWaySafeMode,
			ancestor:    submodule(tGL0, base),
			alpha:       submodule(tGL0, contents{"a": tF2, "b": tF1}),
			beta:        submodule(tGL2, contents{"a": tF1, "b": tF3}),
			expectedAlphaChanges: []*Change{
				{Path: "sub/.git", Old: tGL0, New: tGL2},
				{Path: "sub/b", Old: tF1, New: tF3},
			},
			expectedBetaChanges: []*Change{{Path: "sub/a", Old: tF1, New: tF2}},
		},

		// A Git entry that's missing on one side is attached to that side,
		// rather than deleted on the other, except that one-way modes never
		// give alpha anything.
		{
			description:         "Git entry missing on beta",
			modes:               allModes,
			ancestor:            submodule(tGL0, base),
			alpha:               submodule(tGL0, base),
			beta:                plain(base),
			expectedBetaChanges: []*Change{{Path: "sub/.git", New: tGL0}},
		},
		{
			description:         "Git entry missing on beta with commit moved on alpha",
			modes:               allModes,
			ancestor:            submodule(tGL0, base),
			alpha:               submodule(tGL1, base),
			beta:                plain(base),
			expectedBetaChanges: []*Change{{Path: "sub/.git", New: tGL1}},
		},
		{
			description:         "Git entry missing on beta without one in the ancestor",
			modes:               allModes,
			ancestor:            plain(base),
			alpha:               submodule(tGL0, base),
			beta:                plain(base),
			expectedBetaChanges: []*Change{{Path: "sub/.git", New: tGL0}},
		},
		{
			description: "Git entry missing on beta without an ancestor",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       submodule(tGL0, base),
			beta:        plain(base),
			expectedAncestorChanges: []*Change{
				{Path: "sub", New: tD0},
				{Path: "sub/a", New: tF1},
				{Path: "sub/b", New: tF1},
			},
			expectedBetaChanges: []*Change{{Path: "sub/.git", New: tGL0}},
		},
		{
			description:          "Git entry missing on alpha",
			modes:                twoWayModes,
			ancestor:             submodule(tGL0, base),
			alpha:                plain(base),
			beta:                 submodule(tGL0, base),
			expectedAlphaChanges: []*Change{{Path: "sub/.git", New: tGL0}},
		},
		{
			description: "Git entry missing on alpha",
			modes:       oneWayModes,
			ancestor:    submodule(tGL0, base),
			alpha:       plain(base),
			beta:        submodule(tGL0, base),
		},
		{
			description:          "Git entry missing on alpha with file modified on alpha",
			modes:                twoWayModes,
			ancestor:             submodule(tGL0, base),
			alpha:                plain(contents{"a": tF2, "b": tF1}),
			beta:                 submodule(tGL0, base),
			expectedAlphaChanges: []*Change{{Path: "sub/.git", New: tGL0}},
			expectedBetaChanges:  []*Change{{Path: "sub/a", Old: tF1, New: tF2}},
		},
		{
			description:         "Git entry missing on alpha with file modified on alpha",
			modes:               oneWayModes,
			ancestor:            submodule(tGL0, base),
			alpha:               plain(contents{"a": tF2, "b": tF1}),
			beta:                submodule(tGL0, base),
			expectedBetaChanges: []*Change{{Path: "sub/a", Old: tF1, New: tF2}},
		},

		// A Git entry that's untracked on one side (a repository without
		// commits or an origin) doesn't stop the working tree from being
		// reconciled, but nothing propagates to or from it.
		{
			description:             "Git entry untracked on beta",
			modes:                   allModes,
			ancestor:                submodule(tGL0, base),
			alpha:                   submodule(tGL1, contents{"a": tF2, "b": tF1}),
			beta:                    plain(contents{GitLinkName: tU, "a": tF1, "b": tF1}),
			expectedAncestorChanges: []*Change{{Path: "sub/.git"}},
			expectedBetaChanges:     []*Change{{Path: "sub/a", Old: tF1, New: tF2}},
		},
		{
			description:             "Git entry untracked on alpha",
			modes:                   twoWayModes,
			ancestor:                submodule(tGL0, base),
			alpha:                   plain(contents{GitLinkName: tU, "a": tF1, "b": tF1}),
			beta:                    submodule(tGL1, contents{"a": tF1, "b": tF2}),
			expectedAncestorChanges: []*Change{{Path: "sub/.git"}},
			expectedAlphaChanges:    []*Change{{Path: "sub/b", Old: tF1, New: tF2}},
		},
		{
			description: "Git entry untracked on beta without an ancestor",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       submodule(tGL0, base),
			beta:        plain(contents{GitLinkName: tU, "a": tF1, "b": tF1}),
			expectedAncestorChanges: []*Change{
				{Path: "sub", New: tD0},
				{Path: "sub/a", New: tF1},
				{Path: "sub/b", New: tF1},
			},
		},

		// An empty directory opposite a submodule is populated, whatever the
		// ancestor contains, except that one-way modes never give alpha
		// anything, nor do they delete anything from beta in that case.
		{
			description:         "empty directory on beta without an ancestor",
			modes:               allModes,
			ancestor:            tD0,
			alpha:               submodule(tGL0, contents{"a": tF1, "untracked": tU}),
			beta:                root(tD0),
			expectedBetaChanges: []*Change{{Path: "sub", Old: tD0, New: testingSubmodule(tGL0, contents{"a": tF1})}},
		},
		{
			description:         "empty directory on beta with an empty directory in the ancestor",
			modes:               allModes,
			ancestor:            root(tD0),
			alpha:               submodule(tGL0, base),
			beta:                root(tD0),
			expectedBetaChanges: []*Change{{Path: "sub", Old: tD0, New: testingSubmodule(tGL0, base)}},
		},
		{
			description:         "empty directory on beta with the submodule in the ancestor",
			modes:               allModes,
			ancestor:            submodule(tGL0, base),
			alpha:               submodule(tGL1, contents{"a": tF2}),
			beta:                root(tD0),
			expectedBetaChanges: []*Change{{Path: "sub", Old: tD0, New: testingSubmodule(tGL1, contents{"a": tF2})}},
		},
		{
			description:          "empty directory on alpha without an ancestor",
			modes:                twoWayModes,
			ancestor:             tD0,
			alpha:                root(tD0),
			beta:                 submodule(tGL0, contents{"a": tF1, "untracked": tU}),
			expectedAlphaChanges: []*Change{{Path: "sub", Old: tD0, New: testingSubmodule(tGL0, contents{"a": tF1})}},
		},
		{
			description: "empty directory on alpha without an ancestor",
			modes:       oneWayModes,
			ancestor:    tD0,
			alpha:       root(tD0),
			beta:        submodule(tGL0, base),
		},
		{
			description:          "empty directory on alpha with the submodule in the ancestor",
			modes:                twoWayModes,
			ancestor:             submodule(tGL0, base),
			alpha:                root(tD0),
			beta:                 submodule(tGL0, base),
			expectedAlphaChanges: []*Change{{Path: "sub", Old: tD0, New: testingSubmodule(tGL0, base)}},
		},
		{
			description: "empty directory on alpha with the submodule in the ancestor",
			modes:       oneWayModes,
			ancestor:    submodule(tGL0, base),
			alpha:       root(tD0),
			beta:        submodule(tGL0, base),
		},
		{
			description:             "empty directories on both sides",
			modes:                   allModes,
			ancestor:                submodule(tGL0, base),
			alpha:                   root(tD0),
			beta:                    root(tD0),
			expectedAncestorChanges: []*Change{{Path: "sub/.git"}, {Path: "sub/a"}, {Path: "sub/b"}},
		},

		// A submodule that's missing entirely on one side is created there,
		// with the usual rules for deletions.
		{
			description:         "submodule missing on beta",
			modes:               allModes,
			ancestor:            tD0,
			alpha:               submodule(tGL0, contents{"a": tF1, "untracked": tU}),
			beta:                tD0,
			expectedBetaChanges: []*Change{{Path: "sub", New: testingSubmodule(tGL0, contents{"a": tF1})}},
		},
		{
			description:          "submodule missing on alpha",
			modes:                twoWayModes,
			ancestor:             tD0,
			alpha:                tD0,
			beta:                 submodule(tGL0, base),
			expectedAlphaChanges: []*Change{{Path: "sub", New: testingSubmodule(tGL0, base)}},
		},
		{
			description: "submodule missing on alpha",
			modes:       oneWaySafeMode,
			ancestor:    tD0,
			alpha:       tD0,
			beta:        submodule(tGL0, base),
		},
		{
			description:         "submodule missing on alpha",
			modes:               oneWayReplicaMode,
			ancestor:            tD0,
			alpha:               tD0,
			beta:                submodule(tGL0, base),
			expectedBetaChanges: []*Change{{Path: "sub", Old: testingSubmodule(tGL0, base)}},
		},
		{
			description:         "submodule deleted on alpha",
			modes:               allModes,
			ancestor:            submodule(tGL0, base),
			alpha:               tD0,
			beta:                submodule(tGL0, base),
			expectedBetaChanges: []*Change{{Path: "sub", Old: testingSubmodule(tGL0, base)}},
		},
	}

	// Process test cases.
	for _, test := range tests {
		// Test each mode.
		for _, mode := range test.modes {
			// Perform reconciliation.
			ancestorChanges, alphaChanges, betaChanges, conflicts := ReconcileGitMode(
				test.ancestor, test.alpha, test.beta, mode,
			)

			// Verify the ancestor changes.
			if !testingChangeListsEqual(ancestorChanges, test.expectedAncestorChanges) {
				t.Errorf("%s (%s): ancestor changes do not match expected: %v != %v",
					test.description, mode.Description(), ancestorChanges, test.expectedAncestorChanges,
				)
			}

			// Verify the alpha changes.
			if !testingChangeListsEqual(alphaChanges, test.expectedAlphaChanges) {
				t.Errorf("%s (%s): alpha changes do not match expected: %v != %v",
					test.description, mode.Description(), alphaChanges, test.expectedAlphaChanges,
				)
			}

			// Verify the beta changes.
			if !testingChangeListsEqual(betaChanges, test.expectedBetaChanges) {
				t.Errorf("%s (%s): beta changes do not match expected: %v != %v",
					test.description, mode.Description(), betaChanges, test.expectedBetaChanges,
				)
			}

			// Verify the conflicts.
			if !testingConflictListsEqual(conflicts, test.expectedConflicts) {
				t.Errorf("%s (%s): conflicts do not match expected: %v != %v",
					test.description, mode.Description(), conflicts, test.expectedConflicts,
				)
			}

			// Verify that every change and conflict is valid, as the
			// synchronization loop requires of conflicts, and that new content
			// is synchronizable.
			for _, changes := range [][]*Change{ancestorChanges, alphaChanges, betaChanges} {
				for _, change := range changes {
					if err := change.EnsureValid(true); err != nil {
						t.Errorf("%s (%s): invalid change at %s: %v",
							test.description, mode.Description(), change.Path, err,
						)
					}
				}
			}
			for _, conflict := range conflicts {
				if err := conflict.EnsureValid(); err != nil {
					t.Errorf("%s (%s): invalid conflict at %s: %v",
						test.description, mode.Description(), conflict.Root, err,
					)
				}
			}
		}
	}
}

// TestReconcileGitModeSubmoduleChangesCarryGitLinkFields tests that the changes
// that reconciliation computes for submodules carry the fields of GitLink
// entries that don't participate in equality, such as the URL, which the
// receiving endpoint needs in order to obtain the commit.
func TestReconcileGitModeSubmoduleChangesCarryGitLinkFields(t *testing.T) {
	// Define a GitLink entry with all of its fields set, and a function that
	// verifies that the GitLink entry in a list of changes retains them.
	link := &Entry{Kind: EntryKind_GitLink, Digest: tGL1.Digest, Url: tGitURL, AutoCRLF: "true", Eol: "lf"}
	verify := func(description string, changes []*Change, path string, extract func(*Entry) *Entry) {
		t.Helper()
		if len(changes) != 1 || changes[0].Path != path {
			t.Errorf("%s: incorrect changes: %v", description, changes)
		} else if result := extract(changes[0].New); result == nil {
			t.Errorf("%s: change without GitLink entry: %v", description, changes[0])
		} else if result.Url != link.Url || result.AutoCRLF != link.AutoCRLF || result.Eol != link.Eol {
			t.Errorf("%s: GitLink entry fields not retained: %v", description, result)
		}
	}
	itself := func(entry *Entry) *Entry { return entry }
	contained := func(entry *Entry) *Entry { return entry.gitLink() }

	// Verify the changes for following, attaching, populating, and creating.
	mode := SynchronizationMode_SynchronizationModeTwoWaySafe
	ancestor := nested("sub", testingSubmodule(tGL0, nil))
	alpha := nested("sub", testingSubmodule(link, nil))
	_, _, changes, _ := ReconcileGitMode(ancestor, alpha, ancestor, mode)
	verify("follow", changes, "sub/.git", itself)
	_, _, changes, _ = ReconcileGitMode(
		nested("sub", testingSubmodule(tGL0, map[string]*Entry{"file": tF1})),
		nested("sub", testingSubmodule(link, map[string]*Entry{"file": tF1})),
		nested("sub", nested("file", tF1)),
		mode,
	)
	verify("attach", changes, "sub/.git", itself)
	_, _, changes, _ = ReconcileGitMode(ancestor, alpha, nested("sub", tD0), mode)
	verify("population", changes, "sub", contained)
	_, _, changes, _ = ReconcileGitMode(tD0, alpha, tD0, mode)
	verify("creation", changes, "sub", contained)
}
