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

// usage is the usage summary printed before the option list.
const usage = `Usage: remote [options] <command> [<args>...]

Run a command on the opposite endpoint of the Mutagen synchronization session
whose root contains the current directory. Commands run in the corresponding
directory on the opposite endpoint. Sessions must be connected and have remote
commands enabled with the remote configuration parameter (or the --remote flag
of mutagen sync create).

Arguments are passed without shell interpretation, so shell syntax requires an
explicit shell. Options must precede the command. Standard streams and the exit
status are forwarded, but no pseudo-terminal is allocated. Exit status 255
indicates that the command couldn't be routed, while 126 and 127 indicate that
it couldn't be started.

Options:
`

// options are the parsed command line options.
type options struct {
	// session selects a session by identifier or name.
	session string
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
	flags.StringVarP(&result.session, "session", "s", os.Getenv("MUTAGEN_REMOTE_SESSION"), "Select a session by name or identifier (default $MUTAGEN_REMOTE_SESSION)")
	flags.StringVarP(&result.directory, "cwd", "C", "", "Override the working directory on the opposite endpoint")
	flags.StringArrayVarP(&result.environment, "env", "e", nil, "Set NAME=VALUE, or forward a local variable by NAME (repeatable)")
	flags.StringArrayVar(&result.environmentFiles, "env-file", nil, "Read literal NAME=VALUE entries from a file (repeatable)")
	flags.DurationVar(&result.timeout, "timeout", 0, "Cancel the command after this duration")
	flags.BoolVar(&result.noPrologue, "no-prologue", false, "Don't source the opposite endpoint's remote prologue")
	flags.BoolVar(&result.noFlush, "no-flush", false, "Don't synchronize the session before and after the command")
	flags.BoolVar(&result.flush, "flush", false, "Synchronize the session; without a command, do only that")
	flags.BoolVar(&result.diff, "diff", false, "Synchronize, then diff the given paths (or conflicting files) against their remote copies")
	flags.BoolVarP(&result.help, "help", "h", false, "Show help information")
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
	if result.timeout < 0 {
		return nil, errors.New("timeout must not be negative")
	} else if result.flush && result.noFlush {
		return nil, errors.New("--flush and --no-flush are mutually exclusive")
	} else if result.diff && (result.flush || result.noFlush) {
		return nil, errors.New("--diff can't be combined with --flush or --no-flush")
	} else if !result.help && !result.version && !result.flush && !result.diff && len(result.arguments) == 0 {
		return nil, errors.New("no command specified")
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
		fmt.Fprint(stdout, usage, options.flags.FlagUsages())
		return 0
	} else if options.version {
		fmt.Fprintln(stdout, mutagen.Version)
		return 0
	}

	// Prepare the request, if any. The working directory path, and whether the
	// prologue applies, are determined by the session root once the route is
	// located.
	var request *remote.Request
	if !options.diff && len(options.arguments) > 0 {
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

	// synchronize forces a synchronization cycle for the session and warns
	// about content that couldn't be synchronized, omitting warnings issued by
	// previous cycles. It returns the target if the session was located, along
	// with the report.
	var reports *reporter
	synchronize := func() (*remote.Target, *remote.Report, error) {
		connection, target, err := remote.Dial(ctx, directory, options.session, remote.OperationFlush)
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
		differ, err := compare(ctx, directory, options.session, target, report, options.arguments, stdout, stderr)
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
	connection, target, err := remote.Dial(ctx, directory, options.session, remote.OperationExecute)
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
