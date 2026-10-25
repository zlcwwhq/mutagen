package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/multiplexing"
)

// ErrConnectionLost indicates that the connection to the executing endpoint was
// lost before a command's result was received, such as because the session
// stopped synchronizing, which terminates the command.
var ErrConnectionLost = errors.New("the connection to the executing endpoint was lost")

// commandMultiplexerConfiguration returns the configuration for multiplexing a
// single command's streams. Heartbeats are disabled because each hop of the
// route already fails when its underlying connection or process terminates, and
// heartbeats queued behind command output on a slow link would otherwise cause
// spurious failures.
func commandMultiplexerConfiguration() *multiplexing.Configuration {
	configuration := multiplexing.DefaultConfiguration()
	configuration.HeartbeatTransmitInterval = 0
	configuration.MaximumHeartbeatReceiveInterval = 0
	return configuration
}

// Execute runs one command over an established route (see Dial). It owns and
// closes the connection. Cancellation closes the connection, terminating the
// remote command, as does losing the connection, which is reported with an
// error wrapping ErrConnectionLost. The caller must unblock a blocking stdin
// reader after return if it needs to reclaim the input forwarding goroutine.
func Execute(ctx context.Context, connection io.ReadWriteCloser, request *Request, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	response, err := execute(ctx, connection, request, stdin, stdout, stderr)
	if err != nil {
		return 0, err
	} else if response.Error != "" {
		return response.ExitCode, errors.New(response.Error)
	}
	return response.ExitCode, nil
}

// execute implements Execute, returning the result sent by the executor.
func execute(ctx context.Context, connection io.ReadWriteCloser, request *Request, stdin io.Reader, stdout, stderr io.Writer) (*result, error) {
	if _, err := connect(ctx, connection); err != nil {
		return nil, err
	}
	return exchange(ctx, connection, request, stdin, stdout, stderr)
}

// connect waits for a route to reach an executing endpoint and returns the
// route's status, which reports the executor's features, so that the request
// can be adapted to them before exchange sends it. Any hop that cannot
// establish the route reports why instead of simply disconnecting. The
// connection is closed if this fails.
func connect(ctx context.Context, connection io.ReadWriteCloser) (*status, error) {
	stopRoute := context.AfterFunc(ctx, func() { connection.Close() })
	var route status
	err := readMessage(connection, &route)
	if !stopRoute() {
		return nil, ctx.Err()
	} else if err != nil {
		connection.Close()
		return nil, fmt.Errorf("unable to receive route status: %w", err)
	} else if route.Error != "" {
		connection.Close()
		return nil, errors.New(route.Error)
	}
	return &route, nil
}

// exchange sends a request over a route that has reached an executing endpoint
// (see connect), forwards the command's streams, and returns the result sent
// by the executor. It owns and closes the connection.
func exchange(ctx context.Context, connection io.ReadWriteCloser, request *Request, stdin io.Reader, stdout, stderr io.Writer) (*result, error) {
	// Keep control and all three standard streams independent so that blocked
	// command input cannot prevent output, exit status, or cancellation.
	mux := multiplexing.Multiplex(multiplexing.NewCarrierFromStream(connection), true, commandMultiplexerConfiguration())
	defer mux.Close()
	stop := context.AfterFunc(ctx, func() { mux.Close() })
	defer stop()
	var streams [4]*multiplexing.Stream
	for i := range streams {
		stream, err := mux.OpenStream(ctx)
		if err != nil {
			return nil, fmt.Errorf("unable to open command stream: %w", err)
		}
		streams[i] = stream
	}
	control, input, output, errorOutput := streams[0], streams[1], streams[2], streams[3]
	if err := writeMessage(control, request); err != nil {
		return nil, fmt.Errorf("unable to send command: %w", err)
	}

	// Forward input without waiting for EOF before collecting the exit status.
	inputErrors := make(chan error, 1)
	go func() {
		reader := &inputReader{Reader: stdin}
		io.Copy(input, reader)
		if reader.err != nil {
			inputErrors <- reader.err
			mux.Close()
		}
		input.CloseWrite()
	}()
	outputErrors := make(chan error, 2)
	for _, pair := range []struct {
		source      io.Reader
		destination io.Writer
	}{{output, stdout}, {errorOutput, stderr}} {
		go func(source io.Reader, destination io.Writer) {
			_, err := io.Copy(destination, source)
			if err != nil {
				mux.Close()
			}
			outputErrors <- err
		}(pair.source, pair.destination)
	}

	// Wait for both output streams before acknowledging the result. This keeps
	// the server alive until its final buffered output has been received.
	var response result
	responseErr := readMessage(control, &response)
	if responseErr != nil {
		mux.Close()
	}
	var forwardingErrors [2]error
	for i := range forwardingErrors {
		select {
		case forwardingErrors[i] = <-outputErrors:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case err := <-inputErrors:
		return nil, fmt.Errorf("unable to read command input: %w", err)
	default:
	}
	// The multiplexer only fails internally, rather than being closed here,
	// when the connection fails. Before the result arrives, that terminates
	// the command (see Serve).
	if err := ctx.Err(); err != nil {
		return nil, err
	} else if internalError := mux.InternalError(); responseErr != nil && internalError != nil {
		return nil, fmt.Errorf("%w (%v)", ErrConnectionLost, internalError)
	} else if forwardingError := errors.Join(forwardingErrors[:]...); forwardingError != nil {
		return nil, fmt.Errorf("unable to forward command output: %w", forwardingError)
	} else if responseErr != nil {
		return nil, fmt.Errorf("unable to receive command result: %w", responseErr)
	}
	if _, err := control.Write([]byte{0}); err != nil {
		return nil, fmt.Errorf("unable to acknowledge command result: %w", err)
	}
	// The multiplexer buffers writes. Keep it alive until the server receives
	// the acknowledgement and closes the connection.
	<-mux.Closed()
	return &response, nil
}

// Flush waits for the synchronization cycle requested by a flush submission (see
// Dial) to complete and returns a report of content that the session couldn't
// synchronize. It owns and closes the connection. Cancellation abandons the
// wait, but not the synchronization cycle.
func Flush(ctx context.Context, connection io.ReadWriteCloser) (*Report, error) {
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	var response flushResponse
	err := readMessage(connection, &response)
	if !stop() {
		return nil, ctx.Err()
	} else if err != nil {
		return nil, fmt.Errorf("unable to receive synchronization status: %w", err)
	} else if response.Error != "" {
		return nil, errors.New(response.Error)
	} else if response.Report == nil {
		return nil, errors.New("no synchronization report received")
	}
	return response.Report, nil
}

// Fetch reads the entry at a slash-separated path, relative to the opposite
// endpoint's synchronization root, over an established route (see Dial). It
// owns and closes the connection. An empty path refers to the root itself.
func Fetch(ctx context.Context, connection io.ReadWriteCloser, path string) (*Entry, error) {
	if path == "" {
		path = "."
	}
	var data bytes.Buffer
	response, err := execute(ctx, connection, &Request{File: path}, strings.NewReader(""), &data, io.Discard)
	if err != nil {
		return nil, err
	} else if response.Error != "" {
		return nil, errors.New(response.Error)
	} else if response.Entry == nil {
		return nil, errors.New("no entry received")
	}
	entry := response.Entry
	if data.Len() > 0 {
		entry.Data = data.Bytes()
	}
	return entry, nil
}

// inputReader distinguishes failed source reads from normal remote input closure.
type inputReader struct {
	io.Reader
	err error
}

// Read records non-EOF errors encountered while forwarding input.
func (r *inputReader) Read(buffer []byte) (int, error) {
	count, err := r.Reader.Read(buffer)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return count, err
}
