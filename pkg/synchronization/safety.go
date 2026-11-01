package synchronization

import (
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/fastpath"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
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

// isIgnoreRuleChange determines whether or not a change affects a file that
// provides ignore rules in Git mode, either by leaving a regular file at the
// change's path or by removing one from it. Removals count because they change
// what the next scan synchronizes just as additions do. Only regular files
// count, because rule files that are symbolic links aren't read. Only files
// that can be delivered count (see gitignore.IsDeliverableRuleFile), so a
// change in a Git directory, which scans don't produce and which endpoints
// refuse, can't hold up the rest of a cycle, not even for the exclude file,
// which is each endpoint's own.
func isIgnoreRuleChange(change *core.Change) bool {
	if !gitignore.IsDeliverableRuleFile(change.Path) {
		return false
	} else if change.New != nil {
		return change.New.Kind == core.EntryKind_File
	}
	return change.Old != nil && change.Old.Kind == core.EntryKind_File
}

// isIgnoreRuleReplacement determines whether or not a change replaces a file
// that provides ignore rules in Git mode with something else, such as a symbolic
// link or a directory, none of which provide rules. That removes the file's
// rules just as much as removing the file does (see isIgnoreRuleChange).
func isIgnoreRuleReplacement(change *core.Change) bool {
	return change.Old != nil && change.New != nil &&
		change.Old.Kind == core.EntryKind_File &&
		change.New.Kind != core.EntryKind_File &&
		gitignore.IsDeliverableRuleFile(change.Path)
}

// pruneToIgnoreRules creates a copy of a directory entry containing only the
// files beneath it that can be delivered to provide ignore rules in Git mode
// (see gitignore.IsDeliverableRuleFile) and the directories leading to them, or
// returns nil if there aren't any such files. The path is the directory's path
// relative to the synchronization root. Entries are immutable, so directories
// are newly created and files can be shared.
func pruneToIgnoreRules(path string, directory *core.Entry) *core.Entry {
	// Compute the prefix to add to content names to compute their paths.
	contentPathPrefix := fastpath.Joinable(path)

	// Determine which contents to retain.
	var contents map[string]*core.Entry
	for name, entry := range directory.Contents {
		contentPath := contentPathPrefix + name
		var retained *core.Entry
		if entry.Kind == core.EntryKind_File && gitignore.IsDeliverableRuleFile(contentPath) {
			retained = entry
		} else if entry.Kind == core.EntryKind_Directory {
			retained = pruneToIgnoreRules(contentPath, entry)
		}
		if retained != nil {
			if contents == nil {
				contents = make(map[string]*core.Entry)
			}
			contents[name] = retained
		}
	}

	// A directory that doesn't lead to any rule files isn't retained.
	if contents == nil {
		return nil
	}

	// Create the pruned copy.
	pruned := directory.Copy(core.EntryCopyBehaviorSlim)
	pruned.Contents = contents
	return pruned
}

// ignoreRuleDeliveries computes the changes that deliver ignore rules to an
// endpoint in Git mode, given the transitions computed for that endpoint. There
// are two types of delivery:
//
// A change satisfying isIgnoreRuleChange is included as is.
//
// A change satisfying isIgnoreRuleReplacement is included as the rule file's
// removal, along with the rule files in the replacement, if it's a directory
// (see pruneToIgnoreRules). The rest of the replacement is then created by the
// next synchronization cycle, in which the ancestor and the endpoint have what
// was delivered. Unlike the rest of a replacement, what's delivered can't be
// refused because of the endpoint's rules (see gitTransitionPruner in the local
// endpoint package) or because the endpoint can't create a symbolic link or a
// submodule, either of which would make the session compute it as a delivery in
// every cycle, at the expense of everything else. The rule files have to be
// delivered right away, because a flush is only held for a single cycle that only
// delivers rules (see the synchronization loop), so they can't be left to a
// cycle of their own without a flush returning before the rest is synchronized.
//
// A change that creates a directory (including the root) containing rule files
// is included with a pruned copy of the directory (see pruneToIgnoreRules). The
// remainder of the directory is then reconciled by the next synchronization
// cycle, in which the ancestor and the endpoint have the partial directory and
// the opposite endpoint has all of it, and which also takes the delivered rules
// into account.
//
// Anything else isn't a delivery, including a directory deletion or a type
// change of something other than a rule file that happens to involve rule files
// beneath it. The former removes the rules along with the content that they
// apply to, and the latter is left to the next cycle because it has to replace
// what's there with all of the new content.
func ignoreRuleDeliveries(changes []*core.Change) []*core.Change {
	var deliveries []*core.Change
	for _, change := range changes {
		if isIgnoreRuleChange(change) {
			deliveries = append(deliveries, change)
		} else if isIgnoreRuleReplacement(change) {
			delivery := &core.Change{Path: change.Path, Old: change.Old}
			if change.New.Kind == core.EntryKind_Directory {
				delivery.New = pruneToIgnoreRules(change.Path, change.New)
			}
			deliveries = append(deliveries, delivery)
		} else if change.Old == nil && change.New != nil && change.New.Kind == core.EntryKind_Directory {
			if pruned := pruneToIgnoreRules(change.Path, change.New); pruned != nil {
				deliveries = append(deliveries, &core.Change{Path: change.Path, New: pruned})
			}
		}
	}
	return deliveries
}

// ruleOnlyTransitions takes the transitions computed for each endpoint and
// returns those to perform in this synchronization cycle, for sessions in Git
// mode, along with whether or not they're limited to ignore rule deliveries.
//
// Each endpoint evaluates ignore rules using only the rule files that it has on
// disk, so transitions computed before an endpoint has its rules may be for
// exactly what those rules would have ignored. Transferring a large data
// directory from a server to its opposite endpoint would be wasteful, but
// deleting it, because it's absent from an endpoint that ignores it, would lose
// data. Thus, if either endpoint has rules to receive, then the cycle only
// delivers rules, on both endpoints, and every other transition is dropped.
// Dropped transitions need no record, since the next cycle's reconciliation
// recomputes them from scans that reflect the new rules, and ancestor changes
// don't depend on them, since a transition's path is disjoint from those of all
// other changes.
func ruleOnlyTransitions(αTransitions, βTransitions []*core.Change) ([]*core.Change, []*core.Change, bool) {
	αDeliveries := ignoreRuleDeliveries(αTransitions)
	βDeliveries := ignoreRuleDeliveries(βTransitions)
	if len(αDeliveries) == 0 && len(βDeliveries) == 0 {
		return αTransitions, βTransitions, false
	}
	return αDeliveries, βDeliveries, true
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
