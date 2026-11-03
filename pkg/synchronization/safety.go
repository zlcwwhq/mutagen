package synchronization

import (
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// oneEndpointEmptiedRoot determines whether or not one endpoint (but not both)
// transitioned from a directory root with a non-trivial amount of content to a
// directory root without any content. If gitMode is true, then a root can also
// count as being without content if all that remains in it is untracked (see
// rootEmptied).
func oneEndpointEmptiedRoot(ancestor, alpha, beta *core.Entry, gitMode bool) bool {
	// Check that all three entries are directories. If not, then this check
	// doesn't apply.
	if ancestor == nil || ancestor.Kind != core.EntryKind_Directory {
		return false
	} else if alpha == nil || alpha.Kind != core.EntryKind_Directory {
		return false
	} else if beta == nil || beta.Kind != core.EntryKind_Directory {
		return false
	}

	// Check whether or not the ancestor has a non-trivial amount of content.
	// We define a non-trivial amount of content as two or more entries that are
	// immediate children of the synchronization root. If that's not the case,
	// then this check doesn't apply.
	if len(ancestor.Contents) < 2 {
		return false
	}

	// Check if alpha deleted all content within the root.
	alphaEmptied := rootEmptied(ancestor, alpha, gitMode)

	// Check if beta deleted all content within the root.
	betaEmptied := rootEmptied(ancestor, beta, gitMode)

	// Determine whether one (and only one) endpoint emptied root content.
	return (alphaEmptied || betaEmptied) && !(alphaEmptied && betaEmptied)
}

// rootEmptied determines whether or not an endpoint's directory root has been
// emptied relative to the ancestor's directory root. A root that doesn't have
// any contents has always been emptied. In Git mode, untracked content doesn't
// prevent this conclusion, because a repository's untracked .git entry survives
// the deletion of everything else in a root, but only if the untracked content
// has names that the ancestor doesn't: the ancestor only contains synchronized
// content, so an untracked entry with a name in the ancestor was hidden by an
// ignore rule rather than deleted, and it's still there.
func rootEmptied(ancestor, root *core.Entry, gitMode bool) bool {
	if len(root.Contents) == 0 {
		return true
	} else if !gitMode {
		return false
	}
	for name, entry := range root.Contents {
		if entry.Kind != core.EntryKind_Untracked {
			return false
		} else if _, tracked := ancestor.Contents[name]; tracked {
			return false
		}
	}
	return true
}

// containsRootDeletion determines whether or not any of the specified changes
// is a root deletion change.
func containsRootDeletion(changes []*core.Change) bool {
	// Look for root deletions.
	for _, change := range changes {
		if change.IsRootDeletion() {
			return true
		}
	}

	// Done.
	return false
}

// containsRootTypeChange determines whether or not any of the specified changes
// is a root type change.
func containsRootTypeChange(changes []*core.Change) bool {
	// Look for root type changes.
	for _, change := range changes {
		if change.IsRootTypeChange() {
			return true
		}
	}

	// Done.
	return false
}

// filteredPathsAreSubset checks whether or not a slice of filtered paths is a
// subset of a larger slice of unfiltered paths. The paths in the filtered slice
// must share the same relative ordering as in the original slice.
func filteredPathsAreSubset(filteredPaths, originalPaths []string) bool {
	// Loop over the list of filtered paths.
	for _, filtered := range filteredPaths {
		// Track whether or not we find a match for this path in what remains of
		// the original path list.
		matched := false

		// Loop over what remains of the original paths.
		for o, original := range originalPaths {
			if original == filtered {
				originalPaths = originalPaths[o+1:]
				matched = true
				break
			}
		}

		// Check if we found a match.
		if !matched {
			return false
		}
	}

	// Success.
	return true
}
