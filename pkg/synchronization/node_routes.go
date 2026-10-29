package synchronization

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

// errNodeGraphChanged reports a stale destination selection consistently.
var errNodeGraphChanged = errors.New("node graph changed after target selection; query --nodes again")

// resolveSubmission selects destinations in the daemon for every operation.
// The graph's edge handles never instruct a client to switch registrations:
// only the daemon uses them to route from the authenticated source node.
func (r *commandRelay) resolveSubmission(source int, header *remote.Submission) (*commandRelay, int, *remote.ResolvedRoute, error) {
	graph, err := r.trees.nodes(r, source)
	if err != nil {
		return nil, 0, nil, err
	}
	if header.Revision != "" && header.Revision != graph.Revision {
		return nil, 0, nil, errNodeGraphChanged
	}
	name := header.Node
	if name == "" {
		if len(graph.Nodes) != 2 {
			return nil, 0, nil, errors.New("multiple destination nodes; use --nodes to inspect them and --node NAME to select one")
		}
		for _, node := range graph.Nodes {
			if !node.Current {
				name = node.Name
			}
		}
	}
	var selected *remote.Node
	for i := range graph.Nodes {
		if graph.Nodes[i].Name == name {
			selected = &graph.Nodes[i]
			break
		}
	}
	if selected == nil {
		return nil, 0, nil, fmt.Errorf("unknown node %q; use --nodes to list configured nodes", name)
	}
	if selected.Current {
		return nil, 0, nil, errors.New("target is the current node; run the command locally")
	}
	header.Node = name
	header.Revision = graph.Revision
	route := &remote.ResolvedRoute{SourceNode: graph.Source, Node: name, Tree: graph.Tree, Revision: graph.Revision}
	// Strict tree routing is a policy that the submitting client declares,
	// since Command is set by the client: a submission that omits it takes the
	// lenient adjacent path. That's intended, not a bypass, since any submitter
	// can already run arbitrary commands on an adjacent node, and classifying
	// an OperationExecute submission here would require parsing its request,
	// which conflict resolution also submits for file transfers and
	// replacements.
	strict := graph.Tree && (header.Command || header.Operation == remote.OperationTreeExecute)
	pathConflicts := header.Operation == remote.OperationNodeConflicts
	if strict || pathConflicts && graph.Tree && !selected.Adjacent {
		if graph.Problem != "" {
			return nil, 0, nil, fmt.Errorf("tree commands are unavailable until the workspace tree is repaired: %s", graph.Problem)
		}
		if !selected.Available {
			return nil, 0, nil, remote.ConnectionError(fmt.Errorf("node %s is unavailable: %s", name, selected.Reason))
		}
		if strict && !header.Detached || pathConflicts {
			if strict {
				header.Operation = remote.OperationTreeExecute
			}
			route.Session, route.Alpha = r.controller.session.Identifier, source == 0
			return r, 1 - source, route, nil
		}
	}
	if !selected.Adjacent {
		if len(selected.Path) == 2 {
			return nil, 0, nil, remote.ConnectionError(fmt.Errorf("node %s is unavailable: %s", name, selected.Reason))
		}
		return nil, 0, nil, fmt.Errorf("this operation requires an adjacent node; %s is reached via %s (use --nodes to inspect the path)", name, strings.Join(selected.Path, " -> "))
	}
	r.trees.Lock()
	relay := r.trees.relays[selected.Session]
	r.trees.Unlock()
	if relay == nil {
		return nil, 0, nil, remote.ConnectionError(errors.New("selected node disconnected"))
	}
	target := 0
	if selected.Alpha {
		target = 1
	}
	// A degraded tree can disagree about a node. Never use another edge's
	// different source directory or ignore rules under this registration.
	originalSource := r.controller.session.Alpha
	if source == 1 {
		originalSource = r.controller.session.Beta
	}
	selectedSource := relay.controller.session.Alpha
	configuration := relay.controller.mergedAlphaConfiguration
	if !selected.Alpha {
		selectedSource = relay.controller.session.Beta
		configuration = relay.controller.mergedBetaConfiguration
	}
	if !proto.Equal(originalSource, selectedSource) {
		return nil, 0, nil, errors.New("selected edge describes a different source workspace")
	}
	if configuration == nil {
		configuration = relay.controller.session.Configuration
	}
	route.Ignores = EffectiveIgnores(configuration, relay.controller.session.Version)
	route.Session, route.Alpha = selected.Session, selected.Alpha
	header.Adjacent = true
	if strict {
		header.Operation = remote.OperationTreeExecute
	}
	return relay, target, route, nil
}
