package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Node describes a workspace that the source can reach, or the source itself.
// Availability is only a snapshot of whether a destination executes commands
// and the edge to it is connected: commands still synchronize and report
// conflicts as for any pair.
type Node struct {
	// Name is scoped to the star or implicit pair.
	Name string
	// Center identifies a star's center node, which reaches every other node
	// over its edge, while the other nodes reach the center over their own
	// edges, and each other through the center (see Path).
	Center bool `json:",omitempty"`
	// Host and Root are configured display values, without credentials.
	Host string
	Root string
	// Path includes the source and, for a destination, the destination name.
	// A star's nodes other than the center reach each other through the
	// center, which the path then includes between them, over two edges: the
	// source's and the destination's. Other destinations are reached over a
	// single edge.
	Path []string
	// Mode is the synchronization mode of the edge to a destination, which, for
	// a destination reached through the center, is the destination's edge.
	Mode string `json:",omitempty"`
	// Current identifies the submitting workspace. Available indicates that a
	// destination executes commands, as its edge's remote setting permits,
	// and that the edge runs and is connected for commands at both ends.
	// Reason explains why a destination isn't available.
	Current   bool
	Available bool
	Reason    string `json:",omitempty"`
	// Session is the daemon-only index of a destination's edge. It's never
	// serialized or used by clients to select a submission registration.
	Session string `json:"-"`
}

// NodeGraph describes the nodes that a source can reach, including
// disconnected ones. Star distinguishes a declared star from an implicit pair.
type NodeGraph struct {
	// Source is the submitting node's name.
	Source string
	// Star marks a declared star workspace.
	Star bool
	// Problem describes why a star's persisted edges are inconsistent, such
	// as edges that disagree on the center's endpoint. Operations from the
	// center are then refused, while other nodes keep using their own edges.
	Problem string `json:",omitempty"`
	// Nodes includes offline nodes, in deterministic name order.
	Nodes []Node
}

// nodesResponse is compatible with status messages reporting route failures.
type nodesResponse struct {
	Error     string
	ErrorKind ErrorKind  `json:",omitempty"`
	Graph     *NodeGraph `json:",omitempty"`
}

// RespondWithNodes reports a graph query and closes its connection.
func RespondWithNodes(connection io.ReadWriteCloser, graph *NodeGraph, err error) error {
	defer connection.Close()
	response := &nodesResponse{Graph: graph}
	if err != nil {
		response.Error, response.ErrorKind = err.Error(), ClassifyError(err)
	}
	return writeMessage(connection, response)
}

// ListNodes reads a graph query response. It owns the connection and cancels
// promptly when its caller disconnects or times out.
func ListNodes(ctx context.Context, connection io.ReadWriteCloser) (*NodeGraph, error) {
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	var response nodesResponse
	if err := readMessage(connection, &response); err != nil {
		return nil, ConnectionError(fmt.Errorf("unable to query nodes: %w", err))
	} else if response.Error != "" {
		return nil, receivedError(response.ErrorKind, errors.New(response.Error))
	} else if response.Graph == nil || response.Graph.Source == "" || len(response.Graph.Nodes) < 2 {
		return nil, ConnectionError(errors.New("invalid node graph response"))
	}
	return response.Graph, nil
}

// Route describes the destination that the daemon selected for a submission
// that requested it (see Submission.Resolve), which the acknowledgement of the
// submission reports.
type Route struct {
	// Node names the destination node.
	Node string `json:",omitempty"`
	// Via names a star's center if the destination is another node that the
	// submitting node reaches through the center, over the edges of both nodes
	// (see Target.Via), and is empty if the destination is the opposite
	// endpoint of the edge through which the submission entered.
	Via string `json:",omitempty"`
	// Edge identifies the destination's edge, which pins later submissions of
	// the operation (see Submission.Edge).
	Edge string `json:",omitempty"`
}

// routeAcknowledgement acknowledges a submission's destination. It decodes
// compatibly as a status, which hops that can't reach the daemon send instead.
type routeAcknowledgement struct {
	// Error is the routing error, if any.
	Error string
	// ErrorKind classifies Error independently of its diagnostic text.
	ErrorKind ErrorKind `json:",omitempty"`
	// Route describes the destination if there's no error.
	Route
}

// AcknowledgeRoute acknowledges the destination selected for a submission
// that requested it (see Submission.Resolve), before the operation's own
// protocol. The destination is the opposite endpoint of the edge whose
// registration the submission entered through, unless the submitting node
// reaches it through a star's center (see Route.Via). Refusals are sent with
// Reject instead.
func AcknowledgeRoute(connection io.Writer, route Route) error {
	return writeMessage(connection, &routeAcknowledgement{Route: route})
}

// receiveRouteAcknowledgement waits for the acknowledgement of a submission's
// destination (see AcknowledgeRoute), or for its refusal.
func receiveRouteAcknowledgement(ctx context.Context, connection io.ReadWriteCloser) (Route, error) {
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	var response routeAcknowledgement
	if err := readMessage(connection, &response); err != nil {
		return Route{}, ConnectionError(fmt.Errorf("unable to receive selected node: %w", err))
	} else if response.Error != "" {
		return Route{}, receivedError(response.ErrorKind, errors.New(response.Error))
	}
	return response.Route, nil
}
