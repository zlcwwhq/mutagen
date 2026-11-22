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
// ordinary pair session, and each operation crosses the edge through which it
// was submitted, which, at a star's center, where every edge registers the
// center's workspace, must be the selected node's own, and, from another node
// on a node other than the center, continues over that node's edge (see
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

// submittingEndpoints reports whether a session's alpha and beta endpoints
// submit commands under its remote command and submission modes (see
// Configuration.RemoteSubmissionMode): by default, those whose opposite
// endpoints execute commands, and otherwise those that the submission mode
// names, if their opposite endpoints execute commands or, at a star's node
// other than the center, which is beta, regardless, since it also reaches the
// star's other nodes through the center.
func submittingEndpoints(session *Session) (bool, bool) {
	alphaExecutes, betaExecutes := executingEndpoints(session)
	mode := session.GetConfiguration().GetRemoteSubmissionMode()
	if mode.IsDefault() {
		return betaExecutes, alphaExecutes
	}
	alpha, beta := mode.Executes()
	return alpha && betaExecutes, beta && (alphaExecutes || session.Star != nil)
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
// operations, and otherwise the reason why not: the endpoint must execute
// commands (see errExecutionDisabled), and its command channel must be
// connected, as must, for operations submitted at a star's center, the edge's
// channel at the center, which registers the center's workspace, through which
// operations on the node are submitted. Operations from another node of the
// star, which reaches the destination through the center (see
// resolveSubmission), don't use that channel. Reasons other than
// errExecutionDisabled are connection errors. The index lock must be held.
func availabilityLocked(edge *Session, relay *commandRelay, destination int, fromCenter bool) error {
	if !executes(edge, destination) {
		return errExecutionDisabled
	} else if !connectedLocked(relay, destination) {
		if edge.Star == nil {
			return errPeerDisconnected
		}
		return errNodeUnavailable
	} else if fromCenter && !connectedLocked(relay, 0) {
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
// every node over its edge, and every other node reaches the center over its
// own edge and the star's other nodes through the center. Other sessions are
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
			setAvailability(&node, availabilityLocked(session, source, i, false))
		}
		graph.Nodes = append(graph.Nodes, node)
	}
	return graph
}

// starNodesLocked describes a star from one of its nodes. The center lists
// every node, each over its own edge, which is available while the node
// executes commands and the edge is connected at both ends (see
// availabilityLocked). Any other node lists itself, the center, over its own
// edge, which is available while the center executes commands and the edge is
// connected, and the star's other nodes, through the center over their own
// edges, which are available while they execute commands and their edges are
// connected at the nodes, since operations through the center don't use their
// channels at the center. A star problem makes every node unavailable from the
// center, whose identity is then ambiguous, and so, for the same reason, every
// node reached through the center.
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
	describe := func(name string, edge *Session, path []string) remote.Node {
		host, root := commandExecutionLocation(edge.Beta)
		node := remote.Node{Name: name, Host: host, Root: root, Path: path, Mode: synchronizationModeName(edge), Session: edge.Identifier}
		if problem != nil {
			node.Reason = problem.Error()
		} else {
			setAvailability(&node, availabilityLocked(edge, s.relays[edge.Identifier], 1, index == 0))
		}
		return node
	}
	if index == 1 {
		for i, endpoint := range []*url.URL{session.Alpha, session.Beta} {
			host, root := commandExecutionLocation(endpoint)
			name := session.Star.node(i)
			node := remote.Node{Name: name, Center: i == 0, Host: host, Root: root, Path: []string{from}, Current: i == 1, Available: i == 1}
			if i == 0 {
				node.Path = append(node.Path, name)
				node.Mode = synchronizationModeName(session)
				node.Session = session.Identifier
				setAvailability(&node, availabilityLocked(session, source, 0, false))
			}
			graph.Nodes = append(graph.Nodes, node)
		}
		for name, edge := range edges {
			if name != from {
				graph.Nodes = append(graph.Nodes, describe(name, edge, []string{from, center, name}))
			}
		}
		sort.Slice(graph.Nodes, func(i, j int) bool { return graph.Nodes[i].Name < graph.Nodes[j].Name })
		return graph
	}
	host, root := commandExecutionLocation(session.Alpha)
	graph.Nodes = append(graph.Nodes, remote.Node{Name: center, Center: true, Host: host, Root: root, Path: []string{from}, Current: true, Available: true})
	for name, edge := range edges {
		graph.Nodes = append(graph.Nodes, describe(name, edge, []string{from, name}))
	}
	sort.Slice(graph.Nodes, func(i, j int) bool { return graph.Nodes[i].Name < graph.Nodes[j].Name })
	return graph
}

// destination is the resolved destination of a submission (see
// resolveSubmission): the endpoint with the specified index of the edge run by
// the specified relay, which is the submission's own relay unless the
// submitting node reaches the destination through a star's center.
type destination struct {
	// relay runs the destination's edge.
	relay *commandRelay
	// index is the index of the destination's endpoint on its edge.
	index int
	// node names the destination node.
	node string
	// via names the center if the destination is reached through it, and is
	// empty otherwise.
	via string
}

// route describes a destination for its acknowledgement.
func (d *destination) route() remote.Route {
	return remote.Route{Node: d.node, Via: d.via, Edge: d.relay.controller.session.Identifier}
}

// errPinnedWithoutEdge explains why a pinned submission that doesn't identify
// its destination's edge is refused (see resolveSubmission).
var errPinnedWithoutEdge = errors.New("pinned submission doesn't identify its destination's edge; remote must be the same version as the daemon")

// errAcrossEdges explains why a destination reached through a star's center
// doesn't support conflict listing and resolution, which belong to each edge.
func errAcrossEdges(d *destination) error {
	return fmt.Errorf("conflicts are listed and resolved on the edge where they arise, and node %[1]s is reached through the center node %[2]s, over two edges: "+
		"run remote --node %[1]s --conflicts from %[2]s, or remote --conflicts from %[1]s, for the conflicts on the edge to %[1]s, or remote --conflicts here for those on this node's edge", d.node, d.via)
}

// resolveSubmission resolves the destination of an operation submitted from
// the specified endpoint of the relay's session. The source is identified by
// the submitting endpoint, never by client input, and its registration, which
// exists only if its remote submission mode permits (see submittingEndpoints),
// authorizes it to submit operations. The destination is the opposite endpoint
// of the same edge, except at a star's node other than the center, which also
// reaches the star's other nodes through the center: operations then continue
// from the center over the destination's own edge, which the destination's
// remote command mode must let it execute commands on, as for any destination,
// while the center's remote settings don't matter. Operations never depend on
// other edges. At a star's center, where every edge registers the center's
// workspace, remote submits through the selected node's own edge (see
// remote.Selection). A submission that enters there through another edge,
// because the selected edge's registration isn't live, is refused as a
// connection failure, just as operations on a pair fail while its local
// registration isn't live. Other errors, such as an unknown node or a star
// problem, are reported whichever edge the submission entered through. A
// pinned submission only depends on its own edges, which it identifies (see
// remote.Submission.Edge), so that changes to other edges don't fail later
// submissions of an operation.
func (r *commandRelay) resolveSubmission(source int, header *remote.Submission) (*destination, error) {
	graph, err := r.stars.nodes(r, source)
	if err != nil {
		return nil, err
	}
	own := r.controller.session.Identifier
	star := r.controller.session.Star

	// Select the destination. At a star's node other than the center, the
	// center is the default destination.
	name := header.Node
	if name == "" && star != nil && source == 1 {
		name = star.Center
	} else if name == "" {
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
	// A pinned submission identifies its destination's edge, which it must
	// still select. Without it, it could only come from a client that predates
	// destinations through the center, which pins the registration's peer
	// rather than the destination that the daemon resolved, so it's refused
	// rather than sent to the peer.
	pinned := header.Pinned
	if pinned && header.Edge == "" {
		return nil, errPinnedWithoutEdge
	} else if pinned && (selected == nil || selected.Current || selected.Session != header.Edge) {
		return nil, errNodeGraphChanged
	}
	across := selected != nil && star != nil && source == 1 && selected.Session != own
	if selected == nil {
		return nil, fmt.Errorf("unknown node %q; use --nodes to list configured nodes", name)
	} else if selected.Current {
		return nil, errors.New("target is the current node; run the command locally")
	} else if star != nil && (source == 0 || across) && graph.Problem != "" && !pinned {
		if source == 0 {
			return nil, fmt.Errorf("operations from the center node %s are unavailable until the star is repaired, such as by applying the project again: %s", star.Center, graph.Problem)
		}
		return nil, fmt.Errorf("operations through the center node %s are unavailable until the star is repaired, such as by applying the project again: %s", star.Center, graph.Problem)
	}

	// The selected node is available if its edge is, which a star problem
	// doesn't change, so that an operation that selected its edge before the
	// problem arose keeps using it: other edges never affect it. The reason
	// classifies the refusal (see availabilityLocked).
	result := &destination{relay: r, index: 1 - source, node: name}
	unavailable := errNodeUnavailable
	r.stars.Lock()
	if edge := r.stars.controllers[selected.Session]; edge != nil {
		relay := r.stars.relays[selected.Session]
		if across {
			unavailable = availabilityLocked(edge.session, relay, 1, false)
			result.relay, result.index, result.via = relay, 1, star.Center
		} else {
			unavailable = availabilityLocked(edge.session, relay, 1-source, star != nil && source == 0)
		}
	}
	r.stars.Unlock()
	if unavailable != nil {
		return nil, fmt.Errorf("node %s is unavailable: %w", name, unavailable)
	}

	// At a star's center, the selected edge must be the one through which the
	// submission entered. They only differ when the client couldn't reach the
	// selected edge's registration there, even if the daemon still considers
	// the edge connected, such as while it reconnects.
	if !across && selected.Session != own {
		return nil, fmt.Errorf("node %s is unavailable: %w", name, errCenterUnavailable)
	}
	return result, nil
}
