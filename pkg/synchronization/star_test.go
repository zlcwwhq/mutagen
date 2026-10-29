package synchronization

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/url"
)

func TestWorkspaceRemoteEndpointsOverlap(t *testing.T) {
	for _, test := range []struct {
		name, first, second string
		overlap             bool
	}{
		{"relative-root", ".", "sub", true},
		{"relative-home", "work", "~/work/sub", true},
		{"home-root", "~", "sub", true},
		{"home-parent", "~/..", "work", true},
		{"home-ancestor", "../..", "../work", true},
		{"home-parent-child", "~/../work", "../work/sub", true},
		{"home-parent-distinct", "~/../work", "work", false},
		{"home-separators", "~//../work", "../work/sub", true},
		{"home-separators-distinct", "~//../work", "work", false},
		{"named-home", "~someone/work", "~someone/work/sub", true},
		{"different-homes", "~/work", "~someone/work", false},
		{"absolute", "/work", "/work/sub", true},
		{"absolute-root", "/", "/work", true},
		{"different-anchors", "/work", "work", false},
		{"siblings", "work", "other", false},
		{"prefix", "work", "workspace", false},
		{"clean", "./work/../other", "other/sub", true},
		{"posix-backslash", "work", `work\sub`, false},
		{"posix-double-slash", "//server/share/../work", "/server/work/sub", true},
		{"posix-double-slash-root", "//tmp/work", "/tmp/work/sub", true},
		{"windows-drive", `C:\work`, `C:\work\sub`, true},
		{"windows-separators", `C:\work`, "C:/work/sub", true},
		{"windows-case", `C:\Work`, `c:\work\sub`, true},
		{"windows-root", `C:\`, `C:\work`, true},
		{"windows-clean", `C:\work\..\other`, `C:\other\sub`, true},
		{"windows-clamp", `C:\..\work`, `C:\work\sub`, true},
		{"windows-siblings", `C:\work`, `C:\other`, false},
		{"windows-prefix", `C:\work`, `C:\workspace`, false},
		{"windows-volumes", `C:\work`, `D:\work`, false},
		{"unc", `\\server\share\work`, `\\server\share\work\sub`, true},
		{"unc-separators", `\\server\share\work`, "//server/share/work/sub", true},
		{"unc-clamp", `\\server\share\..\work`, `\\server\share\work\sub`, true},
		{"unc-shares", `\\server\share\work`, `\\server\other\work`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, protocol := range []url.Protocol{url.Protocol_SSH, url.Protocol_Docker} {
				a := &url.URL{Protocol: protocol, Host: "host", Path: test.first}
				b := &url.URL{Protocol: protocol, Host: "host", Path: test.second}
				if actual := WorkspaceEndpointsOverlap(a, b); actual != test.overlap {
					t.Errorf("overlap of %q and %q: %t, expected %t", a.Path, b.Path, actual, test.overlap)
				}
				if actual := WorkspaceEndpointsOverlap(b, a); actual != test.overlap {
					t.Errorf("reversed overlap: %t, expected %t", actual, test.overlap)
				}
				b.Host = "other"
				if WorkspaceEndpointsOverlap(a, b) {
					t.Fatal("overlap reported across different hosts")
				}
			}
		})
	}
}

func TestWorkspaceLocalEndpointsOverlap(t *testing.T) {
	base := t.TempDir()
	root, alias := filepath.Join(base, "root"), filepath.Join(base, "alias")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	} else if err := os.Symlink(root, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("unable to create directory symlink:", err)
		}
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, first, second string
		overlap             bool
	}{
		{"existing-alias", root, alias, true},
		{"missing-child", root, filepath.Join(alias, "new"), true},
		{"missing-descendants", root, filepath.Join(alias, "new", "nested"), true},
		{"missing-both", filepath.Join(root, "new"), filepath.Join(alias, "new", "nested"), true},
		{"missing-siblings", filepath.Join(root, "first"), filepath.Join(alias, "second"), false},
		{"missing-prefix", filepath.Join(root, "new"), filepath.Join(alias, "newer"), false},
		{"unrelated", root, filepath.Join(base, "other"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, b := &url.URL{Path: test.first}, &url.URL{Path: test.second}
			if actual := WorkspaceEndpointsOverlap(a, b); actual != test.overlap {
				t.Errorf("overlap: %t, expected %t", actual, test.overlap)
			}
			if actual := WorkspaceEndpointsOverlap(b, a); actual != test.overlap {
				t.Errorf("reversed overlap: %t, expected %t", actual, test.overlap)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Fatal("overlap checks created a directory:", err)
	}
}

// starTestEdge creates the edge joining a star's center, on host center, to a
// node on its own host. The edge's identifier is the node's name.
func starTestEdge(node string) *Session {
	return &Session{
		Identifier: node, Name: "star-" + node, Version: DefaultVersion,
		Alpha:              &url.URL{Protocol: url.Protocol_SSH, Host: "center", Path: "~/project"},
		Beta:               &url.URL{Protocol: url.Protocol_SSH, Host: node, Path: "~/project"},
		Configuration:      &Configuration{RemoteCommandMode: RemoteCommandMode_RemoteCommandModeBoth},
		ConfigurationAlpha: &Configuration{}, ConfigurationBeta: &Configuration{},
		Labels: map[string]string{"io.mutagen.project": "project"},
		Star:   &StarMembership{Identifier: "project", Center: "center", Node: node},
	}
}

// TestStarMembershipValidation verifies that persisted and API membership is
// checked, including that a star belongs to the project that it names, while
// edges otherwise accept any pair settings, such as their own modes.
func TestStarMembershipValidation(t *testing.T) {
	edge := starTestEdge("gpu")
	edge.Configuration.SynchronizationMode = core.SynchronizationMode_SynchronizationModeOneWayReplica
	if err := validateStarMembership(edge); err != nil {
		t.Fatal("valid edge rejected:", err)
	} else if err := validateStarMembership(&Session{}); err != nil {
		t.Fatal("session without membership rejected:", err)
	}
	for _, test := range []struct {
		name   string
		modify func(*Session)
	}{
		{"project", func(s *Session) { s.Labels["io.mutagen.project"] = "other" }},
		{"identifier", func(s *Session) { s.Star.Identifier = "" }},
		{"center", func(s *Session) { s.Star.Center = "" }},
		{"node", func(s *Session) { s.Star.Node = "" }},
		{"self", func(s *Session) { s.Star.Node = s.Star.Center }},
		{"name", func(s *Session) { s.Star.Node = "not a name" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := proto.Clone(edge).(*Session)
			test.modify(changed)
			if validateStarMembership(changed) == nil {
				t.Fatal("invalid membership accepted")
			}
		})
	}
}

// TestStarTopology verifies the runtime consistency checks, which don't
// inspect the filesystem, and that a star with a problem is still described.
func TestStarTopology(t *testing.T) {
	gpu, server := starTestEdge("gpu"), starTestEdge("server")
	center, edges, err := starTopology([]*Session{server, gpu})
	if err != nil || len(edges) != 2 || edges["gpu"] != gpu || !proto.Equal(center, gpu.Alpha) {
		t.Fatalf("valid star misreported: %v %v", edges, err)
	}
	for _, test := range []struct {
		name, problem string
		modify        func(*Session)
	}{
		{"center-endpoint", "disagree on the endpoint of center node center", func(s *Session) { s.Alpha.Path = "~/other" }},
		{"center-transport", "disagree on the endpoint of center node center", func(s *Session) {
			s.Alpha.Environment = map[string]string{"DOCKER_HOST": "tcp://other:2375"}
		}},
		{"center-name", "disagree on the star's center", func(s *Session) { s.Star.Center = "other" }},
		{"identifier", "disagree on the star's center", func(s *Session) { s.Star.Identifier = "other" }},
		{"duplicate-node", "node gpu has several edges", func(s *Session) { s.Star.Node = "gpu" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := proto.Clone(server).(*Session)
			test.modify(changed)
			_, edges, err := starTopology([]*Session{gpu, changed})
			if err == nil || !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("problem not reported: %v", err)
			} else if edges["gpu"] != gpu {
				t.Fatal("star with a problem not described")
			}
		})
	}
}

// TestValidateStar verifies the validation shared with project orchestration:
// nodes, including the center, must not overlap, and neither may other
// sessions.
func TestValidateStar(t *testing.T) {
	gpu, server := starTestEdge("gpu"), starTestEdge("server")
	if err := ValidateStar([]*Session{gpu, server}, nil); err != nil {
		t.Fatal("valid star rejected:", err)
	}
	nested := proto.Clone(server).(*Session)
	nested.Beta = &url.URL{Protocol: url.Protocol_SSH, Host: "gpu", Path: "~/project/nested"}
	if err := ValidateStar([]*Session{gpu, nested}, nil); err == nil || !strings.Contains(err.Error(), "nodes gpu and server overlap") {
		t.Fatal("overlapping nodes accepted:", err)
	}
	centered := proto.Clone(server).(*Session)
	centered.Beta = &url.URL{Protocol: url.Protocol_SSH, Host: "center", Path: "~"}
	if err := ValidateStar([]*Session{gpu, centered}, nil); err == nil || !strings.Contains(err.Error(), "nodes center and server overlap") {
		t.Fatal("node overlapping the center accepted:", err)
	}
	other := &Session{
		Name:  "other",
		Alpha: &url.URL{Path: "/unrelated"},
		Beta:  &url.URL{Protocol: url.Protocol_SSH, Host: "server", Path: "~/project/data"},
	}
	if err := ValidateStar([]*Session{gpu, server}, []*Session{other}); err == nil || !strings.Contains(err.Error(), "session other overlaps node server") {
		t.Fatal("overlapping session accepted:", err)
	}
	other.Beta.Host = "elsewhere"
	if err := ValidateStar([]*Session{gpu, server}, []*Session{other}); err != nil {
		t.Fatal("unrelated session rejected:", err)
	}
	changed := proto.Clone(server).(*Session)
	changed.Alpha.Path = "~/elsewhere"
	if err := ValidateStar([]*Session{gpu, changed}, nil); err == nil {
		t.Fatal("inconsistent center accepted")
	}
}

// TestStarMembershipPersistence verifies that membership survives encoding,
// and that sessions persisted with the removed tree membership field load as
// plain pairs.
func TestStarMembershipPersistence(t *testing.T) {
	edge := starTestEdge("gpu")
	edge.CreationTime = nil
	data, err := proto.Marshal(edge)
	if err != nil {
		t.Fatal(err)
	}
	loaded := &Session{}
	if err := proto.Unmarshal(data, loaded); err != nil {
		t.Fatal(err)
	} else if !proto.Equal(loaded.Star, edge.Star) {
		t.Fatalf("membership not persisted: %v", loaded.Star)
	}

	// Append a tree membership as recorded in field 15 by earlier versions.
	pair := proto.Clone(edge).(*Session)
	pair.Star = nil
	data, err = proto.Marshal(pair)
	if err != nil {
		t.Fatal(err)
	}
	var tree []byte
	for number, value := range []string{"project", "root", "root", "child"} {
		tree = protowire.AppendTag(tree, protowire.Number(number+1), protowire.BytesType)
		tree = protowire.AppendString(tree, value)
	}
	data = protowire.AppendTag(data, 15, protowire.BytesType)
	data = protowire.AppendBytes(data, tree)
	loaded = &Session{}
	if err := proto.Unmarshal(data, loaded); err != nil {
		t.Fatal(err)
	} else if loaded.Star != nil {
		t.Fatal("tree membership loaded as star membership")
	} else if err := validateStarMembership(loaded); err != nil {
		t.Fatal("former tree edge rejected:", err)
	}
}

// starTestChannel is a connected command channel that routes nothing.
type starTestChannel struct {
	// done is closed when the channel disconnects.
	done chan struct{}
}

func (c *starTestChannel) Accept() (io.ReadWriteCloser, error) {
	return nil, errors.New("unsupported")
}

func (c *starTestChannel) Open(context.Context) (io.ReadWriteCloser, error) {
	return nil, errors.New("unsupported")
}

func (c *starTestChannel) Done() <-chan struct{} {
	return c.done
}

func (c *starTestChannel) Close() error {
	return nil
}

// starTestRegistry registers the specified sessions, and connects relays with
// both command channels for those that are listed as connected.
func starTestRegistry(sessions []*Session, connected ...string) (*commandStars, map[string]*commandRelay) {
	stars := newCommandStars()
	relays := make(map[string]*commandRelay)
	for _, session := range sessions {
		c := &controller{session: session}
		stars.update(c, false)
		if slices.Contains(connected, session.Identifier) {
			relay := &commandRelay{stars: stars, controller: c, ctx: context.Background()}
			relay.channels = [2]remote.Channel{&starTestChannel{make(chan struct{})}, &starTestChannel{make(chan struct{})}}
			stars.setRelay(c, relay)
			relays[session.Identifier] = relay
		}
	}
	return stars, relays
}

// starTestNode finds a node in a graph.
func starTestNode(t *testing.T, graph *remote.NodeGraph, name string) remote.Node {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.Name == name {
			return node
		}
	}
	t.Fatalf("node %s not listed: %+v", name, graph.Nodes)
	return remote.Node{}
}

// TestStarNodes verifies node discovery: the center lists every node over its
// own edge, including offline ones, while another node lists only itself and
// the center.
func TestStarNodes(t *testing.T) {
	gpu, server, offline := starTestEdge("gpu"), starTestEdge("server"), starTestEdge("offline")
	gpu.Configuration.SynchronizationMode = core.SynchronizationMode_SynchronizationModeOneWaySafe
	stars, relays := starTestRegistry([]*Session{gpu, server, offline}, "gpu", "server")

	// From the center, through any of its edges.
	for _, through := range []string{"gpu", "server"} {
		graph, err := stars.nodes(relays[through], 0)
		if err != nil {
			t.Fatal(err)
		} else if graph.Source != "center" || !graph.Star || graph.Problem != "" || len(graph.Nodes) != 4 {
			t.Fatalf("center's graph misreported: %+v", graph)
		}
		if names := []string{graph.Nodes[0].Name, graph.Nodes[1].Name, graph.Nodes[2].Name, graph.Nodes[3].Name}; !slices.IsSorted(names) {
			t.Fatal("nodes not sorted:", names)
		}
		center := starTestNode(t, graph, "center")
		if !center.Center || !center.Current || !center.Available || center.Host != "center" || center.Mode != "" {
			t.Fatalf("center misreported: %+v", center)
		}
		node := starTestNode(t, graph, "gpu")
		if node.Center || node.Current || !node.Available || node.Host != "gpu" || node.Mode != "one-way-safe" ||
			node.Session != "gpu" || !node.Alpha || strings.Join(node.Path, ",") != "center,gpu" {
			t.Fatalf("node misreported: %+v", node)
		}
		if node := starTestNode(t, graph, "server"); !node.Available || node.Mode != "two-way-safe" {
			t.Fatalf("default mode misreported: %+v", node)
		}
		if node := starTestNode(t, graph, "offline"); node.Available || !strings.Contains(node.Reason, "paused, disconnected") {
			t.Fatalf("offline node misreported: %+v", node)
		}
	}

	// From another node, which reaches only the center.
	graph, err := stars.nodes(relays["gpu"], 1)
	if err != nil {
		t.Fatal(err)
	} else if graph.Source != "gpu" || !graph.Star || len(graph.Nodes) != 2 {
		t.Fatalf("node's graph misreported: %+v", graph)
	}
	center := starTestNode(t, graph, "center")
	if !center.Center || center.Current || !center.Available || center.Mode != "one-way-safe" || center.Session != "gpu" || center.Alpha ||
		strings.Join(center.Path, ",") != "gpu,center" {
		t.Fatalf("center misreported from a node: %+v", center)
	}
	if node := starTestNode(t, graph, "gpu"); !node.Current || node.Center {
		t.Fatalf("current node misreported: %+v", node)
	}

	// A disconnected center channel makes the center unavailable from a node,
	// and the node unavailable from the center, since submissions for it can
	// only enter through its own edge's registration at the center.
	close(relays["server"].channels[0].(*starTestChannel).done)
	if graph, err := stars.nodes(relays["server"], 1); err != nil {
		t.Fatal(err)
	} else if node := starTestNode(t, graph, "center"); node.Available || node.Reason == "" {
		t.Fatalf("disconnected center misreported: %+v", node)
	}
	if graph, err := stars.nodes(relays["gpu"], 0); err != nil {
		t.Fatal(err)
	} else if node := starTestNode(t, graph, "server"); node.Available || node.Reason != centerUnavailableReason {
		t.Fatalf("node whose edge is disconnected at the center misreported: %+v", node)
	} else if node := starTestNode(t, graph, "gpu"); !node.Available {
		t.Fatalf("other node affected by a disconnected edge: %+v", node)
	}

	// An implicit pair has nodes named alpha and beta.
	pair := starTestEdge("pair")
	pair.Star, pair.Labels = nil, nil
	stars, relays = starTestRegistry([]*Session{pair}, "pair")
	graph, err = stars.nodes(relays["pair"], 1)
	if err != nil {
		t.Fatal(err)
	} else if graph.Source != "beta" || graph.Star || len(graph.Nodes) != 2 {
		t.Fatalf("pair misreported: %+v", graph)
	} else if node := starTestNode(t, graph, "alpha"); !node.Available || node.Center || node.Mode != "two-way-safe" || node.Session != "pair" || node.Alpha {
		t.Fatalf("pair peer misreported: %+v", node)
	}
}

// TestStarResolution verifies destination selection: the center reaches every
// node over the node's own edge, and only through it, other nodes reach only
// the center, and node names are only omitted when there is a single other
// node.
func TestStarResolution(t *testing.T) {
	gpu, server := starTestEdge("gpu"), starTestEdge("server")
	_, relays := starTestRegistry([]*Session{gpu, server}, "gpu", "server")
	resolve := func(through string, source int, node string) (*remote.ResolvedRoute, error) {
		return relays[through].resolveSubmission(source, &remote.Submission{Operation: remote.OperationFlush, Node: node, Resolve: true})
	}

	// From the center, a submission through the selected node's own edge
	// crosses it.
	route, err := resolve("server", 0, "server")
	if err != nil {
		t.Fatal(err)
	} else if route.Session != "server" || !route.Alpha || route.SourceNode != "center" || route.Node != "server" || !route.Star {
		t.Fatalf("center selected the wrong edge: %+v", route)
	}

	// A submission through another edge is refused as a connection failure,
	// rather than redirected, even though the selected edge is connected.
	if _, err := resolve("gpu", 0, "server"); err == nil || remote.ClassifyError(err) != remote.ErrorKindConnection ||
		!strings.Contains(err.Error(), "node server is unavailable: its edge isn't connected for remote commands at the center") ||
		!strings.Contains(err.Error(), "retry once it reconnects") {
		t.Fatal("submission through another edge not refused as unavailable:", err)
	}

	// Other errors are reported whichever edge the submission entered through.
	if _, err := resolve("gpu", 0, ""); err == nil || !strings.Contains(err.Error(), "multiple destination nodes") {
		t.Fatal("ambiguous destination accepted at the center:", err)
	}
	if _, err := resolve("gpu", 0, "center"); err == nil || !strings.Contains(err.Error(), "current node") {
		t.Fatal("current node accepted as destination:", err)
	}
	if _, err := resolve("gpu", 0, "missing"); err == nil || !strings.Contains(err.Error(), "unknown node") || remote.ClassifyError(err) != remote.ErrorKindOperation {
		t.Fatal("unknown node accepted:", err)
	}

	// From a node, the center is the only destination, so it can be omitted.
	for _, node := range []string{"", "center"} {
		route, err := resolve("gpu", 1, node)
		if err != nil {
			t.Fatal(err)
		} else if route.Session != "gpu" || route.Alpha || route.SourceNode != "gpu" || route.Node != "center" {
			t.Fatalf("node selected the wrong edge: %+v", route)
		}
	}

	// Stars have no multi-hop routes.
	if _, err := resolve("gpu", 1, "server"); err == nil || remote.ClassifyError(err) != remote.ErrorKindOperation ||
		!strings.Contains(err.Error(), "no multi-hop") || !strings.Contains(err.Error(), "run the command from center") {
		t.Fatal("multi-hop route accepted:", err)
	}

	// A single other node can be omitted at the center, too.
	lone := starTestEdge("lone")
	_, single := starTestRegistry([]*Session{lone}, "lone")
	if route, err := single["lone"].resolveSubmission(0, &remote.Submission{Resolve: true}); err != nil || route.Node != "lone" {
		t.Fatalf("single destination not selected: %+v %v", route, err)
	}
}

// TestStarEdgeRevision verifies that a selection pins only the selected edge,
// so that adding, pausing, replacing, or removing other nodes doesn't fail an
// operation's later submissions, while replacing or removing the selected
// edge does.
func TestStarEdgeRevision(t *testing.T) {
	gpu, server := starTestEdge("gpu"), starTestEdge("server")
	stars, relays := starTestRegistry([]*Session{gpu, server}, "gpu", "server")
	header := &remote.Submission{Operation: remote.OperationFlush, Node: "server", Resolve: true}
	route, err := relays["server"].resolveSubmission(0, header)
	if err != nil || header.Revision != edgeRevision("server") || route.Revision != header.Revision {
		t.Fatalf("selected edge not pinned: %+v %+v %v", header, route, err)
	}
	pinned := func(through string) error {
		_, err := relays[through].resolveSubmission(0, &remote.Submission{Operation: remote.OperationFlush, Node: "server", Resolve: true, Revision: header.Revision})
		return err
	}

	// Changes to other edges don't affect the pinned edge.
	stars.update(&controller{session: starTestEdge("extra")}, false)
	if err := pinned("server"); err != nil {
		t.Fatal("added node failed a pinned submission:", err)
	}
	gpuController := relays["gpu"].controller
	stars.setRelay(gpuController, nil)
	if err := pinned("server"); err != nil {
		t.Fatal("paused node failed a pinned submission:", err)
	}
	replacement := starTestEdge("gpu")
	replacement.Identifier = "gpu-replacement"
	stars.update(gpuController, true)
	stars.update(&controller{session: replacement}, false)
	if err := pinned("server"); err != nil {
		t.Fatal("replaced node failed a pinned submission:", err)
	}
	stars.update(&controller{session: replacement}, true)
	if err := pinned("server"); err != nil {
		t.Fatal("removed node failed a pinned submission:", err)
	}

	// Replacing or removing the selected edge fails pinned submissions, which
	// can then only enter through other edges at the center.
	stars, relays = starTestRegistry([]*Session{gpu, server}, "gpu", "server")
	header.Revision = "stale"
	if _, err := relays["server"].resolveSubmission(0, header); !errors.Is(err, errNodeGraphChanged) {
		t.Fatal("stale revision accepted:", err)
	}
	replacement = starTestEdge("server")
	replacement.Identifier = "server-replacement"
	stars.update(relays["server"].controller, true)
	stars.update(&controller{session: replacement}, false)
	if err := pinned("gpu"); !errors.Is(err, errNodeGraphChanged) {
		t.Fatal("replaced edge accepted under its old revision:", err)
	}
	stars.update(&controller{session: replacement}, true)
	if err := pinned("gpu"); !errors.Is(err, errNodeGraphChanged) {
		t.Fatal("removed edge accepted under its old revision:", err)
	}

	// A pair pins its own session.
	pair := starTestEdge("pair")
	pair.Star, pair.Labels = nil, nil
	_, relays = starTestRegistry([]*Session{pair}, "pair")
	header = &remote.Submission{Operation: remote.OperationFlush, Resolve: true}
	if route, err := relays["pair"].resolveSubmission(1, header); err != nil || route.Revision != edgeRevision("pair") {
		t.Fatalf("pair not pinned: %+v %v", route, err)
	}
}

// TestStarResolutionProblems verifies that unavailable nodes are connection
// failures, including nodes whose edges aren't connected at the center, and
// that a star problem refuses operations from the center while other nodes
// keep using their own edges.
func TestStarResolutionProblems(t *testing.T) {
	gpu, server := starTestEdge("gpu"), starTestEdge("server")
	stars, relays := starTestRegistry([]*Session{gpu, server}, "gpu")
	if _, err := relays["gpu"].resolveSubmission(0, &remote.Submission{Node: "server", Resolve: true}); err == nil ||
		remote.ClassifyError(err) != remote.ErrorKindConnection || !strings.Contains(err.Error(), "node server is unavailable") {
		t.Fatal("unavailable node not reported as a connection failure:", err)
	}

	pinned := &remote.Submission{Node: "gpu", Resolve: true}
	if _, err := relays["gpu"].resolveSubmission(0, pinned); err != nil {
		t.Fatal(err)
	}
	server.Alpha = &url.URL{Protocol: url.Protocol_SSH, Host: "center", Path: "~/elsewhere"}
	graph, err := stars.nodes(relays["gpu"], 0)
	if err != nil {
		t.Fatal(err)
	} else if !strings.Contains(graph.Problem, "disagree on the endpoint of center node center") {
		t.Fatalf("star problem not reported: %+v", graph)
	} else if node := starTestNode(t, graph, "gpu"); node.Available || node.Reason != graph.Problem {
		t.Fatalf("node available despite star problem: %+v", node)
	}
	if _, err := relays["gpu"].resolveSubmission(0, &remote.Submission{Node: "gpu", Resolve: true}); err == nil ||
		remote.ClassifyError(err) != remote.ErrorKindOperation || !strings.Contains(err.Error(), "until the star is repaired") {
		t.Fatal("star problem didn't refuse an operation from the center:", err)
	}

	// An operation that selected its edge before the problem keeps using it,
	// subject to its connection at both ends.
	if route, err := relays["gpu"].resolveSubmission(0, pinned); err != nil || route.Node != "gpu" {
		t.Fatalf("star problem refused a pinned operation: %+v %v", route, err)
	}
	close(relays["gpu"].channels[1].(*starTestChannel).done)
	if _, err := relays["gpu"].resolveSubmission(0, pinned); err == nil || remote.ClassifyError(err) != remote.ErrorKindConnection {
		t.Fatal("disconnected pinned edge accepted:", err)
	}
	if graph, err := stars.nodes(relays["gpu"], 1); err != nil || graph.Problem == "" {
		t.Fatalf("star problem not reported from a node: %+v %v", graph, err)
	}
	relays["gpu"].channels[1] = &starTestChannel{make(chan struct{})}
	if route, err := relays["gpu"].resolveSubmission(1, &remote.Submission{Resolve: true}); err != nil || route.Node != "center" {
		t.Fatalf("star problem refused a node's own edge: %+v %v", route, err)
	}
	close(relays["gpu"].channels[0].(*starTestChannel).done)
	if _, err := relays["gpu"].resolveSubmission(0, pinned); err == nil || remote.ClassifyError(err) != remote.ErrorKindConnection ||
		!strings.Contains(err.Error(), "isn't connected for remote commands at the center") {
		t.Fatal("pinned edge disconnected at the center accepted:", err)
	}

	// Removing a session removes its node.
	stars.update(&controller{session: server}, true)
	if graph, err := stars.nodes(relays["gpu"], 0); err != nil || graph.Problem != "" || len(graph.Nodes) != 2 {
		t.Fatalf("terminated edge still listed: %+v %v", graph, err)
	}
}

// TestStarCrossEdgeSubmissionRefused verifies that a submission at a star's
// center that selects a node through another edge's registration, as remote
// does when the node's own registration there isn't live, is refused before
// its route is resolved, as a connection failure, without flushing or using
// either edge, while a submission through the selected edge's own
// registration crosses it.
func TestStarCrossEdgeSubmissionRefused(t *testing.T) {
	data, err := os.MkdirTemp("", "ms-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	_, relays := starTestRegistry([]*Session{starTestEdge("gpu"), starTestEdge("server")}, "gpu", "server")
	for _, relay := range relays {
		relay.flushRequests = make(chan chan error, 1)
		relay.report = func() *remote.Report { return &remote.Report{} }
	}
	source := relays["gpu"]
	channel, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: "gpu", Star: "project", Node: "center", PeerNode: "gpu", Root: t.TempDir(), Alpha: true, Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	source.channels[0] = channel

	// Accept submissions as the relay's maintenance does.
	source.routes.Add(1)
	go func() {
		defer source.routes.Done()
		source.accept(context.Background(), 0, channel)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	submit := func(node string) (net.Conn, *remote.Target, error) {
		return (&remote.Selection{Target: &remote.Target{SourceSession: "gpu", SourceAlpha: true}, Resolve: true, Node: node}).Submit(ctx, t.TempDir(), &remote.Submission{Operation: remote.OperationFlush})
	}

	// A submission for the server through the gpu edge is refused.
	if refused, _, err := submit("server"); err == nil {
		refused.Close()
		t.Fatal("submission through another edge was accepted")
	} else if remote.ClassifyError(err) != remote.ErrorKindConnection || !strings.Contains(err.Error(), "node server is unavailable: its edge isn't connected for remote commands at the center") {
		t.Fatal("submission through another edge wasn't refused as unavailable:", err)
	}
	for name, relay := range relays {
		select {
		case <-relay.flushRequests:
			t.Fatalf("refused submission flushed the %s edge", name)
		default:
		}
	}

	// A submission for the gpu node crosses its own edge.
	connection, target, err := submit("gpu")
	if err != nil {
		t.Fatal(err)
	} else if target.SourceSession != "gpu" || target.Session != "gpu" || !target.Alpha || target.SourceNode != "center" || target.Node != "gpu" {
		t.Fatalf("target misreported: %+v", target)
	}
	select {
	case request := <-source.flushRequests:
		request <- nil
	case <-ctx.Done():
		t.Fatal("flush did not start")
	}
	if _, err := remote.Flush(ctx, connection); err != nil {
		t.Fatal("flush failed:", err)
	}

	// The relay releases its routes once its channel closes.
	channel.Close()
	drained := make(chan struct{})
	go func() {
		source.routes.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal("relay retained completed routes")
	}
}
