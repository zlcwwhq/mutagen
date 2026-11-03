package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// waitDelay is the time that a command created by Command has to
	// close its I/O pipes after it exits or after its cancellation is complete,
	// before waiting on it is abandoned. Killing Git can leave helper processes
	// (such as those of remote transports) behind, and they keep the pipes open
	// and would otherwise keep waiting on the command from returning. The same
	// can happen if a command succeeds, e.g. if ssh leaves a connection master
	// behind that inherited its standard error.
	waitDelay = 5 * time.Second
	// defaultAllowedProtocols is the value of GIT_ALLOW_PROTOCOL that
	// Command uses if it isn't already set. It permits the transports that
	// reach remote hosts, but not the file and ext transports, which can name
	// local paths or execute arbitrary programs and thus have no business
	// being selected by a URL found in a repository.
	defaultAllowedProtocols = "https:http:ssh:git"
)

// inheritedVariables lists the environment variables that tell Git to use a
// repository, working tree, or set of objects other than the ones that it
// would discover from its working directory. They're removed from the
// environment of Git commands because the repositories that Mutagen operates
// on are always the ones that it names by directory, and a stray value (such as
// one set by a Git hook that launched Mutagen) would redirect commands to
// another repository.
var inheritedVariables = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
	"GIT_PREFIX",
}

// sameVariable returns whether or not two environment variable names are
// the same. Names are compared exactly, except on Windows, where the
// environment is case-insensitive.
func sameVariable(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// environment computes the environment of a Git command run in the
// specified directory, which is based on that of the current process. See
// Command for details.
func environment(directory string) []string {
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
	current := os.Environ()
	result := make([]string, 0, len(current)+4)
	allowProtocolSet := false
	for _, entry := range current {
		name, _, _ := strings.Cut(entry, "=")
		if sameVariable(name, "GIT_ALLOW_PROTOCOL") {
			allowProtocolSet = true
		}
		omit := sameVariable(name, "GIT_TERMINAL_PROMPT") ||
			sameVariable(name, "LC_ALL") ||
			sameVariable(name, "GIT_CEILING_DIRECTORIES")
		for _, variable := range inheritedVariables {
			omit = omit || sameVariable(name, variable)
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
		result = append(result, "GIT_ALLOW_PROTOCOL="+defaultAllowedProtocols)
	}
	return result
}

// Command creates a git command that runs in directory with the specified
// arguments, which follow Git's global options and thus begin with a
// subcommand, or with any further global options. The command is hardened for
// operating on repositories that Mutagen doesn't control, and which may be only
// partially present or controlled by another party:
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
// The command is stopped when ctx is canceled, along with its helper processes
// where the platform supports that, which includes giving them a moment to
// clean up, and waiting on the command covering that, where the platform
// supports that (see configureProcess). Waiting on it doesn't block for more
// than a few seconds after that on processes that are still running. The
// command is returned unstarted, so callers can set up its I/O.
func Command(ctx context.Context, directory string, args ...string) *exec.Cmd {
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
	command.Env = environment(directory)
	command.WaitDelay = waitDelay
	configureProcess(command)
	return command
}
