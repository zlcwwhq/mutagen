package cli

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

// replyTestNodes adds discovery to the pair relays used by CLI tests.
func replyTestNodes(connection io.ReadWriteCloser, session, alpha, beta string) {
	remote.RespondWithNodes(connection, &remote.NodeGraph{Source: "alpha", Revision: session, Nodes: []remote.Node{
		{Name: "alpha", Root: alpha, Current: true, Available: true, Path: []string{"alpha"}},
		{Name: "beta", Parent: "alpha", Root: beta, Available: true, Adjacent: true, Path: []string{"alpha", "beta"}, Session: session, Alpha: true},
	}}, nil)
}

func TestNodeListing(t *testing.T) {
	graph := &remote.NodeGraph{Source: "local", Nodes: []remote.Node{
		{Name: "local", Current: true, Available: true, Path: []string{"local"}},
		{Name: "gpu", Available: true, Adjacent: true, Host: "host", Root: "~/project", Path: []string{"local", "gpu"}, Session: "private-handle"},
		{Name: "offline", Reason: "edge local <-> offline is paused", Path: []string{"local", "offline"}},
	}}
	var output strings.Builder
	if err := printNodes(graph, true, &output); err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(output.String())) || strings.Contains(output.String(), "private-handle") || strings.Contains(output.String(), "Session") {
		t.Fatal("public node JSON leaked route handles", output.String())
	}
	output.Reset()
	if err := printNodes(graph, false, &output); err != nil || !strings.Contains(output.String(), "local -> offline") || !strings.Contains(output.String(), "paused") {
		t.Fatal("listing omitted unavailable path", err, output.String())
	}
	if err := printNodes(graph, false, failedCommandIO{}); err == nil {
		t.Fatal("listing ignored output failure")
	}

}

// TestNodeSelectionWithTreeProblem verifies that a tree problem refuses strict
// tree commands without blocking operations on connected adjacent nodes, and
// that it never changes which destination an omitted --node means.
func TestNodeListingWithTreeProblem(t *testing.T) {
	problem := "node b is disconnected from tree root a"
	graph := &remote.NodeGraph{Source: "c", Tree: true, Problem: problem, Nodes: []remote.Node{
		{Name: "a", Reason: problem, Path: []string{}},
		{Name: "b", Adjacent: true, Reason: problem, Path: []string{"c", "b"}, Session: "private-handle"},
		{Name: "c", Current: true, Available: true, Path: []string{"c"}},
		{Name: "e", Reason: "edge c <-> e is paused, disconnected, or has commands disabled", Path: []string{"c", "e"}},
		{Name: "f", Reason: problem, Path: []string{"c", "e", "f"}},
	}}
	var output strings.Builder
	if err := printNodes(graph, false, &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Tree problem: " + problem, "b: adjacent operations only (tree problem)", "a: unavailable: tree problem", "no path over existing edges", "e: unavailable: edge c <-> e"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("listing omitted %q: %s", expected, output.String())
		}
	}
	output.Reset()
	if err := printNodes(graph, true, &output); err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Problem string
		Nodes   []struct {
			Name     string
			Path     []string
			Adjacent bool
		}
	}
	if err := json.Unmarshal([]byte(output.String()), &listing); err != nil || listing.Problem != problem || strings.Contains(output.String(), "private-handle") {
		t.Fatal("JSON listing omitted the problem or leaked handles", err, output.String())
	} else if !strings.Contains(output.String(), `"path":[]`) || !listing.Nodes[1].Adjacent || listing.Nodes[0].Adjacent {
		t.Fatal("JSON listing misreported adjacency or paths", output.String())
	}

}

func TestParseNode(t *testing.T) {
	for _, arguments := range [][]string{
		{"--node", "gpu", "echo", "hello"},
		{"--node", "gpu", "--timeout", "5", "--cwd", "build", "make"},
		{"--node", "gpu", "--script", "-"},
		{"--node", "gpu", "--no-flush", "echo"},
		{"--node", "gpu", "--detach", "echo"},
		{"--node", "gpu", "--flush"}, {"--node", "gpu", "--diff"},
		{"--node", "gpu", "--jobs"}, {"--node", "gpu", "--conflicts"},
	} {
		if options, err := parse(arguments); err != nil || options.node != "gpu" {
			t.Fatalf("valid node command rejected: %v, %v", arguments, err)
		}
	}
	for _, arguments := range [][]string{
		{"--node", "", "echo"}, {"--node", "gpu"},
		{"--node", "gpu", "--session", "pair", "echo"},
		{"--node", "gpu", "--nodes"},
	} {
		if _, err := parse(arguments); err == nil {
			t.Fatalf("invalid node command accepted: %v", arguments)
		}
	}
}

// replyTestRoute mirrors the daemon's acknowledgement on an operation stream.
func replyTestRoute(connection io.ReadWriteCloser, header *remote.Submission, session string) bool {
	if !header.Resolve {
		return true
	}
	if header.Node != "" && header.Node != "beta" {
		remote.Reject(connection, errors.New("unknown node"))
		return false
	}
	if err := remote.RespondWithRoute(connection, &remote.ResolvedRoute{Session: session, Alpha: true, SourceNode: "alpha", Node: "beta", Revision: session}); err != nil {
		connection.Close()
		return false
	}
	return true
}
