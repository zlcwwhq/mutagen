package netpipe

import (
	"context"
	"fmt"
	"net"

	"github.com/mutagen-io/mutagen/pkg/logging"
	remotecommands "github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/endpoint/remote"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
)

// synchronizationProtocolHandler implements the synchronization.ProtocolHandler
// interface for connecting to "remote" endpoints that actually exist in memory
// via an in-memory pipe.
type synchronizationProtocolHandler struct{}

// waitingSynchronizationEndpoint wraps and implements synchronization.Endpoint,
// but adds a waiting function that's invoked after invoking Shutdown on the
// underlying endpoint. It is necessary to ensure full endpoint shutdown in
// tests, where open file descriptors or handles can prevent temporary directory
// removal.
type waitingSynchronizationEndpoint struct {
	// Endpoint is the underlying endpoint.
	synchronization.Endpoint
	// wait is an arbitrary waiting function.
	wait func()
}

// Shutdown implements synchronization.Endpoint.Shutdown.
func (w *waitingSynchronizationEndpoint) Shutdown() error {
	// Shutdown on the underlying endpoint.
	result := w.Endpoint.Shutdown()

	// Perform the wait operation.
	w.wait()

	// Done.
	return result
}

// Dial starts an endpoint server in a background Goroutine and creates an
// endpoint client connected to the server via an in-memory connection.
func (h *synchronizationProtocolHandler) Connect(
	_ context.Context,
	logger *logging.Logger,
	url *urlpkg.URL,
	prompter string,
	session string,
	version synchronization.Version,
	configuration *synchronization.Configuration,
	alpha bool,
) (synchronization.Endpoint, error) {
	// Verify that the URL is of the correct kind and protocol.
	if url.Kind != urlpkg.Kind_Synchronization {
		panic("non-synchronization URL dispatched to synchronization protocol handler")
	} else if url.Protocol != Protocol_Netpipe {
		panic("non-netpipe URL dispatched to netpipe protocol handler")
	}

	// Create an in-memory network connection.
	clientConnection, serverConnection := net.Pipe()

	// Serve the endpoint in a background Goroutine. This will terminate once
	// the client connection is closed. We monitor for its termination so that
	// we can block on it in our endpoint wrapper.
	remoteEndpointDone := make(chan struct{})
	go func() {
		remote.ServeEndpoint(logger.Sublogger("remote"), serverConnection)
		close(remoteEndpointDone)
	}()

	// Create a client for this endpoint.
	endpoint, err := remote.NewEndpoint(
		logger,
		clientConnection,
		url.Path,
		session,
		version,
		configuration,
		alpha,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create in-memory endpoint client: %w", err)
	}

	// Wrap the client so that it blocks on the full shutdown of the remote
	// endpoint after closing the connection. This is necessary for testing,
	// where we need to ensure that all file descriptors or handles point to
	// temporary test directories are closed before attempting to remove those
	// directories. This is not necessary for other remote protocols in normal
	// usage (because we don't have the same constraints) or in testing (because
	// the underlying connection closure waits for agent process termination).
	endpoint = &waitingSynchronizationEndpoint{endpoint, func() { <-remoteEndpointDone }}

	// Success.
	return endpoint, nil
}

// waitingCommandChannel wraps and implements remote.Channel, but waits for the
// in-memory executor to terminate after closing the channel. As with
// waitingSynchronizationEndpoint, this is necessary for test cleanup.
type waitingCommandChannel struct {
	// Channel is the underlying channel.
	remotecommands.Channel
	// done is closed when the executor terminates.
	done <-chan struct{}
}

// Close implements remote.Channel.Close.
func (w *waitingCommandChannel) Close() error {
	result := w.Channel.Close()
	<-w.done
	return result
}

// ConnectCommands implements synchronization.CommandProtocolHandler by serving
// an executor in a background Goroutine connected via an in-memory connection.
// The executor shares the test process' command registry, so both endpoints'
// registrations coexist as they would on separate systems.
func (h *synchronizationProtocolHandler) ConnectCommands(
	ctx context.Context,
	logger *logging.Logger,
	url *urlpkg.URL,
	endpoint *remotecommands.Endpoint,
) (remotecommands.Channel, error) {
	// Verify that the URL is of the correct kind and protocol.
	if url.Kind != urlpkg.Kind_Synchronization {
		panic("non-synchronization URL dispatched to synchronization protocol handler")
	} else if url.Protocol != Protocol_Netpipe {
		panic("non-netpipe URL dispatched to netpipe protocol handler")
	}

	// Serve the executor in the background. No remote executable is installed.
	clientConnection, serverConnection := net.Pipe()
	executorDone := make(chan struct{})
	go func() {
		remotecommands.ServeChannel(context.Background(), logger.Sublogger("executor"), serverConnection, nil)
		close(executorDone)
	}()

	// Create the channel.
	channel, err := remotecommands.NewChannel(ctx, logger, clientConnection, endpoint)
	if err != nil {
		<-executorDone
		return nil, fmt.Errorf("unable to create in-memory command channel: %w", err)
	}
	return &waitingCommandChannel{channel, executorDone}, nil
}

func init() {
	// Register the netpipe protocol handler with the synchronization package.
	synchronization.ProtocolHandlers[Protocol_Netpipe] = &synchronizationProtocolHandler{}
}
