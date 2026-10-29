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
		{Name: "beta", Root: beta, Available: true, Mode: "two-way-safe", Path: []string{"alpha", "beta"}, Session: session, Alpha: true},
	}}, nil)
}

// TestNodeListing verifies the listing of a star from its center in both
// formats, which mark the center and show each edge's mode without exposing
// daemon route handles.
func TestNodeListing(t *testing.T) {
	graph := &remote.NodeGraph{Source: "local", Star: true, Nodes: []remote.Node{
		{Name: "gpu", Available: true, Host: "host", Root: "~/project", Mode: "one-way-safe", Path: []string{"local", "gpu"}, Session: "private-handle", Alpha: true},
		{Name: "local", Center: true, Host: "laptop", Root: "/work", Current: true, Available: true, Path: []string{"local"}},
		{Name: "offline", Host: "offline", Root: "project", Mode: "two-way-safe", Reason: "node is paused, disconnected, or has commands disabled", Path: []string{"local", "offline"}},
	}}
	var output strings.Builder
	if err := printNodes(graph, true, &output); err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(output.String())) || strings.Contains(output.String(), "private-handle") || strings.Contains(output.String(), "Session") ||
		strings.Contains(output.String(), "parent") || strings.Contains(output.String(), "adjacent") {
		t.Fatal("public node JSON leaked route handles or tree fields", output.String())
	}
	var listing struct {
		Current string `json:"current"`
		Star    bool   `json:"star"`
		Nodes   []struct {
			Name      string   `json:"name"`
			Center    bool     `json:"center"`
			Host      string   `json:"host"`
			Root      string   `json:"root"`
			Path      []string `json:"path"`
			Mode      string   `json:"mode"`
			Current   bool     `json:"current"`
			Available bool     `json:"available"`
			Reason    string   `json:"reason"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(output.String()), &listing); err != nil {
		t.Fatal(err)
	} else if listing.Current != "local" || !listing.Star || len(listing.Nodes) != 3 {
		t.Fatalf("incorrect listing: %+v", listing)
	} else if node := listing.Nodes[0]; node.Name != "gpu" || node.Center || node.Mode != "one-way-safe" || !node.Available || strings.Join(node.Path, ",") != "local,gpu" {
		t.Fatalf("incorrect node: %+v", node)
	} else if node := listing.Nodes[1]; !node.Center || !node.Current || node.Mode != "" || node.Root != "/work" {
		t.Fatalf("incorrect center: %+v", node)
	} else if node := listing.Nodes[2]; node.Available || node.Reason == "" {
		t.Fatalf("incorrect unavailable node: %+v", node)
	}
	output.Reset()
	if err := printNodes(graph, false, &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Current node: local\n",
		"  gpu: available; host:~/project; one-way-safe\n",
		"  local: center; current; laptop:/work\n",
		"  offline: unavailable: node is paused, disconnected, or has commands disabled; offline:~/project; two-way-safe\n",
		"report conflicts unless given --no-flush",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("listing omitted %q: %s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), "problem") || strings.Contains(output.String(), "->") {
		t.Fatal("listing described a problem or path that doesn't exist:", output.String())
	}
	if err := printNodes(graph, false, failedCommandIO{}); err == nil {
		t.Fatal("listing ignored output failure")
	}
}

// TestNodeListingWithStarProblem verifies that a star problem is reported in
// both formats, along with its consequence for operations from the center.
func TestNodeListingWithStarProblem(t *testing.T) {
	problem := "sessions a and b disagree on the endpoint of center node local"
	graph := &remote.NodeGraph{Source: "local", Star: true, Problem: problem, Nodes: []remote.Node{
		{Name: "gpu", Reason: problem, Mode: "two-way-safe", Path: []string{"local", "gpu"}},
		{Name: "local", Center: true, Current: true, Available: true, Path: []string{"local"}},
	}}
	var output strings.Builder
	if err := printNodes(graph, false, &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Star problem: " + problem, "gpu: unavailable: star problem", "Operations from the center fail until the star is repaired",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("listing omitted %q: %s", expected, output.String())
		}
	}
	output.Reset()
	if err := printNodes(graph, true, &output); err != nil {
		t.Fatal(err)
	}
	var listing struct{ Problem string }
	if err := json.Unmarshal([]byte(output.String()), &listing); err != nil || listing.Problem != problem {
		t.Fatal("JSON listing omitted the problem", err, output.String())
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
