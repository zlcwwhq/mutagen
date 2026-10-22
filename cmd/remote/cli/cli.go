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
	"strconv"
	"strings"
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
	// sessions indicates that the sessions through which operations can be
	// submitted should be listed.
	sessions bool
	// help indicates that usage information was requested, for the topic
	// specified as an argument, if any.
	help bool
	// version indicates that version information was requested.
	version bool
	// arguments are the command and its arguments, the paths to compare, or
	// the help topic.
	arguments []string
	// flags is the parsed flag set, used to print usage information.
	flags *pflag.FlagSet
	// conflictFlags is the subset of flags containing the conflict options,
	// whose usage is only printed with the conflicts help topic.
	conflictFlags *pflag.FlagSet
}

// timeoutValue is the value of --timeout, which, like the argument of timeout,
// is a number of seconds unless it specifies a unit, such as 10m or 1h30m.
type timeoutValue time.Duration

// Set implements pflag.Value.Set.
func (t *timeoutValue) Set(value string) error {
	duration := value
	if number := strings.TrimPrefix(value, "-"); number != "" && strings.Trim(number, "0123456789.") == "" {
		duration += "s"
	}
	parsed, err := time.ParseDuration(duration)
	if err != nil {
		return errors.New("expected seconds or a duration with a unit, such as 10m")
	}
	*t = timeoutValue(parsed)
	return nil
}

// String implements pflag.Value.String. It formats the value as seconds, which
// also keeps the zero default out of usage information.
func (t *timeoutValue) String() string {
	return strconv.FormatFloat(time.Duration(*t).Seconds(), 'f', -1, 64)
}

// Type implements pflag.Value.Type.
func (t *timeoutValue) Type() string {
	return "duration"
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
	flags.StringVarP(&result.session, "session", "s", "", "Use the session with this `NAME` or identifier, even from outside its root")
	flags.StringVarP(&result.directory, "cwd", "C", "", "Run in `DIR`: absolute and ~ paths are on the opposite endpoint, others are relative to the corresponding directory")
	flags.StringArrayVarP(&result.environment, "env", "e", nil, "Set `NAME[=VALUE]`, where a bare NAME forwards the local value (repeatable)")
	flags.StringArrayVar(&result.environmentFiles, "env-file", nil, "Read literal NAME=VALUE lines from `FILE` (repeatable)")
	flags.Var((*timeoutValue)(&result.timeout), "timeout", "Exit after `DURATION` (seconds, or with a unit such as 10m), terminating the command")
	flags.BoolVar(&result.noPrologue, "no-prologue", false, "Don't source the workspace or configured remote prologue")
	flags.BoolVar(&result.noFlush, "no-flush", false, "Don't synchronize the session before and after the command")
	flags.BoolVar(&result.flush, "flush", false, "Synchronize the session; without a command, do only that")
	flags.BoolVar(&result.diff, "diff", false, "Synchronize, then diff the given paths (or conflicting files) against their remote copies")
	flags.BoolVar(&result.sessions, "sessions", false, "List the connected sessions, showing which ones contain the current directory and which one is used")
	result.conflictFlags = pflag.NewFlagSet("conflicts", pflag.ContinueOnError)
	result.conflictFlags.SortFlags = false
	result.conflictOptions.register(result.conflictFlags)
	flags.AddFlagSet(result.conflictFlags)
	flags.BoolVarP(&result.help, "help", "h", false, "Show this help, or the help topic given as an argument")
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
	if result.help || result.version {
		return result, nil
	} else if result.timeout < 0 {
		return nil, errors.New("timeout must not be negative")
	} else if result.flush && result.noFlush {
		return nil, errors.New("--flush and --no-flush are mutually exclusive")
	} else if result.diff && (result.flush || result.noFlush) {
		return nil, errors.New("--diff can't be combined with --flush or --no-flush")
	} else if result.sessions && (flags.NFlag() > 1 || len(result.arguments) > 0) {
		return nil, errors.New("--sessions doesn't accept other options or arguments")
	} else if !result.flush && !result.diff && !result.sessions && !result.active() && len(result.arguments) == 0 {
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
		if err := printHelp(options, stdout); err != nil {
			return fail(err)
		}
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

	if options.sessions {
		err := printSessions(ctx, directory, options.defaultSession, stdout)
		if code, ok := interrupted("listing sessions"); ok {
			return code
		} else if err != nil {
			return fail(err)
		}
		return 0
	} else if options.active() {
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
			warnOverlapping(stderr, target)
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
