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

// starSessionPrefix prefixes the names of the sessions that a star expands
// into, which are followed by the names of their non-center nodes.
const starSessionPrefix = "star-"

// errTreeRemoved explains how to migrate a project file that declares a
// workspace tree, which stars replaced.
var errTreeRemoved = errors.New("workspace trees (tree:) were removed; declare a star (star:) instead: " +
	"list the same nodes without parent, name the former root as the star's center " +
	"(center: <node>), and apply the project again, which recreates the sessions as star-<node>")

// StarNode is a named workspace. Execution settings belong to nodes. Mode
// configures the synchronization of a non-center node's own edge with the
// center.
type StarNode struct {
	// Endpoint is the URL of this node's directory workspace.
	Endpoint string `yaml:"endpoint"`
	// Mode is the synchronization mode of the node's edge with the center,
	// which is the edge's alpha. It can't be set on the center.
	Mode core.SynchronizationMode `yaml:"mode"`
	// RemotePrologue is the node's default execution prologue.
	RemotePrologue string `yaml:"remotePrologue"`
}

// StarConfiguration declares one star managed by the project's daemon: a
// center node and other nodes, each joined to the center by its own pair
// session. Remote explicitly authorizes commands between the center and every
// other node. Sync is shared by every edge; endpoint-specific pair defaults,
// and the remote command mode and prologue of the synchronization defaults,
// don't apply to star nodes.
type StarConfiguration struct {
	// Remote grants command submission and execution between the center and
	// every other node.
	Remote bool `yaml:"remote"`
	// Sync contains the synchronization settings shared by every edge.
	Sync models.Configuration `yaml:"sync"`
	// Center names the node that every edge joins to another node.
	Center string `yaml:"center"`
	// Nodes maps project-scoped names to directory workspaces, including the
	// center.
	Nodes map[string]StarNode `yaml:"nodes"`
}

// starSpecifications validates a star before producing its sessions, one for
// each non-center node, whose alpha is the center and whose beta is the node.
// Each edge records its membership (see synchronization.StarMembership), which
// lets the daemon reconstruct the star after a restart without access to the
// project's configuration file. Overlap with other sessions is checked by the
// caller.
func starSpecifications(star *StarConfiguration, defaults *synchronization.Configuration, project string, paused bool) ([]*svc.CreationSpecification, error) {
	if star == nil {
		return nil, nil
	}

	// Compute the shared configuration. Execution settings belong to nodes,
	// not edges. The defaults (which include the global configuration) target
	// ordinary pairs: their command mode must not grant star-wide execution,
	// and their prologue must not reach the shared configuration, which
	// underlies every node's merged endpoint configuration and would run it on
	// each node without its own. Only star.remote and each node's
	// remotePrologue apply to star commands.
	configuration := star.Sync.ToInternal()
	if !configuration.RemoteCommandMode.IsDefault() || configuration.RemotePrologue != "" {
		return nil, errors.New("use star.remote and each node's remotePrologue for star commands")
	} else if err := configuration.EnsureValid(false); err != nil {
		return nil, err
	}
	configuration = synchronization.MergeConfigurations(defaults, configuration)
	configuration.RemoteCommandMode = synchronization.RemoteCommandMode_RemoteCommandModeDisabled
	configuration.RemotePrologue = ""
	if star.Remote {
		configuration.RemoteCommandMode = synchronization.RemoteCommandMode_RemoteCommandModeBoth
	}
	if configuration.SynchronizationMode.IsDefault() {
		configuration.SynchronizationMode = core.SynchronizationMode_SynchronizationModeTwoWaySafe
	}
	if err := configuration.EnsureValid(false); err != nil {
		return nil, err
	} else if _, err := synchronization.NewIgnorer(configuration, synchronization.DefaultVersion); err != nil {
		return nil, err
	}

	// Validate the center and the nodes.
	if star.Center == "" {
		return nil, errors.New("star.center is required and must name one of the nodes")
	} else if _, ok := star.Nodes[star.Center]; !ok {
		return nil, fmt.Errorf("star.center %q doesn't name a node", star.Center)
	} else if len(star.Nodes) < 2 {
		return nil, errors.New("a star requires at least one node besides its center")
	}
	var names []string
	endpoints := make(map[string]*url.URL, len(star.Nodes))
	for name, node := range star.Nodes {
		if name == "defaults" {
			return nil, errors.New("the node name defaults is reserved")
		} else if err := synchronization.EnsureStarNodeNameValid(name); err != nil {
			return nil, err
		} else if node.Endpoint == "" {
			return nil, fmt.Errorf("node %s has no endpoint", name)
		} else if name == star.Center && !node.Mode.IsDefault() {
			return nil, fmt.Errorf("mode can't be set on the center node %s: each other node's mode configures its own edge", name)
		} else if !node.Mode.IsDefault() && !node.Mode.Supported() {
			return nil, fmt.Errorf("unsupported synchronization mode for node %s", name)
		}
		// The center is the alpha of every edge, and every other node is the
		// beta of its own edge.
		endpoint, err := url.Parse(node.Endpoint, url.Kind_Synchronization, name == star.Center)
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
		if name != star.Center {
			names = append(names, name)
		}
	}

	// Expand the star into one edge per non-center node. A node's mode
	// overrides the shared mode for its own edge.
	sort.Strings(names)
	center := star.Nodes[star.Center]
	var result []*svc.CreationSpecification
	for _, name := range names {
		node := star.Nodes[name]
		edgeConfiguration := proto.Clone(configuration).(*synchronization.Configuration)
		if !node.Mode.IsDefault() {
			edgeConfiguration.SynchronizationMode = node.Mode
		}
		result = append(result, &svc.CreationSpecification{
			Name:  starSessionPrefix + name,
			Alpha: proto.Clone(endpoints[star.Center]).(*url.URL), Beta: endpoints[name],
			Configuration:      edgeConfiguration,
			ConfigurationAlpha: &synchronization.Configuration{RemotePrologue: center.RemotePrologue},
			ConfigurationBeta:  &synchronization.Configuration{RemotePrologue: node.RemotePrologue},
			Labels: map[string]string{
				LabelKey: project,
			},
			Star: &synchronization.StarMembership{
				Identifier: project,
				Center:     star.Center,
				Node:       name,
			},
			Paused: paused,
		})
	}

	// Validate the workspaces with the same rules that apply to the sessions
	// resulting from applying the project.
	edges := make([]*synchronization.Session, len(result))
	for i, specification := range result {
		edges[i] = specificationSession(specification)
	}
	if err := synchronization.ValidateStar(edges, nil); err != nil {
		return nil, err
	}
	return result, nil
}

// specificationSession describes a planned session without assigning its
// eventual identifier or creation time, which validation doesn't need.
func specificationSession(specification *svc.CreationSpecification) *synchronization.Session {
	return &synchronization.Session{
		Name:               specification.Name,
		Alpha:              specification.Alpha,
		Beta:               specification.Beta,
		Configuration:      specification.Configuration,
		ConfigurationAlpha: specification.ConfigurationAlpha,
		ConfigurationBeta:  specification.ConfigurationBeta,
		Labels:             specification.Labels,
		Star:               specification.Star,
	}
}

// preserveStarCenters makes new and replaced star edges use the exact center
// endpoint of the star's retained edges. Declarative matching intentionally
// ignores captured Docker environment and accepts alternate local paths to the
// same directory, but the center is shared by every edge, whose endpoints must
// agree on it exactly (see synchronization.ValidateStar). A new edge must not
// reinterpret either as a new identity for the center that retained edges
// still use. Other nodes have only one edge each, so nothing anchors them.
// Specifications are cloned when rewritten, so planning doesn't modify its
// caller's configuration. The resulting stars are validated, including their
// overlap with the project's other sessions, before a plan can perform any
// mutation.
func preserveStarCenters(current []*synchronization.Session, changes []*sessionChange[*synchronization.Session, *svc.CreationSpecification]) error {
	removed := make(map[*synchronization.Session]bool, len(changes))
	for _, change := range changes {
		if change.current != nil {
			removed[change.current] = true
		}
	}
	retained := make(map[string]*synchronization.Session)
	var final []*synchronization.Session
	for _, session := range current {
		if removed[session] {
			continue
		}
		final = append(final, session)
		if session.Star == nil {
			continue
		}
		if previous := retained[session.Star.Identifier]; previous != nil && !proto.Equal(previous.Alpha, session.Alpha) {
			return fmt.Errorf("retained sessions %s and %s disagree on the endpoint of center node %s; inspect them with mutagen sync list -l, terminate one of them, and apply again", previous.Identifier, session.Identifier, session.Star.Center)
		}
		retained[session.Star.Identifier] = session
	}
	for _, change := range changes {
		specification := change.specification
		if specification == nil {
			continue
		} else if specification.Star != nil {
			if anchor := retained[specification.Star.Identifier]; anchor != nil && !proto.Equal(anchor.Alpha, specification.Alpha) {
				specification = proto.Clone(specification).(*svc.CreationSpecification)
				specification.Alpha = proto.Clone(anchor.Alpha).(*url.URL)
				change.specification = specification
			}
		}
		final = append(final, specificationSession(specification))
	}

	// Validate the actual post-apply sessions rather than only the freshly
	// parsed declarations. Retained transport environments can differ from
	// the invoking shell, which may also affect overlap with other sessions.
	groups := make(map[string][]*synchronization.Session)
	for _, session := range final {
		if session.Star != nil {
			groups[session.Star.Identifier] = append(groups[session.Star.Identifier], session)
		}
	}
	identifiers := make([]string, 0, len(groups))
	for identifier := range groups {
		identifiers = append(identifiers, identifier)
	}
	sort.Strings(identifiers)
	for _, identifier := range identifiers {
		var others []*synchronization.Session
		for _, session := range final {
			if session.Star.GetIdentifier() != identifier {
				others = append(others, session)
			}
		}
		if err := synchronization.ValidateStar(groups[identifier], others); err != nil {
			return fmt.Errorf("invalid resulting star: %w", err)
		}
	}
	return nil
}
