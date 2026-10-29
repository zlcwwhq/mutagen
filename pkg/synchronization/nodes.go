package synchronization

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// nodeRevision pins immutable session identities. Replacing or adding/removing
// an edge changes the revision, including when the source edge is retained.
func nodeRevision(sessions []*Session) string {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.Identifier
	}
	sort.Strings(ids)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(ids, "\n"))))
}

// nodePath returns the unique path independently of endpoint availability.
func nodePath(parents map[string]*Session, source, target string) []string {
	ancestors := make(map[string]int)
	var ascent []string
	for node := source; ; {
		ancestors[node] = len(ascent)
		ascent = append(ascent, node)
		if parents[node] == nil {
			break
		}
		node = parents[node].Tree.Alpha
	}
	var descent []string
	for node := target; ; {
		if index, ok := ancestors[node]; ok {
			result := append([]string(nil), ascent[:index+1]...)
			for i := len(descent) - 1; i >= 0; i-- {
				result = append(result, descent[i])
			}
			return result
		}
		descent = append(descent, node)
		node = parents[node].Tree.Alpha
	}
}

// sketchTree describes the nodes named by edges that don't form a valid tree,
// as far as they can be described, solely for node discovery. Each node's
// endpoint comes from the first edge naming it, in the specified order. A
// parent is only reported for a node that exactly one edge names as a child,
// and a path is the shortest one from the source over persisted edges, with
// neighbors visited in name order, if there is one. Routing never uses them.
func sketchTree(sessions []*Session, from string) (map[string]*url.URL, map[string]string, map[string][]string) {
	endpoints := make(map[string]*url.URL)
	parents := make(map[string]string)
	children := make(map[string]int)
	neighbors := make(map[string][]string)
	for _, edge := range sessions {
		alpha, beta := edge.Tree.Alpha, edge.Tree.Beta
		if alpha == "" || beta == "" || alpha == beta {
			continue
		}
		for i, name := range []string{alpha, beta} {
			if endpoints[name] == nil {
				endpoints[name] = []*url.URL{edge.Alpha, edge.Beta}[i]
			}
		}
		parents[beta] = alpha
		children[beta]++
		neighbors[alpha] = append(neighbors[alpha], beta)
		neighbors[beta] = append(neighbors[beta], alpha)
	}
	for name, count := range children {
		if count > 1 {
			delete(parents, name)
		}
	}
	paths := map[string][]string{from: {from}}
	for queue := []string{from}; len(queue) > 0; queue = queue[1:] {
		node := queue[0]
		next := slices.Clone(neighbors[node])
		sort.Strings(next)
		for _, neighbor := range next {
			if paths[neighbor] == nil {
				paths[neighbor] = append(slices.Clone(paths[node]), neighbor)
				queue = append(queue, neighbor)
			}
		}
	}
	return endpoints, parents, paths
}

// edgeConnectedLocked reports whether operations can use an edge directly,
// without strict tree routing: its synchronization loop runs, it executes
// commands on both endpoints, and both of its command channels are connected.
// The membership lock must be held, which permits taking channelsLock.
func (t *commandTrees) edgeConnectedLocked(edge *Session) bool {
	relay := t.relays[edge.Identifier]
	if relay == nil || relay.ctx.Err() != nil || edge.Configuration.GetRemoteCommandMode() != RemoteCommandMode_RemoteCommandModeBoth {
		return false
	}
	relay.channelsLock.Lock()
	channels := relay.channels
	relay.channelsLock.Unlock()
	for _, channel := range channels {
		if channel == nil {
			return false
		}
		select {
		case <-channel.Done():
			return false
		default:
		}
	}
	return true
}

// nodes lists persisted members and reports connection availability without
// flushing, inspecting file contents, or opening any execution route. It only
// fails if the submitting edge itself isn't connected: a problem with a tree's
// membership, which refuses every strict tree command, is reported in the
// graph instead, so that discovery can diagnose it and operations on adjacent
// nodes over connected edges, such as job control and conflict resolution,
// keep working.
func (t *commandTrees) nodes(source *commandRelay, index int) (*remote.NodeGraph, error) {
	if t == nil || source.controller == nil {
		return nil, errors.New("node discovery is unavailable")
	}
	t.Lock()
	defer t.Unlock()
	session := source.controller.session
	if t.relays[session.Identifier] != source {
		return nil, remote.ConnectionError(errors.New("source node disconnected"))
	}
	if session.GetTree().GetIdentifier() == "" {
		return pairNodesLocked(source, index), nil
	}
	return t.treeNodesLocked(source, index), nil
}

// pairNodesLocked describes an implicit pair. The registry lock is held by the
// caller while the endpoint channels are sampled.
func pairNodesLocked(source *commandRelay, index int) *remote.NodeGraph {
	session := source.controller.session
	graph := &remote.NodeGraph{Source: endpointName(index), Revision: nodeRevision([]*Session{session})}
	source.channelsLock.Lock()
	defer source.channelsLock.Unlock()
	for i, endpoint := range []*url.URL{session.Alpha, session.Beta} {
		host, root := commandExecutionLocation(endpoint)
		node := remote.Node{Name: endpointName(i), Host: host, Root: root, Current: i == index, Available: i == index}
		if i == 1 {
			node.Parent = "alpha"
		}
		node.Path = []string{graph.Source}
		if !node.Current {
			node.Path = append(node.Path, node.Name)
			node.Session, node.Alpha = session.Identifier, index == 0
			channel := source.channels[i]
			if channel != nil && source.ctx.Err() == nil {
				select {
				case <-channel.Done():
				default:
					node.Available = true
				}
			}
			// A pair's peer is always adjacent, so the edge's own
			// connection determines both.
			node.Adjacent = node.Available
			if !node.Available {
				node.Reason = "node is disconnected or command execution is disabled"
			}
		}
		graph.Nodes = append(graph.Nodes, node)
	}
	return graph
}

// treeNodesLocked describes either the valid cached topology or its degraded
// sketch, then samples each node's current connection availability.
func (t *commandTrees) treeNodesLocked(source *commandRelay, index int) *remote.NodeGraph {
	session := source.controller.session

	// Validate the tree as strict routing does, but describe it either way.
	// Edges are ordered deterministically, starting with the submitting one,
	// so that describing an invalid tree prefers the source's own view.
	from := session.Tree.node(index)
	sessions, parents, endpoints, problem := t.treeLocked(session.GetTree().GetIdentifier())
	sessions = slices.Clone(sessions)
	graph := &remote.NodeGraph{Source: from, Tree: true, Revision: nodeRevision(sessions)}
	if problem != nil {
		graph.Problem = problem.Error()
	}
	sort.Slice(sessions, func(i, j int) bool {
		if first, second := sessions[i] == session, sessions[j] == session; first != second {
			return first
		}
		return sessions[i].Identifier < sessions[j].Identifier
	})
	var sketchedParents map[string]string
	var sketchedPaths map[string][]string
	if parents == nil {
		endpoints, sketchedParents, sketchedPaths = sketchTree(sessions, from)
	}

	// Index the edges joining the source to each adjacent node. An invalid
	// tree may join them more than once.
	adjacent := make(map[string][]*Session)
	for _, edge := range sessions {
		alpha, beta := edge.Tree.Alpha, edge.Tree.Beta
		if alpha == from && beta != from {
			adjacent[beta] = append(adjacent[beta], edge)
		} else if beta == from && alpha != from {
			adjacent[alpha] = append(adjacent[alpha], edge)
		}
	}

	for _, name := range sortedTreeNodes(endpoints) {
		host, root := commandExecutionLocation(endpoints[name])
		node := remote.Node{Name: name, Host: host, Root: root, Current: name == from, Available: name == from}
		if parents != nil {
			node.Path = nodePath(parents, from, name)
			if edge := parents[name]; edge != nil {
				node.Parent = edge.Tree.Alpha
			}
		} else {
			node.Parent, node.Path = sketchedParents[name], sketchedPaths[name]
			if node.Path == nil {
				node.Path = []string{}
			}
		}
		if node.Current {
			graph.Nodes = append(graph.Nodes, node)
			continue
		}

		t.describeNodeAvailabilityLocked(source, index, graph, &node, adjacent[name], problem)
		graph.Nodes = append(graph.Nodes, node)
	}
	return graph
}

// describeNodeAvailabilityLocked separates the usable adjacent edge from the
// stricter whole-tree routing result. Neither path performs static validation.
func (t *commandTrees) describeNodeAvailabilityLocked(source *commandRelay, index int, graph *remote.NodeGraph, node *remote.Node, edges []*Session, problem error) {
	// Operations that only use the adjacent edge need neither a valid tree
	// nor any other edge, so a connected edge provides the route handle
	// regardless of strict availability. Its own endpoint is where they
	// execute, even if another edge describes the node inconsistently.
	for _, edge := range edges {
		if t.edgeConnectedLocked(edge) {
			node.Adjacent, node.Session, node.Alpha = true, edge.Identifier, edge.Tree.Alpha == graph.Source
			endpoint := edge.Beta
			if edge.Tree.Alpha == node.Name {
				endpoint = edge.Alpha
			}
			node.Host, node.Root = commandExecutionLocation(endpoint)
			break
		}
	}

	// Strict availability requires a plan, which refuses any problem.
	if problem != nil {
		node.Reason = problem.Error()
		if !node.Adjacent && len(edges) > 0 {
			node.Reason = fmt.Sprintf("edge %s <-> %s is paused, disconnected, or has commands disabled", edges[0].Tree.Alpha, edges[0].Tree.Beta)
		}
	} else if route, err := t.planLocked(source, index, node.Name); err != nil {
		node.Reason = err.Error()
	} else if err := route.check(source.ctx); err != nil {
		node.Reason = err.Error()
	} else {
		node.Available = true
	}
}

// serveNodeConflicts inspects the full selected path and labels every listing
// by its actual pair, retaining bounded streamed messages for large conflicts.
func (r *commandRelay) serveNodeConflicts(ctx context.Context, submission io.ReadWriteCloser, source int, header *remote.Submission) {
	var steps []treeStep
	var route *treeRoute
	var err error
	if r.controller.session.Tree == nil || header.Adjacent {
		steps = []treeStep{{session: r.controller.session, relay: r, target: 1 - source}}
	} else {
		route, err = r.trees.plan(r, source, header.Node)
		if err == nil && header.Revision != "" && route.revision != header.Revision {
			err = errNodeGraphChanged
		}
		if err == nil {
			steps = route.steps
		}
	}
	if err != nil {
		remote.RespondWithPathConflicts(submission, nil, err)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { submission.Close() })
	defer stop()
	if route != nil {
		release, activationErr := route.activate()
		if activationErr != nil {
			remote.RespondWithPathConflicts(submission, nil, activationErr)
			return
		}
		defer release()
		watch := func(done <-chan struct{}) {
			go func() {
				select {
				case <-done:
					cancel()
				case <-ctx.Done():
				}
			}()
		}
		watch(route.changed)
		for _, step := range steps {
			watch(step.relay.ctx.Done())
			for _, channel := range step.channels {
				watch(channel.Done())
			}
		}
	}
	var edges []remote.EdgeConflicts
	for _, step := range steps {
		alpha, beta := "alpha", "beta"
		if membership := step.session.Tree; membership != nil {
			alpha, beta = membership.Alpha, membership.Beta
		}
		if route != nil {
			err = route.check(ctx)
		}
		if err == nil && header.Progress {
			err = remote.SendPathConflictProgress(submission, &remote.CommandEvent{Alpha: alpha, Beta: beta, Phase: "inspecting conflicts", Progress: &remote.Progress{Status: "scanning"}})
		}
		if err == nil {
			err = step.relay.flushReporting(ctx, submission, header.Progress)
		}
		if header.Progress {
			event := &remote.CommandEvent{Alpha: alpha, Beta: beta, Phase: "inspecting conflicts", Done: true}
			if err != nil {
				event.Error = err.Error()
			}
			if sendErr := remote.SendPathConflictProgress(submission, event); err == nil {
				err = sendErr
			}
		}
		if err != nil {
			break
		}
		if step.relay.conflicts == nil {
			err = errResolutionUnsupported
			break
		}
		listing, listingErr := step.relay.conflicts()
		if listingErr != nil {
			err = listingErr
			break
		}
		edges = append(edges, remote.EdgeConflicts{Alpha: alpha, Beta: beta, Session: step.session.Identifier, SourceAlpha: step.target == 1, Listing: listing})
	}
	remote.RespondWithPathConflicts(submission, edges, err)
}
