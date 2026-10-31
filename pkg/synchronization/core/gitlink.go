package core

import (
	"context"
	"sort"
)

// GitProvider defines the interface that scanning and transitioning use to
// operate on Git repositories, which keeps this package free of any knowledge
// of how Git is executed. All directories are absolute paths on the filesystem
// of the synchronization root. Implementations must return promptly when their
// context is cancelled, and they must be safe for concurrent use, because a
// background scan can overlap with a transition.
type GitProvider interface {
	// Probe examines the repository in a directory that has an entry named
	// .git. If the directory is a repository whose HEAD resolves to a commit
	// and whose origin can be used, then it returns that commit in its raw form
	// (i.e. not hexadecimal) and the URL of its origin, without credentials. If
	// it's a repository without commits or without an origin, then it returns a
	// nil commit, and the directory is an ordinary directory. In any other case
	// it returns an error whose message tells the user what to do about it,
	// because the directory's content can't be classified.
	Probe(ctx context.Context, directory string) (commit []byte, url string, err error)
	// Head returns the commit that's checked out in the repository in a
	// directory, in its raw form.
	Head(ctx context.Context, directory string) ([]byte, error)
	// Clone populates an empty directory with a clone of the repository at a
	// URL and checks out a commit on a detached HEAD. If it fails, then the
	// directory may contain partial content, which the caller must remove.
	Clone(ctx context.Context, directory, url string, commit []byte) error
	// Update checks out a commit on a detached HEAD in the repository in a
	// directory, fetching the commit if it's missing, first from the
	// repository's own origin and then from the specified URL, because the two
	// endpoints may reach the repository through different URLs. If it fails,
	// then the current commit remains checked out.
	Update(ctx context.Context, directory, url string, commit []byte) error
	// UpdateNested updates the submodules nested within the repository in a
	// directory, if it has any, to the commits that the repository records for
	// them. The URL identifies the repository in messages.
	UpdateNested(ctx context.Context, directory, url string) error
}

// GitLinkPaths returns the sorted paths of the submodules within the entry
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
