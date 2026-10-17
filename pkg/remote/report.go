package remote

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
