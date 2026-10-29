package synchronization

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// errNodeGraphChanged reports a stale destination selection consistently.
var errNodeGraphChanged = errors.New("node graph changed after target selection; query --nodes again")

// commandStars indexes the daemon's sessions and their running command relays
// for node discovery and destination selection. Every star edge is an
// ordinary pair session, and each operation crosses exactly one edge: the edge
// through which it was submitted, which, at a star's center, where every edge
// registers the center's workspace, must be the selected node's own (see
// resolveSubmission). The index holds no derived state, so membership changes
// never need to cancel operations.
type commandStars struct {
	sync.Mutex
	// controllers includes paused and disconnected sessions.
	controllers map[string]*controller
	// relays records each currently running command relay.
	relays map[string]*commandRelay
}

// newCommandStars creates the manager's runtime star index.
func newCommandStars() *commandStars {
	return &commandStars{controllers: make(map[string]*controller), relays: make(map[string]*commandRelay)}
}

// update records a session, or removes it when it's terminated.
func (s *commandStars) update(c *controller, remove bool) {
	if s == nil {
		return
	}
	s.Lock()
	defer s.Unlock()
	if remove {
		delete(s.controllers, c.session.Identifier)
	} else {
		s.controllers[c.session.Identifier] = c
	}
}

// setRelay registers a command relay, or removes it when it stops.
func (s *commandStars) setRelay(c *controller, relay *commandRelay) {
	if s == nil {
		return
	}
	s.Lock()
	defer s.Unlock()
	if relay == nil {
		delete(s.relays, c.session.Identifier)
	} else {
		s.relays[c.session.Identifier] = relay
	}
}

// membersLocked returns the persisted edges of a star, including those of
// paused and disconnected sessions. The index lock must be held.
func (s *commandStars) membersLocked(identifier string) []*Session {
	var members []*Session
	for _, controller := range s.controllers {
		if controller.session.GetStar().GetIdentifier() == identifier {
			members = append(members, controller.session)
		}
	}
	return members
}

// edgeRevision pins the immutable identity of the edge that carries an
// operation, identified by its session, so that the operation's later
// submissions fail if that edge is replaced or removed, while changes to other
// edges, such as adding, pausing, replacing, or removing other nodes, don't
// affect them.
func edgeRevision(session string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(session)))
}

// synchronizationModeName names a session's effective synchronization mode.
func synchronizationModeName(session *Session) string {
	mode := session.Configuration.SynchronizationMode
	if mode.IsDefault() {
		mode = session.Version.DefaultSynchronizationMode()
	}
	name, _ := mode.MarshalText()
	return string(name)
}

// connectedLocked reports whether a relay runs and its channel to the endpoint
// with the specified index is connected. The index lock must be held, which
// permits taking channelsLock.
func connectedLocked(relay *commandRelay, index int) bool {
	if relay == nil || relay.ctx.Err() != nil {
		return false
	}
	relay.channelsLock.Lock()
	channel := relay.channels[index]
	relay.channelsLock.Unlock()
	if channel == nil {
		return false
	}
	select {
	case <-channel.Done():
		return false
	default:
		return true
	}
}

// unavailableReason explains why a node's edge can't carry operations.
const unavailableReason = "node is paused or disconnected"

// executionDisabledReason explains why a node can't be the target of
// operations whatever its connection: its session's remote command mode
// doesn't let the node's endpoint execute commands (see executes).
const executionDisabledReason = "command execution is disabled there by the session's remote setting"

// executingEndpoints reports whether a session's alpha and beta endpoints
// execute commands under its remote command mode.
func executingEndpoints(session *Session) (bool, bool) {
	mode := session.GetConfiguration().GetRemoteCommandMode()
	if mode.IsDefault() {
		mode = session.Version.DefaultRemoteCommandMode()
	}
	return mode.Executes()
}

// executes reports whether the endpoint of a session with the specified index
// executes commands under its remote command mode.
func executes(session *Session, index int) bool {
	alpha, beta := executingEndpoints(session)
	return index == 0 && alpha || index == 1 && beta
}

// centerUnavailableReason explains why a node can't be reached from a star's
// center although its edge may run: the edge's command channel at the center,
// which registers the center's workspace for submissions through the edge,
// isn't connected, or the submission couldn't reach that registration, so that
// it entered through another edge (see resolveSubmission).
const centerUnavailableReason = "its edge isn't connected for remote commands at the center (it may be paused, terminated, or reconnecting), so retry once it reconnects"

// starEdgeAvailableLocked reports whether a star's edge, run by the specified
// relay, if any, can carry operations from the center, and otherwise explains
// why not. The node must execute commands, and both of the edge's command
// channels must be connected: the channel at the center registers the
// center's workspace, through which operations on the node are submitted, and
// the channel at the node executes them. The index lock must be held.
func starEdgeAvailableLocked(edge *Session, relay *commandRelay) (bool, string) {
	if !executes(edge, 1) {
		return false, executionDisabledReason
	} else if !connectedLocked(relay, 1) {
		return false, unavailableReason
	} else if !connectedLocked(relay, 0) {
		return false, centerUnavailableReason
	}
	return true, ""
}

// nodes describes the nodes that the source can reach, including paused and
// disconnected ones, and their connection availability, without synchronizing,
// inspecting file contents, or opening any route. A star's center reaches
// every node, and every other node reaches only the center. Other sessions are
// implicit pairs of nodes named alpha and beta. It only fails if the source
// itself isn't connected: a star problem is reported in the graph instead.
func (s *commandStars) nodes(source *commandRelay, index int) (*remote.NodeGraph, error) {
	if s == nil || source.controller == nil {
		return nil, errors.New("node discovery is unavailable")
	}
	s.Lock()
	defer s.Unlock()
	session := source.controller.session
	if s.relays[session.Identifier] != source {
		return nil, remote.ConnectionError(errors.New("source node disconnected"))
	}
	if session.Star == nil {
		return pairNodesLocked(source, index), nil
	}
	return s.starNodesLocked(source, index), nil
}

// pairNodesLocked describes an implicit pair, whose peer is available while it
// executes commands and the source's relay is connected to it. The index lock
// must be held.
func pairNodesLocked(source *commandRelay, index int) *remote.NodeGraph {
	session := source.controller.session
	graph := &remote.NodeGraph{Source: endpointName(index)}
	for i, endpoint := range []*url.URL{session.Alpha, session.Beta} {
		host, root := commandExecutionLocation(endpoint)
		node := remote.Node{Name: endpointName(i), Host: host, Root: root, Path: []string{graph.Source}, Current: i == index, Available: i == index}
		if !node.Current {
			node.Path = append(node.Path, node.Name)
			node.Mode = synchronizationModeName(session)
			node.Session, node.Alpha = session.Identifier, index == 0
			if !executes(session, i) {
				node.Reason = executionDisabledReason
			} else if node.Available = connectedLocked(source, i); !node.Available {
				node.Reason = "node is disconnected"
			}
		}
		graph.Nodes = append(graph.Nodes, node)
	}
	return graph
}

// starNodesLocked describes a star from one of its nodes. The center lists
// every node, each over its own edge, which is available while the node
// executes commands and the edge is connected at both ends (see
// starEdgeAvailableLocked), while any other node lists itself and the center,
// over its own edge, which is available while the center executes commands
// and is connected. A star problem makes every node unavailable from the
// center, whose identity is then ambiguous.
func (s *commandStars) starNodesLocked(source *commandRelay, index int) *remote.NodeGraph {
	session := source.controller.session
	members := s.membersLocked(session.Star.Identifier)
	_, edges, problem := starTopology(members)
	from := session.Star.node(index)
	graph := &remote.NodeGraph{Source: from, Star: true}
	if problem != nil {
		graph.Problem = problem.Error()
	}
	center := session.Star.Center
	if index == 1 {
		for i, endpoint := range []*url.URL{session.Alpha, session.Beta} {
			host, root := commandExecutionLocation(endpoint)
			name := session.Star.node(i)
			node := remote.Node{Name: name, Center: i == 0, Host: host, Root: root, Path: []string{from}, Current: i == 1, Available: i == 1}
			if i == 0 {
				node.Path = append(node.Path, name)
				node.Mode = synchronizationModeName(session)
				node.Session = session.Identifier
				if !executes(session, 0) {
					node.Reason = executionDisabledReason
				} else if node.Available = connectedLocked(source, 0); !node.Available {
					node.Reason = unavailableReason
				}
			}
			graph.Nodes = append(graph.Nodes, node)
		}
		sort.Slice(graph.Nodes, func(i, j int) bool { return graph.Nodes[i].Name < graph.Nodes[j].Name })
		return graph
	}
	host, root := commandExecutionLocation(session.Alpha)
	graph.Nodes = append(graph.Nodes, remote.Node{Name: center, Center: true, Host: host, Root: root, Path: []string{from}, Current: true, Available: true})
	for name, edge := range edges {
		host, root := commandExecutionLocation(edge.Beta)
		node := remote.Node{Name: name, Host: host, Root: root, Path: []string{from, name}, Mode: synchronizationModeName(edge), Session: edge.Identifier, Alpha: true}
		if problem != nil {
			node.Reason = problem.Error()
		} else {
			node.Available, node.Reason = starEdgeAvailableLocked(edge, s.relays[edge.Identifier])
		}
		graph.Nodes = append(graph.Nodes, node)
	}
	sort.Slice(graph.Nodes, func(i, j int) bool { return graph.Nodes[i].Name < graph.Nodes[j].Name })
	return graph
}

// starHasNode reports whether a star has an edge for the named node.
func (s *commandStars) starHasNode(identifier, name string) bool {
	s.Lock()
	defer s.Unlock()
	for _, member := range s.membersLocked(identifier) {
		if member.Star.Node == name {
			return true
		}
	}
	return false
}

// resolveSubmission selects the destination of an operation submitted from
// the specified endpoint of the relay's session, which is always the opposite
// endpoint of the same edge, and describes the route. The source is identified
// by the submitting endpoint, never by client input. Operations are never
// redirected to another edge, so that they don't depend on any other edge. At
// a star's center, where every edge registers the center's workspace, remote
// submits through the selected node's own edge (see
// remote.Selection.Workspace). A submission that enters through another edge,
// because the selected edge's registration isn't live, is refused as a
// connection failure, just as operations on a pair fail while its local
// registration isn't live. Other errors, such as an unknown node or a star
// problem, are reported whichever edge the submission entered through. A
// pinned revision only refers to the selected edge, so that changes to other
// edges don't fail later submissions of an operation.
func (r *commandRelay) resolveSubmission(source int, header *remote.Submission) (*remote.ResolvedRoute, error) {
	graph, err := r.stars.nodes(r, source)
	if err != nil {
		return nil, err
	}
	name := header.Node
	if name == "" {
		if len(graph.Nodes) != 2 {
			return nil, errors.New("multiple destination nodes; use --nodes to inspect them and --node NAME to select one")
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
	pinned := header.Revision != ""
	if pinned && (selected == nil || selected.Current || edgeRevision(selected.Session) != header.Revision) {
		return nil, errNodeGraphChanged
	}
	star := r.controller.session.Star
	if selected == nil {
		if star != nil && r.stars.starHasNode(star.Identifier, name) {
			return nil, fmt.Errorf("stars have no multi-hop: node %s is reachable only from the center node %s; run the command from %s", name, star.Center, star.Center)
		}
		return nil, fmt.Errorf("unknown node %q; use --nodes to list configured nodes", name)
	} else if selected.Current {
		return nil, errors.New("target is the current node; run the command locally")
	} else if star != nil && source == 0 && graph.Problem != "" && !pinned {
		return nil, fmt.Errorf("operations from the center node %s are unavailable until the star is repaired, such as by applying the project again: %s", star.Center, graph.Problem)
	}
	// An operation that selected its edge before a star problem arose keeps
	// using that edge, which the problem doesn't change, subject only to the
	// edge's connection: other edges never affect it.
	available, reason := selected.Available, selected.Reason
	if !available && pinned && source == 0 && graph.Problem != "" {
		r.stars.Lock()
		if edge := r.stars.controllers[selected.Session]; edge != nil {
			available, reason = starEdgeAvailableLocked(edge.session, r.stars.relays[selected.Session])
		}
		r.stars.Unlock()
	}

	// A node that doesn't execute commands is refused as an operation error,
	// as its executor refuses commands, since retrying doesn't help, while
	// other unavailable nodes are connection failures.
	if !available && reason == executionDisabledReason {
		return nil, fmt.Errorf("node %s is unavailable: %s", name, reason)
	} else if !available {
		return nil, remote.ConnectionError(fmt.Errorf("node %s is unavailable: %s", name, reason))
	}

	// The selected edge must be the one through which the submission entered.
	// They only differ at a star's center, when the client couldn't reach the
	// selected edge's registration there, even if the daemon still considers
	// the edge connected, such as while it reconnects.
	if selected.Session != r.controller.session.Identifier {
		return nil, remote.ConnectionError(fmt.Errorf("node %s is unavailable: %s", name, centerUnavailableReason))
	}
	configuration := r.controller.mergedBetaConfiguration
	if selected.Alpha {
		configuration = r.controller.mergedAlphaConfiguration
	}
	if configuration == nil {
		configuration = r.controller.session.Configuration
	}
	revision := edgeRevision(selected.Session)
	header.Node, header.Revision = name, revision
	return &remote.ResolvedRoute{
		Ignores:    EffectiveIgnores(configuration, r.controller.session.Version),
		Session:    selected.Session,
		Alpha:      selected.Alpha,
		SourceNode: graph.Source,
		Node:       name,
		Star:       graph.Star,
		Revision:   revision,
	}, nil
}
