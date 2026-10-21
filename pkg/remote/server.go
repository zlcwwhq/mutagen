package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/multiplexing"
)

const (
	// TerminationGracePeriod is the time that a terminated command's process
	// group has to exit after being asked to (with SIGTERM on POSIX systems),
	// before its remaining processes are killed (with SIGKILL). It's short so
	// that pausing or terminating a session, which terminates its commands,
	// doesn't take long.
	TerminationGracePeriod = 5 * time.Second
	// terminationPollInterval is the interval at which a terminated command's
	// process group is checked for remaining processes once the command has
	// exited.
	terminationPollInterval = 10 * time.Millisecond
)

// terminationGracePeriod is the grace period used for terminated commands. It
// is a variable so that tests can shorten it.
var terminationGracePeriod = TerminationGracePeriod

// Serve executes one command (or answers one file request) received over a
// routed connection, resolving its working directory against the specified
// normalized synchronization root. If the command requests a prologue, then it
// sources the workspace override or, if absent, the specified resolved prologue
// (if non-empty). The alpha flag identifies the executing endpoint for the
// command's environment. Serve owns and closes the connection and cancels the
// command if the client disconnects.
func Serve(ctx context.Context, connection io.ReadWriteCloser, root, prologue string, alpha bool) error {
	// Report that the route has reached an executing endpoint.
	if err := writeMessage(connection, &status{}); err != nil {
		connection.Close()
		return fmt.Errorf("unable to send route status: %w", err)
	}

	mux := multiplexing.Multiplex(multiplexing.NewCarrierFromStream(connection), false, commandMultiplexerConfiguration())
	defer mux.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { mux.Close() })
	defer stop()
	go func() {
		select {
		case <-mux.Closed():
			cancel()
		case <-ctx.Done():
		}
	}()

	// Accept streams in the fixed protocol order.
	var streams [4]*multiplexing.Stream
	for i := range streams {
		stream, err := mux.AcceptStream(ctx)
		if err != nil {
			return fmt.Errorf("unable to accept command stream: %w", err)
		}
		streams[i] = stream
	}
	control, input, output, errorOutput := streams[0], streams[1], streams[2], streams[3]
	var request Request
	if err := readMessage(control, &request); err != nil {
		return fmt.Errorf("unable to receive command: %w", err)
	}
	response := run(ctx, root, prologue, alpha, &request, input, output, errorOutput)
	input.Close()
	output.CloseWrite()
	errorOutput.CloseWrite()
	if err := writeMessage(control, response); err != nil {
		return fmt.Errorf("unable to send command result: %w", err)
	}
	var acknowledgement [1]byte
	if _, err := io.ReadFull(control, acknowledgement[:]); err != nil {
		return fmt.Errorf("unable to receive command acknowledgement: %w", err)
	}
	return nil
}

// workingDirectory computes a request's working directory on this system.
func workingDirectory(root string, request *Request) (string, error) {
	base := filepath.Join(root, filepath.FromSlash(request.Path))
	if request.Directory == "" {
		return base, nil
	} else if filepath.IsAbs(request.Directory) || strings.HasPrefix(request.Directory, "~") {
		return filesystem.Normalize(request.Directory)
	}
	return filepath.Join(base, request.Directory), nil
}

// sendEntry reads the entry at a slash-separated path relative to the root and
// sends any data that it includes as output.
func sendEntry(ctx context.Context, root, path string, output io.Writer) result {
	entry, err := ReadEntry(ctx, filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return result{Error: fmt.Sprintf("unable to read %s: %v", path, err)}
	} else if _, err := output.Write(entry.Data); err != nil {
		return result{Error: fmt.Sprintf("unable to send file contents: %v", err)}
	}
	return result{Entry: entry}
}

// forwardOutput copies a command's output until the command and its children
// close it, returning the first error encountered. Once writing fails, such as
// because the route was cancelled, the remaining output is discarded rather
// than left unread, so that a command writing output while it terminates is
// neither blocked nor killed by a broken pipe.
func forwardOutput(destination io.Writer, source io.Reader) error {
	_, err := io.Copy(destination, source)
	if err != nil {
		io.Copy(io.Discard, source)
	}
	return err
}

// terminate terminates a started command's process group, first by asking it to
// terminate, and then, if any of its processes remain once the grace period
// expires, by killing them. The finished channel must be closed once the
// command has exited and its output has been closed. If the command hasn't
// finished when the grace period expires, then abandon is called after killing
// the process group to stop forwarding output, which a process that left the
// group could otherwise hold open indefinitely. Where commands can't be asked
// to terminate, they're terminated immediately, and the grace period is
// skipped.
func terminate(command *exec.Cmd, finished <-chan struct{}, abandon func()) {
	interruptProcess(command)
	period := terminationGracePeriod
	if !gracefulTermination {
		period = 0
	}
	expired := time.NewTimer(period)
	defer expired.Stop()

	// Wait for the command to finish, and then for any processes that remain
	// in its group, such as children that closed their output, to exit.
	select {
	case <-finished:
		poll := time.NewTicker(terminationPollInterval)
		defer poll.Stop()
		for processRemains(command) {
			select {
			case <-poll.C:
			case <-expired.C:
				killProcess(command)
				return
			}
		}
		return
	case <-expired.C:
	}

	// Kill whatever remains of the command.
	killProcess(command)
	abandon()
}

// run prepares and executes a command without interpreting its arguments, or
// performs a requested file operation.
func run(ctx context.Context, root, prologue string, alpha bool, request *Request, stdin io.Reader, stdout, stderr io.Writer) result {
	if err := request.EnsureValid(); err != nil {
		return result{Error: err.Error()}
	} else if request.File != "" {
		return sendEntry(ctx, root, request.File, stdout)
	} else if request.Copy != "" {
		entry, err := CopyFile(ctx, root, request.Copy, stdout)
		if err != nil {
			return result{Error: fmt.Sprintf("unable to copy %s: %v", request.Copy, err)}
		}
		return result{Entry: entry}
	} else if request.Archive != "" {
		entry, err := ArchiveEntry(ctx, root, request.Archive, stdout)
		if err != nil {
			return result{Error: fmt.Sprintf("unable to archive %s: %v", request.Archive, err)}
		}
		return result{Entry: entry}
	} else if request.Replace != nil {
		// The context is only cancelled once this executor notices that the
		// client stopped waiting, such as on timeout, so a replacement may
		// still write its result afterward. Replacements thus keep their
		// entries locked until they've finished, and those of earlier applies
		// are refused once a later apply's replacement has been admitted (see
		// ReplaceFile).
		if err := ReplaceFile(ctx, root, request.Replace, stdin); err != nil {
			return result{
				Error:      fmt.Sprintf("unable to replace %s: %v", request.Replace.Path, err),
				Stale:      errors.Is(err, ErrStale),
				Superseded: errors.Is(err, ErrSuperseded),
			}
		}
		return result{}
	}
	directory, err := workingDirectory(root, request)
	if err != nil {
		return result{Error: fmt.Sprintf("invalid working directory: %v", err)}
	}
	// Check the working directory explicitly, since a missing one (such as an
	// ignored or not yet synchronized directory) would otherwise be reported as
	// a missing executable by the operating system.
	if info, err := os.Stat(directory); err != nil {
		return result{ExitCode: 126, Error: fmt.Sprintf("unable to access working directory: %v", err)}
	} else if !info.IsDir() {
		return result{ExitCode: 126, Error: fmt.Sprintf("working directory is not a directory: %s", directory)}
	}

	// The executing endpoint supplies its identity after all user-provided
	// variables, including when no prologue is requested or configured.
	endpoint := "beta"
	if alpha {
		endpoint = "alpha"
	}
	environment := append([]string(nil), request.Environment...)
	environment = append(environment, "MUTAGEN_REMOTE_ENDPOINT="+endpoint)

	// Discover workspace overrides at execution time so edits take effect
	// without reconnecting the channel. Disabled prologues aren't inspected.
	if request.Prologue {
		if prologue, err = selectPrologue(root, prologue); err != nil {
			return result{ExitCode: 126, Error: err.Error()}
		}
	}

	// A prologue may change PATH, so the prologue's shell locates the command
	// once the prologue has been sourced.
	arguments := request.Arguments
	var path string
	if request.Prologue && prologue != "" {
		if arguments, err = prologueArguments(prologue, arguments); err != nil {
			return result{ExitCode: 126, Error: err.Error()}
		}
		path = arguments[0]
	} else if path, err = lookPath(arguments[0], directory, append(os.Environ(), environment...)); err != nil {
		return result{ExitCode: 127, Error: fmt.Sprintf("unable to find command: %v", err)}
	}
	command := exec.Command(path, arguments[1:]...)
	command.Args[0] = arguments[0]
	command.Dir = directory
	command.Env = append(command.Environ(), environment...)
	configureProcess(command)

	// Own the output pipes so that termination can interrupt draining even
	// after the command exits and a descendant still holds an output handle.
	// Normal draining has no deadline, preserving output over slow links.
	outputReader, outputWriter, err := os.Pipe()
	if err != nil {
		return result{Error: fmt.Sprintf("unable to create command output: %v", err)}
	}
	defer outputReader.Close()
	defer outputWriter.Close()
	errorReader, errorWriter, err := os.Pipe()
	if err != nil {
		return result{Error: fmt.Sprintf("unable to create command error output: %v", err)}
	}
	defer errorReader.Close()
	defer errorWriter.Close()
	command.Stdout, command.Stderr = outputWriter, errorWriter

	// Manage the stdin copier explicitly: exec.Cmd must not wait for client
	// input when a command that does not consume stdin exits.
	input, err := command.StdinPipe()
	if err != nil {
		return result{Error: fmt.Sprintf("unable to create command input: %v", err)}
	}
	if err := ctx.Err(); err != nil {
		return result{ExitCode: 126, Error: fmt.Sprintf("unable to start command: %v", err)}
	} else if err := command.Start(); err != nil {
		return result{ExitCode: 126, Error: fmt.Sprintf("unable to start command: %v", err)}
	}
	outputWriter.Close()
	errorWriter.Close()
	outputErrors := make(chan error, 2)
	go func() {
		outputErrors <- forwardOutput(stdout, outputReader)
	}()
	go func() {
		outputErrors <- forwardOutput(stderr, errorReader)
	}()
	go func() {
		io.Copy(input, stdin)
		input.Close()
	}()

	// Terminate the command if the route is cancelled, such as because the
	// client disconnected or the session stopped synchronizing, before the
	// command has exited and its output has been closed. Termination then
	// completes before returning, so that it isn't cut short when the
	// executing process exits once its commands are done.
	finished := make(chan struct{})
	terminated := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(terminated)
		terminate(command, finished, func() {
			outputReader.Close()
			errorReader.Close()
		})
	})
	err = command.Wait()
	outputError := errors.Join(<-outputErrors, <-outputErrors)
	close(finished)
	if !stop() {
		<-terminated
	}
	if outputError != nil {
		return result{Error: fmt.Sprintf("unable to forward command output: %v", outputError)}
	}
	if err == nil {
		return result{}
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return result{ExitCode: processExitCode(exitError.ProcessState)}
	}
	return result{Error: fmt.Sprintf("unable to run command: %v", err)}
}
