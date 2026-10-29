package remote

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// locateWorkspaceTest locates the source workspace for a directory.
func locateWorkspaceTest(selection *Selection, directory string) (*Target, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return selection.Locate(ctx, directory)
}

// registered reports whether the registry holds a record of a session.
func registered(t *testing.T, registry, session string) bool {
	t.Helper()
	registrations, err := loadRegistrations(registry)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range registrations {
		if r.Session == session {
			return true
		}
	}
	return false
}

func TestStarSourceSelection(t *testing.T) {
	useTestRegistry(t)
	root := resolveSymbolicLinks(t.TempDir())
	add := func(session, star, node, directory string) Channel {
		t.Helper()
		channel, err := NewLocalChannel(nil, &Endpoint{Session: session, Star: star, Node: node, Root: directory, Submit: true, Execute: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { channel.Close() })
		return channel
	}
	add("center-b", "project", "center", root)
	add("center-c", "project", "center", root)
	selection := &Selection{Workspace: true}
	if target, err := locateWorkspaceTest(selection, root); err != nil || target.Root != root || target.SourceNode != "center" || !target.Star {
		t.Fatalf("a star's edges did not select its center as one source node: %+v %v", target, err)
	}
	other := add("other", "another-project", "center", root)
	if _, err := locateWorkspaceTest(selection, root); err == nil || !strings.Contains(err.Error(), "ambiguous source node") || !strings.Contains(err.Error(), "one star") {
		t.Fatal("ambiguous source star accepted:", err)
	}
	other.Close()
	pair := add("pair", "", "", root)
	if _, err := locateWorkspaceTest(selection, root); err == nil || !strings.Contains(err.Error(), "ambiguous source node") {
		t.Fatal("pair sharing a star's root accepted:", err)
	}
	pair.Close()

	// A nested pair is the more specific workspace, so it's the source rather
	// than the enclosing star.
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	add("pair", "", "", nested)
	if target, err := locateWorkspaceTest(selection, nested); err != nil || target.Session != "pair" || target.Path != "" {
		t.Fatalf("nested pair not selected: %+v %v", target, err)
	}
	if _, err := locateWorkspaceTest(selection, t.TempDir()); err == nil || !strings.Contains(err.Error(), "no connected node contains") {
		t.Fatal("source outside star accepted:", err)
	}
}

// TestStarSourceEntersSelectedEdge verifies that a submission at a star's
// center enters through the registration of the selected node's own edge,
// whatever the registration order, since the daemon only accepts operations
// that cross the edge through which they were submitted, and that any
// registration is used otherwise, so that the daemon can report the problem.
// Pinned submissions only use their source registration, never another
// registration of the same node.
func TestStarSourceEntersSelectedEdge(t *testing.T) {
	for _, order := range [][]string{{"b", "c"}, {"c", "b"}} {
		useTestRegistry(t)
		root := resolveSymbolicLinks(t.TempDir())
		channels := make(map[string]Channel)
		for _, peer := range order {
			channel, err := NewLocalChannel(nil, &Endpoint{Session: "center-" + peer, Alpha: true, Star: "project", Node: "center", PeerNode: peer, Root: root, Submit: true, Execute: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { channel.Close() })
			channels[peer] = channel
			// Registrations are ordered by their creation times.
			time.Sleep(10 * time.Millisecond)
		}
		for _, peer := range order {
			if target, err := locateWorkspaceTest(&Selection{Workspace: true, Node: peer}, root); err != nil || target.SourceSession != "center-"+peer || target.Node != peer {
				t.Fatalf("submission for node %s didn't enter through its edge (order %v): %+v %v", peer, order, target, err)
			}
		}
		for _, node := range []string{"", "missing"} {
			if target, err := locateWorkspaceTest(&Selection{Workspace: true, Node: node}, root); err != nil || target.SourceNode != "center" {
				t.Fatalf("center not located for node %q: %+v %v", node, target, err)
			}
		}

		// A pinned target's selected edge never chooses its registration, even
		// if it names another live registration of the same node.
		mismatched := &Target{SourceSession: "center-b", SourceAlpha: true, Star: true, Resolved: true, SourceNode: "center", Node: "c", Session: "center-c", Alpha: true}
		if target, err := locateWorkspaceTest(&Selection{Target: mismatched}, root); err != nil || target.SourceSession != "center-b" {
			t.Fatalf("pinned target didn't keep its source registration: %+v %v", target, err)
		}

		// Once the selected node's registration isn't live, another one is
		// located, through which the daemon refuses the submission (see
		// TestStarUnavailableNodeIgnores).
		channels["c"].Close()
		if target, err := locateWorkspaceTest(&Selection{Workspace: true, Node: "c"}, root); err != nil || target.SourceSession != "center-b" {
			t.Fatalf("center not located for a node whose registration isn't live: %+v %v", target, err)
		}

		// A pinned target only uses its source registration, even while another
		// registration of the same node is live.
		pinned := &Target{SourceSession: "center-c", SourceAlpha: true, Star: true, Resolved: true, SourceNode: "center", Node: "c", Session: "center-c", Alpha: true}
		if _, err := locateWorkspaceTest(&Selection{Target: pinned}, root); err == nil || !strings.Contains(err.Error(), "no longer connected") || ClassifyError(err) != ErrorKindConnection {
			t.Fatal("pinned target selected another registration of its node:", err)
		}
		pinned = &Target{SourceSession: "center-b", SourceAlpha: true, Star: true, Resolved: true, SourceNode: "center", Node: "b", Session: "center-b", Alpha: true}
		if target, err := locateWorkspaceTest(&Selection{Target: pinned}, root); err != nil || target.SourceSession != "center-b" {
			t.Fatalf("pinned target didn't use its source registration: %+v %v", target, err)
		}
	}
}

// TestStarUnavailableNodeIgnores verifies that, at a star's center, the
// daemon's refusal of a submission that enters through another edge's
// registration, because the selected node's own registration isn't live, is
// reported rather than the entry registration's ignores, which don't apply to
// the selected node, while those ignores are checked before anything is
// submitted for the registration's own node, and once the daemon accepts the
// route otherwise.
func TestStarUnavailableNodeIgnores(t *testing.T) {
	useTestRegistry(t)
	root := resolveSymbolicLinks(t.TempDir())
	for _, directory := range []string{"a/sub", "b/sub"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ignoring := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"a"}}

	// The center's registration of the edge to b, which ignores a, is the only
	// live one. The daemon refuses submissions for c through it, as it does
	// when c's own registration isn't live, and accepts those for b.
	channel, err := NewLocalChannel(nil, &Endpoint{Session: "center-b", Name: "star-b", Alpha: true, Star: "project", Node: "center", PeerNode: "b", Root: root, Submit: true, Ignores: ignoring})
	if err != nil {
		t.Fatal(err)
	}
	var lock sync.Mutex
	var submitted []string
	var handlers sync.WaitGroup
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		for {
			connection, err := channel.Accept()
			if err != nil {
				return
			}
			header, err := ReceiveSubmission(context.Background(), connection)
			if err != nil {
				connection.Close()
				continue
			}
			lock.Lock()
			submitted = append(submitted, header.Node)
			lock.Unlock()
			if header.Node == "c" {
				Reject(connection, ConnectionError(errors.New("node c is unavailable: its edge isn't connected for remote commands at the center")))
				continue
			}
			RespondWithRoute(connection, &ResolvedRoute{Session: "center-b", Alpha: true, SourceNode: "center", Node: "b", Star: true, Revision: "b", Ignores: ignoring})
			connection.Close()
		}
	}()
	t.Cleanup(func() { channel.Close(); handlers.Wait() })
	submit := func(node, directory string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		selection := &Selection{Workspace: true, Node: node, Resolve: true, Corresponding: true}
		connection, target, err := selection.Submit(ctx, filepath.Join(root, directory), &Submission{Operation: OperationFlush})
		if err == nil {
			connection.Close()
			if target.Session != "center-b" || target.SourceSession != "center-b" || target.Node != "b" {
				t.Fatalf("incorrect edge selected for node %q: %+v", node, target)
			}
		}
		return err
	}
	excluded := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "a/sub is inside a, which session star-b ignores") &&
			strings.Contains(err.Error(), "--cwd . runs in the other side's own copy")
	}
	count := func() int {
		lock.Lock()
		defer lock.Unlock()
		return len(submitted)
	}

	// A directory ignored only by the entry registration's edge doesn't hide
	// the daemon's refusal.
	if err := submit("c", "a/sub"); err == nil || ClassifyError(err) != ErrorKindConnection || !strings.Contains(err.Error(), "node c is unavailable") {
		t.Fatal("refusal of a node whose registration isn't live not reported:", err)
	}

	// The registration's own edge is checked before anything is submitted.
	before := count()
	if err := submit("b", "a/sub"); !excluded(err) {
		t.Fatal("directory ignored by the registration's own edge accepted:", err)
	} else if count() != before {
		t.Fatal("excluded directory submitted")
	}
	if err := submit("b", "b/sub"); err != nil {
		t.Fatal("synchronized directory refused:", err)
	}

	// An omitted node is checked once the daemon accepts the route.
	if err := submit("", "a/sub"); !excluded(err) {
		t.Fatal("directory ignored by the edge that carries the operation accepted:", err)
	}
	lock.Lock()
	defer lock.Unlock()
	if strings.Join(submitted, ",") != "c,b," {
		t.Fatalf("unexpected submissions: %q", submitted)
	}
}

func TestWorkspaceVersionMismatch(t *testing.T) {
	registry := useTestRegistry(t)
	mkdirs(t, registry)
	base := resolveSymbolicLinks(t.TempDir())
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	nested := filepath.Join(root, "nested")
	mkdirs(t, nested, outside)
	mismatch := func(err error, subject string) bool {
		return err != nil && ClassifyError(err) == ErrorKindConnection &&
			strings.Contains(err.Error(), subject+" registered by Mutagen 0.0.1, but this is Mutagen "+mutagen.Version) &&
			strings.Contains(err.Error(), "use the remote executable installed with that version")
	}

	// If only another version of Mutagen registered a workspace containing the
	// directory, the version mismatch is explained, such as after an upgrade
	// that left an outdated remote executable on the PATH, rather than
	// reporting that no workspace contains it.
	registerIncompatible(t, registry, "sync_old", root)
	for _, directory := range []string{root, nested} {
		if _, err := locateWorkspaceTest(&Selection{Workspace: true}, directory); !mismatch(err, "the workspaces containing this directory are") {
			t.Errorf("version mismatch not explained from %s: %v", directory, err)
		}
	}

	// Outside every root, it's only explained for selections that may apply
	// outside every root.
	if _, err := locateWorkspaceTest(&Selection{Workspace: true}, outside); err == nil || !strings.Contains(err.Error(), "no connected node contains") {
		t.Error("directory outside every root not reported:", err)
	} else if _, err := locateWorkspaceTest(&Selection{Workspace: true, WorkspaceOutside: true}, outside); !mismatch(err, "the workspaces on this system are") {
		t.Error("version mismatch not explained outside every root:", err)
	}

	// A live workspace registered by another version isn't masked by an
	// enclosing or equally specific workspace of this version, nor by one
	// outside every root.
	var lock sync.Mutex
	var accepted []string
	registerTest(t, &Endpoint{Session: "sync_outside", Root: outside}, "outside", &accepted, &lock)
	if _, err := locateWorkspaceTest(&Selection{Workspace: true, WorkspaceOutside: true}, nested); !mismatch(err, "a workspace containing this directory is") {
		t.Error("workspace outside every root masked another version's workspace:", err)
	}
	registerTest(t, &Endpoint{Session: "sync_enclosing", Root: base}, "enclosing", &accepted, &lock)
	if _, err := locateWorkspaceTest(&Selection{Workspace: true}, nested); !mismatch(err, "a more specific workspace containing this directory is") {
		t.Error("enclosing workspace masked another version's workspace:", err)
	}
	registerTest(t, &Endpoint{Session: "sync_same", Root: root}, "same", &accepted, &lock)
	if _, err := locateWorkspaceTest(&Selection{Workspace: true}, nested); !mismatch(err, "another workspace with the same root is") {
		t.Error("equally specific workspace masked another version's workspace:", err)
	}

	// Workspaces of this version that are more specific are used.
	registerTest(t, &Endpoint{Session: "sync_nested", Root: nested}, "nested", &accepted, &lock)
	if target, err := locateWorkspaceTest(&Selection{Workspace: true}, nested); err != nil || target.Session != "sync_nested" {
		t.Errorf("more specific workspace not used: %+v, %v", target, err)
	}

	// Registrations by other versions whose processes no longer listen are
	// pruned rather than blocking selection.
	data, err := json.Marshal(&record{Version: "0.0.1", Session: "sync_stale", Root: nested})
	if err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(registry, "stale"+recordExtension), data, 0600); err != nil {
		t.Fatal(err)
	}
	if target, err := locateWorkspaceTest(&Selection{Workspace: true}, nested); err != nil || target.Session != "sync_nested" {
		t.Errorf("stale registration by another version blocked selection: %+v, %v", target, err)
	} else if registered(t, registry, "sync_stale") {
		t.Error("stale registration by another version not pruned")
	}
}

func TestWorkspaceDeadNestedRegistration(t *testing.T) {
	registry := useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "inner")
	directory := filepath.Join(inner, "x")
	mkdirs(t, directory, filepath.Join(outer, "y"))
	var lock sync.Mutex
	var accepted []string
	ignores := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"inner"}}
	registerTest(t, &Endpoint{Session: "sync_outer", Star: "project", Node: "a", Root: outer, Alpha: true, Ignores: ignores}, "outer", &accepted, &lock)
	nested := registerTest(t, &Endpoint{Session: "sync_inner", Star: "nested", Node: "b", Root: inner}, "inner", &accepted, &lock)
	if target, err := locateWorkspaceTest(&Selection{Workspace: true}, directory); err != nil || target.Session != "sync_inner" || target.SourceNode != "b" {
		t.Fatalf("incorrect nested selection: %+v, %v", target, err)
	}

	// Once the nested workspace's process exits without removing its record, the
	// record is pruned and the enclosing workspace is located, consistently on
	// every attempt, exactly as if the record had been removed cleanly, as a
	// paused or reconnecting session's is.
	nested.listener.(*registeredListener).Listener.Close()
	<-nested.done
	for attempt := 0; attempt < 2; attempt++ {
		if target, err := locateWorkspaceTest(&Selection{Workspace: true}, directory); err != nil ||
			target.Session != "sync_outer" || target.SourceNode != "a" || target.Path != "inner/x" {
			t.Fatalf("enclosing workspace not located (attempt %d): %+v, %v", attempt, target, err)
		}
	}
	if registered(t, registry, "sync_inner") {
		t.Error("dead nested registration not pruned")
	}

	// Operations corresponding to the directory are still refused through the
	// enclosing workspace, whose ignores exclude the nested workspace's root.
	if _, err := dialTest(&Selection{Workspace: true, Corresponding: true}, directory); err == nil ||
		!strings.Contains(err.Error(), "inner/x is inside inner, which session") {
		t.Error("corresponding operation submitted through an ignoring workspace:", err)
	} else if target, err := dialTest(&Selection{Workspace: true, Corresponding: true}, filepath.Join(outer, "y")); err != nil || target.Path != "y" {
		t.Errorf("synchronized directory refused: %+v, %v", target, err)
	}
}
