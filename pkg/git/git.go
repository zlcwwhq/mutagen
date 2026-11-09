package git

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
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
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
	// probeTimeout is the maximum amount of time that the Git commands of a
	// single probe can take. These commands only read a few files, so this only
	// matters on a filesystem that's stopped responding, but scans hold the
	// scan lock, and they shouldn't be able to do so indefinitely because of
	// that.
	probeTimeout = 30 * time.Second
	// outputTailSize is the amount of a command's output that's retained for
	// use in error messages. It only has to be large enough for what comes
	// after the progress output, which is where Git reports failures.
	outputTailSize = 16 * 1024
	// maximumMessageLines is the maximum number of lines of a command's
	// output that are included in an error message.
	maximumMessageLines = 8
	// maximumMessageSize is the maximum size, in bytes, of the portion of an
	// error message that comes from a command's output.
	maximumMessageSize = 1024
	// defaultAllowedProtocols is the value of GIT_ALLOW_PROTOCOL that
	// NetworkCommand uses if it isn't already set. It permits the transports
	// that reach remote hosts, but not the file and ext transports, which can
	// name local paths or execute arbitrary programs and thus have no business
	// being selected by a URL found in a repository.
	defaultAllowedProtocols = "https:http:ssh:git"
)

// progressLine matches the lines of Git's output that merely report progress
// and that aren't of any use in an error message. These lines come from Git and
// from the remote side (with a prefix), and Git's output is in the C locale.
var progressLine = regexp.MustCompile(
	`^(remote: )?(Enumerating objects|Counting objects|Compressing objects|Total \d|Receiving objects|Resolving deltas|Updating files|Filtering content)|` +
		`^(Cloning into|Fetching submodule|From |Unpacking objects)|` +
		`^ [ *+=!-] `,
)

// urlInText matches the URLs in text, such as in Git's output.
var urlInText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"]+`)

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

// promptVariables lists the environment variables that name programs that
// Git and the programs that it runs (ssh and credential helpers) use to ask a
// person for credentials, or that control whether they may do so. They're
// replaced in the environment of Git commands (see environment), since a
// prompt from a command that Mutagen runs in the background could only appear
// as an unexpected dialog, and it would block the command until it timed out.
var promptVariables = []string{
	"GIT_TERMINAL_PROMPT",
	"GIT_ASKPASS",
	"SSH_ASKPASS_REQUIRE",
	"GCM_INTERACTIVE",
}

// ceilingError indicates that Git can't be confined to a directory, because
// the path of the directory's parent contains the path list separator.
type ceilingError struct {
	// directory is the directory.
	directory string
}

// Error implements error.Error.
func (e *ceilingError) Error() string {
	return "unable to confine Git to " + e.directory +
		", because the path of its parent contains the path list separator (" +
		string(filepath.ListSeparator) + ")"
}

// environment computes the environment of a Git command run in the
// specified directory, which is based on that of the current process, and
// which depends on whether or not the command may use the network. See
// Command and NetworkCommand for details. It fails if the directory can't be
// confined.
func environment(directory string, network bool) ([]string, error) {
	// Prevent Git from searching the parent of the directory for a repository.
	// Git only treats a directory as a repository if it contains a valid Git
	// directory, so a directory whose .git entry is invalid (such as an empty
	// directory left behind by a failed operation) would otherwise lead Git to
	// silently operate on any repository that encloses it. Ceiling directories
	// must be absolute, and the directory is allowed to be relative to the
	// current process's working directory, as Cmd.Dir is.
	// Git compares the ceiling with the physical path of its working
	// directory, so if the directory is (or is within) a symbolic link, then
	// the ceiling has to be the parent of its target, or it wouldn't be one.
	if absolute, err := filepath.Abs(directory); err == nil {
		directory = absolute
	}
	if resolved, err := filepath.EvalSymlinks(directory); err == nil {
		directory = resolved
	}
	ceiling := filepath.Dir(directory)

	// Git splits GIT_CEILING_DIRECTORIES at the path list separator, without
	// any way to escape it, so a ceiling that contains one would silently be
	// ignored, and discovery could reach an enclosing repository. Such paths
	// are rare enough that refusing them is better than running unconfined.
	if strings.ContainsRune(ceiling, filepath.ListSeparator) {
		return nil, &ceilingError{directory: directory}
	}

	// Copy the current environment, omitting variables that are either removed
	// or overridden below. If the caller has chosen the protocols that Git may
	// use for a command that uses the network, then respect their choice,
	// including for tests that need to enable the file transport.
	current := os.Environ()
	result := make([]string, 0, len(current)+8)
	allowProtocolSet := false
	for _, entry := range current {
		name, _, _ := strings.Cut(entry, "=")
		if sameVariable(name, "GIT_ALLOW_PROTOCOL") {
			allowProtocolSet = true
		}
		omit := sameVariable(name, "LC_ALL") ||
			sameVariable(name, "GIT_CEILING_DIRECTORIES") ||
			(!network && sameVariable(name, "GIT_ALLOW_PROTOCOL")) ||
			(!network && sameVariable(name, "GIT_NO_LAZY_FETCH"))
		for _, variable := range inheritedVariables {
			omit = omit || sameVariable(name, variable)
		}
		for _, variable := range promptVariables {
			omit = omit || sameVariable(name, variable)
		}
		if !omit {
			result = append(result, entry)
		}
	}

	// Never let Git wait for a person to answer a prompt for credentials, and
	// make its output independent of the user's locale, so that it can be
	// parsed and its messages can be recognized. GIT_TERMINAL_PROMPT only
	// covers the terminal: Git would still run an askpass program from
	// GIT_ASKPASS, core.askPass, or SSH_ASKPASS, unless GIT_ASKPASS is set but
	// empty, which makes it fail instead. ssh runs SSH_ASKPASS for passphrases
	// and host keys when it has no terminal, unless SSH_ASKPASS_REQUIRE is
	// "never", and Git Credential Manager shows its own dialogs unless
	// GCM_INTERACTIVE is "never". Credentials that are available without asking
	// (such as from an agent or a credential store) are still used.
	result = append(result,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS_REQUIRE=never",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
		"GIT_CEILING_DIRECTORIES="+ceiling,
	)

	// A command that doesn't use the network mustn't start using it either.
	// In a partial clone (one made with --filter, which lacks objects that it
	// can fetch from a promisor remote, such as its origin), Git fetches
	// missing objects on demand (lazily) from within any command that needs
	// them, including those that only look at the repository, which would then
	// wait on the network, without the stall detection, throttling, and
	// backoff that commands that use the network deliberately get. That's
	// what GIT_NO_LAZY_FETCH prevents, but only from Git 2.45 on. Earlier
	// versions run a fetch in a child process, which inherits an empty
	// GIT_ALLOW_PROTOCOL, so that the fetch fails at once, without contacting
	// anything, because it may use none of the transports. Either way, the
	// command proceeds as if the objects didn't exist. Nothing else that
	// commands that don't use the network do involves a transport.
	if !network {
		result = append(result, "GIT_ALLOW_PROTOCOL=", "GIT_NO_LAZY_FETCH=1")
	} else if !allowProtocolSet {
		result = append(result, "GIT_ALLOW_PROTOCOL="+defaultAllowedProtocols)
	}
	return result, nil
}

// Command creates a git command that doesn't use the network, which runs in
// directory with the specified arguments, which follow Git's global options
// and thus begin with a subcommand, or with any further global options. The
// command is hardened for operating on repositories that Mutagen doesn't
// control, and which may be only partially present or controlled by another
// party:
//
//   - Repository discovery is confined to the directory: variables that
//     redirect Git to another repository are removed from the environment, and
//     the directory's parent is made a ceiling that discovery won't climb
//     into, so that an invalid .git entry in the directory makes commands fail
//     instead of operating on an enclosing repository. If the parent's path
//     can't be a ceiling, then the command fails to start. Git also doesn't
//     use a bare repository that it discovers rather than being told about,
//     since an invalid .git entry would otherwise let a directory's other
//     contents, which may have been synchronized from elsewhere, act as one,
//     along with its configuration.
//   - Git never prompts for credentials, never runs a file system monitor, and
//     never runs hooks, since all three would execute programs or block on
//     input that no one can supply, and it doesn't take optional locks, so
//     that commands which only read don't interfere with other Git processes
//     (or with the file watching that notices them).
//   - Git's output is in the C locale.
//   - Git can't use any transport, whatever the caller's environment says, so
//     it can't fetch the objects that a partial clone lacks on demand, which
//     it otherwise does from within any command that needs them. Commands
//     proceed as if the objects didn't exist.
//
// The command is stopped when ctx is canceled, along with its helper processes
// where the platform supports that, which includes giving them a moment to
// clean up, and waiting on the command covering that, where the platform
// supports that (see configureProcess). Waiting on it doesn't block for more
// than a few seconds after that on processes that are still running. The
// command is returned unstarted, so callers can set up its I/O.
func Command(ctx context.Context, directory string, args ...string) *exec.Cmd {
	return command(ctx, directory, false, args)
}

// NetworkCommand is like Command, but it creates a command that uses the
// network, such as one that clones or fetches, or one that runs filters that
// may (such as Git LFS's). It has the same hardening, except that Git is
// limited to the transports that GIT_ALLOW_PROTOCOL lists, which is a default
// list of those that reach remote hosts unless the caller's environment sets
// it, and that it fetches missing objects on demand.
func NetworkCommand(ctx context.Context, directory string, args ...string) *exec.Cmd {
	return command(ctx, directory, true, args)
}

// command implements Command and NetworkCommand.
func command(ctx context.Context, directory string, network bool, args []string) *exec.Cmd {
	// Prepend Git's global options. They must come before the subcommand.
	arguments := make([]string, 0, len(args)+7)
	arguments = append(arguments,
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath="+os.DevNull,
		"-c", "safe.bareRepository=explicit",
		"--no-optional-locks",
	)
	arguments = append(arguments, args...)

	// Create the command.
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = directory
	if environment, err := environment(directory, network); err != nil {
		if command.Err == nil {
			command.Err = err
		}
	} else {
		command.Env = environment
	}
	command.WaitDelay = waitDelay
	configureProcess(command)
	return command
}

// CommandError is the failure of a Git command.
type CommandError struct {
	// Err is the error from running the command.
	Err error
	// Output is a summary of the command's output, without credentials, which
	// may be empty.
	Output string
}

// Error implements error.Error.
func (e *CommandError) Error() string {
	if e.Output != "" {
		return e.Output
	}
	return scrubCredentials(e.Err.Error())
}

// Unwrap returns the error from running the command.
func (e *CommandError) Unwrap() error {
	return e.Err
}

// ExitedWith determines whether or not an error from Run indicates that
// the command ran and exited with the specified exit code. This is how Git
// answers questions (such as whether a configuration value exists) when it's
// able to, as opposed to failing to answer them.
func ExitedWith(err error, code int) bool {
	var exitError *exec.ExitError
	return errors.As(err, &exitError) && exitError.ExitCode() == code
}

// refusedTransport matches the message with which Git refuses to use a
// transport that GIT_ALLOW_PROTOCOL doesn't list.
var refusedTransport = regexp.MustCompile(`transport '[^']*' not allowed`)

// ExplainRefusedTransport returns an explanation for a message that includes
// Git's refusal to use a transport that GIT_ALLOW_PROTOCOL doesn't list, or an
// empty string if the message doesn't include one. Git's message only names
// the transport that it refused, such as "file" for a local path or a file URL,
// which the default list doesn't include (see Command), so it doesn't say what
// Git mode can clone and fetch from.
func ExplainRefusedTransport(message string) string {
	if !refusedTransport.MatchString(message) {
		return ""
	} else if allowed, ok := os.LookupEnv("GIT_ALLOW_PROTOCOL"); ok {
		return fmt.Sprintf("Git mode only clones and fetches submodules over the transports that GIT_ALLOW_PROTOCOL lists (%q)", allowed)
	}
	return "Git mode only clones and fetches submodules from https, http, ssh, and git URLs, not from local paths or file URLs"
}

// OutputRecorder is an io.Writer that retains the tail of a command's output
// for error messages and records when output last arrived, which is how stalled
// commands are detected. The same recorder can be used as both the standard
// output and standard error of a command, in which case they share a pipe.
type OutputRecorder struct {
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

// NewOutputRecorder creates a new recorder. The time of its creation counts
// as the time of the last write, so it should be created when a command starts.
func NewOutputRecorder() *OutputRecorder {
	return &OutputRecorder{lastWrite: time.Now()}
}

// Write implements io.Writer.Write.
func (r *OutputRecorder) Write(data []byte) (int, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.lastWrite = time.Now()
	r.tail = append(r.tail, data...)

	// Discard old output, allowing the buffer to grow a bit so that we don't
	// copy its contents for each write.
	if len(r.tail) > 2*outputTailSize {
		r.tail = append(r.tail[:0], r.tail[len(r.tail)-outputTailSize:]...)
		r.truncated = true
	}
	return len(data), nil
}

// Idle returns the amount of time since the last write.
func (r *OutputRecorder) Idle() time.Duration {
	r.lock.Lock()
	defer r.lock.Unlock()
	return time.Since(r.lastWrite)
}

// Message returns a summary of the output for use in an error message.
func (r *OutputRecorder) Message() string {
	r.lock.Lock()
	defer r.lock.Unlock()

	// Split the output into lines, treating carriage returns as line endings,
	// since that's how progress is overwritten. If the start of the output has
	// been discarded, then the first line is only part of a line, and it must
	// not be used, because if it's part of a URL, then we wouldn't be able to
	// recognize the credentials in it (see scrubCredentials).
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
		if trimmed != "" && !strings.HasPrefix(trimmed, "hint:") && !progressLine.MatchString(line) {
			relevant = append(relevant, trimmed)
		}
	}

	// Use the most recent lines, removing credentials before limiting the size
	// for the same reason as above, and ensuring that the result is valid
	// UTF-8, since it will be transmitted using Protocol Buffers.
	if len(relevant) > maximumMessageLines {
		relevant = relevant[len(relevant)-maximumMessageLines:]
	}
	message := scrubCredentials(strings.Join(relevant, "; "))
	if len(message) > maximumMessageSize {
		message = message[:maximumMessageSize] + "..."
	}
	return strings.ToValidUTF8(message, "�")
}

// Run runs a Git command that doesn't use the network (see Command) and
// returns its standard output, without any trailing line endings. Any error is
// a *CommandError.
func Run(ctx context.Context, directory string, args ...string) (string, error) {
	return run(Command(ctx, directory, args...))
}

// NetworkRun is like Run, but for a command that may use the network (see
// NetworkCommand).
func NetworkRun(ctx context.Context, directory string, args ...string) (string, error) {
	return run(NetworkCommand(ctx, directory, args...))
}

// run implements Run and NetworkRun.
func run(command *exec.Cmd) (string, error) {
	var stdout bytes.Buffer
	stderr := NewOutputRecorder()
	command.Stdout, command.Stderr = &stdout, stderr
	if err := command.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return "", &CommandError{Err: err, Output: stderr.Message()}
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}

// StripCredentials removes credentials from a URL. Git's own configuration
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
func StripCredentials(url string) string {
	// Handle the syntax for a remote helper (e.g. helper::https://host/path) by
	// applying the removal to its address.
	if helper, address, ok := strings.Cut(url, "::"); ok && !strings.ContainsAny(helper, "/:@") {
		return helper + "::" + StripCredentials(address)
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

// scrubCredentials removes credentials from the URLs in arbitrary text, such
// as Git's output. Git tries to do this itself in its messages, but not in all
// of them, and not in those of the helpers and servers that it relays.
func scrubCredentials(text string) string {
	return urlInText.ReplaceAllStringFunc(text, StripCredentials)
}

// UsableURL verifies that a URL can be given to Git and that it means the
// same thing regardless of where it's used, and returns it without credentials.
// Since it can't be used, an unusable URL's only place is in the returned
// error, which must not include it unless its credentials have been removed.
func UsableURL(url string) (string, error) {
	// Verify that the URL can be included in an entry, which is transmitted
	// using Protocol Buffers and which requires valid UTF-8, and that it
	// doesn't contain line breaks and the like, which Git is wary of as well.
	if !utf8.ValidString(url) || strings.IndexFunc(url, unicode.IsControl) != -1 {
		return "", errors.New("URL contains invalid characters")
	}

	// Refuse anything that Git could interpret as an option, even though we
	// separate URLs from options. It would have been refused by Git itself.
	stripped := StripCredentials(url)
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

// CommitFromOutput converts the output of a command that printed the
// identifier of a commit to the identifier's raw form, which is 20 bytes long
// in SHA-1 repositories and 32 bytes long in SHA-256 repositories.
func CommitFromOutput(output string) ([]byte, error) {
	commit, err := hex.DecodeString(strings.TrimSpace(output))
	if err != nil {
		return nil, fmt.Errorf("Git reported an invalid commit identifier: %w", err)
	} else if len(commit) != 20 && len(commit) != 32 {
		return nil, fmt.Errorf("Git reported a commit identifier with unexpected length: %d", len(commit))
	}
	return commit, nil
}

// Repository describes a repository that Probe has found to be a submodule.
type Repository struct {
	// Commit is the commit that's checked out, in its raw form.
	Commit []byte
	// URL is the URL of the repository's origin, without credentials.
	URL string
	// AutoCRLF is the value of the core.autocrlf setting that applies to the
	// repository, or empty if it's unset or unusable (see UsableSetting).
	AutoCRLF string
	// EOL is the value of the core.eol setting, in the same way.
	EOL string
}

// probeSettings is the pattern for the settings that Probe reads, all of which
// it reads with a single command.
const probeSettings = `^(remote\.origin\.url|core\.autocrlf|core\.eol)$`

// Probe examines a directory that contains a .git entry to determine how a
// session in Git mode synchronizes it. If its repository has a commit checked
// out and an origin that the other endpoint can use, then it's a submodule, and
// a description of it is returned. If the repository doesn't have any commits
// or an origin, then the result is nil, and the directory is an ordinary
// directory. An error means that this can't be determined, and its message
// tells the user what to do about it.
func Probe(ctx context.Context, directory string) (*Repository, error) {
	// Bound the duration of the commands.
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// Determine whether this is a usable repository whose working tree is at
	// this directory. Anything else (such as a bare repository in the .git
	// entry, or a repository that's configured to have its working tree
	// elsewhere) can't be used by Mutagen, and operating on one would change
	// something else.
	output, err := Run(ctx, directory, "rev-parse", "--is-inside-work-tree", "--show-prefix")
	if err != nil {
		return nil, probeError(ctx, directory, err)
	} else if output != "true" {
		return nil, errors.New("contains a .git entry for a repository whose working tree isn't this directory; fix or remove the .git entry (not the directory), or add the directory to .gitignore to leave it out")
	}

	// Determine the commit that's checked out. Failure with an exit code of one
	// (which is how --verify --quiet reports a failure) means that HEAD doesn't
	// resolve to a commit. That's expected if there aren't any commits yet, but
	// otherwise it refers to one that the repository doesn't have.
	output, err = Run(ctx, directory, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if ExitedWith(err, 1) {
		if _, err := Run(ctx, directory, "rev-parse", "--verify", "--quiet", "HEAD"); ExitedWith(err, 1) {
			return nil, nil
		} else if err != nil {
			return nil, probeError(ctx, directory, err)
		}
		return nil, errors.New("contains a .git entry for a damaged repository, whose HEAD refers to a commit that it doesn't have; repair it, or remove the .git entry (not the directory) to make the directory an ordinary one")
	} else if err != nil {
		return nil, probeError(ctx, directory, err)
	}
	commit, err := CommitFromOutput(output)
	if err != nil {
		return nil, err
	}

	// Read the URL and the line ending settings. We look at the configuration,
	// rather than asking Git for the remote's URL, because the latter would
	// apply this system's insteadOf rewrites, while the other endpoint should
	// apply its own. If no setting is found, then the exit code is one. Each
	// entry consists of the name, a newline, and the value (which is omitted,
	// along with the newline, for a boolean setting without a value), followed
	// by a null byte. The last value of a setting is the one that applies,
	// except for remote.origin.url, which can have several values: Git
	// fetches from the first, and the others are only used for pushing.
	output, err = Run(ctx, directory, "config", "-z", "--get-regexp", probeSettings)
	if err != nil && !ExitedWith(err, 1) {
		return nil, probeError(ctx, directory, err)
	}
	settings := make(map[string]string)
	for _, entry := range strings.Split(output, "\x00") {
		if name, value, ok := strings.Cut(entry, "\n"); ok {
			if _, ok := settings[name]; ok && name == "remote.origin.url" {
				continue
			}
			settings[name] = value
		} else if entry != "" {
			settings[entry] = "true"
		}
	}

	// Without an origin there's nothing to fetch from, so it can't be a
	// submodule.
	url := settings["remote.origin.url"]
	if url == "" {
		return nil, nil
	}
	url, err = UsableURL(url)
	if err != nil {
		return nil, fmt.Errorf("origin can't be used by the other endpoint: %w; change it with git remote set-url, or add the directory to .gitignore to leave it out", err)
	}

	// Success. Settings that can't be used are left out, which means that a
	// clone on the other endpoint uses its own settings.
	repository := &Repository{Commit: commit, URL: url}
	if value := settings["core.autocrlf"]; UsableSetting(value) {
		repository.AutoCRLF = value
	}
	if value := settings["core.eol"]; UsableSetting(value) {
		repository.EOL = value
	}
	return repository, nil
}

// maximumSettingLength is the maximum length of a setting's value that
// UsableSetting accepts.
const maximumSettingLength = 64

// UsableSetting determines whether or not the value of a setting that's read
// on one endpoint can be passed to Git on another endpoint (with --config):
// it's non-empty, short, valid UTF-8 (since it's transmitted using Protocol
// Buffers), and free of control characters. Git validates what it means.
func UsableSetting(value string) bool {
	return value != "" && len(value) <= maximumSettingLength && utf8.ValidString(value) &&
		strings.IndexFunc(value, unicode.IsControl) == -1
}

// probeError converts the failure of a command run by a probe to an error
// whose message tells the user what to do.
func probeError(ctx context.Context, directory string, err error) error {
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

	// Otherwise, Git can't use the repository, despite there being a .git
	// entry. This is what a .git file looks like that was synchronized here
	// from a submodule, by a session without Git mode, and that points to
	// nothing on this system, but it may also be a real repository that Git
	// can't read (such as one with invalid configuration, or one that uses an
	// extension that this version of Git doesn't support). So the advice is
	// never to remove the directory, which could lose commits and changes, or,
	// if the directory is synchronized, propagate its removal to the other
	// endpoint. Without its .git entry, the directory is an ordinary one, to
	// which the session attaches the repository of a submodule that the other
	// endpoint has there. Note that what this reports can't be the state of a
	// repository enclosing this one: Command prevents that.
	return fmt.Errorf("contains a .git entry that isn't a valid repository (%s), such as one left by an older session; repair it, or remove the .git entry (not the directory), after which the directory is synchronized like any other, and a submodule that the other endpoint has there is attached to it", message)
}
