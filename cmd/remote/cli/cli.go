// Package cli implements the remote command line, which runs a command on the
// opposite endpoint of the Mutagen synchronization session whose root contains
// the current directory. It is used by the standalone remote executable, which
// is installed alongside the Mutagen CLI on the daemon's system, and by agents,
// which act as the remote executable on endpoint systems when invoked by that
// name. Commands are routed through the daemon.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

const (
	// failureExitCode is the exit code for failures to run the command, which
	// matches the convention used by ssh.
	failureExitCode = 255
	// timeoutExitCode is the exit code for commands cancelled by --timeout,
	// which matches the convention used by timeout.
	timeoutExitCode = 124
)

// usage is the usage summary printed before the option list. It's written to
// be sufficient on its own for people and agents unfamiliar with remote.
const usage = `Usage: remote [options] <command> [<args>...]
       remote --flush [options]
       remote --diff [options] [<path>...]
       remote --conflicts [--json]
       remote --conflict-checkout [--base <file>] [--json] <path>...
       remote --conflict-apply [--take local|remote | --delete]
              [--executable true|false] [--json] <path>...
       remote --conflict-discard [--json] <path>...

Run a command on the opposite endpoint of the Mutagen synchronization session
whose root contains the current directory, in the corresponding directory
there. Standard input, output, error, and the exit status are forwarded.

Examples:
  remote pytest -x tests/
  remote sh -c 'make && ./bin/app'      (shell syntax needs an explicit shell)
  remote -e CONDA_ENV=torch python train.py
  remote --timeout 10m make bench
  remote --flush                        (only synchronize)
  remote --diff                         (show how conflicting files differ)
  remote --conflicts                    (list all conflicts)
  remote --conflict-checkout app.go     (download both copies for merging)
  remote --conflict-apply app.go        (write the merged file to both copies)

Notes:
  - No remote shell interprets the arguments, and your local shell expands
    variables, globs, and ~ before remote sees them. Use sh -c '...' for
    anything that should be expanded remotely.
  - Options must precede the command.
  - The session used is the one whose root most specifically contains the
    current directory. --session selects one by name or identifier instead,
    even if its roots don't contain the current directory: commands then run
    at the root of its opposite endpoint, or in --cwd (relative to that
    root), and require --cwd if another session's root contains the
    directory. An exported MUTAGEN_REMOTE_SESSION only selects among the
    sessions containing the current directory, and is ignored if none of
    them matches it; outside every root, it selects a session as --session
    does.
  - Sessions that are paused, disconnected, or don't run commands submitted
    from this side can't be found, so inside the root of such a session that
    is nested in another session's root, commands run through the enclosing
    session (in its copy of the directory), unless --session selects the
    nested session, which makes them fail instead. Once remote has located
    the session, it doesn't switch to another, even if that one disconnects.
  - The session is synchronized before the command, so that it sees local
    changes, and after it, so that its changes are present locally when remote
    exits. Failure to synchronize only produces a warning, as do conflicts
    (files modified on both endpoints, which stay unsynchronized until
    resolved) and other content that couldn't be synchronized.
  - remote --diff shows how the remote copies of the given paths, or of the
    conflicting files, differ from the local copies, as a unified diff
    (- local, + remote). For a single-file session, use --session from outside
    its root. Large conflict reports may omit changes and say how many;
    specify paths to compare omitted files. --timeout also covers file reads.
  - Conflicts can be resolved in two-way-safe and one-way-safe sessions.
    remote --conflicts synchronizes, lists every conflict, how it can be
    resolved, and through which endpoint it's checked out, flags checked-out
    copies that changed since checkout and partially applied resolutions, and
    lists workspaces whose conflicts are no longer listed, with their
    locations: unfinished resolutions, and workspaces with nothing applied,
    whose conflicts were resolved some other way or ended by a change after
    checkout. Conflict operations accept several paths, with options before
    them.
  - Before checking out, stop anything that writes the conflicting paths on
    either endpoint, and leave them unchanged until apply finishes; changing
    them meanwhile isn't supported. Apply fails without writing a copy that
    changed before it ran, but can overwrite changes made while it runs. If
    they change anyway, stop whatever changed them, then discard the
    workspace and check out again. A change that ends the conflict (such as
    reverting or deleting one copy, so that synchronization copies the other
    over it) may leave one side's changes only in the workspace: compare the
    current file with its local and remote files before discarding it, and
    restore what's needed (on alpha in one-way-safe sessions, which never
    copy beta's changes to alpha). Each conflict is checked out once, through
    one endpoint, which alone can apply or discard it: checking it out again,
    through either endpoint, fails until then. A local checkout may be
    another agent's or the user's resolution in progress, so apply or discard
    only checkouts that you made.
  - --conflict-checkout downloads both complete copies into a workspace (in
    the Mutagen data directory) and prints their paths. For two files, it also
    looks for their common ancestor (the file that both endpoints last agreed
    on) in Git's HEAD commit and index, or in the file given with --base,
    verified by its digest. With it, the merged file contains both sides'
    changes, and marks only regions changed differently on each side, showing
    the local, base, and remote versions; without it, every region where the
    copies differ is marked. If changes can't be merged reliably (such as a
    line that each side moved elsewhere), every difference is marked, without
    the base version. Review the merged file against the copies and the
    ancestor even if nothing is marked. Files too large to compare line by
    line or not valid UTF-8 are enclosed whole in one region, and binary
    copies get no merged file; checkout says which applies. Checkout fails
    if the copies changed since they were listed (the ancestor might not be
    theirs); check out again. To check out with --base after checking out
    without it, discard the workspace first. Conflicts involving directories
    require manual resolution; checkout and apply reject them.
  - For two files, --conflict-apply writes the merged file (or the copy chosen
    by --take, or deletion with --delete) to both copies, but refuses a merged
    file that still contains conflict markers, or one that's empty while
    neither copy is, which was probably truncated (such as by a command whose
    output was redirected to the file that it reads). For other conflicts
    (such as a symbolic link and a file), only --take is possible: it writes
    the copy taken, as checked out, to the other endpoint. Apply then
    synchronizes and checks that both copies match and no conflict or problem
    involves the path. The workspace, with the original copies, is then kept
    for 7 days (by the system clock, so a clock jump can delete it early) in
    case the resolution was wrong, as is one that --conflict-discard sets
    aside without changing either copy. Both release the checkout, so the
    conflict can be checked out again; --conflicts also releases checkouts
    whose workspaces are missing, such as after an interrupted checkout.
    Discard also sets aside a workspace that can't be read, which checkout
    and apply report.
  - Apply only replaces or deletes a copy that matches a checked-out copy
    (synchronization may copy one over the other), which the workspace keeps,
    or a result that it applied before, so it fails if either copy changed
    since checkout; then discard the workspace and check out again. Retry
    other failures rather than discarding, since a copy may already have the
    result, even if the conflict is no longer listed or the merged file was
    edited since. If nothing was applied and the conflict is no longer
    listed, apply refuses without changing either copy, since it was resolved
    some other way or ended by a change; compare the current file with the
    workspace's copies before discarding it. Copies differing in
    executability need --executable or --take. A conflict operation fails
    while another one on the same path is running on the same system. Writes
    of a copy wait for each other, since an apply that timed out may still be
    writing the remote copy, and apply fails without changing a copy that a
    later apply (through either endpoint) reached first; run --conflicts.
  - No terminal is allocated, so interactive programs don't work.
  - Commands are terminated when --timeout expires, when remote is
    interrupted, and when the session stops synchronizing (such as when it's
    paused or reconnects, or the daemon stops): their process group receives
    SIGTERM, then SIGKILL 5 seconds later if any of it remains (on Windows,
    the command is killed at once). If the session stops synchronizing,
    remote reports that the connection to the executing endpoint was lost
    and exits with status 255, without synchronizing afterwards.
  - /bin/sh sources .remote-prologue.sh in the executing endpoint's sync root
    if it exists, otherwise that endpoint's configured remotePrologue, before
    commands submitted from inside the root (even with --cwd). --no-prologue
    skips both. The workspace script replaces the configured script; an empty
    file does nothing, and deleting it restores the configured default. Edits
    take effect on the next command that sees them, without a session restart.
    The file follows normal sync rules; ignored files, failed flushes, and
    --no-flush can leave the executing copy outdated. Scripts must use POSIX
    shell syntax and need no executable permission. Failure prevents execution
    (status 126); prologues aren't supported on Windows.
  - Commands and prologues receive MUTAGEN_REMOTE_ENDPOINT=alpha or beta,
    identifying the executing endpoint. Use it to branch in a shared script.
    This overrides inherited values and those passed with -e or --env-file,
    even with --no-prologue or when submitting from outside the root.

Exit status is the command's own, or:
  255  not routed (no connected session with remote commands, or ambiguous),
       the connection was lost while the command ran, or remote --flush or a
       conflict operation failed
  127  command not found
  126  command couldn't be started, or the prologue failed
  124  --timeout expired before the command finished
With --diff, it's 0 if the copies are identical and 1 if they differ.

Options:
`

// options are the parsed command line options.
type options struct {
	conflictOptions
	// session selects a session by identifier or name.
	session string
	// defaultSession is the value of remote.SessionVariable if session wasn't
	// specified, which only selects among the sessions whose roots contain the
	// working directory, unless none does (see remote.Selection).
	defaultSession string
	// directory overrides the working directory.
	directory string
	// environment contains NAME=VALUE entries or names to forward.
	environment []string
	// environmentFiles contains paths to environment files.
	environmentFiles []string
	// timeout is the command timeout, or zero for none.
	timeout time.Duration
	// noPrologue indicates that the opposite endpoint's prologue should not be
	// sourced.
	noPrologue bool
	// flush indicates that synchronization was explicitly requested, which
	// allows omitting the command.
	flush bool
	// noFlush indicates that the session should not be synchronized before
	// and after the command.
	noFlush bool
	// diff indicates that local and remote copies of the paths specified by
	// arguments (or of conflicting files) should be compared.
	diff bool
	// help indicates that usage information was requested.
	help bool
	// version indicates that version information was requested.
	version bool
	// arguments are the command and its arguments, or the paths to compare.
	arguments []string
	// flags is the parsed flag set, used to print usage information.
	flags *pflag.FlagSet
}

// parse parses command line arguments, stopping at the first non-option
// argument so that the command's own options are passed through verbatim.
func parse(arguments []string) (*options, error) {
	result := &options{}
	flags := pflag.NewFlagSet("remote", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.SetInterspersed(false)
	flags.SortFlags = false
	// Names in backquotes are used as value placeholders in usage information.
	flags.StringVarP(&result.session, "session", "s", "", "Use the session with this `NAME` or identifier, which also allows running outside its root (see Notes for $MUTAGEN_REMOTE_SESSION)")
	flags.StringVarP(&result.directory, "cwd", "C", "", "Run in `DIR`: absolute and ~ paths are on the opposite endpoint, others are relative to the corresponding directory")
	flags.StringArrayVarP(&result.environment, "env", "e", nil, "Set `NAME[=VALUE]`, where a bare NAME forwards the local value (repeatable)")
	flags.StringArrayVar(&result.environmentFiles, "env-file", nil, "Read literal NAME=VALUE lines from `FILE` (repeatable)")
	flags.DurationVar(&result.timeout, "timeout", 0, "Exit after `DURATION`, such as 30s or 10m, terminating the command if it's running")
	flags.BoolVar(&result.noPrologue, "no-prologue", false, "Don't source the workspace or configured remote prologue")
	flags.BoolVar(&result.noFlush, "no-flush", false, "Don't synchronize the session before and after the command")
	flags.BoolVar(&result.flush, "flush", false, "Synchronize the session; without a command, do only that")
	flags.BoolVar(&result.diff, "diff", false, "Synchronize, then diff the given paths (or conflicting files) against their remote copies")
	result.conflictOptions.register(flags)
	flags.BoolVarP(&result.help, "help", "h", false, "Show this help")
	flags.BoolVar(&result.version, "version", false, "Show version information")
	if err := flags.Parse(arguments); err != nil {
		if err == pflag.ErrHelp {
			result.help = true
		} else {
			return nil, err
		}
	}
	result.arguments = flags.Args()
	result.flags = flags
	if !flags.Changed("session") {
		result.defaultSession = os.Getenv(remote.SessionVariable)
	}
	if result.timeout < 0 {
		return nil, errors.New("timeout must not be negative")
	} else if result.flush && result.noFlush {
		return nil, errors.New("--flush and --no-flush are mutually exclusive")
	} else if result.diff && (result.flush || result.noFlush) {
		return nil, errors.New("--diff can't be combined with --flush or --no-flush")
	} else if !result.help && !result.version && !result.flush && !result.diff && !result.active() && len(result.arguments) == 0 {
		return nil, errors.New("no command specified")
	}
	if err := result.validateConflicts(); err != nil {
		return nil, err
	}
	return result, nil
}

// Run executes the command line (excluding the executable name) and returns the
// exit code.
func Run(arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "remote:", err)
		return failureExitCode
	}

	// Parse options.
	options, err := parse(arguments)
	if err != nil {
		return fail(fmt.Errorf("%w (see remote --help)", err))
	} else if options.help {
		fmt.Fprint(stdout, usage, options.flags.FlagUsagesWrapped(80))
		return 0
	} else if options.version {
		fmt.Fprintln(stdout, mutagen.Version)
		return 0
	}

	// Prepare the request, if any. The working directory path, and whether the
	// prologue applies, are determined by the session root once the route is
	// located.
	var request *remote.Request
	if !options.diff && !options.active() && len(options.arguments) > 0 {
		environment, err := loadEnvironment(options.environmentFiles, options.environment)
		if err != nil {
			return fail(err)
		}
		request = &remote.Request{
			Arguments:   options.arguments,
			Environment: environment,
			Directory:   options.directory,
		}
		if err := request.EnsureValid(); err != nil {
			return fail(err)
		}
	}

	// Terminate the command on timeout or termination signals.
	ctx := context.Background()
	if options.timeout > 0 {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, options.timeout)
		defer cancelTimeout()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var signalLock sync.Mutex
	var received os.Signal
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case s := <-signals:
			signalLock.Lock()
			received = s
			signalLock.Unlock()
			cancel()
		case <-ctx.Done():
		}
	}()
	interrupted := func(activity string) (int, bool) {
		if ctx.Err() == nil {
			return 0, false
		}
		signalLock.Lock()
		defer signalLock.Unlock()
		if number, ok := received.(syscall.Signal); ok {
			return 128 + int(number), true
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			fmt.Fprintf(stderr, "remote: timed out after %v while %s\n", options.timeout, activity)
			return timeoutExitCode, true
		}
		return failureExitCode, true
	}
	warn := func(format string, arguments ...any) {
		fmt.Fprintf(stderr, "remote: warning: "+format+"\n", arguments...)
	}

	// Determine the working directory, which locates the session.
	directory, err := os.Getwd()
	if err != nil {
		return fail(fmt.Errorf("unable to determine working directory: %w", err))
	}

	if options.active() {
		err := runConflicts(ctx, directory, options, stdout, stderr)
		if code, ok := interrupted("resolving conflicts"); ok {
			return code
		} else if errors.Is(err, errReported) {
			return failureExitCode
		} else if err != nil {
			return fail(err)
		}
		return 0
	}

	// dial submits an operation through the session's endpoint. The first
	// submission locates the session, and later ones are pinned to the session
	// and endpoint that it located, failing rather than reaching a different
	// session if sessions connect or disconnect in between, such as a nested
	// session reconnecting after the enclosing session was synchronized.
	// Commands run in the directory corresponding to the working directory
	// unless --cwd is specified, so an explicitly selected session can't run
	// them at its root while another session synchronizes the directory.
	selection := &remote.Selection{
		Session:       options.session,
		Default:       options.defaultSession,
		Corresponding: request != nil && options.directory == "",
	}
	dial := func(operation remote.Operation) (net.Conn, *remote.Target, error) {
		connection, target, err := selection.Dial(ctx, directory, operation)
		if err == nil && selection.Target == nil {
			selection = &remote.Selection{Target: target}
		}
		return connection, target, err
	}

	// synchronize forces a synchronization cycle for the session and warns
	// about content that couldn't be synchronized, omitting warnings issued by
	// previous cycles. It returns the target if the session was located, along
	// with the report.
	var reports *reporter
	synchronize := func() (*remote.Target, *remote.Report, error) {
		connection, target, err := dial(remote.OperationFlush)
		if err != nil {
			return nil, nil, err
		}
		report, err := remote.Flush(ctx, connection)
		if err != nil {
			return target, nil, err
		} else if reports == nil {
			reports = newReporter(stderr, target)
		}
		reports.print(report)
		return target, report, nil
	}

	// Synchronize local changes before the command. Failure to locate the
	// session is fatal, since it would also prevent running the command, but
	// failure to synchronize isn't, since the command may still be useful.
	var target *remote.Target
	var report *remote.Report
	if !options.noFlush {
		target, report, err = synchronize()
		if interruptedCode, ok := interrupted("synchronizing"); ok {
			return interruptedCode
		} else if err != nil && (target == nil || request == nil) {
			return fail(err)
		} else if err != nil {
			warn("unable to synchronize before the command, which may see outdated files: %v", err)
		}
	}

	// Compare copies if requested.
	if options.diff {
		differ, err := compare(ctx, directory, target, report, options.arguments, stdout, stderr)
		if interruptedCode, ok := interrupted("comparing"); ok {
			return interruptedCode
		} else if err != nil {
			return fail(err)
		} else if differ {
			return 1
		}
		return 0
	} else if request == nil {
		return 0
	}

	// Locate the route and run the command.
	connection, target, err := dial(remote.OperationExecute)
	if interruptedCode, ok := interrupted("running the command"); ok {
		return interruptedCode
	} else if err != nil {
		return fail(err)
	}
	request.Path = target.Path
	request.Prologue = target.Contained && !options.noPrologue
	code, err := remote.Execute(ctx, connection, request, stdin, stdout, stderr)
	if interruptedCode, ok := interrupted("running the command"); ok {
		return interruptedCode
	} else if errors.Is(err, remote.ErrConnectionLost) {
		// The executing endpoint terminates the command when the connection is
		// lost, which happens whenever the session stops synchronizing, so
		// synchronizing afterwards isn't attempted.
		if options.noFlush {
			return fail(errors.New("the connection to the executing endpoint was lost (the session may have stopped synchronizing), so the command was terminated"))
		}
		return fail(errors.New("the connection to the executing endpoint was lost (the session may have stopped synchronizing), so the command was terminated and the session wasn't synchronized afterwards"))
	} else if err != nil {
		// Startup failures such as missing commands report conventional exit
		// codes (126 or 127) alongside the error.
		fmt.Fprintln(stderr, "remote:", err)
		if code != 0 {
			return code
		}
		return failureExitCode
	}

	// Synchronize the command's changes back. Once the command has finished,
	// its exit status takes precedence over failure to do so (including
	// timeouts), but not over signals.
	if !options.noFlush {
		if _, _, err := synchronize(); err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("timed out after %v", options.timeout)
			} else if interruptedCode, ok := interrupted("synchronizing"); ok {
				return interruptedCode
			}
			warn("unable to synchronize after the command, whose changes may not be present locally yet: %v", err)
		}
	}
	return code
}
