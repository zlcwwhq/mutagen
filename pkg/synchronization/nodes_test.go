package synchronization

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// treeTestChannel is a connected command channel that routes nothing.
type treeTestChannel struct {
	// done is never closed, since the tests don't disconnect channels.
	done chan struct{}
}

func (c *treeTestChannel) Accept() (io.ReadWriteCloser, error) {
	return nil, errors.New("unsupported")
}

func (c *treeTestChannel) Open(context.Context) (io.ReadWriteCloser, error) {
	return nil, errors.New("unsupported")
}

func (c *treeTestChannel) Done() <-chan struct{} {
	return c.done
}

func (c *treeTestChannel) Close() error {
	return nil
}

// treeTestRegistry registers the specified edges, and connects relays with
// both command channels for those that are listed as connected.
func treeTestRegistry(edges []*Session, connected ...string) (*commandTrees, map[string]*commandRelay) {
	trees := newCommandTrees()
	relays := make(map[string]*commandRelay)
	for _, edge := range edges {
		c := &controller{session: edge}
		trees.update(c, false)
		if slices.Contains(connected, edge.Identifier) {
			relay := &commandRelay{trees: trees, controller: c, ctx: context.Background()}
			relay.channels = [2]remote.Channel{&treeTestChannel{make(chan struct{})}, &treeTestChannel{make(chan struct{})}}
			trees.setRelay(c, relay)
			relays[edge.Identifier] = relay
		}
	}
	return trees, relays
}

// treeTestNode finds a node in a graph.
func treeTestNode(t *testing.T, graph *remote.NodeGraph, name string) remote.Node {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.Name == name {
			return node
		}
	}
	t.Fatalf("node %s not listed: %+v", name, graph.Nodes)
	return remote.Node{}
}

// TestWorkspaceTreeNodesDegraded verifies that a problem with a tree's
// membership refuses strict routing but doesn't hide the graph or the
// connected edges adjacent to the source, which other operations use.
func TestWorkspaceTreeNodesDegraded(t *testing.T) {
	// The tree is a -> b -> c and a -> d, with the a <-> d edge offline. Each
	// edge's identifier is its child's name. Node c submits as beta.
	edges := []*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c"), treeTestEdge("a", "d")}
	trees, relays := treeTestRegistry(edges, "b", "c")
	graph, err := trees.nodes(relays["c"], 1)
	if err != nil {
		t.Fatal(err)
	} else if graph.Problem != "" || !graph.Tree || graph.Source != "c" || len(graph.Nodes) != 4 {
		t.Fatalf("healthy graph misreported: %+v", graph)
	}
	if b := treeTestNode(t, graph, "b"); !b.Available || !b.Adjacent || b.Session != "c" || b.Alpha || !slices.Equal(b.Path, []string{"c", "b"}) {
		t.Fatalf("adjacent node misreported: %+v", b)
	}
	if a := treeTestNode(t, graph, "a"); !a.Available || a.Adjacent || a.Session != "" || !slices.Equal(a.Path, []string{"c", "b", "a"}) {
		t.Fatalf("path node misreported: %+v", a)
	}
	if d := treeTestNode(t, graph, "d"); d.Available || d.Adjacent || !strings.Contains(d.Reason, "a <-> d") {
		t.Fatalf("offline branch misreported: %+v", d)
	}
	healthy := graph.Revision

	// Removing the a <-> b edge disconnects b and c from the root.
	trees.update(trees.controllers["b"], true)
	graph, err = trees.nodes(relays["c"], 1)
	if err != nil {
		t.Fatal("invalid tree hid the node graph:", err)
	} else if !strings.Contains(graph.Problem, "disconnected from tree root") || graph.Revision == healthy || len(graph.Nodes) != 4 {
		t.Fatalf("tree problem misreported: %+v", graph)
	}
	if b := treeTestNode(t, graph, "b"); b.Available || !b.Adjacent || b.Session != "c" || b.Reason != graph.Problem || !slices.Equal(b.Path, []string{"c", "b"}) {
		t.Fatalf("connected adjacent node misreported: %+v", b)
	}
	for _, name := range []string{"a", "d"} {
		if node := treeTestNode(t, graph, name); node.Available || node.Adjacent || node.Session != "" || len(node.Path) != 0 || node.Path == nil || node.Reason != graph.Problem {
			t.Fatalf("detached node misreported: %+v", node)
		}
	}
	if _, err := trees.plan(relays["c"], 1, "b"); err == nil || !strings.Contains(err.Error(), "disconnected from tree root") {
		t.Fatal("strict route planned for an invalid tree:", err)
	}

	// From the other side of the missing edge, the offline edge to d is
	// reported as such, while the graph still lists the far side.
	trees, relays = treeTestRegistry([]*Session{treeTestEdge("a", "b"), treeTestEdge("x", "c"), treeTestEdge("a", "d")}, "b")
	graph, err = trees.nodes(relays["b"], 0)
	if err != nil {
		t.Fatal(err)
	} else if graph.Source != "a" || graph.Problem == "" {
		t.Fatalf("tree problem not reported from the root: %+v", graph)
	}
	if d := treeTestNode(t, graph, "d"); d.Adjacent || !strings.Contains(d.Reason, "a <-> d") || !slices.Equal(d.Path, []string{"a", "d"}) {
		t.Fatalf("offline adjacent edge misreported: %+v", d)
	}
	if b := treeTestNode(t, graph, "b"); !b.Adjacent || b.Session != "b" || !b.Alpha {
		t.Fatalf("adjacent child misreported: %+v", b)
	}
	if c := treeTestNode(t, graph, "c"); c.Adjacent || c.Parent != "x" || len(c.Path) != 0 {
		t.Fatalf("unreachable node misreported: %+v", c)
	}

	// An overlapping session outside the tree keeps the tree's structure,
	// but refuses every strict route.
	edges = []*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c"), treeTestEdge("a", "d")}
	trees, relays = treeTestRegistry(edges, "b", "c")
	external := &controller{session: &Session{
		Identifier: "external",
		Alpha:      &url.URL{Protocol: url.Protocol_SSH, Host: "b", Path: "~/project/sub"},
		Beta:       &url.URL{Protocol: url.Protocol_SSH, Host: "elsewhere", Path: "~/project"},
	}}
	trees.update(external, false)
	graph, err = trees.nodes(relays["c"], 1)
	if err != nil {
		t.Fatal(err)
	} else if !strings.Contains(graph.Problem, "session external overlaps node b") {
		t.Fatalf("overlap not reported: %+v", graph)
	}
	if b := treeTestNode(t, graph, "b"); b.Available || !b.Adjacent || b.Session != "c" {
		t.Fatalf("adjacent node unusable despite connected edge: %+v", b)
	}
	if a := treeTestNode(t, graph, "a"); a.Available || a.Adjacent || !slices.Equal(a.Path, []string{"c", "b", "a"}) || a.Parent != "" {
		t.Fatalf("overlapping tree lost its structure: %+v", a)
	}
	if _, err := trees.plan(relays["c"], 1, "a"); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatal("strict route planned despite overlap:", err)
	}

	// Discovery still requires the submitting edge's own relay.
	trees.setRelay(relays["c"].controller, nil)
	if _, err := trees.nodes(relays["c"], 1); remote.ClassifyError(err) != remote.ErrorKindConnection {
		t.Fatal("disconnected source accepted:", err)
	}
}
