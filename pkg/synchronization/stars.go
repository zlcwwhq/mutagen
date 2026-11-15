package synchronization

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// errNodeGraphChanged reports that the edge that an operation's earlier
// submission selected was replaced or removed (see remote.Submission.Pinned).
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

// errExecutionDisabled explains why a node can't be the target of operations
// whatever its connection: its session's remote command mode doesn't let the
// node's endpoint execute commands (see executes). It's an operation error, as
// the executor's refusal of commands is, since retrying doesn't help.
var errExecutionDisabled = errors.New("command execution is disabled there by the session's remote setting")

// errNodeUnavailable explains why a star node's edge can't carry operations.
var errNodeUnavailable = remote.ConnectionError(errors.New("node is paused or disconnected"))

// errPeerDisconnected explains why the peer of an implicit pair can't be the
// target of operations.
var errPeerDisconnected = remote.ConnectionError(errors.New("node is disconnected"))

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

// errCenterUnavailable explains why a node can't be reached from a star's
// center although its edge may run: the edge's command channel at the center,
// which registers the center's workspace for submissions through the edge,
// isn't connected, or the submission couldn't reach that registration, so that
// it entered through another edge (see resolveSubmission).
var errCenterUnavailable = remote.ConnectionError(errors.New("its edge isn't connected for remote commands at the center (it may be paused, terminated, or reconnecting), so retry once it reconnects"))

// availabilityLocked returns nil if the endpoint with the specified index of
// an edge, run by the specified relay, if any, can be the destination of
// operations over the edge, and otherwise the reason why not: the endpoint
// must execute commands (see errExecutionDisabled), and its command channel
// must be connected, as must, from a star's center, the edge's channel at the
// center, which registers the center's workspace, through which operations on
// the node are submitted. Reasons other than errExecutionDisabled are
// connection errors. The index lock must be held.
func availabilityLocked(edge *Session, relay *commandRelay, destination int) error {
	if !executes(edge, destination) {
		return errExecutionDisabled
	} else if !connectedLocked(relay, destination) {
		if edge.Star == nil {
			return errPeerDisconnected
		}
		return errNodeUnavailable
	} else if edge.Star != nil && destination == 1 && !connectedLocked(relay, 0) {
		return errCenterUnavailable
	}
	return nil
}

// setAvailability records the availability of a node (see availabilityLocked).
func setAvailability(node *remote.Node, unavailable error) {
	node.Available = unavailable == nil
	if unavailable != nil {
		node.Reason = unavailable.Error()
	}
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
			node.Session = session.Identifier
			setAvailability(&node, availabilityLocked(session, source, i))
		}
		graph.Nodes = append(graph.Nodes, node)
	}
	return graph
}

// starNodesLocked describes a star from one of its nodes. The center lists
// every node, each over its own edge, which is available while the node
// executes commands and the edge is connected at both ends (see
// availabilityLocked), while any other node lists itself and the center,
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
				setAvailability(&node, availabilityLocked(session, source, 0))
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
		node := remote.Node{Name: name, Host: host, Root: root, Path: []string{from, name}, Mode: synchronizationModeName(edge), Session: edge.Identifier}
		if problem != nil {
			node.Reason = problem.Error()
		} else {
			setAvailability(&node, availabilityLocked(edge, s.relays[edge.Identifier], 1))
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

// resolveSubmission checks the destination of an operation submitted from the
// specified endpoint of the relay's session, which is always the opposite
// endpoint of the same edge. The source is identified by the submitting
// endpoint, never by client input. Operations are never redirected to another
// edge, so that they don't depend on any other edge. At a star's center, where
// every edge registers the center's workspace, remote submits through the
// selected node's own edge (see remote.Selection). A submission that enters
// through another edge, because the selected edge's registration isn't live,
// is refused as a connection failure, just as operations on a pair fail while
// its local registration isn't live. Other errors, such as an unknown node or
// a star problem, are reported whichever edge the submission entered through.
// A pinned submission only depends on its own edge, so that changes to other
// edges don't fail later submissions of an operation.
func (r *commandRelay) resolveSubmission(source int, header *remote.Submission) error {
	graph, err := r.stars.nodes(r, source)
	if err != nil {
		return err
	}
	name := header.Node
	if name == "" {
		if len(graph.Nodes) != 2 {
			return errors.New("multiple destination nodes; use --nodes to inspect them and --node NAME to select one")
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
	pinned := header.Pinned
	if pinned && (selected == nil || selected.Current || selected.Session != r.controller.session.Identifier) {
		return errNodeGraphChanged
	}
	star := r.controller.session.Star
	if selected == nil {
		if star != nil && r.stars.starHasNode(star.Identifier, name) {
			return fmt.Errorf("stars have no multi-hop: node %s is reachable only from the center node %s; run the command from %s", name, star.Center, star.Center)
		}
		return fmt.Errorf("unknown node %q; use --nodes to list configured nodes", name)
	} else if selected.Current {
		return errors.New("target is the current node; run the command locally")
	} else if star != nil && source == 0 && graph.Problem != "" && !pinned {
		return fmt.Errorf("operations from the center node %s are unavailable until the star is repaired, such as by applying the project again: %s", star.Center, graph.Problem)
	}
	// The selected node is available if its edge is, which a star problem
	// doesn't change, so that an operation that selected its edge before the
	// problem arose keeps using it: other edges never affect it. The reason
	// classifies the refusal (see availabilityLocked).
	unavailable := errNodeUnavailable
	r.stars.Lock()
	if edge := r.stars.controllers[selected.Session]; edge != nil {
		unavailable = availabilityLocked(edge.session, r.stars.relays[selected.Session], 1-source)
	}
	r.stars.Unlock()
	if unavailable != nil {
		return fmt.Errorf("node %s is unavailable: %w", name, unavailable)
	}

	// The selected edge must be the one through which the submission entered.
	// They only differ at a star's center, when the client couldn't reach the
	// selected edge's registration there, even if the daemon still considers
	// the edge connected, such as while it reconnects.
	if selected.Session != r.controller.session.Identifier {
		return fmt.Errorf("node %s is unavailable: %w", name, errCenterUnavailable)
	}
	return nil
}
