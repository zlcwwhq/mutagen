package core

import (
	"sort"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/fastpath"
)

// gitLink returns the GitLink entry of a directory, if it has one.
func (e *Entry) gitLink() *Entry {
	if e == nil || e.Kind != EntryKind_Directory {
		return nil
	} else if link := e.Contents[GitLinkName]; link != nil && link.Kind == EntryKind_GitLink {
		return link
	}
	return nil
}

// IsSubmodule determines whether or not an entry is a directory with a GitLink
// entry, which makes it a submodule.
func (e *Entry) IsSubmodule() bool {
	return e.gitLink() != nil
}

// GitLinkPaths returns the sorted paths of the GitLink entries within the entry
// hierarchy rooted at the entry, relative to the entry.
func (e *Entry) GitLinkPaths() []string {
	var paths []string
	e.walk("", func(path string, entry *Entry) {
		if entry != nil && entry.Kind == EntryKind_GitLink {
			paths = append(paths, path)
		}
	}, false)
	sort.Strings(paths)
	return paths
}

// Submodules returns the GitLink entries within the entry hierarchy rooted at
// the entry, keyed by the paths of the submodules (the directories that contain
// them), relative to the entry.
func (e *Entry) Submodules() map[string]*Entry {
	var submodules map[string]*Entry
	e.walk("", func(path string, entry *Entry) {
		if entry != nil && entry.Kind == EntryKind_GitLink {
			if directory, ok := gitLinkDirectory(path); ok {
				if submodules == nil {
					submodules = make(map[string]*Entry)
				}
				submodules[directory] = entry
			}
		}
	}, false)
	return submodules
}

// GitLinkSeeds returns the seeds that the GitLink entries within the entry
// hierarchy rooted at the entry carry, in no particular order.
func (e *Entry) GitLinkSeeds() []string {
	var seeds []string
	e.walk("", func(_ string, entry *Entry) {
		if entry != nil && entry.Kind == EntryKind_GitLink && entry.Seed != "" {
			seeds = append(seeds, entry.Seed)
		}
	}, false)
	return seeds
}

// gitLinkDirectory returns the path of the directory that contains the GitLink
// entry at the specified path, or false if the path can't be that of a GitLink
// entry, either because its base name isn't .git or because it's the root.
func gitLinkDirectory(path string) (string, bool) {
	if path == "" || path == GitLinkName {
		return "", false
	} else if parent, base := fastpath.Dir(path), fastpath.Base(path); base == GitLinkName && parent != "" {
		return parent, true
	}
	return "", false
}

// SeedChanges computes the changes that make an ancestor absorb the content of
// submodule populations that endpoints have reported in their snapshots (see
// Snapshot.Seeds), which the ancestor would have absorbed from the transition
// results that yielded them, if the session hadn't stopped before saving it.
// The changes replace the ancestor's content at each path with the content of
// the population, after making each of the path's parents a directory if it
// isn't already one. Parents are only affected if the transition that
// populated the submodule also created them, and a directory is what that
// transition's result would have recorded for them. The changes are ordered so
// that they can be applied with Apply, and they're nil if there's nothing to
// absorb. The snapshots' seeds must be valid (see Snapshot.EnsureValid).
func SeedChanges(ancestor *Entry, seeds ...map[string]*Entry) []*Change {
	// Collect and sort the paths, so that a population within another one is
	// absorbed after it.
	var paths []string
	contents := make(map[string]*Entry)
	for _, endpointSeeds := range seeds {
		for path, content := range endpointSeeds {
			if _, ok := contents[path]; !ok {
				paths = append(paths, path)
			}
			contents[path] = content
		}
	}
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)

	// Compute the changes, applying them as we go, so that the parents of each
	// path are checked against the ancestor that earlier changes yield. This
	// only happens if a session stopped at exactly the wrong time, so we don't
	// mind the copies that Apply makes.
	var changes []*Change
	working := ancestor
	record := func(change *Change) {
		changes = append(changes, change)
		if applied, err := Apply(working, []*Change{change}); err == nil {
			working = applied
		}
	}
	for _, path := range paths {
		// Ensure that each parent is a directory, from the root down.
		parentPaths := []string{""}
		for i := 0; i < len(path); i++ {
			if path[i] == '/' {
				parentPaths = append(parentPaths, path[:i])
			}
		}
		for _, parentPath := range parentPaths {
			if parent := lookup(working, parentPath); parent == nil || parent.Kind != EntryKind_Directory {
				record(&Change{Path: parentPath, New: &Entry{Kind: EntryKind_Directory}})
			}
		}

		// Replace the content at the path.
		record(&Change{Path: path, New: contents[path]})
	}

	// Done.
	return changes
}

// lookup returns the entry at the specified path within an entry hierarchy, or
// nil if there's none.
func lookup(root *Entry, path string) *Entry {
	if path == "" {
		return root
	}
	entry := root
	for _, component := range strings.Split(path, "/") {
		if entry == nil {
			return nil
		}
		entry = entry.Contents[component]
	}
	return entry
}
