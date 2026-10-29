package project

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"google.golang.org/protobuf/proto"

	models "github.com/mutagen-io/mutagen/pkg/api/models/synchronization"
	svc "github.com/mutagen-io/mutagen/pkg/service/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// TreeNode is a named workspace. Parent determines its synchronization edge;
// the sole node without a parent is the structural root, not an authority for
// resolving conflicts. Execution settings belong to nodes, not edges.
type TreeNode struct {
	// Endpoint is the URL of this node's directory workspace.
	Endpoint string `yaml:"endpoint"`
	// Parent names the adjacent node toward the structural root.
	Parent string `yaml:"parent"`
	// RemotePrologue is the node's default execution prologue.
	RemotePrologue string `yaml:"remotePrologue"`
}

// TreeConfiguration declares one tree managed by the project's daemon. Remote
// explicitly authorizes commands between all of its nodes. Sync is shared by
// every edge; endpoint-specific pair defaults, and the remote command mode and
// prologue of the synchronization defaults, do not apply to tree nodes.
type TreeConfiguration struct {
	// Remote grants command submission and execution to every node.
	Remote bool `yaml:"remote"`
	// Sync contains the synchronization settings shared by every edge.
	Sync models.Configuration `yaml:"sync"`
	// Nodes maps project-scoped names to directory workspaces.
	Nodes map[string]TreeNode `yaml:"nodes"`
}

// treeSpecifications validates the complete tree before producing sessions.
// Each edge records its membership (see synchronization.TreeMembership), which
// lets the daemon reconstruct the tree after a restart without access to the
// project's configuration file.
func treeSpecifications(tree *TreeConfiguration, defaults *synchronization.Configuration, project string, paused bool) ([]*svc.CreationSpecification, error) {
	if tree == nil {
		return nil, nil
	}
	configuration := tree.Sync.ToInternal()
	if !configuration.RemoteCommandMode.IsDefault() || configuration.RemotePrologue != "" {
		return nil, errors.New("use tree.remote and each node's remotePrologue for tree commands")
	}
	configuration = synchronization.MergeConfigurations(defaults, configuration)
	if !configuration.SynchronizationMode.IsDefault() && configuration.SynchronizationMode != core.SynchronizationMode_SynchronizationModeTwoWaySafe {
		return nil, errors.New("workspace trees require two-way-safe synchronization")
	}
	configuration.SynchronizationMode = core.SynchronizationMode_SynchronizationModeTwoWaySafe
	// Execution settings belong to nodes, not edges. The defaults (which
	// include the global configuration) target ordinary pairs: their command
	// mode must not grant tree-wide execution, and their prologue must not
	// reach the shared configuration, which underlies every node's merged
	// endpoint configuration and would run it on each node without its own.
	// Only tree.remote and each node's remotePrologue apply to tree commands.
	configuration.RemoteCommandMode = synchronization.RemoteCommandMode_RemoteCommandModeDisabled
	configuration.RemotePrologue = ""
	if tree.Remote {
		configuration.RemoteCommandMode = synchronization.RemoteCommandMode_RemoteCommandModeBoth
	}
	if err := configuration.EnsureValid(false); err != nil {
		return nil, err
	} else if _, err := synchronization.NewIgnorer(configuration, synchronization.DefaultVersion); err != nil {
		return nil, err
	}

	var names []string
	endpoints := make(map[string]*url.URL)
	for name, node := range tree.Nodes {
		if node.Endpoint == "" {
			return nil, fmt.Errorf("node %s has no endpoint", name)
		}
		endpoint, err := url.Parse(node.Endpoint, url.Kind_Synchronization, true)
		if err != nil {
			return nil, fmt.Errorf("invalid endpoint for node %s: %w", name, err)
		}
		if err := (&synchronization.Configuration{RemotePrologue: node.RemotePrologue}).EnsureValid(true); err != nil {
			return nil, fmt.Errorf("invalid execution settings for node %s: %w", name, err)
		}
		if endpoint.Protocol == url.Protocol_Local {
			if info, err := os.Stat(endpoint.Path); err == nil && !info.IsDir() {
				return nil, fmt.Errorf("node %s must be a directory", name)
			} else if err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("unable to inspect node %s: %w", name, err)
			}
		}
		endpoints[name] = endpoint
		names = append(names, name)
	}
	// Validate the complete declared topology with the same rules that the
	// daemon uses to reconstruct persisted tree edges.
	topology := make(map[string]synchronization.WorkspaceTreeNode, len(tree.Nodes))
	for name, node := range tree.Nodes {
		topology[name] = synchronization.WorkspaceTreeNode{Parent: node.Parent, Endpoint: endpoints[name]}
	}
	root, err := synchronization.ValidateWorkspaceTree(topology, "")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var result []*svc.CreationSpecification
	for _, name := range names {
		node := tree.Nodes[name]
		if node.Parent == "" {
			continue
		}
		result = append(result, &svc.CreationSpecification{
			Name:  "tree-" + name,
			Alpha: endpoints[node.Parent], Beta: endpoints[name],
			Configuration:      proto.Clone(configuration).(*synchronization.Configuration),
			ConfigurationAlpha: &synchronization.Configuration{RemotePrologue: tree.Nodes[node.Parent].RemotePrologue},
			ConfigurationBeta:  &synchronization.Configuration{RemotePrologue: node.RemotePrologue},
			Labels: map[string]string{
				LabelKey: project,
			},
			Tree: &synchronization.TreeMembership{
				Identifier: project,
				Root:       root,
				Alpha:      node.Parent,
				Beta:       name,
			},
			Paused: paused,
		})
	}
	return result, nil
}
