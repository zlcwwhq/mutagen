package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/encoding"
	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
)

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
