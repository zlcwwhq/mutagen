package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// queryNodes locates a source workspace and asks its daemon for the complete
// graph. Discovery never flushes or opens an executor, including for offline
// targets. The returned source pins later requests to the same workspace.
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
	Parent    string   `json:"parent,omitempty"`
	Host      string   `json:"host"`
	Root      string   `json:"root"`
	Path      []string `json:"path"`
	Current   bool     `json:"current"`
	Available bool     `json:"available"`
	Adjacent  bool     `json:"adjacent"`
	Reason    string   `json:"reason,omitempty"`
}

func printNodes(graph *remote.NodeGraph, asJSON bool, writer io.Writer) error {
	var nodes []listedNode
	for _, node := range graph.Nodes {
		path := node.Path
		if path == nil {
			path = []string{}
		}
		nodes = append(nodes, listedNode{node.Name, node.Parent, node.Host, node.Root, path, node.Current, node.Available, node.Adjacent, node.Reason})
	}
	if asJSON {
		return json.NewEncoder(writer).Encode(struct {
			Current string       `json:"current"`
			Problem string       `json:"problem,omitempty"`
			Nodes   []listedNode `json:"nodes"`
		}{graph.Source, graph.Problem, nodes})
	}
	output := &checkedWriter{Writer: writer}
	clean := terminal.NeutralizeControlCharacters
	fmt.Fprintf(output, "Current node: %s\n", clean(graph.Source))
	if graph.Problem != "" {
		fmt.Fprintf(output, "Tree problem: %s\n", clean(graph.Problem))
	}
	for _, node := range nodes {
		// A node's reason usually repeats the tree problem shown above.
		reason := node.Reason
		if graph.Problem != "" && reason == graph.Problem {
			reason = "tree problem"
		}
		status := "unavailable: " + reason
		if node.Current {
			status = "current"
		} else if node.Available {
			status = "available"
		} else if node.Adjacent {
			status = "adjacent operations only (" + reason + ")"
		}
		path := "no path over existing edges"
		if len(node.Path) > 0 {
			path = "path " + strings.Join(node.Path, " -> ")
		}
		fmt.Fprintf(output, "  %s: %s; %s:%s; %s\n", clean(node.Name), clean(status), clean(node.Host), clean(executionDirectory(node.Root, "")), clean(path))
	}
	if graph.Problem != "" {
		fmt.Fprintln(output, "New commands, including detached jobs, require a repaired tree and strict preflight.")
		fmt.Fprintln(output, "Existing job control, conflicts, and other adjacent operations still work over connected edges.")
	}
	fmt.Fprintln(output, "Availability describes connections only; commands still check synchronization and conflicts.")
	return output.err
}

// pathConflictListing uses the same listing schema at every target distance.
type pathConflictListing struct {
	Alpha   string          `json:"alpha"`
	Beta    string          `json:"beta"`
	Source  string          `json:"source"`
	Target  string          `json:"target"`
	Listing json.RawMessage `json:"listing"`
}

func printPathConflicts(ctx context.Context, directory string, options *options, writer, errorOutput io.Writer) error {
	progress := newProgressOutput(errorOutput, options.progress)
	connection, source, err := options.selection(false).Submit(ctx, directory, &remote.Submission{Operation: remote.OperationNodeConflicts, Progress: progress.writer != nil})
	if err != nil {
		return err
	}
	events := newCommandEvents(ctx, errorOutput, progress, source)
	defer events.finish()
	edges, err := remote.ListPathConflictsWithProgress(ctx, connection, events.receive)
	if err != nil {
		return err
	}
	var result []pathConflictListing
	output := &checkedWriter{Writer: writer}
	for i, edge := range edges {
		target := *source
		target.Session, target.Alpha = edge.Session, edge.SourceAlpha
		target.SourceNode, target.Node = edge.Alpha, edge.Beta
		if !edge.SourceAlpha {
			target.SourceNode, target.Node = edge.Beta, edge.Alpha
		}
		if i > 0 {
			target.Root, target.Path, target.Contained = "", "", true
		}
		if options.json {
			var listing bytes.Buffer
			if err := renderConflicts(ctx, directory, options, &listing, errorOutput, edge.Listing, &target, i == 0); err != nil {
				return err
			}
			result = append(result, pathConflictListing{edge.Alpha, edge.Beta, target.SourceNode, target.Node, json.RawMessage(listing.Bytes())})
		} else {
			if source.Tree {
				fmt.Fprintf(output, "Edge %s <-> %s:\n", terminal.NeutralizeControlCharacters(edge.Alpha), terminal.NeutralizeControlCharacters(edge.Beta))
			}
			if err := renderConflicts(ctx, directory, options, output, errorOutput, edge.Listing, &target, i == 0); err != nil {
				return err
			}
		}
	}
	if options.json {
		return json.NewEncoder(writer).Encode(struct {
			Edges []pathConflictListing `json:"edges"`
		}{result})
	}
	return output.err
}
