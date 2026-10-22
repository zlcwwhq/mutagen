package integration

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"github.com/mutagen-io/mutagen/cmd"
	"github.com/mutagen-io/mutagen/pkg/agent"
	"github.com/mutagen-io/mutagen/pkg/daemon"
	"github.com/mutagen-io/mutagen/pkg/forwarding"
	"github.com/mutagen-io/mutagen/pkg/grpcutil"
	"github.com/mutagen-io/mutagen/pkg/identifier"
	"github.com/mutagen-io/mutagen/pkg/ipc"
	"github.com/mutagen-io/mutagen/pkg/logging"
	daemonsvc "github.com/mutagen-io/mutagen/pkg/service/daemon"
	forwardingsvc "github.com/mutagen-io/mutagen/pkg/service/forwarding"
	promptingsvc "github.com/mutagen-io/mutagen/pkg/service/prompting"
	synchronizationsvc "github.com/mutagen-io/mutagen/pkg/service/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization"

	// Explicitly import packages that need to register protocol handlers.
	_ "github.com/mutagen-io/mutagen/pkg/forwarding/protocols/docker"
	_ "github.com/mutagen-io/mutagen/pkg/forwarding/protocols/local"
	_ "github.com/mutagen-io/mutagen/pkg/forwarding/protocols/ssh"
	_ "github.com/mutagen-io/mutagen/pkg/integration/protocols/netpipe"
	_ "github.com/mutagen-io/mutagen/pkg/synchronization/protocols/docker"
	_ "github.com/mutagen-io/mutagen/pkg/synchronization/protocols/local"
	_ "github.com/mutagen-io/mutagen/pkg/synchronization/protocols/ssh"
)

// forwardingManager is the forwarding session manager for the integration
// testing daemon. It is exposed for integration tests that operate at the API
// level (as opposed to the gRPC or command line level).
var forwardingManager *forwarding.Manager

// synchronizationManager is the synchronization session manager for the
// integration testing daemon. It is exposed for integration tests that operate
// at the API level (as opposed to the gRPC or command line level).
var synchronizationManager *synchronization.Manager

// lockedBuffer is a buffer that can be read while it's written.
type lockedBuffer struct {
	// lock serializes access to buffer.
	lock sync.Mutex
	// buffer is the underlying buffer.
	buffer bytes.Buffer
}

// Write implements io.Writer.Write.
func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.buffer.Write(data)
}

// String returns the buffer's contents.
func (b *lockedBuffer) String() string {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.buffer.String()
}

// synchronizationLog records the synchronization manager's log at the debug
// level, so that tests can show their sessions' logs if they fail (see
// logSessionOnFailure).
var synchronizationLog lockedBuffer

// logSessionOnFailure adds the synchronization manager's log of a session,
// whose scopes start with its truncated identifier, to a test's output if the
// test fails, such as to show why the session stopped synchronizing. The log
// extends until cleanups registered before this call, such as the session's
// termination, run.
func logSessionOnFailure(t *testing.T, session string) {
	scope := "[" + identifier.Truncated(session)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		var log strings.Builder
		for line := range strings.Lines(synchronizationLog.String()) {
			if strings.Contains(line, scope+"]") || strings.Contains(line, scope+".") {
				log.WriteString(line)
			}
		}
		t.Logf("synchronization log of session %s:\n%s", session, log.String())
	})
}

// TestMain is the entry point for integration tests. It replaces the default
// test entry point so that it can copy the mutagen executable to a well-known
// path, set up the agent bundle to work during testing, set up a functionally
// complete daemon instance for testing, and tear down all of the aforementioned
// infrastructure after running tests.
func TestMain(m *testing.M) {
	// Override the expected agent bundle location.
	agent.ExpectedBundleLocation = agent.BundleLocationBuildDirectory

	// Acquire the daemon lock and defer its release.
	lock, err := daemon.AcquireLock()
	if err != nil {
		cmd.Fatal(fmt.Errorf("unable to acquire daemon lock: %w", err))
	}
	defer lock.Release()

	// Create a forwarding session manager and defer its shutdown.
	forwardingManager, err = forwarding.NewManager(nil)
	if err != nil {
		cmd.Fatal(fmt.Errorf("unable to create forwarding session manager: %w", err))
	}
	defer forwardingManager.Shutdown()

	// Create a session manager and defer its shutdown. Its log is recorded for
	// failing tests (see logSessionOnFailure).
	synchronizationManager, err = synchronization.NewManager(logging.NewLogger(logging.LevelDebug, &synchronizationLog))
	if err != nil {
		cmd.Fatal(fmt.Errorf("unable to create synchronization session manager: %w", err))
	}
	defer synchronizationManager.Shutdown()

	// Create the gRPC server and defer its termination. We use a hard stop
	// rather than a graceful stop so that it doesn't hang on open requests.
	server := grpc.NewServer(
		grpc.MaxSendMsgSize(grpcutil.MaximumMessageSize),
		grpc.MaxRecvMsgSize(grpcutil.MaximumMessageSize),
	)
	defer server.Stop()

	// Create and register the daemon service and defer its shutdown.
	daemonServer := daemonsvc.NewServer()
	daemonsvc.RegisterDaemonServer(server, daemonServer)
	defer daemonServer.Shutdown()

	// Create and register the prompt service.
	promptingsvc.RegisterPromptingServer(server, promptingsvc.NewServer())

	// Create and register the forwarding server.
	forwardingServer := forwardingsvc.NewServer(forwardingManager)
	forwardingsvc.RegisterForwardingServer(server, forwardingServer)

	// Create and register the session service.
	synchronizationServer := synchronizationsvc.NewServer(synchronizationManager)
	synchronizationsvc.RegisterSynchronizationServer(server, synchronizationServer)

	// Compute the path to the daemon IPC endpoint.
	endpoint, err := daemon.EndpointPath()
	if err != nil {
		cmd.Fatal(fmt.Errorf("unable to compute endpoint path: %w", err))
	}

	// Create the daemon listener and defer its closure. Since we hold the
	// daemon lock, we preemptively remove any existing socket since it (should)
	// be stale.
	os.Remove(endpoint)
	listener, err := ipc.NewListener(endpoint)
	if err != nil {
		cmd.Fatal(fmt.Errorf("unable to create daemon listener: %w", err))
	}
	defer listener.Close()

	// Serve incoming connections in a separate Goroutine. We don't monitor for
	// errors since there's nothing that we can do about them and because
	// they'll likely show up in the test output anyway.
	go server.Serve(listener)

	// Run tests.
	m.Run()
}
