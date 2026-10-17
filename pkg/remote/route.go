package remote

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
)

// handshakeTimeout bounds the version handshake with a local submission.
const handshakeTimeout = 10 * time.Second

// Reject reports a routing failure to the submitting side of a route and
// closes the connection.
func Reject(connection io.ReadWriteCloser, err error) {
	writeMessage(connection, &status{Error: err.Error()})
	connection.Close()
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
