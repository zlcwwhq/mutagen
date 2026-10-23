package forwarding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/multiplexing"
	"github.com/mutagen-io/mutagen/pkg/state"
)

// testOperationTimeout bounds every blocking operation performed by the
// controller tests so that a regression fails the test instead of hanging it.
// It is a deadline, not an estimate of how long an operation takes.
const testOperationTimeout = 10 * time.Second

// testListenerEndpoint is a source Endpoint that wraps a TCP listener. It
// exists because this package can't import the local endpoint package (which
// imports this package).
type testListenerEndpoint struct {
	// listener is the underlying listener.
	listener net.Listener
}

// newTestListenerEndpoint creates a new test listener endpoint on a loopback
// address and registers its shutdown with the test.
func newTestListenerEndpoint(t *testing.T) *testListenerEndpoint {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("unable to create source listener:", err)
	}
	endpoint := &testListenerEndpoint{listener: listener}
	t.Cleanup(func() { endpoint.Shutdown() })
	return endpoint
}

// TransportErrors implements Endpoint.TransportErrors.
func (e *testListenerEndpoint) TransportErrors() <-chan error {
	return nil
}

// Open implements Endpoint.Open.
func (e *testListenerEndpoint) Open() (net.Conn, error) {
	return e.listener.Accept()
}

// Shutdown implements Endpoint.Shutdown.
func (e *testListenerEndpoint) Shutdown() error {
	return e.listener.Close()
}

// dial dials the endpoint's listener.
func (e *testListenerEndpoint) dial(t *testing.T) net.Conn {
	t.Helper()
	connection, err := net.DialTimeout("tcp", e.listener.Addr().String(), testOperationTimeout)
	if err != nil {
		t.Fatal("unable to dial source:", err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

// testDialerEndpoint is a destination Endpoint that dials a TCP address. Its
// target can be switched while forwarding is running, and it can be made to
// fail with an injected error instead of dialing.
type testDialerEndpoint struct {
	// dialingCtx regulates dialing operations.
	dialingCtx context.Context
	// dialingCancel cancels dialingCtx.
	dialingCancel context.CancelFunc
	// dialer is the underlying dialer.
	dialer net.Dialer
	// targetLock guards address and err.
	targetLock sync.Mutex
	// address is the address to dial.
	address string
	// err, if non-nil, is returned from Open instead of dialing.
	err error
}

// newTestDialerEndpoint creates a new test dialer endpoint targeting the
// specified address and registers its shutdown with the test.
func newTestDialerEndpoint(t *testing.T, address string) *testDialerEndpoint {
	dialingCtx, dialingCancel := context.WithCancel(context.Background())
	endpoint := &testDialerEndpoint{
		dialingCtx:    dialingCtx,
		dialingCancel: dialingCancel,
		address:       address,
	}
	t.Cleanup(func() { endpoint.Shutdown() })
	return endpoint
}

// setTarget sets the address that the endpoint dials, or (if err is non-nil)
// the error that it returns from Open instead of dialing.
func (e *testDialerEndpoint) setTarget(address string, err error) {
	e.targetLock.Lock()
	defer e.targetLock.Unlock()
	e.address = address
	e.err = err
}

// TransportErrors implements Endpoint.TransportErrors.
func (e *testDialerEndpoint) TransportErrors() <-chan error {
	return nil
}

// Open implements Endpoint.Open.
func (e *testDialerEndpoint) Open() (net.Conn, error) {
	e.targetLock.Lock()
	address, err := e.address, e.err
	e.targetLock.Unlock()
	if err != nil {
		return nil, err
	}
	return e.dialer.DialContext(e.dialingCtx, "tcp", address)
}

// Shutdown implements Endpoint.Shutdown.
func (e *testDialerEndpoint) Shutdown() error {
	e.dialingCancel()
	return nil
}

// testEchoBackend is a TCP server that echoes everything it receives on each
// accepted connection.
type testEchoBackend struct {
	// listener is the underlying listener.
	listener net.Listener
	// acceptDone is closed when the accept loop exits.
	acceptDone chan struct{}
	// connectionsLock guards connections.
	connectionsLock sync.Mutex
	// connections are the accepted connections.
	connections []net.Conn
}

// newTestEchoBackend starts a new echo backend on a loopback address and
// registers its teardown with the test.
func newTestEchoBackend(t *testing.T) *testEchoBackend {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("unable to create backend listener:", err)
	}
	backend := &testEchoBackend{
		listener:   listener,
		acceptDone: make(chan struct{}),
	}
	go backend.accept()
	t.Cleanup(func() {
		backend.closeListener()
		backend.connectionsLock.Lock()
		for _, connection := range backend.connections {
			connection.Close()
		}
		backend.connectionsLock.Unlock()
	})
	return backend
}

// accept is the backend's accept loop.
func (b *testEchoBackend) accept() {
	defer close(b.acceptDone)
	for {
		connection, err := b.listener.Accept()
		if err != nil {
			return
		}
		b.connectionsLock.Lock()
		b.connections = append(b.connections, connection)
		b.connectionsLock.Unlock()
		go func() {
			defer connection.Close()
			buffer := make([]byte, 1024)
			for {
				count, err := connection.Read(buffer)
				if count > 0 {
					if _, err := connection.Write(buffer[:count]); err != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
}

// address returns the backend's listening address.
func (b *testEchoBackend) address() string {
	return b.listener.Addr().String()
}

// closeListener stops accepting new connections and waits for the accept loop
// to exit. Connections that were already accepted keep echoing.
func (b *testEchoBackend) closeListener() {
	b.listener.Close()
	<-b.acceptDone
}

// newTestController creates a controller with only the fields that forward
// requires.
func newTestController(t *testing.T) *controller {
	tracker := state.NewTracker()
	t.Cleanup(tracker.Terminate)
	session := &Session{}
	return &controller{
		logger:    logging.NewLogger(logging.LevelDebug, io.Discard),
		stateLock: state.NewTrackingLock(tracker),
		session:   session,
		state: &State{
			Session:          session,
			SourceState:      &EndpointState{},
			DestinationState: &EndpointState{},
		},
	}
}

// startForwarding runs the controller's forwarding loop in the background and
// returns a channel that receives its result.
func startForwarding(c *controller, source, destination Endpoint) <-chan error {
	forwardingErrors := make(chan error, 1)
	go func() {
		forwardingErrors <- c.forward(source, destination)
	}()
	return forwardingErrors
}

// waitForForwardingTermination waits for the forwarding loop to return and
// returns its error.
func waitForForwardingTermination(t *testing.T, forwardingErrors <-chan error) error {
	t.Helper()
	select {
	case err := <-forwardingErrors:
		return err
	case <-time.After(testOperationTimeout):
		t.Fatal("forwarding loop did not terminate")
		return nil
	}
}

// checkForwardingRunning verifies that the forwarding loop hasn't returned.
func checkForwardingRunning(t *testing.T, forwardingErrors <-chan error) {
	t.Helper()
	select {
	case err := <-forwardingErrors:
		t.Fatal("forwarding loop terminated unexpectedly:", err)
	default:
	}
}

// checkEcho verifies that a message written to the connection is echoed back.
func checkEcho(t *testing.T, connection net.Conn, message string) {
	t.Helper()
	connection.SetDeadline(time.Now().Add(testOperationTimeout))
	defer connection.SetDeadline(time.Time{})
	if _, err := connection.Write([]byte(message)); err != nil {
		t.Fatalf("unable to write %q: %v", message, err)
	}
	buffer := make([]byte, len(message))
	if _, err := io.ReadFull(connection, buffer); err != nil {
		t.Fatalf("unable to read echo of %q: %v", message, err)
	} else if string(buffer) != message {
		t.Fatalf("echo (%q) does not match message (%q)", buffer, message)
	}
}

// checkClosed verifies that the peer closes the connection without sending
// any data.
func checkClosed(t *testing.T, connection net.Conn) {
	t.Helper()
	connection.SetReadDeadline(time.Now().Add(testOperationTimeout))
	count, err := connection.Read(make([]byte, 1))
	var netErr net.Error
	if err == nil {
		t.Fatalf("read %d byte(s) from connection that should be closed", count)
	} else if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("connection was not closed")
	}
}

// checkState verifies the forwarding status, connection counts, and last
// error. If expectedError is empty, then the last error must be empty,
// otherwise it must contain expectedError.
func checkState(t *testing.T, c *controller, expectedOpen, expectedTotal uint64, expectedError string) {
	t.Helper()
	state := c.currentState()
	if state.Status != Status_ForwardingConnections {
		t.Errorf("status (%s) is not forwarding", state.Status)
	}
	if state.OpenConnections != expectedOpen {
		t.Errorf("open connection count (%d) does not match expected (%d)", state.OpenConnections, expectedOpen)
	}
	if state.TotalConnections != expectedTotal {
		t.Errorf("total connection count (%d) does not match expected (%d)", state.TotalConnections, expectedTotal)
	}
	if expectedError == "" && state.LastError != "" {
		t.Errorf("last error (%q) is not empty", state.LastError)
	} else if !strings.Contains(state.LastError, expectedError) {
		t.Errorf("last error (%q) does not contain %q", state.LastError, expectedError)
	}
	if t.Failed() {
		t.FailNow()
	}
}

// TestForwardDestinationOpenFailureIsPerConnection tests that a destination
// open failure only closes the connection that triggered it, leaving existing
// connections and the forwarding loop running, that it's recorded as the last
// error without counting a connection, and that the next successful open
// clears that error.
func TestForwardDestinationOpenFailureIsPerConnection(t *testing.T) {
	// Set up the endpoints and start forwarding.
	controller := newTestController(t)
	source := newTestListenerEndpoint(t)
	backend := newTestEchoBackend(t)
	destination := newTestDialerEndpoint(t, backend.address())
	forwardingErrors := startForwarding(controller, source, destination)

	// Establish a connection that stays open throughout the test.
	first := source.dial(t)
	checkEcho(t, first, "first")
	checkState(t, controller, 1, 1, "")

	// Stop the backend from accepting connections so that the next dial is
	// refused. The connection that it already accepted keeps working.
	backend.closeListener()

	// Verify that a connection whose dial fails is closed, and that this is
	// recorded without affecting the first connection or the loop. The error
	// is recorded before the connection is closed, so it's visible once we
	// observe the closure.
	refused := source.dial(t)
	checkClosed(t, refused)
	checkState(t, controller, 1, 1, "unable to open forwarding connection")
	checkEcho(t, first, "first after dial failure")
	checkForwardingRunning(t, forwardingErrors)

	// Verify the same for a stream rejection, which is how a remote destination
	// reports that its receive backlog is full.
	destination.setTarget("", multiplexing.ErrStreamRejected)
	rejected := source.dial(t)
	checkClosed(t, rejected)
	checkState(t, controller, 1, 1, multiplexing.ErrStreamRejected.Error())
	checkEcho(t, first, "first after stream rejection")
	checkForwardingRunning(t, forwardingErrors)

	// Restore the backend by switching the destination to a new listener (we
	// don't relisten on the old port because rebinding it isn't reliable on
	// all platforms) and verify that new connections are forwarded again and
	// that the last error is cleared.
	replacement := newTestEchoBackend(t)
	destination.setTarget(replacement.address(), nil)
	restored := source.dial(t)
	checkEcho(t, restored, "restored")
	checkState(t, controller, 2, 2, "")
	checkEcho(t, first, "first after restoration")

	// Shut down the source and verify that the loop terminates promptly and
	// closes the connections that it was forwarding.
	source.Shutdown()
	if err := waitForForwardingTermination(t, forwardingErrors); err == nil {
		t.Error("forwarding loop terminated without error")
	}
	checkClosed(t, first)
	checkClosed(t, restored)
}

// TestForwardDestinationMultiplexerClosureTerminates tests that a destination
// open failure due to multiplexer closure terminates the forwarding loop and
// closes existing connections.
func TestForwardDestinationMultiplexerClosureTerminates(t *testing.T) {
	// Set up the endpoints and start forwarding.
	controller := newTestController(t)
	source := newTestListenerEndpoint(t)
	backend := newTestEchoBackend(t)
	destination := newTestDialerEndpoint(t, backend.address())
	forwardingErrors := startForwarding(controller, source, destination)

	// Establish a connection.
	first := source.dial(t)
	checkEcho(t, first, "first")
	checkState(t, controller, 1, 1, "")

	// Make the destination fail as a remote destination does once its
	// multiplexer has closed (wrapping the error to check that it's detected
	// through wrapping) and trigger an open.
	destination.setTarget("", fmt.Errorf("unable to open stream: %w", multiplexing.ErrMultiplexerClosed))
	second := source.dial(t)

	// Verify that the loop terminates with that error and closes both the new
	// and the existing connection.
	if err := waitForForwardingTermination(t, forwardingErrors); !errors.Is(err, multiplexing.ErrMultiplexerClosed) {
		t.Fatal("forwarding loop error does not indicate multiplexer closure:", err)
	}
	checkClosed(t, second)
	checkClosed(t, first)
}
