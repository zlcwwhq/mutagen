package local

import (
	"slices"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
)

// containsGitRulePath determines whether or not any of the specified re-check
// paths identifies a file whose contents affect ignore evaluation in Git mode.
func containsGitRulePath(recheckPaths map[string]bool) bool {
	for path := range recheckPaths {
		if gitignore.IsRulePath(path) {
			return true
		}
	}
	return false
}

// gitModeEventRelevant determines whether or not a filesystem event from a
// recursive watcher should wake a session in Git mode. The path is specified in
// its watch-root-relative, slash-separated form.
//
// The contents of temporary directories are never relevant, wherever they are
// in the hierarchy, because they aren't scanned. Their contents may have
// arbitrary names and can be numerous.
//
// The contents of Git directories are never synchronized and most of their
// contents change constantly, even as a result of commands that fail or don't
// change anything. If those changes were to wake the session, then anything
// that the session itself runs (or that runs in response to its activity) in
// the repository could keep it awake indefinitely. Thus, only events for an
// entry that changes ignore rules (a repository's info/exclude file) are
// relevant.
func gitModeEventRelevant(path string) bool {
	// Split the path into components and reject any with a temporary name.
	components := strings.Split(path, "/")
	for _, component := range components {
		if strings.HasPrefix(component, filesystem.TemporaryNamePrefix) {
			return false
		}
	}

	// If the path isn't in a Git directory, then it's relevant.
	gitDirectory := slices.IndexFunc(components, func(component string) bool {
		return strings.EqualFold(component, ".git")
	})
	if gitDirectory == -1 {
		return true
	}
	components = components[gitDirectory+1:]

	// Otherwise, only the exclude file is relevant. We don't have to worry about
	// the lock file that's used to update it, because that has a different name,
	// and the update is reported for the file when the lock file is renamed.
	last := len(components) - 1
	return last > 0 && components[last] == "exclude" && components[last-1] == "info"
}
