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
