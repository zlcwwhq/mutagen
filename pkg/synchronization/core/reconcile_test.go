package core

import (
	"testing"
)

// allModes is shorthand for all synchronization modes.
var allModes = []SynchronizationMode{
	SynchronizationMode_SynchronizationModeTwoWaySafe,
	SynchronizationMode_SynchronizationModeTwoWayResolved,
	SynchronizationMode_SynchronizationModeOneWaySafe,
	SynchronizationMode_SynchronizationModeOneWayReplica,
}

// twoWayModes is shorthand for all bidirectional synchronization modes.
var twoWayModes = []SynchronizationMode{
	SynchronizationMode_SynchronizationModeTwoWaySafe,
	SynchronizationMode_SynchronizationModeTwoWayResolved,
}

// oneWayModes is shorthand for all unidirectional synchronization modes.
var oneWayModes = []SynchronizationMode{
	SynchronizationMode_SynchronizationModeOneWaySafe,
	SynchronizationMode_SynchronizationModeOneWayReplica,
}

// safeModes is shorthand for all safe synchronization modes.
var safeModes = []SynchronizationMode{
	SynchronizationMode_SynchronizationModeTwoWaySafe,
	SynchronizationMode_SynchronizationModeOneWaySafe,
}

// resolvedModes is shorthand for all resolved synchronization modes.
var resolvedModes = []SynchronizationMode{
	SynchronizationMode_SynchronizationModeTwoWayResolved,
	SynchronizationMode_SynchronizationModeOneWayReplica,
}

// twoWaySafeMode is shorthand for only the two-way-safe mode.
var twoWaySafeMode = []SynchronizationMode{
	SynchronizationMode_SynchronizationModeTwoWaySafe,
}

// twoWayResolvedMode is shorthand for only the two-way-resolved mode.
var twoWayResolvedMode = []SynchronizationMode{
	SynchronizationMode_SynchronizationModeTwoWayResolved,
}

// oneWaySafeMode is shorthand for only the one-way-safe mode.
var oneWaySafeMode = []SynchronizationMode{
	SynchronizationMode_SynchronizationModeOneWaySafe,
}

// oneWayReplicaMode is shorthand for only the one-way-replica mode.
var oneWayReplicaMode = []SynchronizationMode{
	SynchronizationMode_SynchronizationModeOneWayReplica,
}

// TestExtractNonDeletionChanges tests extractNonDeletionChanges.
func TestExtractNonDeletionChanges(t *testing.T) {
	// Define test cases.
	var tests = []struct {
		unfiltered []*Change
		expected   []*Change
	}{
		{nil, nil},
		{[]*Change{}, []*Change{}},
		{[]*Change{{New: tF1}}, []*Change{{New: tF1}}},
		{[]*Change{{New: tU}}, []*Change{{New: tU}}},
		{[]*Change{{New: tP1}}, []*Change{{New: tP1}}},
		{[]*Change{{New: tD0}, {Path: "untracked", New: tU}}, []*Change{{New: tD0}, {Path: "untracked", New: tU}}},
		{[]*Change{{Old: tF1, New: tF2}}, []*Change{{Old: tF1, New: tF2}}},
		{[]*Change{{Old: tF1}}, []*Change{}},
		{[]*Change{{Path: "changed", Old: tF1, New: tF2}, {Path: "removed", Old: tD1}}, []*Change{{Path: "changed", Old: tF1, New: tF2}}},
	}

	// Process test cases.
	for i, test := range tests {
		filtered := extractNonDeletionChanges(test.unfiltered)
		if !testingChangeListsEqual(filtered, test.expected) {
			t.Errorf("test index %d: filtered changes don't match expected: %v != %v",
				i, filtered, test.expected,
			)
		}
	}
}

// TestReconcile tests Reconcile.
func TestReconcile(t *testing.T) {
	// Define test cases.
	var tests = []struct {
		// description is a human readable description of the test case.
		description string
		// modes are the synchronization modes for which the test case should apply.
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
		// Test cases where alpha and beta (and potentially the ancestor) agree.
		{
			description: "all nil",
			modes:       allModes,
		},
		{
			description: "all same file",
			modes:       allModes,
			ancestor:    tF1,
			alpha:       tF1,
			beta:        tF1,
		},
		{
			description: "all same relative symbolic link",
			modes:       allModes,
			ancestor:    tSR,
			alpha:       tSR,
			beta:        tSR,
		},
		{
			description: "all same absolute symbolic link",
			modes:       allModes,
			ancestor:    tSA,
			alpha:       tSA,
			beta:        tSA,
		},
		{
			description: "all same empty directory",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       tD0,
			beta:        tD0,
		},
		{
			description: "all same directory",
			modes:       allModes,
			ancestor:    tD1,
			alpha:       tD1,
			beta:        tD1,
		},
		{
			description:             "both created same file",
			modes:                   allModes,
			alpha:                   tF1,
			beta:                    tF1,
			expectedAncestorChanges: []*Change{{New: tF1}},
		},
		{
			description:             "both created same relative symbolic link",
			modes:                   allModes,
			alpha:                   tSR,
			beta:                    tSR,
			expectedAncestorChanges: []*Change{{New: tSR}},
		},
		{
			description:             "both created same absolute symbolic link",
			modes:                   allModes,
			alpha:                   tSA,
			beta:                    tSA,
			expectedAncestorChanges: []*Change{{New: tSA}},
		},
		{
			description:             "both created same empty directory",
			modes:                   allModes,
			alpha:                   tD0,
			beta:                    tD0,
			expectedAncestorChanges: []*Change{{New: tD0}},
		},
		{
			description: "both created same directory",
			modes:       allModes,
			alpha:       tD1,
			beta:        tD1,
			expectedAncestorChanges: []*Change{
				{New: tD0},
				{Path: "file", New: tF1},
			},
		},
		{
			description:             "both created same file in directory",
			modes:                   allModes,
			ancestor:                tD0,
			alpha:                   tD1,
			beta:                    tD1,
			expectedAncestorChanges: []*Change{{Path: "file", New: tF1}},
		},
		{
			description:             "both created same relative symbolic link in directory",
			modes:                   allModes,
			ancestor:                tD1,
			alpha:                   tDSR,
			beta:                    tDSR,
			expectedAncestorChanges: []*Change{{Path: "symlink", New: tSR}},
		},
		{
			description:             "both created same absolute symbolic link in directory",
			modes:                   allModes,
			ancestor:                tD0,
			alpha:                   tDSA,
			beta:                    tDSA,
			expectedAncestorChanges: []*Change{{Path: "symlink", New: tSA}},
		},
		{
			description:             "both deleted file",
			modes:                   allModes,
			ancestor:                tF1,
			expectedAncestorChanges: []*Change{{}},
		},
		{
			description:             "both deleted file in directory",
			modes:                   allModes,
			ancestor:                tD1,
			alpha:                   tD0,
			beta:                    tD0,
			expectedAncestorChanges: []*Change{{Path: "file"}},
		},
		{
			description:             "both deleted relative symbolic link in directory",
			modes:                   allModes,
			ancestor:                tDSR,
			alpha:                   tD1,
			beta:                    tD1,
			expectedAncestorChanges: []*Change{{Path: "symlink"}},
		},
		{
			description:             "both deleted absolute symbolic link in directory",
			modes:                   allModes,
			ancestor:                tDSA,
			alpha:                   tD0,
			beta:                    tD0,
			expectedAncestorChanges: []*Change{{Path: "symlink"}},
		},
		{
			description:             "both deleted empty directory",
			modes:                   allModes,
			ancestor:                tD0,
			expectedAncestorChanges: []*Change{{}},
		},
		{
			description:             "both deleted directory",
			modes:                   allModes,
			ancestor:                tD1,
			expectedAncestorChanges: []*Change{{}},
		},

		// Test cases where both sides are either nil or untracked.
		{
			description: "beta created untracked",
			modes:       allModes,
			beta:        tU,
		},
		{
			description: "alpha created untracked",
			modes:       allModes,
			alpha:       tU,
		},
		{
			description: "both created untracked",
			modes:       allModes,
			alpha:       tU,
			beta:        tU,
		},
		{
			description:             "alpha deleted file and beta created untracked",
			modes:                   allModes,
			ancestor:                tF1,
			beta:                    tU,
			expectedAncestorChanges: []*Change{{}},
		},
		{
			description:             "alpha created untracked and beta deleted file",
			modes:                   allModes,
			ancestor:                tF1,
			alpha:                   tU,
			expectedAncestorChanges: []*Change{{}},
		},
		{
			description:             "both deleted directory and created untracked",
			modes:                   allModes,
			ancestor:                tD1,
			alpha:                   tU,
			beta:                    tU,
			expectedAncestorChanges: []*Change{{}},
		},

		// Test cases where one or both sides is problematic.
		{
			description: "alpha problematic others nil",
			modes:       allModes,
			alpha:       tP1,
		},
		{
			description: "alpha problematic ancestor file",
			modes:       allModes,
			ancestor:    tF1,
			alpha:       tP1,
		},
		{
			description: "alpha problematic ancestor empty directory",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       tP1,
		},
		{
			description: "alpha problematic others file",
			modes:       allModes,
			ancestor:    tF1,
			alpha:       tP1,
			beta:        tF1,
		},
		{
			description: "beta problematic others nil",
			modes:       allModes,
			beta:        tP1,
		},
		{
			description: "beta problematic ancestor file",
			modes:       allModes,
			ancestor:    tF1,
			beta:        tP1,
		},
		{
			description: "beta problematic others file",
			modes:       allModes,
			ancestor:    tF1,
			alpha:       tF1,
			beta:        tP1,
		},
		{
			description: "beta problematic ancestor empty directory",
			modes:       allModes,
			ancestor:    tD0,
			beta:        tP1,
		},
		{
			description: "both problematic ancestor nil",
			modes:       allModes,
			alpha:       tP1,
			beta:        tP1,
		},
		{
			description: "both problematic ancestor file",
			modes:       allModes,
			ancestor:    tF1,
			alpha:       tP1,
			beta:        tP1,
		},
		{
			description: "both problematic ancestor directory",
			modes:       allModes,
			ancestor:    tD1,
			alpha:       tP1,
			beta:        tP1,
		},

		// Test cases where only alpha has modified content.
		{
			description:         "alpha created file",
			modes:               allModes,
			alpha:               tF1,
			expectedBetaChanges: []*Change{{New: tF1}},
		},
		{
			description:         "alpha created empty directory",
			modes:               allModes,
			alpha:               tD0,
			expectedBetaChanges: []*Change{{New: tD0}},
		},
		{
			description:         "alpha created directory",
			modes:               allModes,
			alpha:               tD1,
			expectedBetaChanges: []*Change{{New: tD1}},
		},
		{
			description:         "alpha created file in existing directory",
			modes:               allModes,
			ancestor:            tD0,
			alpha:               tD1,
			beta:                tD0,
			expectedBetaChanges: []*Change{{Path: "file", New: tF1}},
		},
		{
			description:         "alpha modified file",
			modes:               allModes,
			ancestor:            tF1,
			alpha:               tF2,
			beta:                tF1,
			expectedBetaChanges: []*Change{{Old: tF1, New: tF2}},
		},
		{
			description:         "alpha replaced file with directory",
			modes:               allModes,
			ancestor:            tF1,
			alpha:               tD1,
			beta:                tF1,
			expectedBetaChanges: []*Change{{Old: tF1, New: tD1}},
		},
		{
			description:         "alpha deleted file",
			modes:               allModes,
			ancestor:            tF1,
			beta:                tF1,
			expectedBetaChanges: []*Change{{Old: tF1}},
		},
		{
			description:         "alpha deleted directory",
			modes:               allModes,
			ancestor:            tD1,
			beta:                tD1,
			expectedBetaChanges: []*Change{{Old: tD1}},
		},

		// Test cases where only beta has modified content.
		{
			description:          "beta created file",
			modes:                twoWayModes,
			beta:                 tF1,
			expectedAlphaChanges: []*Change{{New: tF1}},
		},
		{
			description: "beta created file",
			modes:       oneWaySafeMode,
			beta:        tF1,
		},
		{
			description:         "beta created file",
			modes:               oneWayReplicaMode,
			beta:                tF1,
			expectedBetaChanges: []*Change{{Old: tF1}},
		},
		{
			description:          "beta created empty directory",
			modes:                twoWayModes,
			beta:                 tD0,
			expectedAlphaChanges: []*Change{{New: tD0}},
		},
		{
			description: "beta created empty directory",
			modes:       oneWaySafeMode,
			beta:        tD0,
		},
		{
			description:         "beta created empty directory",
			modes:               oneWayReplicaMode,
			beta:                tD0,
			expectedBetaChanges: []*Change{{Old: tD0}},
		},
		{
			description:          "beta created directory",
			modes:                twoWayModes,
			beta:                 tD1,
			expectedAlphaChanges: []*Change{{New: tD1}},
		},
		{
			description: "beta created directory",
			modes:       oneWaySafeMode,
			beta:        tD1,
		},
		{
			description:         "beta created directory",
			modes:               oneWayReplicaMode,
			beta:                tD1,
			expectedBetaChanges: []*Change{{Old: tD1}},
		},
		{
			description:          "beta created file in existing directory",
			modes:                twoWayModes,
			ancestor:             tD0,
			alpha:                tD0,
			beta:                 tD1,
			expectedAlphaChanges: []*Change{{Path: "file", New: tF1}},
		},
		{
			description: "beta created file in existing directory",
			modes:       oneWaySafeMode,
			ancestor:    tD0,
			alpha:       tD0,
			beta:        tD1,
		},
		{
			description:         "beta created file in existing directory",
			modes:               oneWayReplicaMode,
			ancestor:            tD0,
			alpha:               tD0,
			beta:                tD1,
			expectedBetaChanges: []*Change{{Path: "file", Old: tF1}},
		},
		{
			description:          "beta modified file",
			modes:                twoWayModes,
			ancestor:             tF1,
			alpha:                tF1,
			beta:                 tF2,
			expectedAlphaChanges: []*Change{{Old: tF1, New: tF2}},
		},
		{
			description: "beta modified file",
			modes:       oneWaySafeMode,
			ancestor:    tF1,
			alpha:       tF1,
			beta:        tF2,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tF1, New: tF1}},
				BetaChanges:  []*Change{{Old: tF1, New: tF2}},
			}},
		},
		{
			description:         "beta modified file",
			modes:               oneWayReplicaMode,
			ancestor:            tF1,
			alpha:               tF1,
			beta:                tF2,
			expectedBetaChanges: []*Change{{Old: tF2, New: tF1}},
		},
		{
			description:          "beta replaced file with directory",
			modes:                twoWayModes,
			ancestor:             tF1,
			alpha:                tF1,
			beta:                 tD1,
			expectedAlphaChanges: []*Change{{Old: tF1, New: tD1}},
		},
		{
			description: "beta replaced file with directory",
			modes:       oneWaySafeMode,
			ancestor:    tF1,
			alpha:       tF1,
			beta:        tD1,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tF1, New: tF1}},
				BetaChanges:  []*Change{{Old: tF1, New: tD1}},
			}},
		},
		{
			description:         "beta replaced file with directory",
			modes:               oneWayReplicaMode,
			ancestor:            tF1,
			alpha:               tF1,
			beta:                tD1,
			expectedBetaChanges: []*Change{{Old: tD1, New: tF1}},
		},
		{
			description:          "beta deleted file",
			modes:                twoWayModes,
			ancestor:             tF1,
			alpha:                tF1,
			expectedAlphaChanges: []*Change{{Old: tF1}},
		},
		{
			description:         "beta deleted file",
			modes:               oneWayModes,
			ancestor:            tF1,
			alpha:               tF1,
			expectedBetaChanges: []*Change{{New: tF1}},
		},
		{
			description:          "beta deleted directory",
			modes:                twoWayModes,
			ancestor:             tD1,
			alpha:                tD1,
			expectedAlphaChanges: []*Change{{Old: tD1}},
		},
		{
			description:         "beta deleted directory",
			modes:               oneWayModes,
			ancestor:            tD1,
			alpha:               tD1,
			expectedBetaChanges: []*Change{{New: tD1}},
		},

		// Test cases where both sides have modified content.
		{
			description: "both created different file",
			modes:       safeModes,
			alpha:       tF1,
			beta:        tF2,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tF1}},
				BetaChanges:  []*Change{{New: tF2}},
			}},
		},
		{
			description:         "both created different file",
			modes:               resolvedModes,
			alpha:               tF1,
			beta:                tF2,
			expectedBetaChanges: []*Change{{Old: tF2, New: tF1}},
		},
		{
			description: "alpha created file beta created directory",
			modes:       safeModes,
			alpha:       tF1,
			beta:        tD1,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tF1}},
				BetaChanges:  []*Change{{New: tD1}},
			}},
		},
		{
			description:         "alpha created file beta created directory",
			modes:               resolvedModes,
			alpha:               tF1,
			beta:                tD1,
			expectedBetaChanges: []*Change{{Old: tD1, New: tF1}},
		},
		{
			description:             "both created directory alpha created file in directory",
			modes:                   allModes,
			alpha:                   tD1,
			beta:                    tD0,
			expectedAncestorChanges: []*Change{{New: tD0}},
			expectedBetaChanges:     []*Change{{Path: "file", New: tF1}},
		},
		{
			description:             "both created directory beta created file in directory",
			modes:                   twoWayModes,
			alpha:                   tD0,
			beta:                    tD1,
			expectedAncestorChanges: []*Change{{New: tD0}},
			expectedAlphaChanges:    []*Change{{Path: "file", New: tF1}},
		},
		{
			description:             "both created directory beta created file in directory",
			modes:                   oneWaySafeMode,
			alpha:                   tD0,
			beta:                    tD1,
			expectedAncestorChanges: []*Change{{New: tD0}},
		},
		{
			description:             "both created directory beta created file in directory",
			modes:                   oneWayReplicaMode,
			alpha:                   tD0,
			beta:                    tD1,
			expectedAncestorChanges: []*Change{{New: tD0}},
			expectedBetaChanges:     []*Change{{Path: "file", Old: tF1}},
		},
		{
			description:             "both created directory with different file",
			modes:                   safeModes,
			alpha:                   tD1,
			beta:                    tD2,
			expectedAncestorChanges: []*Change{{New: tD0}},
			expectedConflicts: []*Conflict{{
				Root:         "file",
				AlphaChanges: []*Change{{Path: "file", New: tF1}},
				BetaChanges:  []*Change{{Path: "file", New: tF2}},
			}},
		},
		{
			description:             "both created directory with different file",
			modes:                   resolvedModes,
			alpha:                   tD1,
			beta:                    tD2,
			expectedAncestorChanges: []*Change{{New: tD0}},
			expectedBetaChanges:     []*Change{{Path: "file", Old: tF2, New: tF1}},
		},
		{
			description:         "alpha modified file beta deleted",
			modes:               allModes,
			ancestor:            tF1,
			alpha:               tF2,
			expectedBetaChanges: []*Change{{New: tF2}},
		},
		{
			description:          "beta modified file alpha deleted",
			modes:                twoWayModes,
			ancestor:             tF1,
			beta:                 tF2,
			expectedAlphaChanges: []*Change{{New: tF2}},
		},
		{
			description:             "beta modified file alpha deleted",
			modes:                   oneWaySafeMode,
			ancestor:                tF1,
			beta:                    tF2,
			expectedAncestorChanges: []*Change{{}},
		},
		{
			description:         "beta modified file alpha deleted",
			modes:               oneWayReplicaMode,
			ancestor:            tF1,
			beta:                tF2,
			expectedBetaChanges: []*Change{{Old: tF2}},
		},
		{
			description:         "alpha deleted all of directory, beta deleted part of directory",
			modes:               allModes,
			ancestor:            tD1,
			beta:                tD0,
			expectedBetaChanges: []*Change{{Old: tD0}},
		},
		{
			description:          "alpha deleted part of directory, beta deleted all of directory",
			modes:                twoWayModes,
			ancestor:             tD1,
			alpha:                tD0,
			expectedAlphaChanges: []*Change{{Old: tD0}},
		},
		{
			description:         "alpha deleted part of directory, beta deleted all of directory",
			modes:               oneWayModes,
			ancestor:            tD1,
			alpha:               tD0,
			expectedBetaChanges: []*Change{{New: tD0}},
		},

		// Test cases with a combination of synchronizable and unsynchronizable
		// content, including cases with modified content.
		{
			description: "alpha created untracked, beta created file",
			modes:       twoWayModes,
			alpha:       tU,
			beta:        tF1,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tU}},
				BetaChanges:  []*Change{{New: tF1}},
			}},
		},
		{
			description: "alpha created untracked, beta created file",
			modes:       oneWaySafeMode,
			alpha:       tU,
			beta:        tF1,
		},
		{
			description:         "alpha created untracked, beta created file",
			modes:               oneWayReplicaMode,
			alpha:               tU,
			beta:                tF1,
			expectedBetaChanges: []*Change{{Old: tF1}},
		},
		{
			description: "alpha created problematic in existing directory",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       tDP1,
			beta:        tD0,
		},
		{
			description: "alpha created untracked in existing directory",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       tDU,
			beta:        tD0,
		},
		{
			description:         "alpha created directory with problematic",
			modes:               allModes,
			alpha:               tDP1,
			expectedBetaChanges: []*Change{{New: tD0}},
		},
		{
			description:         "alpha created directory with untracked",
			modes:               allModes,
			alpha:               tDU,
			expectedBetaChanges: []*Change{{New: tD0}},
		},
		{
			description: "alpha created file, beta created untracked",
			modes:       allModes,
			alpha:       tF1,
			beta:        tU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tF1}},
				BetaChanges:  []*Change{{New: tU}},
			}},
		},
		{
			description: "alpha created file, beta created directory with untracked",
			modes:       safeModes,
			alpha:       tF1,
			beta:        tDU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tF1}},
				BetaChanges:  []*Change{{New: tD0}},
			}},
		},
		{
			description: "alpha created file, beta created directory with untracked",
			modes:       resolvedModes,
			alpha:       tF1,
			beta:        tDU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tF1}},
				BetaChanges:  []*Change{{Path: "untracked", New: tU}},
			}},
		},
		{
			description: "beta created problematic in existing directory",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       tD0,
			beta:        tDP1,
		},
		{
			description: "beta created untracked in existing directory",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       tDU,
			beta:        tD0,
		},
		{
			description:          "beta created directory with problematic",
			modes:                twoWayModes,
			beta:                 tDP1,
			expectedAlphaChanges: []*Change{{New: tD0}},
		},
		{
			description: "beta created directory with problematic",
			modes:       oneWaySafeMode,
			beta:        tDP1,
		},
		{
			description: "beta created directory with problematic",
			modes:       oneWayReplicaMode,
			beta:        tDP1,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{}},
				BetaChanges:  []*Change{{Path: "problematic", New: tP1}},
			}},
		},
		{
			description:          "beta created directory with untracked",
			modes:                twoWayModes,
			beta:                 tDU,
			expectedAlphaChanges: []*Change{{New: tD0}},
		},
		{
			description: "beta created directory with untracked",
			modes:       oneWaySafeMode,
			beta:        tDU,
		},
		{
			description: "beta created directory with untracked",
			modes:       oneWayReplicaMode,
			beta:        tDU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{}},
				BetaChanges:  []*Change{{Path: "untracked", New: tU}},
			}},
		},
		{
			description:          "beta replaced file with untracked",
			modes:                twoWayModes,
			ancestor:             tF1,
			alpha:                tF1,
			beta:                 tU,
			expectedAlphaChanges: []*Change{{Old: tF1}},
		},
		{
			description: "beta replaced file with untracked",
			modes:       oneWayModes,
			ancestor:    tF1,
			alpha:       tF1,
			beta:        tU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tF1, New: tF1}},
				BetaChanges:  []*Change{{New: tU}},
			}},
		},
		{
			description:          "beta replaced file with directory containing untracked",
			modes:                twoWayModes,
			ancestor:             tF1,
			alpha:                tF1,
			beta:                 tDU,
			expectedAlphaChanges: []*Change{{Old: tF1, New: tD0}},
		},
		{
			description: "beta replaced file with directory containing untracked",
			modes:       oneWaySafeMode,
			ancestor:    tF1,
			alpha:       tF1,
			beta:        tDU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tF1, New: tF1}},
				BetaChanges:  []*Change{{Old: tF1, New: tD0}},
			}},
		},
		{
			description: "beta replaced file with directory containing untracked",
			modes:       oneWayReplicaMode,
			ancestor:    tF1,
			alpha:       tF1,
			beta:        tDU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tF1, New: tF1}},
				BetaChanges:  []*Change{{Path: "untracked", New: tU}},
			}},
		},
		{
			description: "alpha deleted all of directory, beta deleted part of directory and created unsynchronizable content",
			modes:       allModes,
			ancestor:    tD1,
			beta:        tDU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tD1}},
				BetaChanges:  []*Change{{Path: "untracked", New: tU}},
			}},
		},
		{
			description: "alpha deleted part of directory and created unsynchronizable content, beta deleted all of directory",
			modes:       twoWayModes,
			ancestor:    tD1,
			alpha:       tDU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Path: "untracked", New: tU}},
				BetaChanges:  []*Change{{Old: tD1}},
			}},
		},
		{
			description:         "alpha deleted part of directory and created unsynchronizable content, beta deleted all of directory",
			modes:               oneWayModes,
			ancestor:            tD1,
			alpha:               tDU,
			expectedBetaChanges: []*Change{{New: tD0}},
		},
		{
			description:         "alpha replaced directory with unsynchronizable content, beta deleted part of directory",
			modes:               allModes,
			ancestor:            tD1,
			alpha:               tU,
			beta:                tD0,
			expectedBetaChanges: []*Change{{Old: tD0}},
		},
		{
			description:          "alpha deleted part of directory, beta replaced directory with unsynchronizable content",
			modes:                twoWayModes,
			ancestor:             tD1,
			alpha:                tD0,
			beta:                 tU,
			expectedAlphaChanges: []*Change{{Old: tD0}},
		},
		{
			description: "alpha deleted part of directory, beta replaced directory with unsynchronizable content",
			modes:       oneWayModes,
			ancestor:    tD1,
			alpha:       tD0,
			beta:        tU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tD1, New: tD0}},
				BetaChanges:  []*Change{{New: tU}},
			}},
		},
		{
			description: "alpha deleted part of directory and created untracked content, beta replaced directory with file",
			modes:       twoWayModes,
			ancestor:    tD1,
			alpha:       tDU,
			beta:        tF2,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Path: "untracked", New: tU}},
				BetaChanges:  []*Change{{Old: tD1, New: tF2}},
			}},
		},
		{
			description: "alpha deleted part of directory and created untracked content, beta replaced directory with file",
			modes:       oneWaySafeMode,
			ancestor:    tD1,
			alpha:       tDU,
			beta:        tF2,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tD1, New: tDU}},
				BetaChanges:  []*Change{{Old: tD1, New: tF2}},
			}},
		},
		{
			description:         "alpha deleted part of directory and created untracked content, beta replaced directory with file",
			modes:               oneWayReplicaMode,
			ancestor:            tD1,
			alpha:               tDU,
			beta:                tF2,
			expectedBetaChanges: []*Change{{Old: tF2, New: tD0}},
		},
		{
			description: "beta deleted part of directory and created untracked content, alpha replaced directory with file",
			modes:       allModes,
			ancestor:    tD1,
			alpha:       tF2,
			beta:        tDU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tD1, New: tF2}},
				BetaChanges:  []*Change{{Path: "untracked", New: tU}},
			}},
		},
		{
			description: "alpha deleted part of directory and created problematic content, beta replaced directory with file",
			modes:       twoWayModes,
			ancestor:    tD1,
			alpha:       tDP1,
			beta:        tF2,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Path: "problematic", New: tP1}},
				BetaChanges:  []*Change{{Old: tD1, New: tF2}},
			}},
		},
		{
			description: "alpha deleted part of directory and created problematic content, beta replaced directory with file",
			modes:       oneWaySafeMode,
			ancestor:    tD1,
			alpha:       tDP1,
			beta:        tF2,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tD1, New: tDP1}},
				BetaChanges:  []*Change{{Old: tD1, New: tF2}},
			}},
		},
		{
			description:         "alpha deleted part of directory and created problematic content, beta replaced directory with file",
			modes:               oneWayReplicaMode,
			ancestor:            tD1,
			alpha:               tDP1,
			beta:                tF2,
			expectedBetaChanges: []*Change{{Old: tF2, New: tD0}},
		},
		{
			description: "beta deleted part of directory and created problematic content, alpha replaced directory with file",
			modes:       allModes,
			ancestor:    tD1,
			alpha:       tF2,
			beta:        tDP1,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tD1, New: tF2}},
				BetaChanges:  []*Change{{Path: "problematic", New: tP1}},
			}},
		},

		// Test cases where alpha and beta agree on a submodule. Only the commit
		// identifies a submodule, so differing URLs don't constitute a
		// disagreement.
		{
			description: "all same submodule",
			modes:       allModes,
			ancestor:    tG1,
			alpha:       tG1,
			beta:        tG1,
		},
		{
			description: "all same submodule with different URLs",
			modes:       allModes,
			ancestor:    tG1,
			alpha:       tG1U,
			beta:        tG1,
		},
		{
			description:             "both created same submodule with different URLs",
			modes:                   allModes,
			alpha:                   tG1,
			beta:                    tG1U,
			expectedAncestorChanges: []*Change{{New: tG1}},
		},
		{
			description:             "both moved submodule to same commit",
			modes:                   allModes,
			ancestor:                tG1,
			alpha:                   tG2,
			beta:                    tG2,
			expectedAncestorChanges: []*Change{{New: tG2}},
		},
		{
			description:             "both deleted submodule",
			modes:                   allModes,
			ancestor:                tG1,
			expectedAncestorChanges: []*Change{{}},
		},

		// Test cases where a submodule moves on one side.
		{
			description:         "alpha moved submodule",
			modes:               allModes,
			ancestor:            tG1,
			alpha:               tG2,
			beta:                tG1,
			expectedBetaChanges: []*Change{{Old: tG1, New: tG2}},
		},
		{
			description:          "beta moved submodule",
			modes:                twoWayModes,
			ancestor:             tG1,
			alpha:                tG1,
			beta:                 tG2,
			expectedAlphaChanges: []*Change{{Old: tG1, New: tG2}},
		},
		{
			description: "beta moved submodule",
			modes:       oneWaySafeMode,
			ancestor:    tG1,
			alpha:       tG1,
			beta:        tG2,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tG1, New: tG1}},
				BetaChanges:  []*Change{{Old: tG1, New: tG2}},
			}},
		},
		{
			description:         "beta moved submodule",
			modes:               oneWayReplicaMode,
			ancestor:            tG1,
			alpha:               tG1,
			beta:                tG2,
			expectedBetaChanges: []*Change{{Old: tG2, New: tG1}},
		},

		// Test cases where a submodule moves on both sides.
		{
			description: "both moved submodule to different commits",
			modes:       safeModes,
			ancestor:    tG1,
			alpha:       tG2,
			beta:        tG3,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{Old: tG1, New: tG2}},
				BetaChanges:  []*Change{{Old: tG1, New: tG3}},
			}},
		},
		{
			description:         "both moved submodule to different commits",
			modes:               resolvedModes,
			ancestor:            tG1,
			alpha:               tG2,
			beta:                tG3,
			expectedBetaChanges: []*Change{{Old: tG3, New: tG2}},
		},
		{
			description: "both created submodule at different commits",
			modes:       safeModes,
			alpha:       tG1,
			beta:        tG2,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tG1}},
				BetaChanges:  []*Change{{New: tG2}},
			}},
		},
		{
			description:         "both created submodule at different commits",
			modes:               resolvedModes,
			alpha:               tG1,
			beta:                tG2,
			expectedBetaChanges: []*Change{{Old: tG2, New: tG1}},
		},

		// Test cases where a submodule is created or deleted on one side.
		{
			description:         "alpha created submodule",
			modes:               allModes,
			alpha:               tG1,
			expectedBetaChanges: []*Change{{New: tG1}},
		},
		{
			description:          "beta created submodule",
			modes:                twoWayModes,
			beta:                 tG1,
			expectedAlphaChanges: []*Change{{New: tG1}},
		},
		{
			description: "beta created submodule",
			modes:       oneWaySafeMode,
			beta:        tG1,
		},
		{
			description:         "beta created submodule",
			modes:               oneWayReplicaMode,
			beta:                tG1,
			expectedBetaChanges: []*Change{{Old: tG1}},
		},
		{
			description:         "alpha deleted submodule",
			modes:               allModes,
			ancestor:            tG1,
			beta:                tG1,
			expectedBetaChanges: []*Change{{Old: tG1}},
		},
		{
			description:          "beta deleted submodule",
			modes:                twoWayModes,
			ancestor:             tG1,
			alpha:                tG1,
			expectedAlphaChanges: []*Change{{Old: tG1}},
		},
		{
			description:         "beta deleted submodule",
			modes:               oneWayModes,
			ancestor:            tG1,
			alpha:               tG1,
			expectedBetaChanges: []*Change{{New: tG1}},
		},

		// Test cases where one side moves a submodule and the other deletes it,
		// which is the way to manually resolve a conflict over its commit.
		{
			description:         "alpha moved submodule, beta deleted submodule",
			modes:               allModes,
			ancestor:            tG1,
			alpha:               tG2,
			expectedBetaChanges: []*Change{{New: tG2}},
		},
		{
			description:          "alpha deleted submodule, beta moved submodule",
			modes:                twoWayModes,
			ancestor:             tG1,
			beta:                 tG2,
			expectedAlphaChanges: []*Change{{New: tG2}},
		},
		{
			description:             "alpha deleted submodule, beta moved submodule",
			modes:                   oneWaySafeMode,
			ancestor:                tG1,
			beta:                    tG2,
			expectedAncestorChanges: []*Change{{}},
		},
		{
			description:         "alpha deleted submodule, beta moved submodule",
			modes:               oneWayReplicaMode,
			ancestor:            tG1,
			beta:                tG2,
			expectedBetaChanges: []*Change{{Old: tG2}},
		},

		// Test cases where a submodule is opposite a non-empty directory, which
		// a submodule can't be fetched over and which must not be overlooked.
		{
			description: "alpha created submodule, beta created directory",
			modes:       safeModes,
			alpha:       tG1,
			beta:        tD1,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tG1}},
				BetaChanges:  []*Change{{New: tD1}},
			}},
		},
		{
			description:         "alpha created submodule, beta created directory",
			modes:               resolvedModes,
			alpha:               tG1,
			beta:                tD1,
			expectedBetaChanges: []*Change{{Old: tD1, New: tG1}},
		},
		{
			description: "alpha created directory, beta created submodule",
			modes:       safeModes,
			alpha:       tD1,
			beta:        tG1,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tD1}},
				BetaChanges:  []*Change{{New: tG1}},
			}},
		},
		{
			description:         "alpha created directory, beta created submodule",
			modes:               resolvedModes,
			alpha:               tD1,
			beta:                tG1,
			expectedBetaChanges: []*Change{{Old: tG1, New: tD1}},
		},
		{
			description:         "alpha replaced submodule with directory",
			modes:               allModes,
			ancestor:            tG1,
			alpha:               tD1,
			beta:                tG1,
			expectedBetaChanges: []*Change{{Old: tG1, New: tD1}},
		},
		{
			description: "alpha created submodule, beta created directory with only untracked content",
			modes:       safeModes,
			alpha:       tG1,
			beta:        tDU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tG1}},
				BetaChanges:  []*Change{{New: tD0}},
			}},
		},
		{
			description: "alpha created submodule, beta created directory with only untracked content",
			modes:       resolvedModes,
			alpha:       tG1,
			beta:        tDU,
			expectedConflicts: []*Conflict{{
				AlphaChanges: []*Change{{New: tG1}},
				BetaChanges:  []*Change{{Path: "untracked", New: tU}},
			}},
		},

		// Test cases where a submodule is opposite an empty directory, which
		// isn't a conflict, whatever the ancestor contains, since that's what
		// a clone without submodules or a deinitialized submodule looks like.
		{
			description:         "alpha has submodule, beta has empty directory, no ancestor",
			modes:               allModes,
			alpha:               tG1,
			beta:                tD0,
			expectedBetaChanges: []*Change{{Old: tD0, New: tG1}},
		},
		{
			description:         "alpha has submodule, beta deinitialized submodule",
			modes:               allModes,
			ancestor:            tG1,
			alpha:               tG1,
			beta:                tD0,
			expectedBetaChanges: []*Change{{Old: tD0, New: tG1}},
		},
		{
			description:         "alpha moved submodule, beta deinitialized submodule",
			modes:               allModes,
			ancestor:            tG1,
			alpha:               tG2,
			beta:                tD0,
			expectedBetaChanges: []*Change{{Old: tD0, New: tG2}},
		},
		{
			description:         "alpha replaced empty directory with submodule",
			modes:               allModes,
			ancestor:            tD0,
			alpha:               tG1,
			beta:                tD0,
			expectedBetaChanges: []*Change{{Old: tD0, New: tG1}},
		},
		{
			description:         "alpha replaced directory with submodule, beta emptied directory",
			modes:               allModes,
			ancestor:            tD1,
			alpha:               tG1,
			beta:                tD0,
			expectedBetaChanges: []*Change{{Old: tD0, New: tG1}},
		},
		{
			description:             "alpha has submodule in directory, beta has empty directory",
			modes:                   allModes,
			alpha:                   nested("submodule", tG1),
			beta:                    nested("submodule", tD0),
			expectedAncestorChanges: []*Change{{New: tD0}},
			expectedBetaChanges:     []*Change{{Path: "submodule", Old: tD0, New: tG1}},
		},
		{
			description:          "alpha has empty directory, beta has submodule, no ancestor",
			modes:                twoWayModes,
			alpha:                tD0,
			beta:                 tG1,
			expectedAlphaChanges: []*Change{{Old: tD0, New: tG1}},
		},
		{
			description: "alpha has empty directory, beta has submodule, no ancestor",
			modes:       oneWayModes,
			alpha:       tD0,
			beta:        tG1,
		},
		{
			description:          "alpha deinitialized submodule, beta has submodule",
			modes:                twoWayModes,
			ancestor:             tG1,
			alpha:                tD0,
			beta:                 tG1,
			expectedAlphaChanges: []*Change{{Old: tD0, New: tG1}},
		},
		{
			description: "alpha deinitialized submodule, beta has submodule",
			modes:       oneWayModes,
			ancestor:    tG1,
			alpha:       tD0,
			beta:        tG1,
		},
		{
			description:          "alpha deinitialized submodule, beta moved submodule",
			modes:                twoWayModes,
			ancestor:             tG1,
			alpha:                tD0,
			beta:                 tG2,
			expectedAlphaChanges: []*Change{{Old: tD0, New: tG2}},
		},
		{
			description: "alpha deinitialized submodule, beta moved submodule",
			modes:       oneWayModes,
			ancestor:    tG1,
			alpha:       tD0,
			beta:        tG2,
		},
		{
			description:             "alpha has empty directory, beta has submodule in directory",
			modes:                   twoWayModes,
			alpha:                   nested("submodule", tD0),
			beta:                    nested("submodule", tG1),
			expectedAncestorChanges: []*Change{{New: tD0}},
			expectedAlphaChanges:    []*Change{{Path: "submodule", Old: tD0, New: tG1}},
		},
		{
			description:             "alpha has empty directory, beta has submodule in directory",
			modes:                   oneWayModes,
			alpha:                   nested("submodule", tD0),
			beta:                    nested("submodule", tG1),
			expectedAncestorChanges: []*Change{{New: tD0}},
		},
	}

	// Process test cases.
	for _, test := range tests {
		// Test each mode.
		for _, mode := range test.modes {
			// Perform reconciliation.
			ancestorChanges, alphaChanges, betaChanges, conflicts := Reconcile(
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
		}
	}
}

// TestReconcilePanicWithInvalidSynchronizationMode tests that Reconcile panics
// when provided with disagreeing contents and an invalid synchronization mode.
func TestReconcilePanicWithInvalidSynchronizationMode(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Reconcile did not panic with invalid synchronization mode")
		}
	}()
	Reconcile(nil, tF1, nil, SynchronizationMode(-1))
}

// TestReconcileGitMode tests ReconcileGitMode, in particular that a rule that
// has reached one endpoint only (and is thus making content untracked on that
// endpoint only) doesn't result in the deletion of content on the other.
func TestReconcileGitMode(t *testing.T) {
	// Define helper variables. The ancestor and endpoints in these test cases
	// are all directories, as is the case for a synchronization root.
	rules := nested(".gitignore", tF2)
	withRules := func(name string, entry *Entry) *Entry {
		return &Entry{Contents: map[string]*Entry{".gitignore": tF2, name: entry}}
	}
	pkg := func(entry *Entry) *Entry {
		return nested("pkg", nested("build", entry))
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
		// expectedAncestorChanges are the expected ancestor changes.
		expectedAncestorChanges []*Change
		// expectedAlphaChanges are the expected alpha changes.
		expectedAlphaChanges []*Change
		// expectedBetaChanges are the expected beta changes.
		expectedBetaChanges []*Change
		// expectedConflicts are the expected conflicts.
		expectedConflicts []*Conflict
	}{
		// A rule has been added on one side, with the other side unchanged. In
		// non-Git-mode reconciliation, these propagate a deletion.
		{
			description:             "file untracked on alpha, unchanged on beta",
			modes:                   allModes,
			ancestor:                nested("file", tF1),
			alpha:                   nested("file", tU),
			beta:                    nested("file", tF1),
			expectedAncestorChanges: []*Change{{Path: "file"}},
		},
		{
			description:             "file unchanged on alpha, untracked on beta",
			modes:                   allModes,
			ancestor:                nested("file", tF1),
			alpha:                   nested("file", tF1),
			beta:                    nested("file", tU),
			expectedAncestorChanges: []*Change{{Path: "file"}},
		},
		{
			description:             "directory untracked on alpha, unchanged on beta",
			modes:                   allModes,
			ancestor:                nested("data", tD1),
			alpha:                   nested("data", tU),
			beta:                    nested("data", tD1),
			expectedAncestorChanges: []*Change{{Path: "data"}},
		},
		{
			description:             "directory unchanged on alpha, untracked on beta",
			modes:                   allModes,
			ancestor:                nested("data", tD1),
			alpha:                   nested("data", tD1),
			beta:                    nested("data", tU),
			expectedAncestorChanges: []*Change{{Path: "data"}},
		},
		{
			description:             "nested directory untracked on alpha, unchanged on beta",
			modes:                   allModes,
			ancestor:                pkg(tD1),
			alpha:                   pkg(tU),
			beta:                    pkg(tD1),
			expectedAncestorChanges: []*Change{{Path: "pkg/build"}},
		},
		{
			description:             "nested directory unchanged on alpha, untracked on beta",
			modes:                   allModes,
			ancestor:                pkg(tD1),
			alpha:                   pkg(tD1),
			beta:                    pkg(tU),
			expectedAncestorChanges: []*Change{{Path: "pkg/build"}},
		},
		{
			description:             "file within directory untracked on alpha, unchanged on beta",
			modes:                   allModes,
			ancestor:                nested("directory", tD1),
			alpha:                   nested("directory", nested("file", tU)),
			beta:                    nested("directory", tD1),
			expectedAncestorChanges: []*Change{{Path: "directory/file"}},
		},

		// The rule that made content untracked is itself being propagated.
		{
			description:             "file untracked on alpha along with new rules",
			modes:                   allModes,
			ancestor:                nested("file", tF1),
			alpha:                   withRules("file", tU),
			beta:                    nested("file", tF1),
			expectedAncestorChanges: []*Change{{Path: "file"}},
			expectedBetaChanges:     []*Change{{Path: ".gitignore", New: tF2}},
		},
		{
			description:             "file untracked on beta along with new rules",
			modes:                   twoWayModes,
			ancestor:                nested("file", tF1),
			alpha:                   nested("file", tF1),
			beta:                    withRules("file", tU),
			expectedAncestorChanges: []*Change{{Path: "file"}},
			expectedAlphaChanges:    []*Change{{Path: ".gitignore", New: tF2}},
		},
		{
			description:             "file untracked on beta along with new rules",
			modes:                   oneWaySafeMode,
			ancestor:                nested("file", tF1),
			alpha:                   nested("file", tF1),
			beta:                    withRules("file", tU),
			expectedAncestorChanges: []*Change{{Path: "file"}},
		},
		{
			description:             "file untracked on beta along with new rules",
			modes:                   oneWayReplicaMode,
			ancestor:                nested("file", tF1),
			alpha:                   nested("file", tF1),
			beta:                    withRules("file", tU),
			expectedAncestorChanges: []*Change{{Path: "file"}},
			expectedBetaChanges:     []*Change{{Path: ".gitignore", Old: tF2}},
		},

		// The other side has changes, which would usually be propagated, or
		// result in a conflict, but which aren't and don't.
		{
			description:             "file untracked on alpha, modified on beta",
			modes:                   allModes,
			ancestor:                nested("file", tF1),
			alpha:                   nested("file", tU),
			beta:                    nested("file", tF2),
			expectedAncestorChanges: []*Change{{Path: "file"}},
		},
		{
			description:             "file modified on alpha, untracked on beta",
			modes:                   allModes,
			ancestor:                nested("file", tF1),
			alpha:                   nested("file", tF2),
			beta:                    nested("file", tU),
			expectedAncestorChanges: []*Change{{Path: "file"}},
		},
		{
			description: "new file opposite untracked content on beta",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       nested("file", tF1),
			beta:        nested("file", tU),
		},
		{
			description: "new file opposite untracked content on alpha",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       nested("file", tU),
			beta:        nested("file", tF1),
		},
		{
			description: "new directory opposite untracked content on beta",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       nested("data", tD1),
			beta:        nested("data", tU),
		},
		{
			description: "new directory opposite untracked content on alpha",
			modes:       allModes,
			ancestor:    tD0,
			alpha:       nested("data", tU),
			beta:        nested("data", tD1),
		},
		{
			description: "new nested directory opposite untracked content on beta",
			modes:       allModes,
			ancestor:    nested("pkg", tD0),
			alpha:       pkg(tD1),
			beta:        pkg(tU),
		},
		{
			description: "new nested directory opposite untracked content on alpha",
			modes:       allModes,
			ancestor:    nested("pkg", tD0),
			alpha:       pkg(tU),
			beta:        pkg(tD1),
		},
		{
			description:             "file replaced by directory opposite untracked content",
			modes:                   allModes,
			ancestor:                nested("path", tF1),
			alpha:                   nested("path", tD1),
			beta:                    nested("path", tU),
			expectedAncestorChanges: []*Change{{Path: "path"}},
		},

		// Both sides lack tracked content at the path.
		{
			description:             "untracked on both sides",
			modes:                   allModes,
			ancestor:                nested("file", tF1),
			alpha:                   nested("file", tU),
			beta:                    nested("file", tU),
			expectedAncestorChanges: []*Change{{Path: "file"}},
		},
		{
			description:             "untracked on alpha, deleted on beta",
			modes:                   allModes,
			ancestor:                nested("file", tF1),
			alpha:                   nested("file", tU),
			beta:                    tD0,
			expectedAncestorChanges: []*Change{{Path: "file"}},
		},
		{
			description:             "deleted on alpha, untracked on beta",
			modes:                   allModes,
			ancestor:                nested("file", tF1),
			alpha:                   tD0,
			beta:                    nested("file", tU),
			expectedAncestorChanges: []*Change{{Path: "file"}},
		},

		// Problematic content takes precedence and leaves the ancestor alone.
		{
			description: "untracked on alpha, problematic on beta",
			modes:       allModes,
			ancestor:    nested("file", tF1),
			alpha:       nested("file", tU),
			beta:        nested("file", tP1),
		},
		{
			description: "problematic on alpha, untracked on beta",
			modes:       allModes,
			ancestor:    nested("file", tF1),
			alpha:       nested("file", tP1),
			beta:        nested("file", tU),
		},

		// Real deletions, where the content is gone rather than untracked,
		// continue to be propagated.
		{
			description:         "file deleted on alpha",
			modes:               allModes,
			ancestor:            nested("file", tF1),
			alpha:               tD0,
			beta:                nested("file", tF1),
			expectedBetaChanges: []*Change{{Path: "file", Old: tF1}},
		},
		{
			description:          "file deleted on beta",
			modes:                twoWayModes,
			ancestor:             nested("file", tF1),
			alpha:                nested("file", tF1),
			beta:                 tD0,
			expectedAlphaChanges: []*Change{{Path: "file", Old: tF1}},
		},
		{
			description:         "file deleted on beta",
			modes:               oneWayModes,
			ancestor:            nested("file", tF1),
			alpha:               nested("file", tF1),
			beta:                tD0,
			expectedBetaChanges: []*Change{{Path: "file", New: tF1}},
		},
		{
			description:         "directory deleted on alpha",
			modes:               allModes,
			ancestor:            nested("data", tD1),
			alpha:               tD0,
			beta:                nested("data", tD1),
			expectedBetaChanges: []*Change{{Path: "data", Old: tD1}},
		},
		{
			description:         "nested directory deleted on alpha",
			modes:               allModes,
			ancestor:            pkg(tD1),
			alpha:               nested("pkg", tD0),
			beta:                pkg(tD1),
			expectedBetaChanges: []*Change{{Path: "pkg/build", Old: tD1}},
		},
		{
			description:         "new rules on alpha with a deletion alongside them",
			modes:               allModes,
			ancestor:            nested("file", tF1),
			alpha:               rules,
			beta:                nested("file", tF1),
			expectedBetaChanges: []*Change{{Path: ".gitignore", New: tF2}, {Path: "file", Old: tF1}},
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
		}
	}
}

// TestReconcileGitModeWithoutUntrackedContent tests that ReconcileGitMode
// behaves identically to Reconcile unless an endpoint has untracked content at
// a path, and that Reconcile continues to propagate deletions of untracked
// content.
func TestReconcileGitModeWithoutUntrackedContent(t *testing.T) {
	// Exhaustively compare the functions for a selection of entries that don't
	// have any untracked content.
	ancestors := []*Entry{nil, tF1, tF2, tSR, tG1, tD0, tD1, tD2, tDD0, tDM}
	sides := append(ancestors[:len(ancestors):len(ancestors)], tP1, tDP1, tG2)
	for _, mode := range allModes {
		for _, ancestor := range ancestors {
			for _, alpha := range sides {
				for _, beta := range sides {
					ancestorChanges, alphaChanges, betaChanges, conflicts := Reconcile(ancestor, alpha, beta, mode)
					gitAncestorChanges, gitAlphaChanges, gitBetaChanges, gitConflicts := ReconcileGitMode(ancestor, alpha, beta, mode)
					if !testingChangeListsEqual(gitAncestorChanges, ancestorChanges) ||
						!testingChangeListsEqual(gitAlphaChanges, alphaChanges) ||
						!testingChangeListsEqual(gitBetaChanges, betaChanges) ||
						!testingConflictListsEqual(gitConflicts, conflicts) {
						t.Errorf("results differ for ancestor %v, alpha %v, beta %v (%s)",
							ancestor, alpha, beta, mode.Description(),
						)
					}
				}
			}
		}
	}

	// Verify that Reconcile still sees untracked content on one side and
	// unchanged content on the other as a deletion in two-way modes.
	for _, mode := range twoWayModes {
		_, _, betaChanges, _ := Reconcile(nested("file", tF1), nested("file", tU), nested("file", tF1), mode)
		if !testingChangeListsEqual(betaChanges, []*Change{{Path: "file", Old: tF1}}) {
			t.Errorf("unexpected beta changes (%s): %v", mode.Description(), betaChanges)
		}
	}
}
