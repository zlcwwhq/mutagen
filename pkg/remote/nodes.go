package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Node describes a workspace in the source's graph. Reachability is only a
// connection snapshot: command preflight still checks synchronization results.
type Node struct {
	// Name and Parent are scoped to this workspace graph.
	Name   string
	Parent string `json:",omitempty"`
	// Host and Root are configured display values, without credentials.
	Host string
	Root string
	// Path includes both the source and destination names.
	Path []string
	// Current identifies the submitting workspace. Available describes only
	// connections: in a declared tree, it means that a strict tree command can
	// be routed along Path, and in an implicit pair, that the peer is
	// connected. Reason explains why a node isn't available.
	Current   bool
	Available bool
	Reason    string `json:",omitempty"`
	// Adjacent indicates that the node is directly adjacent to the source over
	// an edge whose synchronization loop runs and whose command channels are
	// both connected. Operations that only use that edge, such as job control
	// and conflict resolution, require it rather than Available. New commands,
	// including detached starts, additionally require strict tree preflight.
	// It equals Available for an implicit pair's peer.
	Adjacent bool `json:",omitempty"`
	// Session and Alpha are daemon-only indexes for adjacent edges. They are
	// never serialized or used by clients to select a submission registration.
	Session string `json:"-"`
	Alpha   bool   `json:"-"`
}

// NodeGraph describes the configured nodes, including disconnected members.
// Tree distinguishes an explicitly declared tree from an implicit pair.
type NodeGraph struct {
	// Source is the submitting node's name.
	Source string
	// Tree marks a declared tree, whose foreground execution uses strict sync.
	Tree bool
	// Revision pins immutable graph membership for subsequent operations.
	Revision string
	// Problem describes why a declared tree's persisted membership can't be
	// used for strict tree commands, such as an edge missing from the tree,
	// inconsistent edge configurations, or another session overlapping a node.
	// Every strict tree command is then refused, but the graph still lists the
	// nodes known from the tree's edges, as far as they can be reconstructed,
	// and adjacent nodes over connected edges remain usable (see Adjacent).
	Problem string `json:",omitempty"`
	// Nodes includes offline members, in deterministic name order.
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
// Session and Alpha describe the actual adjacent edge solely for checkout
// ownership and conflict direction; they never select a local registration.
type ResolvedRoute struct {
	// Ignores belong to the selected edge, even in a degraded tree.
	Ignores    *Ignores `json:",omitempty"`
	Session    string
	Alpha      bool
	SourceNode string
	Node       string
	Tree       bool
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
