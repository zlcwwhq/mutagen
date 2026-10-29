package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// queryNodes locates a source workspace and asks its daemon for the nodes
// that it can reach. Discovery never flushes or opens an executor, including
// for offline targets. The returned source pins later requests to the same
// workspace.
func queryNodes(ctx context.Context, directory string, corresponding, outside bool) (*remote.NodeGraph, *remote.Target, error) {
	selection := &remote.Selection{Workspace: true, WorkspaceOutside: outside, Corresponding: corresponding}
	connection, source, err := selection.Submit(ctx, directory, &remote.Submission{Operation: remote.OperationNodes})
	if err != nil {
		return nil, nil, err
	}
	graph, err := remote.ListNodes(ctx, connection)
	return graph, source, err
}

// listedNode omits daemon route handles from the public node listing.
type listedNode struct {
	Name      string   `json:"name"`
	Center    bool     `json:"center"`
	Host      string   `json:"host"`
	Root      string   `json:"root"`
	Path      []string `json:"path"`
	Mode      string   `json:"mode,omitempty"`
	Current   bool     `json:"current"`
	Available bool     `json:"available"`
	Reason    string   `json:"reason,omitempty"`
}

// printNodes prints a node graph as text or JSON.
func printNodes(graph *remote.NodeGraph, asJSON bool, writer io.Writer) error {
	var nodes []listedNode
	for _, node := range graph.Nodes {
		path := node.Path
		if path == nil {
			path = []string{}
		}
		nodes = append(nodes, listedNode{node.Name, node.Center, node.Host, node.Root, path, node.Mode, node.Current, node.Available, node.Reason})
	}
	if asJSON {
		return json.NewEncoder(writer).Encode(struct {
			Current string       `json:"current"`
			Star    bool         `json:"star"`
			Problem string       `json:"problem,omitempty"`
			Nodes   []listedNode `json:"nodes"`
		}{graph.Source, graph.Star, graph.Problem, nodes})
	}
	output := &checkedWriter{Writer: writer}
	clean := terminal.NeutralizeControlCharacters
	fmt.Fprintf(output, "Current node: %s\n", clean(graph.Source))
	if graph.Problem != "" {
		fmt.Fprintf(output, "Star problem: %s\n", clean(graph.Problem))
	}
	for _, node := range nodes {
		// A node's reason usually repeats the star problem shown above.
		reason := node.Reason
		if graph.Problem != "" && reason == graph.Problem {
			reason = "star problem"
		}
		status := "unavailable: " + reason
		if node.Current {
			status = "current"
		} else if node.Available {
			status = "available"
		}
		var details []string
		if node.Center {
			details = append(details, "center")
		}
		details = append(details, status, node.Host+":"+executionDirectory(node.Root, ""))
		if node.Mode != "" {
			details = append(details, node.Mode)
		}
		fmt.Fprintf(output, "  %s: %s\n", clean(node.Name), clean(strings.Join(details, "; ")))
	}
	if graph.Problem != "" {
		fmt.Fprintln(output, "Operations from the center fail until the star is repaired, such as by applying the project again.")
	}
	fmt.Fprintln(output, "Available means that a node executes commands and the edge to it is connected at both ends, not that it's in sync; commands still synchronize and report conflicts unless given --no-flush.")
	return output.err
}
