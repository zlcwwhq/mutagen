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

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/multiplexing"
)

// Serve executes one command (or answers one file request) received over a
// routed connection, resolving its working directory against the specified
// normalized synchronization root and sourcing the specified resolved prologue
// (if non-empty) first if the command requests it. It owns and closes the
// connection and cancels the command if the client disconnects.
func Serve(ctx context.Context, connection io.ReadWriteCloser, root, prologue string) error {
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
	response := run(ctx, root, prologue, &request, input, output, errorOutput)
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

// run prepares and executes a command without interpreting its arguments, or
// performs a requested file operation.
func run(ctx context.Context, root, prologue string, request *Request, stdin io.Reader, stdout, stderr io.Writer) result {
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
	} else if request.Replace != nil {
		if err := ReplaceFile(ctx, root, request.Replace, stdin); err != nil {
			return result{Error: fmt.Sprintf("unable to replace %s: %v", request.Replace.Path, err)}
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
	// A prologue may change PATH, so the prologue's shell locates the command
	// once the prologue has been sourced.
	arguments := request.Arguments
	var path string
	if request.Prologue && prologue != "" {
		if arguments, err = prologueArguments(prologue, arguments); err != nil {
			return result{ExitCode: 126, Error: err.Error()}
		}
		path = arguments[0]
	} else if path, err = lookPath(arguments[0], directory, append(os.Environ(), request.Environment...)); err != nil {
		return result{ExitCode: 127, Error: fmt.Sprintf("unable to find command: %v", err)}
	}
	command := exec.CommandContext(ctx, path, arguments[1:]...)
	command.Args[0] = arguments[0]
	command.Dir = directory
	command.Env = append(command.Environ(), request.Environment...)
	configureProcess(command)

	// Own the output pipes so that cancellation can interrupt draining even
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
	if err := command.Start(); err != nil {
		return result{ExitCode: 126, Error: fmt.Sprintf("unable to start command: %v", err)}
	}
	outputWriter.Close()
	errorWriter.Close()
	stop := context.AfterFunc(ctx, func() {
		command.Cancel()
		outputReader.Close()
		errorReader.Close()
	})
	defer stop()
	outputErrors := make(chan error, 2)
	go func() {
		_, err := io.Copy(stdout, outputReader)
		outputErrors <- err
	}()
	go func() {
		_, err := io.Copy(stderr, errorReader)
		outputErrors <- err
	}()
	go func() {
		io.Copy(input, stdin)
		input.Close()
	}()
	err = command.Wait()
	if outputError := errors.Join(<-outputErrors, <-outputErrors); outputError != nil {
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
