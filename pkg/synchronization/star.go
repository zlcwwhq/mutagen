package synchronization

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// EnsureStarNodeNameValid applies the naming rules shared by project
// configuration and persisted membership. Node names follow the rules for
// session names, and they're also valid label values, so that they can be used
// to select sessions.
func EnsureStarNodeNameValid(name string) error {
	if name == "" || selection.EnsureNameValid(name) != nil || selection.EnsureLabelValueValid(name) != nil {
		return fmt.Errorf("invalid star node name %q", name)
	}
	return nil
}

// EnsureValid ensures that star membership is valid.
func (m *StarMembership) EnsureValid() error {
	if m == nil {
		return errors.New("nil star membership")
	} else if m.Identifier == "" || selection.EnsureLabelValueValid(m.Identifier) != nil {
		return errors.New("invalid star identifier")
	} else if err := EnsureStarNodeNameValid(m.Center); err != nil {
		return err
	} else if err := EnsureStarNodeNameValid(m.Node); err != nil {
		return err
	} else if m.Node == m.Center {
		return errors.New("a star node can't be its own center")
	}
	return nil
}

// node names the node of the edge at the specified endpoint index, which is 0
// for alpha (the center) and 1 for beta (the other node).
func (m *StarMembership) node(index int) string {
	if index == 0 {
		return m.GetCenter()
	}
	return m.GetNode()
}

// validateStarMembership checks the membership of one edge, including for
// sessions created through the API rather than by project orchestration.
// Sessions without membership aren't constrained. A star belongs to the
// project whose identifier it bears, so that project commands manage all of
// its edges. Edges otherwise have the settings of ordinary pairs.
func validateStarMembership(s *Session) error {
	if s.Star == nil {
		return nil
	} else if err := s.Star.EnsureValid(); err != nil {
		return err
	} else if s.Labels[selection.ProjectLabelKey] != s.Star.Identifier {
		return errors.New("star must belong to its project")
	}
	return nil
}

// starTopology indexes the persisted edges of one star by node, without
// inspecting the filesystem. It returns the center's endpoint, as recorded by
// the edge with the lowest identifier, and an edge for each node, along with
// any problem: edges that disagree on the star's identity, on its center, or
// on the center's endpoint, or several edges for one node. Each node's edge is
// still the one with the lowest identifier, so that a star with a problem can
// be described. Every edge must have star membership.
func starTopology(sessions []*Session) (*url.URL, map[string]*Session, error) {
	sessions = append([]*Session(nil), sessions...)
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Identifier < sessions[j].Identifier })
	if len(sessions) == 0 {
		return nil, nil, errors.New("empty star")
	}
	base := sessions[0]
	edges := make(map[string]*Session, len(sessions))
	var problem error
	for _, s := range sessions {
		if problem == nil && (s.Star.Identifier != base.Star.Identifier || s.Star.Center != base.Star.Center) {
			problem = fmt.Errorf("sessions %s and %s disagree on the star's center node", base.Identifier, s.Identifier)
		} else if problem == nil && !proto.Equal(s.Alpha, base.Alpha) {
			problem = fmt.Errorf("sessions %s and %s disagree on the endpoint of center node %s", base.Identifier, s.Identifier, base.Star.Center)
		}
		if previous := edges[s.Star.Node]; previous != nil {
			if problem == nil {
				problem = fmt.Errorf("node %s has several edges (sessions %s and %s)", s.Star.Node, previous.Identifier, s.Identifier)
			}
			continue
		}
		edges[s.Star.Node] = s
	}
	return base.Alpha, edges, problem
}

// ValidateStar checks a complete collection of star edges, along with the
// other sessions that must not overlap the star's workspaces: the edges must
// form one star (see starTopology), and distinct nodes, including the center,
// must not use overlapping directories. Project orchestration uses it for both
// the declared star and the sessions that result from applying a project.
func ValidateStar(edges, others []*Session) error {
	center, nodes, err := starTopology(edges)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	workspaces := []struct {
		name     string
		endpoint *url.URL
	}{{edges[0].Star.Center, center}}
	for _, name := range names {
		workspaces = append(workspaces, struct {
			name     string
			endpoint *url.URL
		}{name, nodes[name].Beta})
	}
	for i, workspace := range workspaces {
		for _, other := range workspaces[i+1:] {
			if WorkspaceEndpointsOverlap(workspace.endpoint, other.endpoint) {
				return fmt.Errorf("nodes %s and %s overlap", workspace.name, other.name)
			}
		}
		for _, session := range others {
			if WorkspaceEndpointsOverlap(workspace.endpoint, session.Alpha) || WorkspaceEndpointsOverlap(workspace.endpoint, session.Beta) {
				return fmt.Errorf("session %s overlaps node %s of the star", session.Name, workspace.name)
			}
		}
	}
	return nil
}

// resolveWorkspaceLocalPath resolves the existing portion of a local root. New
// workspaces may not exist yet, but symlinks in their parents still determine
// whether they overlap another workspace. Do not create directories while
// checking.
func resolveWorkspaceLocalPath(value string) string {
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

// workspacePath separates a filesystem or home directory anchor from its path.
// parents counts unresolved ascents above a remote home directory, whose
// absolute location is unknown to the daemon. Keeping these separate prevents
// cleaning ~/../work into work, or treating . as unrelated to its descendants.
type workspacePath struct {
	anchor  string
	parents int
	path    string
}

// normalizeWorkspacePath recognizes remote home paths and explicit Windows
// volumes independently of the daemon's platform. Remote agents start in their
// user's home directory, so unqualified relative paths share the ~ anchor.
// Windows volumes are kept outside path cleaning so .. cannot escape a drive or
// share.
func normalizeWorkspacePath(value string, windows bool) workspacePath {
	if windows {
		value = strings.ReplaceAll(value, `\`, "/")
	}
	result := workspacePath{anchor: "~"}
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
func (p workspacePath) contains(other workspacePath) bool {
	if p.anchor != other.anchor || p.parents < other.parents {
		return false
	} else if p.parents > other.parents {
		return p.path == ""
	}
	return p.path == "" || p.path == other.path || strings.HasPrefix(other.path, p.path+"/")
}

// WorkspaceEndpointsOverlap checks for overlapping paths on the same
// configured system. Different remote aliases cannot be proven to identify the
// same system; callers must not use aliases to give one workspace two
// identities.
func WorkspaceEndpointsOverlap(a, b *url.URL) bool {
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
		x, y = filepath.ToSlash(resolveWorkspaceLocalPath(x)), filepath.ToSlash(resolveWorkspaceLocalPath(y))
		windows = runtime.GOOS == "windows"
	}
	firstPath, secondPath := normalizeWorkspacePath(x, windows), normalizeWorkspacePath(y, windows)
	return firstPath.contains(secondPath) || secondPath.contains(firstPath)
}
