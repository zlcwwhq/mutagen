package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/encoding"
	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// centerChannels wraps the local synchronization protocol handler so that
// tests can disconnect the command channel of a local alpha endpoint, such as
// a star's center, on one edge only, without affecting the edge's
// synchronization, its beta endpoint's channel, or any other session. A local
// endpoint's channel lives in the daemon process and only closes when its
// relay closes it, so it can't be disconnected from outside, such as by
// killing a process, and removing its registration's files would leave the
// relay considering it connected. Instead, the wrapper records each channel
// that it creates, and closing one has exactly the effects of a failing
// channel: its registration is removed, and its relay notices, removes it, and
// tries to reconnect it after its reconnection interval. The handler map may
// only be modified during initialization (see
// synchronization.ProtocolHandlers), before any manager reads it.
var centerChannels = &heldCommandChannels{held: make(map[string]bool), channels: make(map[string]remote.Channel)}

func init() {
	centerChannels.ProtocolHandler = synchronization.ProtocolHandlers[url.Protocol_Local]
	synchronization.ProtocolHandlers[url.Protocol_Local] = centerChannels
}

// heldCommandChannels is a synchronization protocol handler that delegates to
// another, recording the command channels that it connects for alpha endpoints
// by session, and refusing to connect those of held sessions.
type heldCommandChannels struct {
	// ProtocolHandler is the wrapped handler, which must also implement
	// synchronization.CommandProtocolHandler.
	synchronization.ProtocolHandler
	// lock serializes access to held and channels, and is held while channels
	// connect, so that a channel can't connect unnoticed while it's held.
	lock sync.Mutex
	// held records the sessions whose alpha channels are held disconnected.
	held map[string]bool
	// channels records the open alpha channel of each session.
	channels map[string]remote.Channel
}

// ConnectCommands implements synchronization.CommandProtocolHandler.
func (h *heldCommandChannels) ConnectCommands(ctx context.Context, logger *logging.Logger, url *url.URL, endpoint *remote.Endpoint) (remote.Channel, error) {
	connect := h.ProtocolHandler.(synchronization.CommandProtocolHandler).ConnectCommands
	if !endpoint.Alpha {
		return connect(ctx, logger, url, endpoint)
	}
	h.lock.Lock()
	defer h.lock.Unlock()
	if h.held[endpoint.Session] {
		return nil, errors.New("command channel held disconnected by a test")
	}
	channel, err := connect(ctx, logger, url, endpoint)
	if err != nil {
		return nil, err
	}
	recorded := &recordedChannel{Channel: channel, owner: h, session: endpoint.Session}
	h.channels[endpoint.Session] = recorded
	return recorded, nil
}

// recordedChannel is a command channel recorded by heldCommandChannels, which
// forgets it once it's closed.
type recordedChannel struct {
	remote.Channel
	// owner records the channel.
	owner *heldCommandChannels
	// session is the channel's session.
	session string
}

// Close implements remote.Channel.Close.
func (c *recordedChannel) Close() error {
	c.owner.lock.Lock()
	if c.owner.channels[c.session] == remote.Channel(c) {
		delete(c.owner.channels, c.session)
	}
	c.owner.lock.Unlock()
	return c.Channel.Close()
}

// hold disconnects the alpha command channel of a session, if it's connected,
// and holds it disconnected until the returned function, which must be called
// before the test ends, is called. The channel's registration is removed
// before hold returns.
func (h *heldCommandChannels) hold(session string) func() {
	h.lock.Lock()
	h.held[session] = true
	channel := h.channels[session]
	delete(h.channels, session)
	h.lock.Unlock()
	if channel != nil {
		channel.Close()
	}
	return func() {
		h.lock.Lock()
		delete(h.held, session)
		h.lock.Unlock()
	}
}

// restartWithSavedSession shuts down a manager, modifies one of the sessions
// that it saved, and returns a new manager that loads the sessions, as a
// daemon restart would. Loading doesn't validate stars against each other, so
// this can produce states that creation refuses.
func restartWithSavedSession(t *testing.T, manager *synchronization.Manager, id string, modify func(*synchronization.Session)) *synchronization.Manager {
	t.Helper()
	manager.Shutdown()
	path, err := filesystem.Mutagen(false, filesystem.MutagenSynchronizationSessionsDirectoryName, id)
	if err != nil {
		t.Fatal(err)
	}
	session := &synchronization.Session{}
	if err := encoding.LoadAndUnmarshalProtobuf(path, session); err != nil {
		t.Fatal(err)
	}
	modify(session)
	if err := encoding.MarshalAndSaveProtobuf(path, session); err != nil {
		t.Fatal(err)
	}
	manager, err = synchronization.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// waitForStarCenter waits until a non-center node reports the star's center as
// available, which requires both sides of the node's edge to be connected.
// Unlike starFixture.ready, it doesn't require the center to reach every
// node, which a star problem prevents.
func waitForStarCenter(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		connection, _, err := (&remote.Selection{Workspace: true}).Submit(ctx, root, &remote.Submission{Operation: remote.OperationNodes})
		var graph *remote.NodeGraph
		if err == nil {
			graph, err = remote.ListNodes(ctx, connection)
			connection.Close()
		}
		cancel()
		if err == nil {
			for _, node := range graph.Nodes {
				if node.Center && !node.Current && node.Available {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("star center did not become available:", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
