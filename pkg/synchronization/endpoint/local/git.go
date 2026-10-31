package local

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
)

const (
	// gitWaitDelay is the time that a command created by gitCommand has to
	// close its I/O pipes after it exits or its context is canceled, before
	// waiting on it is abandoned. Killing Git can leave helper processes (such
	// as those of remote transports) behind, and they keep the pipes open and
	// would otherwise keep waiting on the command from returning. The same can
	// happen if a command succeeds, e.g. if ssh leaves a connection master
	// behind that inherited its standard error.
	gitWaitDelay = 5 * time.Second

	// gitProbeTimeout is the maximum amount of time that the Git commands of a
	// single probe can take. These commands only read a few files, so this only
	// matters on a filesystem that's stopped responding, but scans hold the
	// scan lock, and they shouldn't be able to do so indefinitely because of
	// that.
	gitProbeTimeout = 30 * time.Second

	// gitDefaultStallTimeout is the amount of time that a command that uses the
	// network can run without producing any output before it's considered to
	// have stalled. There's no limit on the overall duration of such a command,
	// because a clone of a large repository can legitimately take a long time,
	// but it reports progress all the while.
	gitDefaultStallTimeout = 60 * time.Second

	// gitDefaultStallBackoff is the amount of time for which a URL won't be
	// contacted after a command that used it has stalled. A transition holds up
	// its session's synchronization cycle, and thus flushes, so without this,
	// each flush on a system that can't reach the repository would take at
	// least as long as the stall timeout.
	gitDefaultStallBackoff = 5 * time.Minute

	// gitOutputTailSize is the amount of a command's output that's retained for
	// use in error messages. It only has to be large enough for what comes
	// after the progress output, which is where Git reports failures.
	gitOutputTailSize = 16 * 1024

	// gitMaximumMessageLines is the maximum number of lines of a command's
	// output that are included in an error message.
	gitMaximumMessageLines = 8

	// gitMaximumMessageSize is the maximum size, in bytes, of the portion of an
	// error message that comes from a command's output.
	gitMaximumMessageSize = 1024

	// gitShortCommitLength is the length of the abbreviated commit identifiers
	// that are used in messages.
	gitShortCommitLength = 12

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

	// maximumDirtySubmoduleWarnings is the maximum number of submodules for
	// which individual warnings will be generated about uncommitted changes.
	// It's limited for the same reason as maximumTrackedIgnoredWarnings.
	maximumDirtySubmoduleWarnings = 20
)

// gitProgressLine matches the lines of Git's output that merely report progress
// and that aren't of any use in an error message. These lines come from Git and
// from the remote side (with a prefix), and Git's output is in the C locale.
var gitProgressLine = regexp.MustCompile(
	`^(remote: )?(Enumerating objects|Counting objects|Compressing objects|Total \d|Receiving objects|Resolving deltas|Updating files|Filtering content)|` +
		`^(Cloning into|Fetching submodule|From |Unpacking objects)|` +
		`^ [ *+=!-] `,
)

// gitURLInText matches the URLs in text, such as in Git's output.
var gitURLInText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"]+`)

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
// The command is killed when ctx is canceled, along with its helper processes
// where the platform supports that, and waiting on it doesn't block for more
// than a few seconds after that on helper processes that outlive it. The
// command is returned unstarted, so callers can set up its I/O.
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
	configureGitProcess(command)
	return command
}

// gitCommandError is the failure of a Git command.
type gitCommandError struct {
	// err is the error from running the command.
	err error
	// message is a summary of the command's output, without credentials, which
	// may be empty.
	message string
}

// Error implements error.Error.
func (e *gitCommandError) Error() string {
	if e.message != "" {
		return e.message
	}
	return gitScrubCredentials(e.err.Error())
}

// Unwrap returns the error from running the command.
func (e *gitCommandError) Unwrap() error {
	return e.err
}

// gitExitedWith determines whether or not an error from gitRun indicates that
// the command ran and exited with the specified exit code. This is how Git
// answers questions (such as whether a configuration value exists) when it's
// able to, as opposed to failing to answer them.
func gitExitedWith(err error, code int) bool {
	var exitError *exec.ExitError
	return errors.As(err, &exitError) && exitError.ExitCode() == code
}

// gitOutputRecorder is an io.Writer that retains the tail of a command's output
// for error messages and records when output last arrived, which is how stalled
// commands are detected. The same recorder can be used as both the standard
// output and standard error of a command, in which case they share a pipe.
type gitOutputRecorder struct {
	// lock serializes access to the other fields, because the output arrives on
	// a different Goroutine from those that look at when it last did.
	lock sync.Mutex
	// lastWrite is the time of the last write.
	lastWrite time.Time
	// tail is the tail of the output.
	tail []byte
	// truncated indicates that output has been discarded from the start of
	// tail.
	truncated bool
}

// newGitOutputRecorder creates a new recorder. The time of its creation counts
// as the time of the last write, so it should be created when a command starts.
func newGitOutputRecorder() *gitOutputRecorder {
	return &gitOutputRecorder{lastWrite: time.Now()}
}

// Write implements io.Writer.Write.
func (r *gitOutputRecorder) Write(data []byte) (int, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.lastWrite = time.Now()
	r.tail = append(r.tail, data...)

	// Discard old output, allowing the buffer to grow a bit so that we don't
	// copy its contents for each write.
	if len(r.tail) > 2*gitOutputTailSize {
		r.tail = append(r.tail[:0], r.tail[len(r.tail)-gitOutputTailSize:]...)
		r.truncated = true
	}
	return len(data), nil
}

// idle returns the amount of time since the last write.
func (r *gitOutputRecorder) idle() time.Duration {
	r.lock.Lock()
	defer r.lock.Unlock()
	return time.Since(r.lastWrite)
}

// message returns a summary of the output for use in an error message.
func (r *gitOutputRecorder) message() string {
	r.lock.Lock()
	defer r.lock.Unlock()

	// Split the output into lines, treating carriage returns as line endings,
	// since that's how progress is overwritten. If the start of the output has
	// been discarded, then the first line is only part of a line, and it must
	// not be used, because if it's part of a URL, then we wouldn't be able to
	// recognize the credentials in it (see gitScrubCredentials).
	lines := strings.FieldsFunc(string(r.tail), func(character rune) bool {
		return character == '\n' || character == '\r'
	})
	if r.truncated && len(lines) > 0 {
		lines = lines[1:]
	}

	// Remove progress lines and hints, which only take up space.
	var relevant []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "hint:") && !gitProgressLine.MatchString(line) {
			relevant = append(relevant, trimmed)
		}
	}

	// Use the most recent lines, removing credentials before limiting the size
	// for the same reason as above, and ensuring that the result is valid
	// UTF-8, since it will be transmitted using Protocol Buffers.
	if len(relevant) > gitMaximumMessageLines {
		relevant = relevant[len(relevant)-gitMaximumMessageLines:]
	}
	message := gitScrubCredentials(strings.Join(relevant, "; "))
	if len(message) > gitMaximumMessageSize {
		message = message[:gitMaximumMessageSize] + "..."
	}
	return strings.ToValidUTF8(message, "�")
}

// gitRun runs a Git command that doesn't use the network and returns its
// standard output, without any trailing line endings. Any error is a
// *gitCommandError.
func gitRun(ctx context.Context, directory string, args ...string) (string, error) {
	command := gitCommand(ctx, directory, args...)
	var stdout bytes.Buffer
	stderr := newGitOutputRecorder()
	command.Stdout, command.Stderr = &stdout, stderr
	if err := command.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return "", &gitCommandError{err: err, message: stderr.message()}
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}

// gitStripCredentials removes credentials from a URL. Git's own configuration
// and the URLs of remotes can contain these (e.g.
// https://user:token@host/path), and the result is what's safe to send to the
// other endpoint, store in archives, and include in logs and messages.
//
// HTTP and HTTPS URLs lose all of their user information, along with their
// query and fragment, which is another place where tokens are found, though Git
// doesn't otherwise treat them specially. URLs that use SSH keep the user,
// because it's usually part of what identifies the repository (e.g.
// git@host:path) rather than a secret, while authentication happens by other
// means. They do lose any password. Other transports lose all of their user
// information. Scp-style URLs (user@host:path) can't include a password, and
// paths have nothing to remove.
func gitStripCredentials(url string) string {
	// Handle the syntax for a remote helper (e.g. helper::https://host/path) by
	// applying the removal to its address.
	if helper, address, ok := strings.Cut(url, "::"); ok && !strings.ContainsAny(helper, "/:@") {
		return helper + "::" + gitStripCredentials(address)
	}

	// Anything without a scheme doesn't have an authority to remove them from.
	scheme, remainder, ok := strings.Cut(url, "://")
	if !ok {
		return url
	}
	scheme = strings.ToLower(scheme)

	// Split the authority from what follows it, which is the same way that Git
	// recognizes credentials.
	authorityEnd := strings.IndexAny(remainder, "/?#")
	if authorityEnd == -1 {
		authorityEnd = len(remainder)
	}
	authority, rest := remainder[:authorityEnd], remainder[authorityEnd:]

	// Remove the appropriate portion of the user information.
	if at := strings.LastIndexByte(authority, '@'); at != -1 {
		userInformation, host := authority[:at], authority[at+1:]
		authority = host
		if scheme == "ssh" || scheme == "git+ssh" || scheme == "ssh+git" {
			if user, _, _ := strings.Cut(userInformation, ":"); user != "" {
				authority = user + "@" + host
			}
		}
	}

	// Remove any query and fragment from web URLs.
	if scheme == "http" || scheme == "https" {
		if end := strings.IndexAny(rest, "?#"); end != -1 {
			rest = rest[:end]
		}
	}

	// Done.
	return url[:len(scheme)] + "://" + authority + rest
}

// gitScrubCredentials removes credentials from the URLs in arbitrary text, such
// as Git's output. Git tries to do this itself in its messages, but not in all
// of them, and not in those of the helpers and servers that it relays.
func gitScrubCredentials(text string) string {
	return gitURLInText.ReplaceAllStringFunc(text, gitStripCredentials)
}

// gitUsableURL verifies that a URL can be given to Git and that it means the
// same thing regardless of where it's used, and returns it without credentials.
// Since it can't be used, an unusable URL's only place is in the returned
// error, which must not include it unless its credentials have been removed.
func gitUsableURL(url string) (string, error) {
	// Verify that the URL can be included in an entry, which is transmitted
	// using Protocol Buffers and which requires valid UTF-8, and that it
	// doesn't contain line breaks and the like, which Git is wary of as well.
	if !utf8.ValidString(url) || strings.IndexFunc(url, unicode.IsControl) != -1 {
		return "", errors.New("URL contains invalid characters")
	}

	// Refuse anything that Git could interpret as an option, even though we
	// separate URLs from options. It would have been refused by Git itself.
	stripped := gitStripCredentials(url)
	if strings.HasPrefix(url, "-") {
		return "", fmt.Errorf("URL %q starts with a dash", stripped)
	}

	// Without a scheme or a remote helper, the URL is a path or in scp-style.
	// Git recognizes the latter by a colon before the first slash, and a
	// relative path wouldn't mean anything on the other endpoint.
	if !strings.Contains(url, "://") && !strings.Contains(url, "::") && !filepath.IsAbs(url) && !strings.HasPrefix(url, "/") {
		colon := strings.IndexByte(url, ':')
		separator := strings.IndexAny(url, `/\`)
		if colon == -1 || (separator != -1 && separator < colon) {
			return "", fmt.Errorf("URL %q is relative", stripped)
		}

		// Git would treat something like user:password@host:path as a path on a
		// host named user, so it wouldn't work anyway, but it's easy to write
		// by mistake, and the result would include what's probably a password.
		beforeColon, afterColon := url[:colon], url[colon+1:]
		if at := strings.IndexByte(afterColon, '@'); at != -1 && !strings.Contains(beforeColon, "@") &&
			!strings.Contains(afterColon[:at], "/") && strings.Contains(afterColon[at:], ":") {
			return "", errors.New("URL contains what appears to be a password")
		}
	}

	// Success.
	return stripped, nil
}

// gitCommitFromOutput converts the output of a command that printed the
// identifier of a commit to the identifier's raw form, which is 20 bytes long
// in SHA-1 repositories and 32 bytes long in SHA-256 repositories.
func gitCommitFromOutput(output string) ([]byte, error) {
	commit, err := hex.DecodeString(strings.TrimSpace(output))
	if err != nil {
		return nil, fmt.Errorf("Git reported an invalid commit identifier: %w", err)
	} else if len(commit) != 20 && len(commit) != 32 {
		return nil, fmt.Errorf("Git reported a commit identifier with unexpected length: %d", len(commit))
	}
	return commit, nil
}

// gitShortCommit returns the abbreviated form of a commit's hexadecimal
// identifier for use in messages.
func gitShortCommit(commit string) string {
	return commit[:min(len(commit), gitShortCommitLength)]
}

// gitStallError indicates that a command was stopped because it stalled, or
// that it wasn't started because of a recent stall. In either case, there's no
// point in trying anything else that uses the same URL.
type gitStallError struct {
	// message is the error message.
	message string
}

// Error implements error.Error.
func (e *gitStallError) Error() string {
	return e.message
}

// gitProvider implements core.GitProvider by running Git. It's safe for
// concurrent usage. Stalls are tracked here rather than in the core package
// because they have to outlive individual calls to core.Transition.
type gitProvider struct {
	// stallTimeout is the amount of time that a command that uses the network
	// can run without producing any output.
	stallTimeout time.Duration
	// stallBackoff is the amount of time for which a URL won't be contacted
	// after a command that used it has stalled.
	stallBackoff time.Duration
	// logger is the underlying logger.
	logger *logging.Logger
	// backoffsLock serializes access to backoffs.
	backoffsLock sync.Mutex
	// backoffs maps URLs that have stalled to the time until which they won't
	// be contacted.
	backoffs map[string]time.Time
}

// newGitProvider creates a new provider.
func newGitProvider(logger *logging.Logger) *gitProvider {
	return &gitProvider{
		stallTimeout: gitDefaultStallTimeout,
		stallBackoff: gitDefaultStallBackoff,
		logger:       logger,
		backoffs:     make(map[string]time.Time),
	}
}

// backoffRemaining returns the amount of time until a URL can be contacted
// again, which is zero or less if it can be contacted now.
func (g *gitProvider) backoffRemaining(url string) time.Duration {
	g.backoffsLock.Lock()
	defer g.backoffsLock.Unlock()
	until, ok := g.backoffs[url]
	if !ok {
		return 0
	}
	remaining := time.Until(until)
	if remaining <= 0 {
		delete(g.backoffs, url)
	}
	return remaining
}

// runNetwork runs a Git command that may contact the specified URL, stopping it
// if it stalls, which is the only failure that results in the URL not being
// contacted for a while. Other failures, such as a missing commit or a failure
// to authenticate, are worth retrying right away, because they don't cost much
// time and because the commit may be pushed or the credentials fixed at any
// moment. The command must be one that reports its progress.
func (g *gitProvider) runNetwork(ctx context.Context, directory, url string, args ...string) error {
	// If this URL has stalled recently, then don't contact it.
	if remaining := g.backoffRemaining(url); remaining > 0 {
		return &gitStallError{fmt.Sprintf(
			"not contacting %s, because it showed no progress recently; it will be tried again in %s",
			url, remaining.Round(time.Second),
		)}
	}

	// Start the command in a context that we can cancel if it stalls.
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := gitCommand(commandCtx, directory, args...)
	output := newGitOutputRecorder()
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		return &gitCommandError{err: err}
	}

	// Watch for the command to stop producing output. We wait for this
	// Goroutine to exit before we look at its result.
	stalled := make(chan bool, 1)
	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(g.stallTimeout)
		defer timer.Stop()
		for {
			select {
			case <-done:
				stalled <- false
				return
			case <-timer.C:
				idle := output.idle()
				if idle >= g.stallTimeout {
					stalled <- true
					cancel()
					return
				}
				timer.Reset(g.stallTimeout - idle)
			}
		}
	}()

	// Wait for the command, then for the Goroutine.
	err := command.Wait()
	close(done)
	didStall := <-stalled

	// If the command succeeded, then it doesn't matter what else happened. This
	// includes helper processes outliving the command and keeping its output
	// open.
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}

	// If the command stalled, then avoid this URL for a while.
	if didStall {
		g.backoffsLock.Lock()
		g.backoffs[url] = time.Now().Add(g.stallBackoff)
		g.backoffsLock.Unlock()
		g.logger.Debugf("Git command for %s stalled, avoiding it for %s", url, g.stallBackoff)
		return &gitStallError{fmt.Sprintf(
			"stopped because %s showed no progress for %s; it won't be tried again for %s",
			url, g.stallTimeout, g.stallBackoff,
		)}
	}

	// Otherwise, it's a normal failure.
	return &gitCommandError{err: err, message: output.message()}
}

// hasCommit determines whether or not the repository in a directory has a
// commit, specified by its hexadecimal identifier.
func (g *gitProvider) hasCommit(ctx context.Context, directory, commit string) bool {
	_, err := gitRun(ctx, directory, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// fetchCommit fetches a commit, specified by its hexadecimal identifier, into
// the repository in a directory. The remote is what Git is told to fetch from,
// which is either the name of a remote or a URL, and the URL is what it refers
// to, which must not have credentials. Nothing but objects is added to the
// repository: no references change, and FETCH_HEAD isn't written.
func (g *gitProvider) fetchCommit(ctx context.Context, directory, remote, url, commit string) error {
	// Try to fetch only the commit that we need.
	err := g.runNetwork(ctx, directory, url, "fetch", "--no-write-fetch-head", "--progress", "--", remote, commit)
	if err == nil {
		return nil
	}

	// Stalls and cancellation aren't going to be any different the second time.
	var stall *gitStallError
	if errors.As(err, &stall) {
		return err
	} else if ctx.Err() != nil {
		return ctx.Err()
	}

	// Not all servers allow commits to be requested by their identifiers, in
	// which case the commit might still be reachable from what they do provide,
	// so see if we can get it that way. If we're fetching from a URL rather
	// than a remote, then there's no configuration to indicate what to fetch,
	// and Git will only fetch the repository's default branch.
	err = g.runNetwork(ctx, directory, url, "fetch", "--no-write-fetch-head", "--progress", "--", remote)
	if errors.As(err, &stall) {
		return err
	} else if ctx.Err() != nil {
		return ctx.Err()
	} else if err != nil {
		return fmt.Errorf("unable to fetch commit %s from %s: %w", gitShortCommit(commit), url, err)
	}

	// The fetch has succeeded, so we can tell if the commit is available.
	if !g.hasCommit(ctx, directory, commit) {
		return fmt.Errorf("commit %s isn't available from %s; if it hasn't been pushed, push it, and the next synchronization will fetch it", gitShortCommit(commit), url)
	}
	return nil
}

// checkout checks out a commit, specified by its hexadecimal identifier, on a
// detached HEAD in the repository in a directory. It leaves any conflicting
// local changes for Git to refuse.
func (g *gitProvider) checkout(ctx context.Context, directory, commit string) error {
	if _, err := gitRun(ctx, directory, "checkout", "--detach", commit); err != nil {
		return fmt.Errorf("unable to check out commit %s: %w", gitShortCommit(commit), err)
	}
	return nil
}

// Probe implements core.GitProvider.Probe.
func (g *gitProvider) Probe(ctx context.Context, directory string) ([]byte, string, error) {
	// Bound the duration of the commands.
	ctx, cancel := context.WithTimeout(ctx, gitProbeTimeout)
	defer cancel()

	// Determine whether this is a usable repository whose working tree is at
	// this directory. Anything else (such as a bare repository in the .git
	// entry, or a repository that's configured to have its working tree
	// elsewhere) can't be checked out by Mutagen, and checking one out would
	// change something else.
	output, err := gitRun(ctx, directory, "rev-parse", "--is-inside-work-tree", "--show-prefix")
	if err != nil {
		return nil, "", gitProbeError(ctx, directory, err)
	} else if output != "true" {
		return nil, "", errors.New("contains a .git entry for a repository whose working tree isn't this directory; remove the directory, or add it to .gitignore to leave it out")
	}

	// Determine the commit that's checked out. Failure with an exit code of one
	// (which is how --verify --quiet reports a failure) means that HEAD doesn't
	// resolve to a commit. That's expected if there aren't any commits yet, but
	// otherwise it refers to one that the repository doesn't have.
	output, err = gitRun(ctx, directory, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if gitExitedWith(err, 1) {
		if _, err := gitRun(ctx, directory, "rev-parse", "--verify", "--quiet", "HEAD"); gitExitedWith(err, 1) {
			return nil, "", nil
		} else if err != nil {
			return nil, "", gitProbeError(ctx, directory, err)
		}
		return nil, "", errors.New("contains a .git entry for a damaged repository, whose HEAD refers to a commit that it doesn't have; repair it, or remove the directory")
	} else if err != nil {
		return nil, "", gitProbeError(ctx, directory, err)
	}
	commit, err := gitCommitFromOutput(output)
	if err != nil {
		return nil, "", err
	}

	// Determine the URL. We look at the configuration, rather than asking Git
	// for the remote's URL, because the latter would apply this system's
	// insteadOf rewrites, while the other endpoint should apply its own. A key
	// that isn't found results in an exit code of one. Without an origin
	// there's nothing to fetch from, so it can't be a submodule.
	url, err := gitRun(ctx, directory, "config", "--get", "remote.origin.url")
	if gitExitedWith(err, 1) || (err == nil && url == "") {
		return nil, "", nil
	} else if err != nil {
		return nil, "", gitProbeError(ctx, directory, err)
	}
	url, err = gitUsableURL(url)
	if err != nil {
		return nil, "", fmt.Errorf("origin can't be used by the other endpoint: %w; change it with git remote set-url, or add the directory to .gitignore to leave it out", err)
	}

	// Success.
	return commit, url, nil
}

// gitProbeError converts the failure of a command run by a probe to an error
// whose message tells the user what to do.
func gitProbeError(ctx context.Context, directory string, err error) error {
	// Handle the case where we've been unable to get an answer.
	if ctx.Err() != nil {
		return fmt.Errorf("unable to examine repository: %w", ctx.Err())
	}

	// Handle the case where Git isn't available.
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("contains a .git entry, but git isn't installed on this system, and it's required for submodules; install it, or add the directory to .gitignore to leave it out")
	}

	// Look for the failures that we know about. Git's output is in the C
	// locale. We deliberately don't override Git's refusal to operate on a
	// repository owned by someone else, since it protects against running what
	// such a repository has configured. Thus, this has to be the user's
	// decision.
	message := err.Error()
	if strings.Contains(message, "dubious ownership") {
		return fmt.Errorf("Git refuses to use this repository, because it's owned by someone else; if you trust it, run: git config --global --add safe.directory %s", directory)
	} else if strings.Contains(message, "unknown option") {
		return fmt.Errorf("the version of Git on this system is too old for submodules, which require Git 2.29 or later: %s", message)
	}

	// Otherwise, it's not a repository, despite having a .git entry. This is
	// what a .git file looks like that was synchronized here from a submodule,
	// by a session without Git mode, and that points to nothing on this system.
	// Removing the directory is the only way to get the session to fill it in,
	// and that's not something that we can do for the user, since there's no
	// way to tell what its content is. Note that what this reports can't be the
	// state of a repository enclosing this one: gitCommand prevents that.
	return fmt.Errorf("contains a .git entry that isn't a valid repository (%s), such as one left by an older session; remove the directory, and the session will fill it in", message)
}

// Head implements core.GitProvider.Head.
func (g *gitProvider) Head(ctx context.Context, directory string) ([]byte, error) {
	output, err := gitRun(ctx, directory, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	return gitCommitFromOutput(output)
}

// Clone implements core.GitProvider.Clone.
func (g *gitProvider) Clone(ctx context.Context, directory, url string, commit []byte) error {
	// Verify that the URL can be used and convert the commit.
	url, err := gitUsableURL(url)
	if err != nil {
		return fmt.Errorf("unable to clone submodule: %w", err)
	}
	identifier := hex.EncodeToString(commit)

	// Clone the repository without checking anything out, since what the
	// default branch has checked out isn't what we're after. If we've been
	// given a URL with credentials, then we don't use them, though nothing that
	// we report would.
	if err := g.runNetwork(ctx, directory, url, "clone", "--no-checkout", "--progress", "--", url, "."); err != nil {
		var stall *gitStallError
		if errors.As(err, &stall) || ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("unable to clone %s: %w", url, err)
	}

	// The commit might not be reachable from any branch, in which case it may
	// still be possible to request it.
	if !g.hasCommit(ctx, directory, identifier) {
		if err := g.fetchCommit(ctx, directory, "origin", url, identifier); err != nil {
			return err
		}
	}

	// Check out the commit.
	return g.checkout(ctx, directory, identifier)
}

// Update implements core.GitProvider.Update.
func (g *gitProvider) Update(ctx context.Context, directory, url string, commit []byte) error {
	// Fetch the commit if it's missing.
	identifier := hex.EncodeToString(commit)
	if !g.hasCommit(ctx, directory, identifier) {
		if err := g.fetchMissing(ctx, directory, url, identifier); err != nil {
			return err
		}
	}

	// Check out the commit.
	return g.checkout(ctx, directory, identifier)
}

// fetchMissing fetches a commit, specified by its hexadecimal identifier, into
// the repository in a directory, from its own origin and then from a URL, which
// is the one that the other endpoint has for the repository, since they may
// reach it in different ways. If both attempts are unsuccessful, then the error
// describes both.
func (g *gitProvider) fetchMissing(ctx context.Context, directory, url, commit string) error {
	var failures []string

	// Try the repository's own origin. It's okay if there's no longer an
	// origin, because we have another URL to try. We refer to the origin by its
	// name, which allows Git to use whatever credentials are in its URL, but we
	// only refer to it without them, and backoffs for it are shared with all of
	// its other uses.
	var originURL string
	if origin, err := gitRun(ctx, directory, "config", "--get", "remote.origin.url"); err == nil && origin != "" {
		originURL = gitStripCredentials(origin)
		err := g.fetchCommit(ctx, directory, "origin", originURL, commit)
		if err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		failures = append(failures, err.Error())
	}

	// Try the other endpoint's URL, unless it's the same one that just failed.
	usable, err := gitUsableURL(url)
	if err != nil {
		failures = append(failures, fmt.Sprintf("unable to use the other endpoint's URL: %v", err))
	} else if usable != originURL {
		err := g.fetchCommit(ctx, directory, usable, usable, commit)
		if err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		failures = append(failures, err.Error())
	}

	// Report the failures.
	return errors.New(strings.Join(failures, "; "))
}

// UpdateNested implements core.GitProvider.UpdateNested.
func (g *gitProvider) UpdateNested(ctx context.Context, directory, url string) error {
	// Without a .gitmodules file, there are no nested submodules. Git would
	// figure that out for itself, but there's no need to run it for what's the
	// most common case by far.
	if _, err := os.Lstat(filepath.Join(directory, ".gitmodules")); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("unable to query .gitmodules: %w", err)
	}

	// Update them. We don't know which URLs this will use, because they're
	// specified by the repository's content, so we track backoff for them as a
	// group. This has a different key than the repository's own URL, which has
	// shown that it works, and which we must not avoid as a result of this.
	description := "the submodules nested in " + gitStripCredentials(url)
	return g.runNetwork(ctx, directory, description, "submodule", "update", "--init", "--recursive", "--progress")
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
	warnings = append(warnings, e.dirtySubmoduleWarnings(ctx, snapshot)...)
	return warnings
}

// dirtySubmoduleWarnings computes warnings about submodules with uncommitted
// changes, which aren't synchronized, because the other endpoint only gets the
// commit. There's one warning for each such submodule, up to a maximum, with
// any remainder being covered by a single summary warning.
//
// Checking the status of a repository would normally update its index, which is
// inside the directory that's being watched, but gitCommand prevents that,
// because optional locks are what allow this kind of update.
func (e *endpoint) dirtySubmoduleWarnings(ctx context.Context, snapshot *core.Snapshot) []*core.Problem {
	// Find submodules, which is usually the end of it.
	paths := snapshot.Content.GitLinkPaths()
	if len(paths) == 0 {
		return nil
	}

	// Bound the duration of the commands.
	ctx, cancel := context.WithTimeout(ctx, gitWarningTimeout)
	defer cancel()

	// Check each submodule.
	var warnings []*core.Problem
	for _, path := range paths {
		status, err := gitRun(ctx, filepath.Join(e.root, filepath.FromSlash(path)), "status", "--porcelain")
		if err != nil {
			e.logger.Debug("Unable to determine the status of submodule:", err)
		} else if strings.TrimSpace(status) != "" {
			warnings = append(warnings, &core.Problem{
				Path:    path,
				Error:   "submodule has uncommitted changes, which aren't synchronized; commit and push them to synchronize them",
				Warning: true,
			})
		}
	}

	// Sort the warnings and limit them in the same way as for tracked files.
	core.SortProblems(warnings)
	if len(warnings) > maximumDirtySubmoduleWarnings+1 {
		omitted := len(warnings) - maximumDirtySubmoduleWarnings
		warnings = warnings[:maximumDirtySubmoduleWarnings+1]
		warnings[maximumDirtySubmoduleWarnings] = &core.Problem{
			Path: warnings[maximumDirtySubmoduleWarnings].Path,
			Error: fmt.Sprintf(
				"this and %d more submodules after it have uncommitted changes, which aren't synchronized; commit and push them to synchronize them",
				omitted-1,
			),
			Warning: true,
		}
	}
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
