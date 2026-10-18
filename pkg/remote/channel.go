package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/multiplexing"
)

// livenessInterval is the interval within which each side of an agent
// connection must receive data from the other. It is a variable so that tests
// can shorten it.
var livenessInterval = 10 * time.Second

var (
	// errExecutionDisabled indicates that an endpoint does not execute commands.
	errExecutionDisabled = errors.New("remote commands are not enabled for this endpoint")
	// errLivenessTimeout indicates that an agent connection failed a liveness
	// check.
	errLivenessTimeout = errors.New("no data received from the opposite side of the connection")
)

// Endpoint describes one endpoint of a synchronization session for the
// purposes of command routing.
type Endpoint struct {
	// Session is the session identifier.
	Session string
	// Name is the session name, if any.
	Name string
	// Root is the synchronization root, interpreted on the endpoint's system.
	Root string
	// Alpha indicates whether the endpoint is alpha.
	Alpha bool
	// Submit indicates whether invocations on the endpoint's system may submit
	// commands to the opposite endpoint.
	Submit bool
	// Execute indicates whether the endpoint executes commands submitted from
	// the opposite endpoint.
	Execute bool
	// Prologue is the default shell script sourced before commands that request
	// it, unless a workspace override exists. Absolute and home-relative paths
	// are interpreted on the endpoint's system, while other paths are resolved
	// against Root.
	Prologue string
}

// Channel connects the daemon to one endpoint's command facilities. Routes
// returned by Accept and Open carry the protocol consumed by Execute and must
// be closed by the caller.
type Channel interface {
	// Accept waits for a command submitted on the endpoint's system. It fails
	// once the channel is closed.
	Accept() (io.ReadWriteCloser, error)
	// Open opens a route to the endpoint's command executor.
	Open(ctx context.Context) (io.ReadWriteCloser, error)
	// Done is closed when the channel fails or is closed.
	Done() <-chan struct{}
	// Close closes the channel, terminating its routes and commands.
	Close() error
}

// localChannel implements Channel for an endpoint on the daemon's system.
type localChannel struct {
	// root is the normalized synchronization root.
	root string
	// alpha indicates whether this is the alpha endpoint.
	alpha bool
	// execute indicates whether the endpoint executes commands.
	execute bool
	// prologue is the resolved prologue path, if any.
	prologue string
	// ctx is cancelled when the channel is closed.
	ctx context.Context
	// cancel cancels ctx.
	cancel context.CancelFunc
	// listener accepts local submissions. It is nil if submission is disabled.
	listener net.Listener
	// acceptor delivers local submissions to Accept.
	acceptor *acceptor
	// tasksLock serializes task registration with closure.
	tasksLock sync.Mutex
	// tasks tracks submission handling and command execution.
	tasks sync.WaitGroup
}

// NewLocalChannel creates a channel for an endpoint on the daemon's system.
// Commands are executed by the daemon process itself.
func NewLocalChannel(logger *logging.Logger, endpoint *Endpoint) (Channel, error) {
	root, err := filesystem.Normalize(endpoint.Root)
	if err != nil {
		return nil, fmt.Errorf("unable to normalize synchronization root: %w", err)
	}
	var prologue string
	if endpoint.Execute {
		if prologue, err = resolvePrologue(root, endpoint.Prologue); err != nil {
			return nil, fmt.Errorf("unable to resolve remote prologue: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	channel := &localChannel{
		root:     root,
		alpha:    endpoint.Alpha,
		execute:  endpoint.Execute,
		prologue: prologue,
		ctx:      ctx,
		cancel:   cancel,
		acceptor: newAcceptor(ctx),
	}
	if endpoint.Submit {
		if channel.listener, err = register(endpoint, root); err != nil {
			cancel()
			return nil, err
		}
		channel.tasks.Add(1)
		go func() {
			defer channel.tasks.Done()
			serveSubmissions(logger, channel.listener, channel.acceptor.deliver)
		}()
	}
	return channel, nil
}

// Accept implements Channel.Accept.
func (c *localChannel) Accept() (io.ReadWriteCloser, error) {
	return c.acceptor.accept()
}

// Open implements Channel.Open.
func (c *localChannel) Open(_ context.Context) (io.ReadWriteCloser, error) {
	if !c.execute {
		return nil, errExecutionDisabled
	}
	c.tasksLock.Lock()
	defer c.tasksLock.Unlock()
	if c.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	client, server := net.Pipe()
	c.tasks.Add(1)
	go func() {
		defer c.tasks.Done()
		Serve(c.ctx, server, c.root, c.prologue, c.alpha)
	}()
	return client, nil
}

// Done implements Channel.Done.
func (c *localChannel) Done() <-chan struct{} {
	return c.ctx.Done()
}

// Close implements Channel.Close.
func (c *localChannel) Close() error {
	c.tasksLock.Lock()
	c.cancel()
	c.tasksLock.Unlock()
	var err error
	if c.listener != nil {
		err = c.listener.Close()
	}
	c.tasks.Wait()
	return err
}

// ExecutableInstaller installs the remote executable on an executor's system,
// if necessary, and returns its path.
type ExecutableInstaller func() (string, error)

// initializeResponse reports executor initialization results.
type initializeResponse struct {
	// Error is the initialization error, if any.
	Error string
	// Warning is a non-fatal initialization problem, if any.
	Warning string
	// Executable is the remote executable path on the endpoint's system, if
	// submission is enabled.
	Executable string
}

// channelMultiplexerConfiguration returns the configuration for multiplexing
// routes over an agent connection. Heartbeats are transmitted to keep idle
// connections alive, but the multiplexer's heartbeat requirement is replaced by
// livenessStream: heartbeats queue behind route data, so a slow link would
// otherwise fail while transferring command I/O.
func channelMultiplexerConfiguration() *multiplexing.Configuration {
	configuration := multiplexing.DefaultConfiguration()
	configuration.HeartbeatTransmitInterval = livenessInterval / 2
	configuration.MaximumHeartbeatReceiveInterval = 0
	return configuration
}

// livenessStream records data received over an agent connection, any of which
// demonstrates that the connection is alive.
type livenessStream struct {
	io.ReadWriteCloser
	// received indicates whether data has been received since the last check.
	received atomic.Bool
	// expired indicates whether the connection failed a liveness check.
	expired atomic.Bool
}

// Read implements io.Reader.Read.
func (s *livenessStream) Read(buffer []byte) (int, error) {
	count, err := s.ReadWriteCloser.Read(buffer)
	if count > 0 {
		s.received.Store(true)
	}
	return count, err
}

// monitor closes the multiplexer using the stream if no data is received within
// the specified liveness interval. It returns once the multiplexer is closed.
func (s *livenessStream) monitor(multiplexer *multiplexing.Multiplexer, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !s.received.Swap(false) {
				s.expired.Store(true)
				multiplexer.Close()
				return
			}
		case <-multiplexer.Closed():
			return
		}
	}
}

// agentChannel implements Channel over an agent executor connection.
type agentChannel struct {
	// multiplexer multiplexes routes over the agent connection.
	multiplexer *multiplexing.Multiplexer
}

// initializeExecutor performs the daemon side of executor initialization.
func initializeExecutor(logger *logging.Logger, carrier multiplexing.Carrier, endpoint *Endpoint) error {
	if err := writeMessage(carrier, endpoint); err != nil {
		return fmt.Errorf("unable to send initialization request: %w", err)
	}
	var response initializeResponse
	if err := readMessage(carrier, &response); err != nil {
		return fmt.Errorf("unable to receive initialization response: %w", err)
	} else if response.Error != "" {
		return fmt.Errorf("executor initialization failed: %s", response.Error)
	}
	if response.Warning != "" {
		logger.Warn("Remote executable unavailable:", response.Warning)
	} else if response.Executable != "" {
		logger.Debug("Remote executable available at", response.Executable)
	}
	return nil
}

// NewChannel initializes the executor at the other end of an agent connection
// and creates a channel using it. It owns and closes the stream. Cancelling the
// context aborts initialization by closing the stream, but has no effect once
// the channel is created.
func NewChannel(ctx context.Context, logger *logging.Logger, stream io.ReadWriteCloser, endpoint *Endpoint) (Channel, error) {
	live := &livenessStream{ReadWriteCloser: stream}
	carrier := multiplexing.NewCarrierFromStream(live)

	// Initialize the executor. A stalled exchange can only be interrupted by
	// closing the stream.
	stop := context.AfterFunc(ctx, func() { carrier.Close() })
	err := initializeExecutor(logger, carrier, endpoint)
	if !stop() {
		return nil, ctx.Err()
	} else if err != nil {
		carrier.Close()
		return nil, err
	}

	// Multiplex routes over the connection.
	multiplexer := multiplexing.Multiplex(carrier, false, channelMultiplexerConfiguration())
	go live.monitor(multiplexer, livenessInterval)
	return &agentChannel{multiplexer}, nil
}

// Accept implements Channel.Accept.
func (c *agentChannel) Accept() (io.ReadWriteCloser, error) {
	stream, err := c.multiplexer.AcceptStream(context.Background())
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// Open implements Channel.Open.
func (c *agentChannel) Open(ctx context.Context) (io.ReadWriteCloser, error) {
	stream, err := c.multiplexer.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// Done implements Channel.Done.
func (c *agentChannel) Done() <-chan struct{} {
	return c.multiplexer.Closed()
}

// Close implements Channel.Close.
func (c *agentChannel) Close() error {
	return c.multiplexer.Close()
}

// serverInitialization is the result of the executor side of initialization.
type serverInitialization struct {
	// endpoint is the endpoint description.
	endpoint Endpoint
	// root is the normalized synchronization root.
	root string
	// prologue is the resolved prologue path, if any.
	prologue string
	// listener accepts local submissions. It is nil if submission is disabled.
	listener net.Listener
	// err is the initialization error, if any.
	err error
}

// initializeServer performs the executor side of initialization, installing
// the remote executable using install (if non-nil) if submission is enabled.
func initializeServer(carrier multiplexing.Carrier, install ExecutableInstaller) (result serverInitialization) {
	// Receive the endpoint description.
	if err := readMessage(carrier, &result.endpoint); err != nil {
		result.err = fmt.Errorf("unable to receive initialization request: %w", err)
		return
	}
	endpoint := &result.endpoint

	// Register for local submissions and report initialization results. The
	// remote executable is only needed by submitting endpoints, and failure to
	// install it is reported as a warning, since it may be installed
	// separately.
	fail := func(err error) serverInitialization {
		writeMessage(carrier, &initializeResponse{Error: err.Error()})
		return serverInitialization{err: err}
	}
	root, err := filesystem.Normalize(endpoint.Root)
	if err != nil {
		return fail(fmt.Errorf("unable to normalize synchronization root: %w", err))
	}
	result.root = root
	if endpoint.Execute {
		if result.prologue, err = resolvePrologue(root, endpoint.Prologue); err != nil {
			return fail(fmt.Errorf("unable to resolve remote prologue: %w", err))
		}
	}
	response := &initializeResponse{}
	if endpoint.Submit {
		if result.listener, err = register(endpoint, root); err != nil {
			return fail(err)
		}
		if install != nil {
			if response.Executable, err = install(); err != nil {
				response.Warning = err.Error()
			}
		}
	}
	if err := writeMessage(carrier, response); err != nil {
		if result.listener != nil {
			result.listener.Close()
		}
		return serverInitialization{err: fmt.Errorf("unable to send initialization response: %w", err)}
	}
	return
}

// ServeChannel serves the executor side of an agent connection established by
// NewChannel. If install is non-nil and submission is enabled, then it's used to
// install the remote executable. It owns and closes the stream and returns once
// the connection or context terminates.
func ServeChannel(ctx context.Context, logger *logging.Logger, stream io.ReadWriteCloser, install ExecutableInstaller) error {
	live := &livenessStream{ReadWriteCloser: stream}
	carrier := multiplexing.NewCarrierFromStream(live)
	defer carrier.Close()

	// Initialize in the background so that cancellation is observed even if
	// the stream can't be closed to interrupt initialization (as is the case
	// for standard input/output). If initialization is abandoned, then release
	// any registration that initialization completes.
	initialized := make(chan serverInitialization, 1)
	go func() {
		initialized <- initializeServer(carrier, install)
	}()
	var initialization serverInitialization
	select {
	case initialization = <-initialized:
	case <-ctx.Done():
		go func() {
			if abandoned := <-initialized; abandoned.listener != nil {
				abandoned.listener.Close()
			}
		}()
		return ctx.Err()
	}
	if initialization.err != nil {
		return initialization.err
	}
	endpoint, root, prologue, listener := &initialization.endpoint, initialization.root, initialization.prologue, initialization.listener

	// Multiplex routes and terminate all routes and commands on disconnection.
	multiplexer := multiplexing.Multiplex(carrier, true, channelMultiplexerConfiguration())
	ctx, cancel := context.WithCancel(ctx)
	var tasks sync.WaitGroup
	defer func() {
		cancel()
		multiplexer.Close()
		if listener != nil {
			listener.Close()
		}
		tasks.Wait()
	}()
	tasks.Add(1)
	go func(interval time.Duration) {
		defer tasks.Done()
		live.monitor(multiplexer, interval)
	}(livenessInterval)
	go func() {
		select {
		case <-multiplexer.Closed():
			cancel()
		case <-ctx.Done():
		}
	}()

	// Forward local submissions to the daemon.
	if listener != nil {
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			serveSubmissions(logger, listener, func(connection net.Conn) {
				stream, err := multiplexer.OpenStream(ctx)
				if err != nil {
					Reject(connection, fmt.Errorf("unable to reach the Mutagen daemon: %w", err))
					return
				}
				Splice(connection, stream)
			})
		}()
	}

	// Execute commands routed from the opposite endpoint.
	for {
		stream, err := multiplexer.AcceptStream(ctx)
		if err != nil {
			break
		}
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			if !endpoint.Execute {
				Reject(stream, errExecutionDisabled)
			} else if err := Serve(ctx, stream, root, prologue, endpoint.Alpha); err != nil {
				logger.Debug("Command route failed:", err)
			}
		}()
	}
	if err := multiplexer.InternalError(); err != nil {
		return err
	} else if live.expired.Load() {
		return errLivenessTimeout
	} else if err := ctx.Err(); err != nil {
		return err
	}
	return multiplexing.ErrMultiplexerClosed
}
