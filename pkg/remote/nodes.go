package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Node describes a workspace that the source can reach, or the source itself.
// Availability is only a connection snapshot: commands still synchronize and
// report conflicts as for any pair.
type Node struct {
	// Name is scoped to the star or implicit pair.
	Name string
	// Center identifies a star's center node, which reaches every other node,
	// while the other nodes reach only the center.
	Center bool `json:",omitempty"`
	// Host and Root are configured display values, without credentials.
	Host string
	Root string
	// Path includes the source and, for a destination, the destination name.
	// Every destination is reached over a single edge.
	Path []string
	// Mode is the synchronization mode of the edge to a destination.
	Mode string `json:",omitempty"`
	// Current identifies the submitting workspace. Available indicates that
	// the edge to a destination runs and is connected for commands. Reason
	// explains why a destination isn't available.
	Current   bool
	Available bool
	Reason    string `json:",omitempty"`
	// Session and Alpha are daemon-only indexes of a destination's edge and
	// the source's role on it. They are never serialized or used by clients
	// to select a submission registration.
	Session string `json:"-"`
	Alpha   bool   `json:"-"`
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

// ResolvedRoute acknowledges destination selection on an operation connection.
// Session and Alpha describe the selected edge for checkout ownership and
// conflict direction, and identify its registration at the submitting node,
// which later submissions prefer (see Selection.Target). Revision pins the
// selected edge for them.
type ResolvedRoute struct {
	// Ignores belong to the selected edge.
	Ignores    *Ignores `json:",omitempty"`
	Session    string
	Alpha      bool
	SourceNode string
	Node       string
	Star       bool
	Revision   string
}

type resolvedRouteResponse struct {
	Error     string
	ErrorKind ErrorKind      `json:",omitempty"`
	Route     *ResolvedRoute `json:",omitempty"`
}

// RespondWithRoute sends routing metadata before the operation's own protocol.
func RespondWithRoute(connection io.Writer, route *ResolvedRoute) error {
	return writeMessage(connection, &resolvedRouteResponse{Route: route})
}

func receiveResolvedRoute(ctx context.Context, connection io.ReadWriteCloser) (*ResolvedRoute, error) {
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	var response resolvedRouteResponse
	if err := readMessage(connection, &response); err != nil {
		return nil, ConnectionError(fmt.Errorf("unable to receive selected node: %w", err))
	} else if response.Error != "" {
		return nil, receivedError(response.ErrorKind, errors.New(response.Error))
	} else if response.Route == nil || response.Route.Node == "" {
		return nil, ConnectionError(errors.New("missing selected node"))
	}
	return response.Route, nil
}
