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
	"slices"
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
	// failureExitCode is the exit code for remote input and operation errors,
	// following the wrapper-failure convention used by timeout and env.
	failureExitCode = 125
	// connectionExitCode denotes unavailable sessions and communication errors.
	connectionExitCode = 255
	// timeoutExitCode is the exit code for commands cancelled by --timeout,
	// which matches the convention used by timeout.
	timeoutExitCode = 124
)

// errorExitCode maps remote's own failures at the CLI boundary. Commands and
// completed jobs bypass this mapping and retain their original exit status.
func errorExitCode(err error) int {
	switch remote.ClassifyError(err) {
	case remote.ErrorKindTimeout:
		return timeoutExitCode
	case remote.ErrorKindConnection:
		return connectionExitCode
	default:
		return failureExitCode
	}
}

// checkedWriter retains output errors from formatters that don't return them.
// Operation output is written serially; command streams use remote.Execute's
// separate source and destination error tracking instead.
type checkedWriter struct {
	io.Writer
	err error
}

func (w *checkedWriter) Write(data []byte) (int, error) {
	count, err := w.Writer.Write(data)
	if err == nil && count != len(data) {
		err = io.ErrShortWrite
	}
	if w.err == nil {
		w.err = err
	}
	return count, err
}

// lockedWriter serializes writes to an output that several goroutines write.
type lockedWriter struct {
	// lock serializes writes.
	lock sync.Mutex
	// writer is the underlying output.
	writer io.Writer
}

func (w *lockedWriter) Write(data []byte) (int, error) {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.writer.Write(data)
}

// Fd preserves terminal detection (see terminalWidth) when the underlying
// output exposes a file descriptor.
func (w *lockedWriter) Fd() uintptr {
	if file, ok := w.writer.(interface{ Fd() uintptr }); ok {
		return file.Fd()
	}
	return ^uintptr(0)
}

// options are the parsed command line options.
type options struct {
	// node selects the destination node.
	node string
	// nodes requests a read-only listing of the nodes that can be reached.
	nodes bool
	conflictOptions
	// Deprecated selectors are parsed solely to report migration errors.
	deprecatedSession  string
	deprecatedSessions bool
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
	// progress indicates whether synchronization progress was explicitly
	// enabled or disabled, or is nil if progress is only shown when standard
	// error is a terminal (see newProgressOutput).
	progress *bool
	// script is the path of a local script to run instead of a command, or -
	// for standard input, in which case the arguments are its arguments.
	script string
	// detach indicates that the command should run as a detached job.
	detach bool
	// log is the log of a detached job, if specified.
	log string
	// jobs indicates that detached jobs should be listed.
	jobs bool
	// wait is the identifier of a detached job to wait for, if any.
	wait string
	// tail is the number of final log lines to print after waiting for a job,
	// or with which following its log starts.
	tail int64
	// follow indicates that a job's log should be printed while waiting for
	// the job.
	follow bool
	// kill is the identifier of a detached job to terminate, if any.
	kill string
	// diff indicates that local and remote copies of the paths specified by
	// arguments (or of conflicting files) should be compared.
	diff bool
	// ignores indicates that the session's ignores, or whether the paths
	// specified by arguments are ignored, should be shown. In Git mode, paths
	// can also be ignored by Git rules, which are read from this system's copy
	// of the synchronization root.
	ignores bool
	// pull indicates that the ignored paths specified by arguments should be
	// copied from the opposite endpoint. In Git mode, that includes content
	// that .gitignore rules ignore.
	pull bool
	// help indicates that usage information was requested, for the topic
	// specified as an argument, if any.
	help bool
	// version indicates that version information was requested.
	version bool
	// arguments are the command and its arguments, the script's arguments,
	// the paths to operate on, or the help topic.
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
	flags.StringVarP(&result.deprecatedSession, "session", "s", "", "Deprecated; use --node")
	flags.StringVar(&result.node, "node", "", "Select destination node `NAME`")
	flags.BoolVar(&result.nodes, "nodes", false, "List reachable nodes and their availability")
	flags.StringVarP(&result.directory, "cwd", "C", "", "Override execution directory with `DIR`")
	flags.StringArrayVarP(&result.environment, "env", "e", nil, "Set or forward `NAME[=VALUE]` (repeatable)")
	flags.StringArrayVar(&result.environmentFiles, "env-file", nil, "Read NAME=VALUE from `FILE` (repeatable)")
	flags.StringVar(&result.script, "script", "", "Run local script `FILE` (- reads stdin)")
	flags.Var((*timeoutValue)(&result.timeout), "timeout", "Limit runtime to `DURATION` (seconds or 10m)")
	flags.BoolVar(&result.noPrologue, "no-prologue", false, "Skip the execution prologue")
	flags.BoolVar(&result.noFlush, "no-flush", false, "Skip synchronization (see --help sync)")
	flags.BoolVar(&result.flush, "flush", false, "Synchronize, optionally before a command")
	var progress bool
	flags.BoolVar(&progress, "progress", false, "Show sync progress; =false hides it")
	flags.BoolVar(&result.detach, "detach", false, "Start a detached job and print its ID")
	flags.StringVar(&result.log, "log", "", "Write detached output to `FILE`")
	flags.BoolVar(&result.jobs, "jobs", false, "List detached jobs on the other endpoint")
	flags.StringVar(&result.wait, "wait", "", "Wait for job `ID`, sync, return its status")
	flags.Int64Var(&result.tail, "tail", 0, "With --wait, print final `N` log lines")
	flags.BoolVar(&result.follow, "follow", false, "With --wait, stream the job's log")
	flags.StringVar(&result.kill, "kill", "", "Terminate job `ID` without synchronizing")
	flags.BoolVar(&result.diff, "diff", false, "Sync, then compare paths or conflicts")
	flags.BoolVar(&result.ignores, "ignores", false, "Show ignore rules or check given paths")
	flags.BoolVar(&result.pull, "pull", false, "Pull ignored paths, preserving file mtime")
	flags.BoolVar(&result.deprecatedSessions, "sessions", false, "Deprecated; use --nodes")
	flags.MarkHidden("session")
	flags.MarkHidden("sessions")
	result.conflictFlags = pflag.NewFlagSet("conflicts", pflag.ContinueOnError)
	result.conflictFlags.SortFlags = false
	result.conflictOptions.register(result.conflictFlags)
	flags.AddFlagSet(result.conflictFlags)
	flags.BoolVarP(&result.help, "help", "h", false, "Show help, optionally for a topic")
	flags.BoolVar(&result.version, "version", false, "Show version information")
	options, operands := arguments, []string(nil)
	if boundary := scriptBoundary(flags, arguments); boundary >= 0 {
		options, operands = arguments[:boundary], arguments[boundary:]
	}
	if err := flags.Parse(options); err != nil {
		if err == pflag.ErrHelp {
			result.help = true
		} else {
			return nil, err
		}
	}
	result.arguments = append(flags.Args(), operands...)
	result.flags = flags
	if flags.Changed("session") || flags.Changed("sessions") {
		return nil, errors.New("--session, -s, and --sessions are deprecated and no longer supported; use --nodes to discover targets and --node NAME to select one")
	}
	if flags.Changed("progress") {
		result.progress = &progress
	}
	if result.help || result.version {
		return result, nil
	} else if os.Getenv(remote.SessionVariable) != "" {
		return nil, errors.New("MUTAGEN_REMOTE_SESSION is deprecated and no longer supported; unset it and select a destination with --node NAME")
	} else if err := result.validate(flags); err != nil {
		return nil, err
	} else if err := result.validateConflicts(); err != nil {
		return nil, err
	}
	return result, nil
}

// scriptBoundary returns the number of leading arguments that are options if
// --script is among them, since the arguments that follow its file are the
// script's arguments, even if they look like options, as with a shell's script
// arguments. It returns -1 otherwise, including for arguments that aren't
// valid options, which parsing then reports.
func scriptBoundary(flags *pflag.FlagSet, arguments []string) int {
	for a := 0; a < len(arguments); a++ {
		argument := arguments[a]
		if argument == "--" || argument == "-" || !strings.HasPrefix(argument, "-") {
			return -1
		}
		var flag *pflag.Flag
		name, _, value := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if strings.HasPrefix(argument, "--") {
			flag = flags.Lookup(name)
		} else if name != "" {
			flag = flags.ShorthandLookup(name[:1])
			value = value || len(name) > 1
		}
		if flag == nil {
			return -1
		} else if flag.Name == "script" && value {
			return a + 1
		} else if flag.Name == "script" {
			return min(a+2, len(arguments))
		} else if flag.NoOptDefVal == "" && !value {
			a++
		}
	}
	return -1
}

// operation is an operation that an option selects instead of running a
// command.
type operation struct {
	// name is the name of the option that selects it.
	name string
	// accepted are the names of the other options that it accepts.
	accepted []string
	// arguments are the arguments that it takes: noArguments, optionalPaths,
	// or requiredPaths.
	arguments int
}

// The arguments that operations take. Paths can't look like options, which
// must precede them, except for --diff, which compares whatever paths it's
// given.
const (
	noArguments = iota
	optionalPaths
	requiredPaths
)

// operations are the operations other than running a command. Running a
// command accepts commandOptions, and the other options require an operation
// that accepts them. Some operations accept options that have no effect for
// them, such as --no-flush and --progress for --kill, and command options for
// --diff, as they always have.
var operations = []operation{
	{"nodes", []string{"json", "timeout"}, noArguments},
	{"jobs", []string{"node"}, noArguments},
	{"wait", []string{"node", "timeout", "no-flush", "tail", "follow", "progress"}, noArguments},
	{"kill", []string{"node", "timeout", "no-flush", "progress"}, noArguments},
	{"ignores", []string{"node"}, optionalPaths},
	{"pull", []string{"node", "timeout"}, requiredPaths},
	{"diff", []string{"node", "timeout", "progress", "cwd", "env", "env-file", "no-prologue"}, optionalPaths},
	{"conflicts", []string{"node", "timeout", "progress", "json"}, noArguments},
	{"conflict-checkout", []string{"node", "timeout", "progress", "json", "base"}, requiredPaths},
	{"conflict-apply", []string{"node", "timeout", "progress", "json", "take", "delete", "executable"}, requiredPaths},
	{"conflict-discard", []string{"node", "timeout", "progress", "json"}, requiredPaths},
}

// commandOptions are the options that running a command (or script) accepts.
var commandOptions = []string{
	"node", "cwd", "env", "env-file", "script", "timeout", "no-prologue",
	"no-flush", "flush", "progress", "detach", "log",
}

// selects indicates whether a set option that names an operation selects it,
// which a boolean option set to false, such as --diff=false, doesn't.
func selects(flag *pflag.Flag) bool {
	return flag.Changed && (flag.Value.Type() != "bool" || flag.Value.String() == "true")
}

// operation returns the operation that the options select, or nil if they
// request running a command.
func (o *options) operation() *operation {
	for i := range operations {
		if selects(o.flags.Lookup(operations[i].name)) {
			return &operations[i]
		}
	}
	return nil
}

// command indicates whether the options request running a command (or script).
func (o *options) command() bool {
	return o.operation() == nil && (o.script != "" || len(o.arguments) > 0)
}

// validate checks the combination of options, other than the values of
// conflict options, which validateConflicts checks.
func (o *options) validate(flags *pflag.FlagSet) error {
	if flags.Changed("node") && o.node == "" {
		return errors.New("--node requires a node name")
	} else if o.tail < 0 {
		return errors.New("tail count must not be negative")
	} else if o.timeout < 0 {
		return errors.New("timeout must not be negative")
	}

	// Check that the operation, or running a command, accepts the other
	// options, skipping those that select no operation.
	selected := o.operation()
	accepted := commandOptions
	if selected != nil {
		accepted = selected.accepted
	}
	var err error
	flags.Visit(func(flag *pflag.Flag) {
		if err != nil || slices.Contains(accepted, flag.Name) || selected != nil && flag.Name == selected.name ||
			!selects(flag) && slices.ContainsFunc(operations, func(other operation) bool { return other.name == flag.Name }) {
			return
		} else if selected != nil {
			err = fmt.Errorf("--%s can't be combined with --%s", flag.Name, selected.name)
			return
		}
		var accepting []string
		for _, other := range operations {
			if slices.Contains(other.accepted, flag.Name) {
				accepting = append(accepting, "--"+other.name)
			}
		}
		err = fmt.Errorf("--%s requires %s", flag.Name, strings.Join(accepting, " or "))
	})
	if err != nil {
		return err
	}

	// Check the arguments and the remaining combinations.
	paths := selected != nil && selected.arguments != noArguments && selected.name != "diff"
	if selected != nil && selected.arguments == noArguments && len(o.arguments) > 0 {
		return fmt.Errorf("--%s doesn't accept arguments", selected.name)
	} else if selected != nil && selected.arguments == requiredPaths && len(o.arguments) == 0 {
		return errors.New("no paths specified")
	} else if paths && slices.ContainsFunc(o.arguments, func(argument string) bool { return strings.HasPrefix(argument, "-") }) {
		return errors.New("options must precede paths (prefix paths starting with - with ./)")
	} else if (flags.Changed("wait") && o.wait == "") || (flags.Changed("kill") && o.kill == "") {
		return errors.New("--wait and --kill require a job identifier")
	} else if o.flush && o.noFlush {
		return errors.New("--flush and --no-flush are mutually exclusive")
	} else if flags.Changed("script") && o.script == "" {
		return errors.New("--script requires a file (or - for standard input)")
	} else if flags.Changed("log") && !o.detach {
		return errors.New("--log requires --detach")
	} else if o.detach && o.timeout > 0 {
		return fmt.Errorf("--timeout can't be combined with --detach, which returns once the job starts; to limit the job's duration, run it with timeout, such as %s --detach timeout 1h make bench", followUpCommand(o.node))
	} else if o.detach && o.script == "" && len(o.arguments) == 0 {
		return errors.New("--detach requires a command or --script")
	} else if selected == nil && !o.flush && o.script == "" && len(o.arguments) == 0 {
		return errors.New("no command specified")
	}
	return nil
}

// Run executes the command line (excluding the executable name) and returns the
// exit code.
func Run(arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Remote stderr, progress refreshes, and daemon reports may arrive on
	// different goroutines. Cancellation diagnostics use this writer too: a
	// command's stderr may still be forwarded when cancellation ends
	// remote.Execute, and the diagnostic that follows must wait for it.
	// Standard output needs no lock: Run only writes it itself after the
	// operations that forward output to it have completed, and never after
	// cancellation ends one of them.
	stderr = &lockedWriter{writer: stderr}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "remote:", err)
		return errorExitCode(err)
	}

	// Parse options.
	options, err := parse(arguments)
	if err != nil {
		return fail(fmt.Errorf("%w (see remote --help)", err))
	} else if options.help {
		output := &checkedWriter{Writer: stdout}
		if err := errors.Join(printHelp(options, output), output.err); err != nil {
			return fail(err)
		}
		return 0
	} else if options.version {
		if _, err := fmt.Fprintln(stdout, mutagen.Version); err != nil {
			return fail(err)
		}
		return 0
	}

	// Prepare the request, if any. The working directory path, and whether the
	// prologue applies, are determined by the session root once the route is
	// located. A script read from standard input leaves no input for the
	// command.
	var request *remote.Request
	if options.command() {
		environment, err := loadEnvironment(options.environmentFiles, options.environment)
		if err != nil {
			return fail(err)
		}
		request = &remote.Request{
			Arguments:   options.arguments,
			Environment: environment,
			Directory:   options.directory,
		}
		if options.script != "" {
			if request.Script, err = readScript(options.script, stdin, stderr); err != nil {
				return fail(err)
			} else if options.script == "-" {
				stdin = strings.NewReader("")
			}
		}
		if options.detach {
			request.Job = &remote.JobRequest{Operation: remote.JobStart, Log: options.log}
		}
		if err := request.EnsureValid(); err != nil {
			return fail(err)
		}
	}

	// Terminate the command on timeout or termination signals, which cancel
	// the context with a signalError as its cause (see router.signaled).
	ctx := context.Background()
	if options.timeout > 0 {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, options.timeout)
		defer cancelTimeout()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case s := <-signals:
			cancel(signalError{s})
		case <-ctx.Done():
		}
	}()

	// Determine the working directory, which locates the session.
	directory, err := os.Getwd()
	if err != nil {
		return fail(fmt.Errorf("unable to determine working directory: %w", err))
	}
	router := &router{
		ctx: ctx, directory: directory, selection: options.selection(request != nil && options.directory == ""),
		node: options.node, stderr: stderr, progress: options.progress, timeout: options.timeout,
	}

	// Discovery is explicit; all other operations resolve their destination
	// on the same daemon connection that performs the operation.
	if options.nodes {
		graph, err := queryNodes(ctx, directory)
		if err == nil {
			err = printNodes(graph, options.json, stdout)
		}
		return router.exit("querying nodes", 0, err)
	}

	// Perform operations other than running commands.
	var activity string
	var code int
	output := &checkedWriter{Writer: stdout}
	switch {
	case options.active():
		activity = "resolving conflicts"
		if options.conflicts {
			activity = "listing conflicts"
		}
		err = runConflicts(router, options, output)
	case options.jobs:
		activity, err = "listing jobs", listJobs(router, output)
	case options.wait != "":
		return waitJob(router, options, stdout)
	case options.kill != "":
		activity, err = "terminating the job", killJob(router, options.kill)
	case options.ignores:
		activity = "checking ignores"
		code, err = printIgnores(router, options.arguments, output)
	case options.pull:
		activity, err = "pulling", pull(router, options.arguments)
	default:
		return runCommand(router, options, request, stdin, stdout, stderr)
	}
	if output.err != nil {
		err = errors.Join(err, fmt.Errorf("unable to print output: %w", output.err))
	}
	return router.exit(activity, code, err)
}

// signalError is the cause with which a termination signal cancels the context
// of remote's operations.
type signalError struct {
	os.Signal
}

// Error implements error.Error.
func (e signalError) Error() string {
	return "received " + e.Signal.String()
}

// router submits related operations through one session's endpoint, which the
// first submission locates.
type router struct {
	// ctx is the context for submissions.
	ctx context.Context
	// directory is the working directory, which locates the session.
	directory string
	// selection selects the session, and is pinned to the endpoint that the
	// first submission locates.
	selection *remote.Selection
	// node is the destination node selected with --node, if any, which the
	// remote command lines that remote suggests repeat (see followUp).
	node string
	// stderr receives warnings and synchronization progress.
	stderr io.Writer
	// progress is the --progress setting (see options.progress).
	progress *bool
	// reports prints synchronization reports, once created by synchronize.
	reports *reporter
	// timeout is the --timeout setting, which diagnostics name when it expires.
	timeout time.Duration
}

// dial submits an operation through the session's endpoint. The first
// submission locates the session, and later ones are pinned to the session and
// endpoint that it located, failing rather than reaching a different session if
// sessions connect or disconnect in between, such as a nested session
// reconnecting after the enclosing session was synchronized.
func (r *router) dial(operation remote.Operation) (net.Conn, *remote.Target, error) {
	return r.submit(&remote.Submission{Operation: operation})
}

// submit is dial with the specified submission header.
func (r *router) submit(header *remote.Submission) (net.Conn, *remote.Target, error) {
	connection, target, err := r.selection.Submit(r.ctx, r.directory, header)
	if err == nil {
		r.selection = &remote.Selection{Target: target}
	}
	return connection, target, err
}

// resolve has the daemon select the destination, as it does for every
// submission, without performing an operation, and pins later submissions to
// it, as dial does, unless an earlier submission already resolved it. The
// daemon then refuses unknown or unavailable nodes, multi-hop selections, an
// omitted node with several destinations, and star problems, exactly as it
// does for commands, before the caller reads or writes anything.
func (r *router) resolve() (*remote.Target, error) {
	if target := r.selection.Target; target != nil && target.Resolved {
		return target, nil
	}
	connection, target, err := r.submit(&remote.Submission{Operation: remote.OperationResolve})
	if err != nil {
		return nil, err
	}
	connection.Close()
	return target, nil
}

// locate locates the session's endpoint without submitting an operation,
// pinning later submissions to it, as dial does. Since the daemon doesn't
// resolve the destination, it only serves the inspection of local rules, such
// as by --ignores without --node; operations on a destination use resolve.
func (r *router) locate() (*remote.Target, error) {
	if r.selection.Target != nil {
		return r.selection.Target, nil
	}
	target, err := r.selection.Locate(r.ctx, r.directory)
	if err == nil {
		r.selection = &remote.Selection{Target: target}
	}
	return target, err
}

// synchronize forces a synchronization cycle for the session, showing its
// progress if enabled (see newProgressOutput), and warns about content that
// couldn't be synchronized, omitting warnings issued by previous cycles. It
// returns the target if the session was located, along with the report.
func (r *router) synchronize() (*remote.Target, *remote.Report, error) {
	output := newProgressOutput(r.stderr, r.progress)
	connection, target, err := r.submit(&remote.Submission{Operation: remote.OperationFlush, Progress: output.writer != nil})
	if err != nil {
		return nil, nil, err
	}
	display := newProgressDisplay(output, target)
	report, err := remote.FlushWithProgress(r.ctx, connection, display.begin())
	display.finish(r.ctx, err)
	if err != nil {
		return target, nil, err
	} else if r.reports == nil {
		r.reports = newReporter(r.stderr, target, r.followUp())
	}
	r.reports.print(report)
	return target, report, nil
}

// warn prints a warning.
func (r *router) warn(format string, arguments ...any) {
	fmt.Fprintf(r.stderr, "remote: warning: "+format+"\n", arguments...)
}

// Operations determine remote's exit status with exit or, once a command or
// job has finished, with finished, which implement the precedence that the
// termination help topic documents. A termination signal takes precedence over
// everything else. The status of a command or job that has finished takes
// precedence over failures that follow it, including the expiry of the timeout
// while synchronizing afterwards, which are only warnings. Otherwise, the
// expiry of the timeout takes precedence over failures, and failures over the
// operation's own status, such as that of --diff. The failures of a batch are
// joined, which gives connection failures precedence over operation failures
// (see errorExitCode).

// signaled returns the exit status for a termination signal, 128+N for signal
// N, if remote received one.
func (r *router) signaled() (int, bool) {
	var received signalError
	if !errors.As(context.Cause(r.ctx), &received) {
		return 0, false
	} else if number, ok := received.Signal.(syscall.Signal); ok {
		return 128 + int(number), true
	}
	return failureExitCode, true
}

// interrupted returns the exit status for a termination signal (see signaled)
// or for the expiry of the timeout, which it reports, if either ended the
// specified activity.
func (r *router) interrupted(activity string) (int, bool) {
	if status, ok := r.signaled(); ok {
		return status, true
	} else if r.ctx.Err() == nil {
		return 0, false
	} else if errors.Is(r.ctx.Err(), context.DeadlineExceeded) {
		return r.timedOut(activity), true
	}
	return failureExitCode, true
}

// timedOut reports that the timeout expired while performing the specified
// activity and returns the exit status for that.
func (r *router) timedOut(activity string) int {
	fmt.Fprintf(r.stderr, "remote: timed out after %v while %s\n", r.timeout, activity)
	return timeoutExitCode
}

// exit returns remote's exit status once the specified activity has ended,
// given the status that it determined and the error that it failed with, if
// any. Errors are printed, except for errReported, whose causes were printed
// as they occurred.
func (r *router) exit(activity string, code int, err error) int {
	if status, ok := r.interrupted(activity); ok {
		return status
	} else if err != nil {
		if !errors.Is(err, errReported) {
			fmt.Fprintln(r.stderr, "remote:", err)
		}
		return errorExitCode(err)
	}
	return code
}

// finished returns remote's exit status once a command or job, as named by
// what, has finished with the specified status, given the error with which
// synchronizing afterwards failed, if it did. The status takes precedence over
// that failure, including the expiry of the timeout, which is reported as a
// warning, but not over termination signals.
func (r *router) finished(what string, code int, err error) int {
	if status, ok := r.signaled(); ok {
		return status
	} else if err != nil {
		if errors.Is(r.ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %v", r.timeout)
		}
		r.warn("unable to synchronize after the %s, whose changes may not be present locally yet: %v", what, err)
	}
	return code
}

// followUp returns the start of the remote command lines that remote suggests
// for later operations on the destination (see followUpCommand).
func (r *router) followUp() string {
	return followUpCommand(r.node)
}

// runCommand synchronizes the session, if requested, and then runs a command,
// starts it as a detached job, or compares copies, returning the exit code.
func runCommand(router *router, options *options, request *remote.Request, stdin io.Reader, stdout, stderr io.Writer) int {
	// Synchronize local changes before the command. Failure to locate the
	// session is fatal, since it would also prevent running the command, but
	// failure to synchronize isn't, since the command may still be useful.
	var target *remote.Target
	var report *remote.Report
	var err error
	if !options.noFlush {
		target, report, err = router.synchronize()
		if (err != nil && (target == nil || request == nil)) || router.ctx.Err() != nil {
			return router.exit("synchronizing", 0, err)
		} else if err != nil {
			router.warn("unable to synchronize before the command, which may see outdated files: %v", err)
		}
	}

	// Compare copies if requested.
	if options.diff {
		differ, err := compare(router, target, report, options.arguments, stdout)
		code := 0
		if differ {
			code = 1
		}
		return router.exit("comparing", code, err)
	} else if request == nil {
		return 0
	}

	// Locate the route and run the command, or start it as a detached job.
	connection, target, err := router.dial(remote.OperationExecute)
	if err != nil || router.ctx.Err() != nil {
		return router.exit("running the command", 0, err)
	}
	request.Path = target.Path
	request.Prologue = target.Contained && !options.noPrologue
	if request.Job != nil {
		code, err := startJob(router.ctx, connection, target, request, router.followUp(), stdout, stderr)
		if status, ok := router.interrupted("starting the job"); ok {
			return status
		} else if err != nil {
			printCommandError(stderr, err, request.Directory)
			if code != 0 {
				return code
			}
			return errorExitCode(err)
		}
		return 0
	}
	code, err := remote.Execute(router.ctx, connection, request, stdin, stdout, stderr)
	if status, ok := router.interrupted("running the command"); ok {
		return status
	} else if errors.Is(err, remote.ErrConnectionLost) {
		// The executing endpoint terminates the command when the connection is
		// lost, which happens whenever the session stops synchronizing, so
		// synchronizing afterwards isn't attempted.
		lost := "the connection to the executing endpoint was lost (the session may have stopped synchronizing), so the command was terminated"
		if !options.noFlush {
			lost += " and the session wasn't synchronized afterwards"
		}
		return router.exit("running the command", 0, remote.ConnectionError(errors.New(lost)))
	} else if err != nil {
		// Startup failures such as missing commands report conventional exit
		// codes (126 or 127) alongside the error.
		printCommandError(stderr, err, request.Directory)
		if code != 0 {
			return code
		}
		return errorExitCode(err)
	}

	// Synchronize the command's changes back, which can fail without changing
	// the command's status (see router.finished).
	if !options.noFlush {
		_, _, err = router.synchronize()
	}
	return router.finished("command", code, err)
}
