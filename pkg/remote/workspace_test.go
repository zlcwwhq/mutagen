package remote

import (
	"context"
	"encoding/json"
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

// TestStarSourcePrefersSelectedEdge verifies that a submission at a star's
// center enters through the registration of the selected node's own edge,
// whatever the registration order, so that it doesn't depend on other edges,
// and that pinned submissions prefer that registration too, falling back to
// their original source registration only while it's unavailable.
func TestStarSourcePrefersSelectedEdge(t *testing.T) {
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

		// A pinned target whose first submission entered through another edge
		// prefers the selected edge's registration while it's live.
		pinned := &Target{SourceSession: "center-b", SourceAlpha: true, Star: true, Resolved: true, SourceNode: "center", Node: "c", Session: "center-c", Alpha: true}
		if target, err := locateWorkspaceTest(&Selection{Target: pinned}, root); err != nil || target.SourceSession != "center-c" {
			t.Fatalf("pinned target didn't prefer the selected edge: %+v %v", target, err)
		}
		unresolved := *pinned
		unresolved.Resolved = false
		if target, err := locateWorkspaceTest(&Selection{Target: &unresolved}, root); err != nil || target.SourceSession != "center-b" {
			t.Fatalf("unresolved target didn't keep its source: %+v %v", target, err)
		}
		channels["c"].Close()
		if target, err := locateWorkspaceTest(&Selection{Target: pinned}, root); err != nil || target.SourceSession != "center-b" {
			t.Fatalf("pinned target didn't fall back to its source: %+v %v", target, err)
		}
		channels["b"].Close()
		if _, err := locateWorkspaceTest(&Selection{Target: pinned}, root); err == nil || !strings.Contains(err.Error(), "no longer connected") {
			t.Fatal("pinned target selected another registration:", err)
		}
	}
}

// TestStarRedirectIgnores verifies that the ignores of the edge that the
// daemon selects decide whether a submission directory may be used, rather than
// those of the registration through which the submission enters, which differ
// when the selected edge's own registration at the center isn't live.
func TestStarRedirectIgnores(t *testing.T) {
	useTestRegistry(t)
	root := resolveSymbolicLinks(t.TempDir())
	for _, directory := range []string{"a/sub", "b/sub"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ignoring := func(pattern string) *Ignores {
		return &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{pattern}}
	}

	// The center's registration of the edge to b, which ignores a, is the only
	// live one, so a submission for c enters through it, while the daemon
	// selects the edge to c, which ignores b.
	channel, err := NewLocalChannel(nil, &Endpoint{Session: "center-b", Name: "star-b", Alpha: true, Star: "project", Node: "center", PeerNode: "b", Root: root, Submit: true, Ignores: ignoring("a")})
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
			route := &ResolvedRoute{Session: "center-" + header.Node, Alpha: true, SourceNode: "center", Node: header.Node, Star: true, Revision: header.Node, Ignores: ignoring("a")}
			if header.Node == "c" {
				route.Ignores = ignoring("b")
			}
			RespondWithRoute(connection, route)
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
			if target.Session != "center-"+node {
				t.Fatalf("incorrect edge selected for node %s: %+v", node, target)
			}
		}
		return err
	}
	excluded := func(err error, location string) bool {
		return err != nil && strings.Contains(err.Error(), location) && strings.Contains(err.Error(), "--cwd . runs in the other side's own copy")
	}

	// Ignored only by the entry registration's edge.
	if err := submit("c", "a/sub"); err != nil {
		t.Fatal("directory refused by the ignores of an edge that doesn't carry the operation:", err)
	}

	// Ignored only by the selected edge.
	if err := submit("c", "b/sub"); !excluded(err, "b/sub is inside b, which session center-c ignores") {
		t.Fatal("directory ignored by the selected edge accepted:", err)
	}

	// The registration's own edge is checked before anything is submitted.
	lock.Lock()
	before := len(submitted)
	lock.Unlock()
	if err := submit("b", "a/sub"); !excluded(err, "a/sub is inside a, which session star-b ignores") {
		t.Fatal("directory ignored by the registration's own edge accepted:", err)
	}
	lock.Lock()
	defer lock.Unlock()
	if len(submitted) != before || before != 2 {
		t.Fatalf("unexpected submissions: %v", submitted)
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
