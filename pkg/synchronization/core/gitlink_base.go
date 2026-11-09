package core

import (
	"bytes"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/fastpath"
)

// WalkSubmodule calls a function for each file and symbolic link within the
// content of a submodule (a directory with a GitLink entry), and for each
// submodule nested in it, without visiting the content of nested submodules,
// since that belongs to their own repositories. Paths are relative to the
// submodule, and the order is unspecified. Other entries, such as untracked
// and problematic content, aren't visited.
func (e *Entry) WalkSubmodule(visit func(path string, entry *Entry)) {
	var walk func(prefix string, directory *Entry)
	walk = func(prefix string, directory *Entry) {
		for name, entry := range directory.Contents {
			path := prefix + name
			switch entry.Kind {
			case EntryKind_Directory:
				if entry.IsSubmodule() {
					visit(path, entry)
				} else {
					walk(path+"/", entry)
				}
			case EntryKind_File, EntryKind_SymbolicLink:
				visit(path, entry)
			}
		}
	}
	if e.IsSubmodule() {
		walk("", e)
	}
}

// placeEntry places an entry at a path within a directory hierarchy that the
// caller owns, creating parent directories as necessary. If there's already an
// entry at the path, then it's returned instead, without any change. It fails
// if a parent of the path isn't a directory.
func placeEntry(root *Entry, path string, entry *Entry) (*Entry, bool) {
	parent := root
	components := strings.Split(path, "/")
	for _, component := range components[:len(components)-1] {
		child := parent.Contents[component]
		if child == nil {
			child = &Entry{Kind: EntryKind_Directory, Contents: make(map[string]*Entry)}
			parent.Contents[component] = child
		} else if child.Kind != EntryKind_Directory {
			return nil, false
		}
		parent = child
	}
	name := components[len(components)-1]
	if existing := parent.Contents[name]; existing != nil {
		return existing, true
	}
	parent.Contents[name] = entry
	return nil, true
}

// removeEntry removes the entry at a path within a directory hierarchy that the
// caller owns, if there's one.
func removeEntry(root *Entry, path string) {
	parentPath, name := fastpath.Dir(path), fastpath.Base(path)
	if parent := lookup(root, parentPath); parent != nil {
		delete(parent.Contents, name)
	}
}

// submoduleBase computes the merge base for a submodule that both endpoints
// have, with the same commit checked out, from what Git says about their
// working trees (see SubmoduleStatus). A file or symbolic link that one
// endpoint has unchanged from the commit is the commit's content at that path,
// so it's the merge base, against which the other endpoint's content counts as
// changed, if it differs, and propagates. If both endpoints have the commit's
// content at a path, but their entries differ (e.g. because their line ending
// settings differ), then there's no telling which is right, so the merge base
// has nothing at that path, and the difference is a conflict, as it is if
// both endpoints have changed a path. The content of each submodule nested in
// the submodule is the ancestor's, since it's reconciled separately, by the
// same rule if both endpoints have moved it to the same commit. It returns
// false if the statuses contradict each other, since both endpoints can't have
// the commit's content if they have entries of different kinds at a path.
func submoduleBase(ancestor, alpha, beta *Entry, alphaStatus, betaStatus *SubmoduleStatus) (*Entry, bool) {
	// Start with the Git entry, which the endpoints agree on.
	base := &Entry{
		Kind:     EntryKind_Directory,
		Contents: map[string]*Entry{GitLinkName: alpha.gitLink().Copy(EntryCopyBehaviorSlim)},
	}

	// Add the unchanged files and symbolic links of each endpoint, recording
	// where they disagree, along with the paths of nested submodules.
	disputed := make(map[string]bool)
	nested := make(map[string]bool)
	consistent := true
	for _, side := range []struct {
		content *Entry
		status  *SubmoduleStatus
	}{{alpha, alphaStatus}, {beta, betaStatus}} {
		changed := make(map[string]bool, len(side.status.Changed))
		for _, path := range side.status.Changed {
			changed[path] = true
		}
		side.content.WalkSubmodule(func(path string, entry *Entry) {
			if entry.Kind == EntryKind_Directory {
				nested[path] = true
				return
			} else if changed[path] || !consistent {
				return
			}
			existing, ok := placeEntry(base, path, entry.Copy(EntryCopyBehaviorSlim))
			if !ok || (existing != nil && existing.Kind == EntryKind_Directory) {
				consistent = false
			} else if existing != nil && !existing.Equal(entry, false) {
				disputed[path] = true
			}
		})
	}
	if !consistent {
		return nil, false
	}
	for path := range disputed {
		removeEntry(base, path)
	}

	// Add the ancestor's content for nested submodules.
	for path := range nested {
		if content := lookup(ancestor, path); content != nil {
			if existing, ok := placeEntry(base, path, content.Copy(EntryCopyBehaviorDeepPreservingLeaves)); !ok || existing != nil {
				return nil, false
			}
		}
	}

	// Done.
	return base, true
}

// rebaseSubmodule determines the ancestor with which to reconcile a path. If
// both endpoints have a submodule at the path with the same commit checked
// out, and the ancestor doesn't have that commit for it (because the endpoints
// have both moved to it since they last agreed, such as when a divergence is
// resolved by checking out the same commit on both, or because the ancestor
// doesn't have the submodule at all), then the ancestor's content for the
// submodule belongs to another commit (or to nothing), and reconciling against
// it would make every file in which the commits differ look like a conflict,
// as would any uncommitted change. If both endpoints have reported what Git
// says about their working trees, then the content that Git considers to be
// the commit's on either endpoint is used as the merge base instead (see
// submoduleBase), so that changes propagate from the endpoint that made them,
// and that merge base is recorded as the ancestor's content for the
// submodule. Otherwise, the ancestor is used as it is.
func (r *reconciler) rebaseSubmodule(path string, ancestor, alpha, beta *Entry) *Entry {
	// Check whether the rule applies.
	alphaLink, betaLink := alpha.gitLink(), beta.gitLink()
	if alphaLink == nil || betaLink == nil || !alphaLink.Equal(betaLink, false) {
		return ancestor
	} else if ancestorLink := ancestor.gitLink(); ancestorLink != nil && ancestorLink.Equal(alphaLink, false) {
		return ancestor
	}
	alphaStatus, betaStatus := r.alphaStatuses[path], r.betaStatuses[path]
	if alphaStatus == nil || betaStatus == nil ||
		!bytes.Equal(alphaStatus.Commit, alphaLink.Digest) || !bytes.Equal(betaStatus.Commit, betaLink.Digest) {
		return ancestor
	}

	// Compute the merge base and record it.
	base, ok := submoduleBase(ancestor, alpha, beta, alphaStatus, betaStatus)
	if !ok {
		return ancestor
	}
	r.ancestorChanges = append(r.ancestorChanges, &Change{Path: path, New: base})
	return base
}
