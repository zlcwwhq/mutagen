package synchronization

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// errNodeGraphChanged reports a stale destination selection consistently.
var errNodeGraphChanged = errors.New("node graph changed after target selection; query --nodes again")

// commandStars indexes the daemon's sessions and their running command relays
// for node discovery and destination selection. Every star edge is an
// ordinary pair session, so each operation crosses exactly one edge: the only
// routing decision is which edge carries a submission that arrives at a star's
// center, since every edge registers the center's workspace. The index holds
// no derived state, so membership changes never need to cancel operations.
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

// nodeRevision pins immutable session identities. Replacing, adding, or
// removing an edge changes the revision.
func nodeRevision(sessions []*Session) string {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.Identifier
	}
	sort.Strings(ids)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(ids, "\n"))))
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
const unavailableReason = "node is paused, disconnected, or has commands disabled"

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

// pairNodesLocked describes an implicit pair, whose peer is available while
// the source's relay is connected to it. The index lock must be held.
func pairNodesLocked(source *commandRelay, index int) *remote.NodeGraph {
	session := source.controller.session
	graph := &remote.NodeGraph{Source: endpointName(index), Revision: nodeRevision([]*Session{session})}
	for i, endpoint := range []*url.URL{session.Alpha, session.Beta} {
		host, root := commandExecutionLocation(endpoint)
		node := remote.Node{Name: endpointName(i), Host: host, Root: root, Path: []string{graph.Source}, Current: i == index, Available: i == index}
		if !node.Current {
			node.Path = append(node.Path, node.Name)
			node.Mode = synchronizationModeName(session)
			node.Session, node.Alpha = session.Identifier, index == 0
			if node.Available = connectedLocked(source, i); !node.Available {
				node.Reason = "node is disconnected or command execution is disabled"
			}
		}
		graph.Nodes = append(graph.Nodes, node)
	}
	return graph
}

// starNodesLocked describes a star from one of its nodes. The center lists
// every node, each over its own edge, while any other node lists itself and
// the center, over its own edge. A star problem makes every node unavailable
// from the center, whose identity is then ambiguous.
func (s *commandStars) starNodesLocked(source *commandRelay, index int) *remote.NodeGraph {
	session := source.controller.session
	members := s.membersLocked(session.Star.Identifier)
	_, edges, problem := starTopology(members)
	from := session.Star.node(index)
	graph := &remote.NodeGraph{Source: from, Star: true, Revision: nodeRevision(members)}
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
				if node.Available = connectedLocked(source, 0); !node.Available {
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
		} else if node.Available = connectedLocked(s.relays[edge.Identifier], 1); !node.Available {
			node.Reason = unavailableReason
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

// resolveSubmission selects the destination of an operation, which is always
// the opposite endpoint of a single edge, along with the relay of that edge.
// The source is identified by the submitting endpoint, never by client input.
// A submission at a star's center is redirected to the selected node's edge;
// all other submissions use their own edge.
func (r *commandRelay) resolveSubmission(source int, header *remote.Submission) (*commandRelay, int, *remote.ResolvedRoute, error) {
	graph, err := r.stars.nodes(r, source)
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
	star := r.controller.session.Star
	if selected == nil {
		if star != nil && r.stars.starHasNode(star.Identifier, name) {
			return nil, 0, nil, fmt.Errorf("stars have no multi-hop: node %s is reachable only from the center node %s; run the command from %s", name, star.Center, star.Center)
		}
		return nil, 0, nil, fmt.Errorf("unknown node %q; use --nodes to list configured nodes", name)
	} else if selected.Current {
		return nil, 0, nil, errors.New("target is the current node; run the command locally")
	} else if star != nil && source == 0 && graph.Problem != "" {
		return nil, 0, nil, fmt.Errorf("operations from the center node %s are unavailable until the star is repaired, such as by applying the project again: %s", star.Center, graph.Problem)
	} else if !selected.Available {
		return nil, 0, nil, remote.ConnectionError(fmt.Errorf("node %s is unavailable: %s", name, selected.Reason))
	}
	relay := r
	if selected.Session != r.controller.session.Identifier {
		r.stars.Lock()
		relay = r.stars.relays[selected.Session]
		r.stars.Unlock()
		if relay == nil {
			return nil, 0, nil, remote.ConnectionError(errors.New("selected node disconnected"))
		}
	}
	target := 0
	configuration := relay.controller.mergedBetaConfiguration
	if selected.Alpha {
		target = 1
		configuration = relay.controller.mergedAlphaConfiguration
	}
	if configuration == nil {
		configuration = relay.controller.session.Configuration
	}
	header.Node, header.Revision = name, graph.Revision
	return relay, target, &remote.ResolvedRoute{
		Ignores:    EffectiveIgnores(configuration, relay.controller.session.Version),
		Session:    selected.Session,
		Alpha:      selected.Alpha,
		SourceNode: graph.Source,
		Node:       name,
		Star:       graph.Star,
		Revision:   graph.Revision,
	}, nil
}
