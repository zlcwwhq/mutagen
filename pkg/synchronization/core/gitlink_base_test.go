package core

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestReconcileGitModeWithStatuses tests ReconcileGitModeWithStatuses for
// submodules that both sides have moved to the same commit since the ancestor
// (or that the ancestor doesn't have), whose merge base is the content that
// Git considers to be the commit's on either side, so that changes propagate
// from the side that made them, rather than conflicting because the ancestor
// has the content of another commit.
func TestReconcileGitModeWithStatuses(t *testing.T) {
	// Define helper variables. The ancestor and endpoints in these test cases
	// are all directories, with the submodule (if any) at sub. The ancestor
	// records the submodule at the commit of tGL0 (unless otherwise specified),
	// with the files of that commit, and both sides have moved to the commit of
	// tGL1, unless otherwise specified.
	type contents = map[string]*Entry
	submodule := func(link *Entry, files contents) *Entry {
		return nested("sub", testingSubmodule(link, files))
	}
	status := func(link *Entry, changed ...string) map[string]*SubmoduleStatus {
		return map[string]*SubmoduleStatus{"sub": {Commit: link.Digest, Changed: changed}}
	}
	ancestor := submodule(tGL0, contents{"lib": tF1, "other": tF1})
	clean := status(tGL1)
	directory := func(files contents) *Entry {
		return &Entry{Kind: EntryKind_Directory, Contents: files}
	}

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
		// alphaStatuses are alpha's submodule statuses.
		alphaStatuses map[string]*SubmoduleStatus
		// betaStatuses are beta's submodule statuses.
		betaStatuses map[string]*SubmoduleStatus
		// expectedAncestorChanges are the expected ancestor changes.
		expectedAncestorChanges []*Change
		// expectedAlphaChanges are the expected alpha changes.
		expectedAlphaChanges []*Change
		// expectedBetaChanges are the expected beta changes.
		expectedBetaChanges []*Change
		// expectedConflicts are the expected conflicts.
		expectedConflicts []*Conflict
	}{
		// An uncommitted change on alpha propagates to beta, which has the
		// commit's content, rather than conflicting with it, since the
		// ancestor has the content of another commit.
		{
			description:             "change on alpha",
			modes:                   allModes,
			ancestor:                ancestor,
			alpha:                   submodule(tGL1, contents{"lib": tF3, "other": tF1}),
			beta:                    submodule(tGL1, contents{"lib": tF2, "other": tF1}),
			alphaStatuses:           status(tGL1, "lib"),
			betaStatuses:            clean,
			expectedAncestorChanges: []*Change{{Path: "sub", New: testingSubmodule(tGL1, contents{"lib": tF2, "other": tF1})}},
			expectedBetaChanges:     []*Change{{Path: "sub/lib", Old: tF2, New: tF3}},
		},

		// Without statuses, the ancestor is the merge base, as it is if either
		// side lacks a status for the submodule, or if a status is for another
		// commit than the one that the side has, which makes the change look
		// like a conflict.
		{
			description:   "change on alpha without statuses",
			modes:         []SynchronizationMode{SynchronizationMode_SynchronizationModeTwoWaySafe},
			ancestor:      ancestor,
			alpha:         submodule(tGL1, contents{"lib": tF3, "other": tF1}),
			beta:          submodule(tGL1, contents{"lib": tF2, "other": tF1}),
			alphaStatuses: status(tGL1, "lib"),
			betaStatuses:  status(tGL2),
			expectedAncestorChanges: []*Change{
				{Path: "sub/.git", New: tGL1},
			},
			expectedConflicts: []*Conflict{{
				Root:         "sub/lib",
				AlphaChanges: []*Change{{Path: "sub/lib", Old: tF1, New: tF3}},
				BetaChanges:  []*Change{{Path: "sub/lib", Old: tF1, New: tF2}},
			}},
		},

		// A change on beta propagates to alpha in the two-way modes, while the
		// one-way modes treat it as they treat any change on beta.
		{
			description:             "change on beta",
			modes:                   twoWayModes,
			ancestor:                ancestor,
			alpha:                   submodule(tGL1, contents{"lib": tF2, "other": tF1}),
			beta:                    submodule(tGL1, contents{"lib": tF3, "other": tF1}),
			alphaStatuses:           clean,
			betaStatuses:            status(tGL1, "lib"),
			expectedAncestorChanges: []*Change{{Path: "sub", New: testingSubmodule(tGL1, contents{"lib": tF2, "other": tF1})}},
			expectedAlphaChanges:    []*Change{{Path: "sub/lib", Old: tF2, New: tF3}},
		},
		{
			description:             "change on beta",
			modes:                   oneWaySafeMode,
			ancestor:                ancestor,
			alpha:                   submodule(tGL1, contents{"lib": tF2, "other": tF1}),
			beta:                    submodule(tGL1, contents{"lib": tF3, "other": tF1}),
			alphaStatuses:           clean,
			betaStatuses:            status(tGL1, "lib"),
			expectedAncestorChanges: []*Change{{Path: "sub", New: testingSubmodule(tGL1, contents{"lib": tF2, "other": tF1})}},
			expectedConflicts: []*Conflict{{
				Root:         "sub/lib",
				AlphaChanges: []*Change{{Path: "sub/lib", Old: tF2, New: tF2}},
				BetaChanges:  []*Change{{Path: "sub/lib", Old: tF2, New: tF3}},
			}},
		},

		// Creations and deletions propagate in the same way. A file that Git
		// doesn't track counts as a change, and so does a file that a side
		// lacks.
		{
			description:   "creation and deletion on beta",
			modes:         twoWayModes,
			ancestor:      ancestor,
			alpha:         submodule(tGL1, contents{"lib": tF2, "other": tF1}),
			beta:          submodule(tGL1, contents{"lib": tF2, "new": tF3}),
			alphaStatuses: clean,
			betaStatuses:  status(tGL1, "new"),
			expectedAncestorChanges: []*Change{
				{Path: "sub", New: testingSubmodule(tGL1, contents{"lib": tF2, "other": tF1})},
			},
			expectedAlphaChanges: []*Change{
				{Path: "sub/new", New: tF3},
				{Path: "sub/other", Old: tF1},
			},
		},

		// If both sides have changed a file, then that's a conflict in the
		// two-way-safe mode, whatever the ancestor has, and alpha wins in the
		// two-way-resolved mode.
		{
			description:   "changes on both sides",
			modes:         []SynchronizationMode{SynchronizationMode_SynchronizationModeTwoWaySafe},
			ancestor:      submodule(tGL0, contents{"lib": tF3, "other": tF1}),
			alpha:         submodule(tGL1, contents{"lib": tF3, "other": tF1}),
			beta:          submodule(tGL1, contents{"lib": tF2, "other": tF1}),
			alphaStatuses: status(tGL1, "lib"),
			betaStatuses:  status(tGL1, "lib"),
			expectedAncestorChanges: []*Change{
				{Path: "sub", New: testingSubmodule(tGL1, contents{"other": tF1})},
			},
			expectedConflicts: []*Conflict{{
				Root:         "sub/lib",
				AlphaChanges: []*Change{{Path: "sub/lib", New: tF3}},
				BetaChanges:  []*Change{{Path: "sub/lib", New: tF2}},
			}},
		},
		{
			description:   "changes on both sides",
			modes:         []SynchronizationMode{SynchronizationMode_SynchronizationModeTwoWayResolved},
			ancestor:      submodule(tGL0, contents{"lib": tF3, "other": tF1}),
			alpha:         submodule(tGL1, contents{"lib": tF3, "other": tF1}),
			beta:          submodule(tGL1, contents{"lib": tF2, "other": tF1}),
			alphaStatuses: status(tGL1, "lib"),
			betaStatuses:  status(tGL1, "lib"),
			expectedAncestorChanges: []*Change{
				{Path: "sub", New: testingSubmodule(tGL1, contents{"other": tF1})},
			},
			expectedBetaChanges: []*Change{{Path: "sub/lib", Old: tF2, New: tF3}},
		},

		// If both sides have the commit's content according to Git, but their
		// files differ (such as because their line ending settings differ),
		// then there's no telling which is right, and that's a conflict.
		{
			description:   "unchanged on both sides with different content",
			modes:         []SynchronizationMode{SynchronizationMode_SynchronizationModeTwoWaySafe},
			ancestor:      ancestor,
			alpha:         submodule(tGL1, contents{"lib": tF2, "other": tF1}),
			beta:          submodule(tGL1, contents{"lib": tF3, "other": tF1}),
			alphaStatuses: clean,
			betaStatuses:  clean,
			expectedAncestorChanges: []*Change{
				{Path: "sub", New: testingSubmodule(tGL1, contents{"other": tF1})},
			},
			expectedConflicts: []*Conflict{{
				Root:         "sub/lib",
				AlphaChanges: []*Change{{Path: "sub/lib", New: tF2}},
				BetaChanges:  []*Change{{Path: "sub/lib", New: tF3}},
			}},
		},

		// The same applies if the ancestor doesn't have the submodule at all,
		// such as when a session is created between two checkouts.
		{
			description:             "change on alpha without ancestor",
			modes:                   allModes,
			ancestor:                tD0,
			alpha:                   submodule(tGL1, contents{"lib": tF3, "other": tF1}),
			beta:                    submodule(tGL1, contents{"lib": tF2, "other": tF1}),
			alphaStatuses:           status(tGL1, "lib"),
			betaStatuses:            clean,
			expectedAncestorChanges: []*Change{{Path: "sub", New: testingSubmodule(tGL1, contents{"lib": tF2, "other": tF1})}},
			expectedBetaChanges:     []*Change{{Path: "sub/lib", Old: tF2, New: tF3}},
		},

		// If the ancestor already has the commit, then statuses aren't used.
		{
			description:   "commit unchanged",
			modes:         []SynchronizationMode{SynchronizationMode_SynchronizationModeTwoWaySafe},
			ancestor:      submodule(tGL1, contents{"lib": tF1}),
			alpha:         submodule(tGL1, contents{"lib": tF3}),
			beta:          submodule(tGL1, contents{"lib": tF2}),
			alphaStatuses: status(tGL1, "lib"),
			betaStatuses:  status(tGL1),
			expectedConflicts: []*Conflict{{
				Root:         "sub/lib",
				AlphaChanges: []*Change{{Path: "sub/lib", Old: tF1, New: tF3}},
				BetaChanges:  []*Change{{Path: "sub/lib", Old: tF1, New: tF2}},
			}},
		},

		// Statuses that contradict each other, since both sides can't have the
		// commit's content at a path if they have different kinds of entries
		// there, aren't used.
		{
			description:   "contradictory statuses",
			modes:         []SynchronizationMode{SynchronizationMode_SynchronizationModeTwoWaySafe},
			ancestor:      submodule(tGL0, contents{"d": tF1}),
			alpha:         submodule(tGL1, contents{"d": tF2}),
			beta:          submodule(tGL1, contents{"d": directory(contents{"f": tF1})}),
			alphaStatuses: clean,
			betaStatuses:  clean,
			expectedAncestorChanges: []*Change{
				{Path: "sub/.git", New: tGL1},
			},
			expectedConflicts: []*Conflict{{
				Root:         "sub/d",
				AlphaChanges: []*Change{{Path: "sub/d", Old: tF1, New: tF2}},
				BetaChanges:  []*Change{{Path: "sub/d", Old: tF1, New: directory(contents{"f": tF1})}},
			}},
		},

		// The content of a nested submodule is the ancestor's, since it belongs
		// to another repository, so a change to it propagates as usual.
		{
			description: "nested submodule at the ancestor's commit",
			modes:       twoWayModes,
			ancestor: submodule(tGL0, contents{
				"lib":    tF1,
				"nested": testingSubmodule(tGL2, contents{"x": tF1}),
			}),
			alpha: submodule(tGL1, contents{
				"lib":    tF1,
				"nested": testingSubmodule(tGL2, contents{"x": tF1}),
			}),
			beta: submodule(tGL1, contents{
				"lib":    tF1,
				"nested": testingSubmodule(tGL2, contents{"x": tF2}),
			}),
			alphaStatuses: clean,
			betaStatuses:  clean,
			expectedAncestorChanges: []*Change{{Path: "sub", New: testingSubmodule(tGL1, contents{
				"lib":    tF1,
				"nested": testingSubmodule(tGL2, contents{"x": tF1}),
			})}},
			expectedAlphaChanges: []*Change{{Path: "sub/nested/x", Old: tF1, New: tF2}},
		},

		// A nested submodule that both sides have moved to the same commit is
		// subject to the same rule, with its own statuses.
		{
			description: "nested submodule moved on both sides",
			modes:       twoWayModes,
			ancestor:    submodule(tGL1, contents{"nested": testingSubmodule(tGL2, contents{"x": tF1})}),
			alpha:       submodule(tGL1, contents{"nested": testingSubmodule(tGL0, contents{"x": tF3})}),
			beta:        submodule(tGL1, contents{"nested": testingSubmodule(tGL0, contents{"x": tF2})}),
			alphaStatuses: map[string]*SubmoduleStatus{
				"sub/nested": {Commit: tGL0.Digest},
			},
			betaStatuses: map[string]*SubmoduleStatus{
				"sub/nested": {Commit: tGL0.Digest, Changed: []string{"x"}},
			},
			expectedAncestorChanges: []*Change{{Path: "sub/nested", New: testingSubmodule(tGL0, contents{"x": tF3})}},
			expectedAlphaChanges:    []*Change{{Path: "sub/nested/x", Old: tF3, New: tF2}},
		},
	}

	// Process test cases.
	for _, test := range tests {
		for _, mode := range test.modes {
			// Perform reconciliation.
			ancestorChanges, alphaChanges, betaChanges, conflicts := ReconcileGitModeWithStatuses(
				test.ancestor, test.alpha, test.beta, test.alphaStatuses, test.betaStatuses, mode,
			)

			// Verify the results.
			if !testingChangeListsEqual(ancestorChanges, test.expectedAncestorChanges) {
				t.Errorf("%s (%s): ancestor changes do not match expected: %v != %v",
					test.description, mode.Description(), ancestorChanges, test.expectedAncestorChanges,
				)
			}
			if !testingChangeListsEqual(alphaChanges, test.expectedAlphaChanges) {
				t.Errorf("%s (%s): alpha changes do not match expected: %v != %v",
					test.description, mode.Description(), alphaChanges, test.expectedAlphaChanges,
				)
			}
			if !testingChangeListsEqual(betaChanges, test.expectedBetaChanges) {
				t.Errorf("%s (%s): beta changes do not match expected: %v != %v",
					test.description, mode.Description(), betaChanges, test.expectedBetaChanges,
				)
			}
			if !testingConflictListsEqual(conflicts, test.expectedConflicts) {
				t.Errorf("%s (%s): conflicts do not match expected: %v != %v",
					test.description, mode.Description(), conflicts, test.expectedConflicts,
				)
			}

			// Verify that the ancestor that results from the changes is valid,
			// since the controller refuses to save one that isn't.
			if result, err := Apply(test.ancestor, ancestorChanges); err != nil {
				t.Errorf("%s (%s): unable to apply ancestor changes: %v", test.description, mode.Description(), err)
			} else if err := result.EnsureValid(true); err != nil {
				t.Errorf("%s (%s): invalid ancestor: %v", test.description, mode.Description(), err)
			}
		}
	}
}

// TestEntryWalkSubmodule tests Entry.WalkSubmodule.
func TestEntryWalkSubmodule(t *testing.T) {
	submodule := testingSubmodule(tGL0, map[string]*Entry{
		"file":       tF1,
		"link":       tSR,
		"directory":  nested("file", tF2),
		"empty":      tD0,
		"untracked":  {Kind: EntryKind_Untracked},
		"repository": nested(GitLinkName, &Entry{Kind: EntryKind_Untracked}),
		"nested":     testingSubmodule(tGL1, map[string]*Entry{"file": tF1}),
	})
	var paths []string
	submodule.WalkSubmodule(func(path string, _ *Entry) {
		paths = append(paths, path)
	})
	slices.Sort(paths)
	if expected := []string{"directory/file", "file", "link", "nested"}; !slices.Equal(paths, expected) {
		t.Errorf("incorrect paths visited: %v != %v", paths, expected)
	}

	// Entries that aren't submodules have nothing to visit.
	tD1.WalkSubmodule(func(path string, _ *Entry) {
		t.Error("path visited in directory that isn't a submodule:", path)
	})
}

// TestSnapshotSubmoduleStatusesValidation tests the validation of a snapshot's
// submodule statuses.
func TestSnapshotSubmoduleStatusesValidation(t *testing.T) {
	content := nested("sub", testingSubmodule(tGL0, map[string]*Entry{"file": tF1}))
	tests := []struct {
		description string
		statuses    map[string]*SubmoduleStatus
		valid       bool
	}{
		{"none", nil, true},
		{"valid", map[string]*SubmoduleStatus{"sub": {Commit: tGL0.Digest, Changed: []string{"file", "other/file"}}}, true},
		{"root", map[string]*SubmoduleStatus{"": {Commit: tGL0.Digest}}, false},
		{"invalid path", map[string]*SubmoduleStatus{"sub/": {Commit: tGL0.Digest}}, false},
		{"nil", map[string]*SubmoduleStatus{"sub": nil}, false},
		{"no submodule", map[string]*SubmoduleStatus{"other": {Commit: tGL0.Digest}}, false},
		{"other commit", map[string]*SubmoduleStatus{"sub": {Commit: tGL1.Digest}}, false},
		{"invalid changed path", map[string]*SubmoduleStatus{"sub": {Commit: tGL0.Digest, Changed: []string{"/file"}}}, false},
	}
	for _, test := range tests {
		snapshot := (&Snapshot{Content: content}).WithSubmoduleStatuses(test.statuses)
		if err := snapshot.EnsureValid(); (err == nil) != test.valid {
			t.Errorf("%s: incorrect validity: %v", test.description, err)
		}
	}
}

// TestSnapshotWithSubmoduleStatuses tests that Snapshot.WithSubmoduleStatuses
// copies every other field of the snapshot, including any that are added in
// the future, without modifying the original, that statuses don't participate
// in equality, and that they survive serialization.
func TestSnapshotWithSubmoduleStatuses(t *testing.T) {
	// Create a snapshot with every field populated.
	old := map[string]*SubmoduleStatus{"old": {Commit: tGL0.Digest}}
	original := &Snapshot{
		Content:                tD1,
		PreservesExecutability: true,
		DecomposesUnicode:      true,
		Directories:            1,
		Files:                  2,
		SymbolicLinks:          3,
		TotalFileSize:          4,
		Warnings:               []*Problem{{Path: "path", Error: "warning"}},
		Seeds:                  map[string]*Entry{"path": tD1},
		SubmoduleStatuses:      old,
	}
	reflected := original.ProtoReflect()
	fields := reflected.Descriptor().Fields()
	statusesField := fields.ByName("submoduleStatuses")
	for f := 0; f < fields.Len(); f++ {
		if !reflected.Has(fields.Get(f)) {
			t.Fatalf("field %s not populated by test", fields.Get(f).Name())
		}
	}

	// Add new statuses and verify the result.
	statuses := map[string]*SubmoduleStatus{"new": {Commit: tGL1.Digest, Changed: []string{"file"}}}
	result := original.WithSubmoduleStatuses(statuses)
	if result == original {
		t.Fatal("snapshot not copied")
	}
	reflectedResult := result.ProtoReflect()
	for f := 0; f < fields.Len(); f++ {
		field := fields.Get(f)
		if field == statusesField {
			continue
		} else if !reflectedResult.Get(field).Equal(reflected.Get(field)) {
			t.Errorf("field %s not copied", field.Name())
		}
	}
	if len(result.SubmoduleStatuses) != 1 || result.SubmoduleStatuses["new"] != statuses["new"] {
		t.Errorf("incorrect statuses in result: %v", result.SubmoduleStatuses)
	} else if len(original.SubmoduleStatuses) != 1 || original.SubmoduleStatuses["old"] != old["old"] {
		t.Errorf("original statuses modified: %v", original.SubmoduleStatuses)
	}

	// Statuses don't participate in equality, and adding warnings or seeds
	// retains them.
	if !original.Equal(result) || !result.Equal(original) {
		t.Error("snapshots differing only in statuses not considered equal")
	} else if retained := result.WithWarnings(nil).WithSeeds(nil); retained.SubmoduleStatuses["new"] != statuses["new"] {
		t.Errorf("statuses not retained: %v", retained.SubmoduleStatuses)
	}

	// Statuses survive serialization.
	serialized, err := proto.Marshal(result)
	if err != nil {
		t.Fatal("unable to marshal snapshot:", err)
	}
	deserialized := &Snapshot{}
	if err := proto.Unmarshal(serialized, deserialized); err != nil {
		t.Fatal("unable to unmarshal snapshot:", err)
	} else if !proto.Equal(deserialized.SubmoduleStatuses["new"], statuses["new"]) {
		t.Errorf("statuses not preserved: %v", deserialized.SubmoduleStatuses)
	}
}
