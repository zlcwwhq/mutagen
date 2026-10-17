package remote

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
)

const (
	// handshakeTimeout bounds the version handshake with a local submission.
	handshakeTimeout = 10 * time.Second
	// submissionTimeout bounds the time that the daemon waits for a
	// submission's header.
	submissionTimeout = 30 * time.Second
)

// ReceiveSubmission reads the header of a submission arriving at the daemon and
// returns the requested operation. The connection is closed if the header isn't
// received within a timeout or before cancellation.
func ReceiveSubmission(ctx context.Context, connection io.ReadWriteCloser) (Operation, error) {
	ctx, cancel := context.WithTimeout(ctx, submissionTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	var header submission
	err := readMessage(connection, &header)
	if !stop() {
		return 0, fmt.Errorf("submission not received: %w", ctx.Err())
	} else if err != nil {
		return 0, fmt.Errorf("unable to receive submission: %w", err)
	} else if header.Operation != OperationExecute && header.Operation != OperationFlush {
		return 0, fmt.Errorf("unsupported submission operation: %d", header.Operation)
	}
	return header.Operation, nil
}

// Reject reports a routing failure to the submitting side of a route and
// closes the connection.
func Reject(connection io.ReadWriteCloser, err error) {
	writeMessage(connection, &status{Error: err.Error()})
	connection.Close()
}

// RespondToFlush reports the outcome of a flush submission, including a report
// of content that the session couldn't synchronize if it succeeded, and closes
// the connection. It returns any error encountered sending the response.
func RespondToFlush(connection io.ReadWriteCloser, report *Report, err error) error {
	defer connection.Close()
	if err != nil {
		return writeMessage(connection, &status{Error: err.Error()})
	} else if report == nil {
		report = &Report{}
	}
	if report, err = boundReport(report); err != nil {
		return writeMessage(connection, &status{Error: err.Error()})
	}
	return writeMessage(connection, &flushResponse{Report: report})
}

// Splice forwards data between the two halves of a route until both directions
// reach end-of-file or either fails, then closes both connections. Half-closure
// is propagated when the destination supports it.
func Splice(first, second io.ReadWriteCloser) {
	defer first.Close()
	defer second.Close()
	results := make(chan error, 2)
	forward := func(destination io.Writer, source io.Reader) {
		_, err := io.Copy(destination, source)
		if err == nil {
			if closer, ok := destination.(interface{ CloseWrite() error }); ok {
				err = closer.CloseWrite()
			} else {
				// Without half-closure, end-of-file cannot be forwarded, so
				// the route is finished.
				err = io.EOF
			}
		}
		results <- err
	}
	go forward(first, second)
	go forward(second, first)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			return
		}
	}
}

// serveSubmissions accepts local submissions until the listener is closed,
// passing each to handle after a successful version handshake. It waits for
// all handlers to return before returning.
func serveSubmissions(logger *logging.Logger, listener net.Listener, handle func(net.Conn)) {
	var handlers sync.WaitGroup
	defer handlers.Wait()
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			connection.SetDeadline(time.Now().Add(handshakeTimeout))
			if err := mutagen.ServerVersionHandshake(connection); err != nil {
				// Registry liveness probes connect without handshaking.
				logger.Debug("Local submission handshake failed:", err)
				connection.Close()
				return
			}
			connection.SetDeadline(time.Time{})
			handle(connection)
		}()
	}
}

// acceptor adapts submissions to a blocking Accept method.
type acceptor struct {
	// ctx is cancelled when the acceptor closes.
	ctx context.Context
	// submissions delivers submissions to Accept.
	submissions chan net.Conn
}

// newAcceptor creates an acceptor that is closed with the specified context.
func newAcceptor(ctx context.Context) *acceptor {
	return &acceptor{ctx: ctx, submissions: make(chan net.Conn)}
}

// deliver passes a submission to Accept, closing it if the acceptor closes.
func (a *acceptor) deliver(connection net.Conn) {
	select {
	case a.submissions <- connection:
	case <-a.ctx.Done():
		connection.Close()
	}
}

// accept waits for a submission.
func (a *acceptor) accept() (io.ReadWriteCloser, error) {
	select {
	case connection := <-a.submissions:
		return connection, nil
	case <-a.ctx.Done():
		return nil, net.ErrClosed
	}
}
