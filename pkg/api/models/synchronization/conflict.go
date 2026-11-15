package synchronization

import (
	"fmt"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// Conflict represents a filesystem change conflict.
type Conflict struct {
	// Root is the root path for the conflict, relative to the synchronization
	// root.
	Root string `json:"root"`
	// AlphaChanges are the relevant changes on alpha.
	AlphaChanges []Change `json:"alphaChanges"`
	// BetaChanges are the relevant changes on beta.
	BetaChanges []Change `json:"betaChanges"`
}

// loadFromInternal sets a conflict to match an internal Protocol Buffers
// representation. The conflict must be valid.
func (c *Conflict) loadFromInternal(conflict *core.Conflict) {
	// Propagate the conflict root.
	c.Root = conflict.Root

	// Propagate alpha changes.
	c.AlphaChanges = make([]Change, len(conflict.AlphaChanges))
	for i := 0; i < len(conflict.AlphaChanges); i++ {
		c.AlphaChanges[i].loadFromInternal(conflict.AlphaChanges[i])
	}

	// Propagate beta changes.
	c.BetaChanges = make([]Change, len(conflict.BetaChanges))
	for i := 0; i < len(conflict.BetaChanges); i++ {
		c.BetaChanges[i].loadFromInternal(conflict.BetaChanges[i])
	}
}

// Repository represents the commit and branch of the repository at an
// endpoint's synchronization root in Git mode.
type Repository struct {
	// Commit is the hexadecimal identifier of the commit that HEAD identifies.
	Commit string `json:"commit"`
	// Branch is the branch that HEAD refers to, or empty if HEAD is detached.
	Branch string `json:"branch,omitempty"`
}

// exportRepository converts an internal Protocol Buffers representation of a
// repository's commit and branch, which must have a commit.
func exportRepository(repository *core.Repository) Repository {
	return Repository{Commit: fmt.Sprintf("%x", repository.Commit), Branch: repository.Branch}
}

// RepositoryConflict represents a disagreement between the commits and branches
// of the repositories at the endpoints' synchronization roots in Git mode,
// which synchronization can't resolve.
type RepositoryConflict struct {
	// Ancestor is the commit and branch on which the repositories last agreed,
	// if they ever have.
	Ancestor *Repository `json:"ancestor,omitempty"`
	// Alpha is the repository at alpha.
	Alpha Repository `json:"alpha"`
	// Beta is the repository at beta.
	Beta Repository `json:"beta"`
}

// exportRepositoryConflict converts an internal Protocol Buffers representation
// of a repository conflict, which must be valid if it's non-nil.
func exportRepositoryConflict(conflict *core.RepositoryConflict) *RepositoryConflict {
	if conflict == nil {
		return nil
	}
	result := &RepositoryConflict{
		Alpha: exportRepository(conflict.Alpha),
		Beta:  exportRepository(conflict.Beta),
	}
	if conflict.Ancestor != nil {
		ancestor := exportRepository(conflict.Ancestor)
		result.Ancestor = &ancestor
	}
	return result
}

// exportConflicts is a convenience function that calls
// Conflict.loadFromInternal for a slice of conflicts.
func exportConflicts(conflicts []*core.Conflict) []Conflict {
	// If there are no conflicts, then just return a nil slice.
	count := len(conflicts)
	if count == 0 {
		return nil
	}

	// Create the resulting slice.
	results := make([]Conflict, count)
	for i := 0; i < count; i++ {
		results[i].loadFromInternal(conflicts[i])
	}

	// Done.
	return results
}
