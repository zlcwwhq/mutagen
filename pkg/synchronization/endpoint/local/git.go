package local

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/git"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
)

const (
	// gitWarningTimeout is the maximum amount of time that the Git commands of
	// each warning check can take. Warnings are only informational, so they
	// mustn't be able to hold up a synchronization cycle indefinitely (e.g. on
	// a filesystem that's stopped responding).
	gitWarningTimeout = 30 * time.Second

	// maximumTrackedIgnoredWarnings is the maximum number of paths for which
	// individual warnings will be generated about files that are tracked by Git
	// but matched by ignore rules. It only limits the size of snapshots, so
	// it's somewhat arbitrary, though it exceeds what session listings will
	// display.
	maximumTrackedIgnoredWarnings = 20
)

// containsGitRulePath determines whether or not any of the specified re-check
// paths identifies a file whose contents affect ignore evaluation in Git mode.
// This includes the exclude files of the root repository and of nested
// repositories, for which gitModeEventRelevant lets events through, and which
// aren't synchronized, but which local edits can change. An exclude file
// applies throughout its repository's working tree, so a change to one
// requires a full scan, like a change to a rule file.
func containsGitRulePath(recheckPaths map[string]bool) bool {
	for path := range recheckPaths {
		if gitignore.AffectsIgnoreEvaluation(path) {
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

// gitWarnings computes warnings about the content of a snapshot in Git mode,
// such as content that's missing from synchronization despite being part of a
// repository. It's called for each synchronization cycle. Warnings are
// advisory, so a check that can't run for any reason (e.g. because Git isn't
// installed or because the root isn't a repository) simply results in no
// warnings from that check rather than an error.
func (e *endpoint) gitWarnings(ctx context.Context, snapshot *core.Snapshot) []*core.Problem {
	var warnings []*core.Problem
	warnings = append(warnings, e.trackedIgnoredWarnings(ctx, snapshot)...)
	return warnings
}

// trackedIgnoredWarnings computes warnings about files that are tracked by the
// repository at the root (if any), but matched by ignore rules, and which thus
// aren't synchronized, even though they're probably assumed to be. There's one
// warning for each such file in the snapshot or, if a file is in an ignored
// directory, for the directory, up to a maximum, with any remainder being
// covered by a single summary warning.
func (e *endpoint) trackedIgnoredWarnings(ctx context.Context, snapshot *core.Snapshot) []*core.Problem {
	// This check only applies to repositories at the root. We don't need to
	// involve Git to determine that a root doesn't have a repository, which is
	// the common case for an endpoint that only receives content. Note that Git
	// won't discover a repository above the root (see git.Command).
	if snapshot.Content == nil || snapshot.Content.Kind != core.EntryKind_Directory {
		return nil
	} else if _, err := os.Lstat(filepath.Join(e.root, ".git")); err != nil {
		return nil
	}

	// List the tracked files that match ignore rules.
	paths, err := e.listTrackedIgnored(ctx)
	if err != nil {
		e.logger.Debug("Unable to list files tracked by Git but matching ignore rules:", err)
		return nil
	}

	// Identify the shallowest untracked entry for each file, if there is one.
	// If the snapshot has the file itself, then the session's ignores must have
	// overridden the rules that match it, in which case there's nothing to
	// report. If the snapshot doesn't have an entry for the file or its parent
	// directories at all, then there's also nothing to report, since the file
	// isn't on disk (or is reported separately because of its name). Paths need
	// to be normalized in the same way as the snapshot's names to be found.
	// Git's paths are not necessarily valid UTF-8, but the paths that we find
	// in the snapshot are, and only those are used in warnings, which must be.
	var warnings []*core.Problem
	reported := make(map[string]bool)
	for _, path := range paths {
		if snapshot.DecomposesUnicode {
			path = norm.NFC.String(path)
		}
		untracked := shallowestUntracked(snapshot.Content, path)
		if untracked == "" || reported[untracked] {
			continue
		}
		reported[untracked] = true
		var message string
		if untracked == path {
			message = fmt.Sprintf(
				"tracked by Git but matched by .gitignore, so it isn't synchronized; add %q to ignore.paths to synchronize it",
				"!"+untracked,
			)
		} else {
			// Unignore rules can't reach beneath an ignored directory because
			// traversal doesn't continue into it, so the only option is to
			// synchronize the whole directory.
			message = fmt.Sprintf(
				"contains files tracked by Git but is matched by .gitignore, so they aren't synchronized; add %q to ignore.paths to synchronize the whole directory",
				"!"+untracked,
			)
		}
		warnings = append(warnings, &core.Problem{Path: untracked, Error: message, Warning: true})
	}

	// Sort the warnings so that they don't vary between synchronization cycles,
	// and limit them, using the path of the first one that's omitted for the
	// summary. The summary must have a real path, because tools match the paths
	// of problems against the paths that they're working on, and they treat the
	// empty path (which identifies the root) as affecting everything.
	core.SortProblems(warnings)
	if len(warnings) > maximumTrackedIgnoredWarnings+1 {
		omitted := len(warnings) - maximumTrackedIgnoredWarnings
		warnings = warnings[:maximumTrackedIgnoredWarnings+1]
		warnings[maximumTrackedIgnoredWarnings] = &core.Problem{
			Path: warnings[maximumTrackedIgnoredWarnings].Path,
			Error: fmt.Sprintf(
				"this and %d more paths after it are tracked by Git or contain files tracked by Git, but are matched by .gitignore, so they aren't synchronized",
				omitted-1,
			),
			Warning: true,
		}
	}
	return warnings
}

// listTrackedIgnored lists the root-relative paths of the files that are
// tracked by the repository at the root but that match its .gitignore files or
// its exclude file. Like the ignorer used for scanning, it doesn't consider
// global configuration.
func (e *endpoint) listTrackedIgnored(ctx context.Context) ([]string, error) {
	// Bound the duration of the commands.
	ctx, cancel := context.WithTimeout(ctx, gitWarningTimeout)
	defer cancel()

	// Locate the exclude file, which isn't necessarily in the root (e.g. if the
	// root is a linked worktree). Git treats the file as relative to the
	// working directory, and it refuses to list files if the file doesn't
	// exist.
	arguments := []string{"ls-files", "-z", "-c", "-i", "--exclude-per-directory=.gitignore"}
	output, err := git.Command(ctx, e.root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return nil, fmt.Errorf("unable to locate exclude file: %w", err)
	}
	excludePath := strings.TrimRight(string(output), "\r\n")
	excludeFile := excludePath
	if !filepath.IsAbs(excludeFile) {
		excludeFile = filepath.Join(e.root, excludeFile)
	}
	if _, err := os.Stat(excludeFile); err == nil {
		arguments = append(arguments, "--exclude-from="+excludePath)
	}

	// List and split paths. Paths aren't quoted because we've requested
	// null-terminated output.
	output, err = git.Command(ctx, e.root, arguments...).Output()
	if err != nil {
		return nil, fmt.Errorf("unable to list files: %w", err)
	}
	var paths []string
	for _, path := range bytes.Split(output, []byte{0}) {
		if len(path) > 0 {
			paths = append(paths, string(path))
		}
	}
	return paths, nil
}

// shallowestUntracked returns the path of the shallowest untracked entry at or
// above the specified root-relative path in an entry hierarchy, or an empty
// string if there's no such entry, either because the entry at the path isn't
// untracked or because the path can't be followed that far.
func shallowestUntracked(root *core.Entry, path string) string {
	entry := root
	for end := 0; end < len(path); {
		// Find the end of the next path component.
		start := end
		if slash := strings.IndexByte(path[start:], '/'); slash >= 0 {
			end = start + slash
		} else {
			end = len(path)
		}

		// Descend to the content with that name.
		entry = entry.Contents[path[start:end]]
		if entry == nil {
			return ""
		} else if entry.Kind == core.EntryKind_Untracked {
			return path[:end]
		}

		// Move past the separator.
		end++
	}
	return ""
}
