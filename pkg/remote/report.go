package remote

import (
	"encoding/json"
	"errors"
	"slices"
)

// Report describes content that a session couldn't synchronize, as of the
// synchronization cycle performed by a flush. Paths are slash-separated and
// relative to the synchronization root.
type Report struct {
	// Conflicts are the session's conflicts, sorted by root path.
	Conflicts []Conflict
	// ExcludedConflicts is the number of conflicts omitted from Conflicts.
	ExcludedConflicts uint64
	// Problems are the scan and transition problems on both endpoints, sorted
	// by path for each endpoint and kind of problem.
	Problems []Problem
	// ExcludedProblems is the number of problems omitted from Problems.
	ExcludedProblems uint64
	// Resolution indicates whether remote can resolve the session's conflicts
	// (see ListConflicts), which requires the two-way-safe mode.
	Resolution bool `json:",omitempty"`
}

// Empty indicates whether the report contains no conflicts or problems.
func (r *Report) Empty() bool {
	return r == nil || (len(r.Conflicts) == 0 && r.ExcludedConflicts == 0 &&
		len(r.Problems) == 0 && r.ExcludedProblems == 0)
}

// Conflict describes conflicting changes on the two endpoints, which prevent
// synchronization of the content at and beneath its root path.
type Conflict struct {
	// Root is the conflict's root path.
	Root string
	// Alpha contains the relevant changes on alpha.
	Alpha []Change
	// Beta contains the relevant changes on beta.
	Beta []Change
	// ExcludedChanges is the number of changes omitted to fit the report into
	// a protocol message. Root is retained for comparison and diagnostics.
	ExcludedChanges uint64 `json:",omitempty"`
}

// Change describes a change on one endpoint since the last synchronization.
type Change struct {
	// Path is the changed path.
	Path string
	// Old describes the kind of the previous entry, or is empty if there was
	// none.
	Old string
	// New describes the kind of the current entry, or is empty if there is
	// none.
	New string
}

// Problem describes content that an endpoint couldn't scan or update.
type Problem struct {
	// Alpha indicates whether the problem occurred on alpha.
	Alpha bool
	// Path is the problematic path.
	Path string
	// Error describes the problem.
	Error string
}

// boundReport makes a report fit into a flush response, without modifying the
// original. Conflict counts alone don't bound the size: a single conflict can
// include changes to every file beneath a replaced directory. Trim the longest
// change lists first, retaining conflict roots and counting omitted changes.
// Oversized problems and roots are omitted only after change lists are empty.
func boundReport(report *Report) (*Report, error) {
	bounded := *report
	bounded.Conflicts = slices.Clone(report.Conflicts)
	for {
		data, err := json.Marshal(&flushResponse{Report: &bounded})
		if err != nil {
			return nil, err
		} else if len(data) <= maximumMessageSize {
			return &bounded, nil
		}

		// Halving a list bounds the number of encoding attempts, even when
		// a report contains a very large number of changes.
		var longest *[]Change
		var conflict *Conflict
		for c := range bounded.Conflicts {
			candidate := &bounded.Conflicts[c]
			for _, changes := range []*[]Change{&candidate.Alpha, &candidate.Beta} {
				if len(*changes) > 0 && (longest == nil || len(*changes) > len(*longest)) {
					longest, conflict = changes, candidate
				}
			}
		}
		if longest != nil {
			removed := (len(*longest) + 1) / 2
			conflict.ExcludedChanges += uint64(removed)
			*longest = (*longest)[:len(*longest)-removed]
		} else if len(bounded.Problems) > 0 {
			removed := (len(bounded.Problems) + 1) / 2
			bounded.ExcludedProblems += uint64(removed)
			bounded.Problems = bounded.Problems[:len(bounded.Problems)-removed]
		} else if len(bounded.Conflicts) > 0 {
			removed := (len(bounded.Conflicts) + 1) / 2
			bounded.ExcludedConflicts += uint64(removed)
			bounded.Conflicts = bounded.Conflicts[:len(bounded.Conflicts)-removed]
		} else {
			return nil, errors.New("synchronization report exceeds size limit")
		}
	}
}
