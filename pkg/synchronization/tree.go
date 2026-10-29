package synchronization

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// EnsureValid ensures that tree membership is valid. Node names follow the
// rules for session names, and they're also valid label values, so that they
// can be used to select sessions.
func (m *TreeMembership) EnsureValid() error {
	if m == nil {
		return errors.New("nil tree membership")
	} else if m.Identifier == "" || selection.EnsureLabelValueValid(m.Identifier) != nil {
		return errors.New("invalid workspace tree identifier")
	}
	for _, name := range []string{m.Root, m.Alpha, m.Beta} {
		if err := ensureTreeNodeNameValid(name); err != nil {
			return err
		}
	}
	if m.Alpha == m.Beta || m.Beta == m.Root {
		return errors.New("invalid workspace tree parent")
	}
	return nil
}

// ensureTreeNodeNameValid applies the shared project and persisted membership
// naming rules. Both names and label values must accept a tree node name.
func ensureTreeNodeNameValid(name string) error {
	if name == "" || selection.EnsureNameValid(name) != nil || selection.EnsureLabelValueValid(name) != nil {
		return fmt.Errorf("invalid workspace tree node name %q", name)
	}
	return nil
}

// WorkspaceTreeNode is the topology and location of a workspace. The root has
// no parent; all other nodes name their adjacent parent.
type WorkspaceTreeNode struct {
	Parent   string
	Endpoint *url.URL
}

// ValidateWorkspaceTree validates node names, the structural root, parent
// chains, and overlapping workspaces. Project planning and persisted session
// reconstruction share these rules. If expectedRoot is empty, the sole root
// is derived from the nodes; otherwise every node must reach that root.
func ValidateWorkspaceTree(nodes map[string]WorkspaceTreeNode, expectedRoot string) (string, error) {
	if len(nodes) < 2 {
		return "", errors.New("a workspace tree requires at least two nodes")
	}
	endpoints := make(map[string]*url.URL, len(nodes))
	for name, node := range nodes {
		endpoints[name] = node.Endpoint
	}
	names := sortedTreeNodes(endpoints)
	root := expectedRoot
	for _, name := range names {
		node := nodes[name]
		if err := ensureTreeNodeNameValid(name); err != nil {
			return "", err
		} else if node.Endpoint == nil {
			return "", fmt.Errorf("node %s has no endpoint", name)
		} else if node.Parent != "" {
			if _, ok := nodes[node.Parent]; !ok {
				return "", fmt.Errorf("node %s has unknown parent %s", name, node.Parent)
			}
		} else if expectedRoot == "" {
			if root != "" {
				return "", errors.New("workspace tree has multiple roots")
			}
			root = name
		}
	}
	if root == "" {
		return "", errors.New("workspace tree has no root")
	} else if err := ensureTreeNodeNameValid(root); err != nil {
		return "", err
	} else if node, ok := nodes[root]; ok && node.Parent != "" {
		return "", errors.New("workspace tree root has a parent")
	}
	for _, name := range names {
		seen := make(map[string]bool)
		for current := name; current != root; {
			if seen[current] {
				return "", fmt.Errorf("cycle in workspace tree at node %s", current)
			}
			seen[current] = true
			parent := nodes[current].Parent
			if parent == "" {
				return "", fmt.Errorf("node %s is disconnected from tree root %s", name, root)
			}
			current = parent
		}
	}
	for i, name := range names {
		for _, other := range names[i+1:] {
			if TreeEndpointsOverlap(nodes[name].Endpoint, nodes[other].Endpoint) {
				return "", fmt.Errorf("nodes %s and %s overlap", name, other)
			}
		}
	}
	return root, nil
}

// node names the node of the edge at the specified endpoint index, which is 0
// for alpha (the parent) and 1 for beta (the child).
func (m *TreeMembership) node(index int) string {
	if index == 0 {
		return m.GetAlpha()
	}
	return m.GetBeta()
}

// resolveTreeLocalPath resolves the existing portion of a local root. New
// workspaces may not exist yet, but symlinks in their parents still determine
// whether they overlap another node. Do not create directories while checking.
func resolveTreeLocalPath(value string) string {
	prefix, suffix := value, ""
	for {
		if resolved, err := filepath.EvalSymlinks(prefix); err == nil {
			return filepath.Join(resolved, suffix)
		} else if !os.IsNotExist(err) {
			return value
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return value
		}
		suffix = filepath.Join(filepath.Base(prefix), suffix)
		prefix = parent
	}
}

// treePath separates a filesystem or home directory anchor from its path.
// parents counts unresolved ascents above a remote home directory, whose
// absolute location is unknown to the daemon. Keeping these separate prevents
// cleaning ~/../work into work, or treating . as unrelated to its descendants.
type treePath struct {
	anchor  string
	parents int
	path    string
}

// normalizeTreePath recognizes remote home paths and explicit Windows volumes
// independently of the daemon's platform. Remote agents start in their user's
// home directory, so unqualified relative paths share the ~ anchor. Windows
// volumes are kept outside path cleaning so .. cannot escape a drive or share.
func normalizeTreePath(value string, windows bool) treePath {
	if windows {
		value = strings.ReplaceAll(value, `\`, "/")
	}
	result := treePath{anchor: "~"}
	if volumeLength := url.WindowsVolumeLength(value); windows && volumeLength != 0 {
		result.anchor, value = value[:volumeLength], path.Clean("/"+value[volumeLength:])
	} else if strings.HasPrefix(value, "/") {
		result.anchor, value = "/", path.Clean(value)
	} else if strings.HasPrefix(value, "~") {
		result.anchor, value, _ = strings.Cut(value, "/")
		value = strings.TrimLeft(value, "/")
	}
	value = path.Clean(value)
	for value == ".." || strings.HasPrefix(value, "../") {
		result.parents++
		value = strings.TrimPrefix(strings.TrimPrefix(value, ".."), "/")
	}
	if value != "." {
		result.path = strings.TrimPrefix(value, "/")
	}
	if windows {
		// Conservatively account for case-insensitive Windows workspaces.
		result.anchor = strings.ToLower(result.anchor)
		result.path = strings.ToLower(result.path)
	}
	return result
}

// contains checks containment using only relationships known without resolving
// a remote user's home directory. Different home anchors cannot be compared.
func (p treePath) contains(other treePath) bool {
	if p.anchor != other.anchor || p.parents < other.parents {
		return false
	} else if p.parents > other.parents {
		return p.path == ""
	}
	return p.path == "" || p.path == other.path || strings.HasPrefix(other.path, p.path+"/")
}

// TreeEndpointsOverlap checks for overlapping paths on the same configured
// system. Different remote aliases cannot be proven to identify the same
// system; callers must not use aliases to give one workspace two identities.
func TreeEndpointsOverlap(a, b *url.URL) bool {
	first, second := proto.Clone(a).(*url.URL), proto.Clone(b).(*url.URL)
	first.Path, second.Path = "", ""
	if !proto.Equal(first, second) {
		return false
	}
	x, y := a.Path, b.Path
	// A double forward slash can also start a POSIX path. Only a drive or
	// backslash UNC prefix establishes Windows syntax for an unknown remote;
	// once established, both separator forms share the same normalization.
	windows := url.WindowsVolumeLength(x) != 0 && !strings.HasPrefix(x, "//") ||
		url.WindowsVolumeLength(y) != 0 && !strings.HasPrefix(y, "//")
	if a.Protocol == url.Protocol_Local {
		x, y = filepath.ToSlash(resolveTreeLocalPath(x)), filepath.ToSlash(resolveTreeLocalPath(y))
		windows = runtime.GOOS == "windows"
	}
	firstPath, secondPath := normalizeTreePath(x, windows), normalizeTreePath(y, windows)
	return firstPath.contains(secondPath) || secondPath.contains(firstPath)
}

// validateTreeMembership checks the membership of one edge along with the
// settings that it requires, including for sessions created through the API
// rather than by project orchestration. Sessions without membership aren't
// constrained. A tree belongs to the project whose identifier it bears, so
// that project commands manage all of its edges.
func validateTreeMembership(s *Session) error {
	if s.Tree == nil {
		return nil
	} else if err := s.Tree.EnsureValid(); err != nil {
		return err
	} else if s.Labels[selection.ProjectLabelKey] != s.Tree.Identifier {
		return errors.New("workspace tree must belong to its project")
	} else if s.Configuration == nil {
		return errors.New("missing workspace tree configuration")
	} else if s.Configuration.SynchronizationMode != core.SynchronizationMode_SynchronizationModeTwoWaySafe {
		return errors.New("workspace trees require explicit two-way-safe synchronization")
	} else if m := s.Configuration.RemoteCommandMode; m != RemoteCommandMode_RemoteCommandModeBoth && m != RemoteCommandMode_RemoteCommandModeDisabled {
		return errors.New("workspace tree commands must be explicitly enabled or disabled for both endpoints")
	} else if s.Configuration.RemotePrologue != "" {
		// Prologues are node execution settings, stored only in the
		// endpoint-specific configurations. A shared prologue would underlie
		// both endpoints' merged configurations and run on every node of the
		// edge that doesn't specify its own.
		return errors.New("workspace tree prologues must be specified per node")
	}
	return nil
}

// ValidateWorkspaceTreeSessions checks a complete collection of tree edges,
// including shared configuration, node execution settings, topology, and
// workspace locations. Project planning uses it after combining retained
// sessions with newly created edges, before making any session changes.
func ValidateWorkspaceTreeSessions(sessions []*Session) error {
	_, _, err := workspaceTree(sessions)
	return err
}

// workspaceTree reconstructs and validates a tree from its persisted edges.
// It includes paused/disconnected sessions so an unavailable branch doesn't
// make another path disappear. Endpoints and node prologues must agree across
// incident edges, and all edges must share their synchronization settings.
func workspaceTree(sessions []*Session) (map[string]*Session, map[string]*url.URL, error) {
	parents := make(map[string]*Session)
	nodes := make(map[string]*url.URL)
	prologues := make(map[string]string)
	if len(sessions) == 0 {
		return nil, nil, errors.New("empty workspace tree")
	}
	base := sessions[0]
	if base == nil {
		return nil, nil, errors.New("nil workspace tree edge")
	}
	root := base.Tree.GetRoot()
	for _, s := range sessions {
		if s == nil || s.Tree == nil {
			return nil, nil, errors.New("workspace tree edge without membership")
		} else if err := validateTreeMembership(s); err != nil {
			return nil, nil, err
		}
		if s.Tree.Identifier != base.Tree.Identifier || s.Tree.Root != root || !s.Configuration.Equal(base.Configuration) {
			return nil, nil, errors.New("inconsistent workspace tree configuration")
		}
		parent, child := s.Tree.Alpha, s.Tree.Beta
		if parents[child] != nil {
			return nil, nil, fmt.Errorf("node %s has multiple parent edges", child)
		}
		parents[child] = s
		for index, name := range []string{parent, child} {
			endpoint := []*url.URL{s.Alpha, s.Beta}[index]
			configuration := []*Configuration{s.ConfigurationAlpha, s.ConfigurationBeta}[index]
			// Node overrides only contain execution settings; synchronization
			// rules must remain identical regardless of the incident edge.
			if configuration == nil || !configuration.Equal(&Configuration{RemotePrologue: configuration.RemotePrologue}) {
				return nil, nil, errors.New("workspace tree nodes only support remotePrologue overrides")
			}
			if previous := nodes[name]; previous != nil && (!proto.Equal(previous, endpoint) || prologues[name] != configuration.RemotePrologue) {
				return nil, nil, fmt.Errorf("inconsistent endpoint for node %s", name)
			}
			nodes[name], prologues[name] = endpoint, configuration.RemotePrologue
		}
	}
	// Validate the same topology and workspace relationships as project
	// planning, including disconnected persisted components.
	topology := make(map[string]WorkspaceTreeNode, len(nodes))
	for name, endpoint := range nodes {
		node := WorkspaceTreeNode{Endpoint: endpoint}
		if edge := parents[name]; edge != nil {
			node.Parent = edge.Tree.Alpha
		}
		topology[name] = node
	}
	if _, err := ValidateWorkspaceTree(topology, root); err != nil {
		return nil, nil, err
	}
	return parents, nodes, nil
}
