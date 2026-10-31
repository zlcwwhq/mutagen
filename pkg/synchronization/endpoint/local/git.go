package local

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
)

const (
	// gitWaitDelay is the time that a command created by gitCommand has to
	// close its I/O pipes after it exits or its context is canceled, before
	// waiting on it is abandoned. Killing Git can leave helper processes (such
	// as those of remote transports) behind, and they keep the pipes open and
	// would otherwise keep waiting on the command from returning.
	gitWaitDelay = 5 * time.Second

	// gitDefaultAllowedProtocols is the value of GIT_ALLOW_PROTOCOL that
	// gitCommand uses if it isn't already set. It permits the transports that
	// reach remote hosts, but not the file and ext transports, which can name
	// local paths or execute arbitrary programs and thus have no business
	// being selected by a URL found in a repository.
	gitDefaultAllowedProtocols = "https:http:ssh:git"

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

// gitInheritedVariables lists the environment variables that tell Git to use a
// repository, working tree, or set of objects other than the ones that it
// would discover from its working directory. They're removed from the
// environment of Git commands because the repositories that Mutagen operates
// on are always the ones that it names by directory, and a stray value (such as
// one set by a Git hook that launched Mutagen) would redirect commands to
// another repository.
var gitInheritedVariables = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
	"GIT_PREFIX",
}

// gitSameVariable returns whether or not two environment variable names are
// the same. Names are compared exactly, except on Windows, where the
// environment is case-insensitive.
func gitSameVariable(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// gitEnvironment computes the environment of a Git command run in the
// specified directory, which is based on that of the current process. See
// gitCommand for details.
func gitEnvironment(directory string) []string {
	// Prevent Git from searching the parent of the directory for a repository.
	// Git only treats a directory as a repository if it contains a valid Git
	// directory, so a directory whose .git entry is invalid (such as an empty
	// directory left behind by a failed operation) would otherwise lead Git to
	// silently operate on any repository that encloses it. Ceiling directories
	// must be absolute, and the directory is allowed to be relative to the
	// current process's working directory, as Cmd.Dir is.
	if absolute, err := filepath.Abs(directory); err == nil {
		directory = absolute
	}
	ceiling := filepath.Dir(directory)

	// Copy the current environment, omitting variables that are either removed
	// or overridden below. If the caller has chosen the protocols that Git may
	// use, then respect their choice, including for tests that need to enable
	// the file transport.
	environment := os.Environ()
	result := make([]string, 0, len(environment)+4)
	allowProtocolSet := false
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if gitSameVariable(name, "GIT_ALLOW_PROTOCOL") {
			allowProtocolSet = true
		}
		omit := gitSameVariable(name, "GIT_TERMINAL_PROMPT") ||
			gitSameVariable(name, "LC_ALL") ||
			gitSameVariable(name, "GIT_CEILING_DIRECTORIES")
		for _, variable := range gitInheritedVariables {
			omit = omit || gitSameVariable(name, variable)
		}
		if !omit {
			result = append(result, entry)
		}
	}

	// Never let Git wait for a person to answer a prompt for credentials, and
	// make its output independent of the user's locale, so that it can be
	// parsed and its messages can be recognized.
	result = append(result,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"GIT_CEILING_DIRECTORIES="+ceiling,
	)
	if !allowProtocolSet {
		result = append(result, "GIT_ALLOW_PROTOCOL="+gitDefaultAllowedProtocols)
	}
	return result
}

// gitCommand creates a git command that runs in directory with the specified
// arguments, which follow Git's global options and thus begin with a
// subcommand. The command is hardened for operating on repositories that
// Mutagen doesn't control, and which may be only partially present or
// controlled by another party:
//
//   - Repository discovery is confined to the directory: variables that
//     redirect Git to another repository are removed from the environment, and
//     the directory's parent is made a ceiling that discovery won't climb
//     into, so that an invalid .git entry in the directory makes commands fail
//     instead of operating on an enclosing repository.
//   - Git never prompts for credentials, never runs a file system monitor, and
//     never runs hooks, since all three would execute programs or block on
//     input that no one can supply, and it doesn't take optional locks, so
//     that commands which only read don't interfere with other Git processes
//     (or with the file watching that notices them).
//   - Git's output is in the C locale.
//   - Git is limited to the transports that GIT_ALLOW_PROTOCOL lists, which is
//     the default list unless the caller's environment sets it.
//
// The command is killed when ctx is canceled, and waiting on it doesn't block
// for more than a few seconds after that on helper processes that outlive it.
// The command is returned unstarted, so callers can set up its I/O.
func gitCommand(ctx context.Context, directory string, args ...string) *exec.Cmd {
	// Prepend Git's global options. They must come before the subcommand.
	arguments := make([]string, 0, len(args)+5)
	arguments = append(arguments,
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath="+os.DevNull,
		"--no-optional-locks",
	)
	arguments = append(arguments, args...)

	// Create the command.
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = directory
	command.Env = gitEnvironment(directory)
	command.WaitDelay = gitWaitDelay
	return command
}

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
// arbitrary names and can be numerous (e.g. those of a temporary clone).
//
// The contents of Git directories are never synchronized and most of their
// contents change constantly, even as a result of commands that fail or don't
// change anything. If those changes were to wake the session, then anything
// that the session itself runs (or that runs in response to its activity) in
// the repository could keep it awake indefinitely. Thus, only events for
// entries that indicate a change of the current commit (a repository's HEAD,
// its references, and its packed references) or of ignore rules (its
// info/exclude file) are relevant. The Git directories of submodules are nested
// within that of the enclosing repository (e.g. .git/modules/x/y/HEAD), or are
// found at the submodule's path, and the same applies to them.
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

	// The Git directory itself isn't relevant. Events are reported for it when
	// entries are created or removed within it, which happens continuously due
	// to lock files.
	if len(components) == 0 {
		return false
	}

	// Look for the components that identify references and reference logs.
	// These have to be distinguished because reference logs mirror the
	// locations of what they record (e.g. logs/HEAD and logs/refs/heads/main),
	// and they are also updated in response to the same changes as what they
	// record, so we don't lose anything by excluding them. Whichever occurs
	// first determines the classification (e.g. a branch can be named
	// logs/refs). The component following one named modules is (at least the
	// first part of) the name of a submodule, which can be anything, including
	// refs or logs.
	for c := 0; c < len(components); c++ {
		switch components[c] {
		case "modules":
			c++
		case "logs":
			return false
		case "refs":
			return true
		}
	}

	// Otherwise, only a few specific files are relevant. We don't have to worry
	// about the lock files that are used to update them, because these have
	// different names, and the update is reported for the file when the lock
	// file is renamed.
	last := len(components) - 1
	switch components[last] {
	case "HEAD", "packed-refs":
		return true
	case "exclude":
		return last > 0 && components[last-1] == "info"
	}
	return false
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
	// won't discover a repository above the root (see gitCommand).
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
	output, err := gitCommand(ctx, e.root, "rev-parse", "--git-path", "info/exclude").Output()
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
	output, err = gitCommand(ctx, e.root, arguments...).Output()
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
