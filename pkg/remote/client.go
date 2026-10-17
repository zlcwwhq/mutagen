package remote

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/mutagen-io/mutagen/pkg/multiplexing"
)

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
// remote command. The caller must unblock a blocking stdin reader after return
// if it needs to reclaim the input forwarding goroutine.
func Execute(ctx context.Context, connection io.ReadWriteCloser, request *Request, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	// Wait for the route to reach an executing endpoint. Any hop that cannot
	// establish the route reports why instead of simply disconnecting.
	stopRoute := context.AfterFunc(ctx, func() { connection.Close() })
	var route status
	err := readMessage(connection, &route)
	if !stopRoute() {
		return 0, ctx.Err()
	} else if err != nil {
		connection.Close()
		return 0, fmt.Errorf("unable to receive route status: %w", err)
	} else if route.Error != "" {
		connection.Close()
		return 0, errors.New(route.Error)
	}

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
			return 0, fmt.Errorf("unable to open command stream: %w", err)
		}
		streams[i] = stream
	}
	control, input, output, errorOutput := streams[0], streams[1], streams[2], streams[3]
	if err := writeMessage(control, request); err != nil {
		return 0, fmt.Errorf("unable to send command: %w", err)
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
			return 0, ctx.Err()
		}
	}
	select {
	case err := <-inputErrors:
		return 0, fmt.Errorf("unable to read command input: %w", err)
	default:
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	} else if forwardingError := errors.Join(forwardingErrors[:]...); forwardingError != nil {
		return 0, fmt.Errorf("unable to forward command output: %w", forwardingError)
	} else if responseErr != nil {
		return 0, fmt.Errorf("unable to receive command result: %w", responseErr)
	}
	if _, err := control.Write([]byte{0}); err != nil {
		return 0, fmt.Errorf("unable to acknowledge command result: %w", err)
	}
	// The multiplexer buffers writes. Keep it alive until the server receives
	// the acknowledgement and closes the connection.
	<-mux.Closed()
	if response.Error != "" {
		return response.ExitCode, errors.New(response.Error)
	}
	return response.ExitCode, nil
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
