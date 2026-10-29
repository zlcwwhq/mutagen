package synchronization

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/url"
)

func TestTreeRemoteEndpointsOverlap(t *testing.T) {
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
				if actual := TreeEndpointsOverlap(a, b); actual != test.overlap {
					t.Errorf("overlap of %q and %q: %t, expected %t", a.Path, b.Path, actual, test.overlap)
				}
				if actual := TreeEndpointsOverlap(b, a); actual != test.overlap {
					t.Errorf("reversed overlap: %t, expected %t", actual, test.overlap)
				}
				b.Host = "other"
				if TreeEndpointsOverlap(a, b) {
					t.Fatal("overlap reported across different hosts")
				}
			}
		})
	}
}

func TestTreeLocalEndpointsOverlap(t *testing.T) {
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
			if actual := TreeEndpointsOverlap(a, b); actual != test.overlap {
				t.Errorf("overlap: %t, expected %t", actual, test.overlap)
			}
			if actual := TreeEndpointsOverlap(b, a); actual != test.overlap {
				t.Errorf("reversed overlap: %t, expected %t", actual, test.overlap)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Fatal("overlap checks created a directory:", err)
	}
}

func treeTestEdge(parent, child string) *Session {
	return &Session{
		Identifier: child, Name: "tree-" + child,
		Alpha:              &url.URL{Protocol: url.Protocol_SSH, Host: parent, Path: "~/project"},
		Beta:               &url.URL{Protocol: url.Protocol_SSH, Host: child, Path: "~/project"},
		Configuration:      &Configuration{SynchronizationMode: core.SynchronizationMode_SynchronizationModeTwoWaySafe, RemoteCommandMode: RemoteCommandMode_RemoteCommandModeBoth},
		ConfigurationAlpha: &Configuration{}, ConfigurationBeta: &Configuration{},
		Labels: map[string]string{"io.mutagen.project": "project"},
		Tree:   &TreeMembership{Identifier: "project", Root: "a", Alpha: parent, Beta: child},
	}
}

// TestWorkspaceTreeRuntimeValidation verifies that malformed persisted/API
// membership cannot bypass project validation or broaden command authority.
func TestWorkspaceTreeRuntimeValidation(t *testing.T) {
	a, b := treeTestEdge("a", "b"), treeTestEdge("b", "c")
	if _, nodes, err := workspaceTree([]*Session{a, b}); err != nil || len(nodes) != 3 {
		t.Fatal("valid tree rejected", err)
	}
	for _, test := range []struct {
		name   string
		modify func(*Session)
	}{
		{"project", func(s *Session) { s.Labels["io.mutagen.project"] = "other" }},
		{"tree", func(s *Session) { s.Tree.Identifier = "other" }},
		{"root", func(s *Session) { s.Tree.Root = "other" }},
		{"duplicate-child", func(s *Session) { s.Tree.Beta = "b"; s.Tree.Alpha = "a" }},
		{"disconnected", func(s *Session) { s.Tree.Alpha = "missing" }},
		{"missing", func(s *Session) { s.Tree = nil }},
		{"unsafe", func(s *Session) {
			s.Configuration.SynchronizationMode = core.SynchronizationMode_SynchronizationModeTwoWayResolved
		}},
		{"authorization", func(s *Session) { s.Configuration.RemoteCommandMode = RemoteCommandMode_RemoteCommandModeBeta }},
		{"ignores", func(s *Session) { s.Configuration.Ignores = []string{"different"} }},
		{"endpoint", func(s *Session) { s.Alpha.Host = "another-host" }},
		{"prologue", func(s *Session) { s.ConfigurationAlpha.RemotePrologue = "different" }},
		{"override", func(s *Session) { s.ConfigurationBeta.WatchPollingInterval = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := proto.Clone(b).(*Session)
			test.modify(changed)
			if _, _, err := workspaceTree([]*Session{a, changed}); err == nil {
				t.Fatal("invalid tree accepted")
			}
		})
	}
}

// TestWorkspaceTreeInvalidation verifies that membership updates cancel active
// routes, through their changed channels, only when they affect the route's
// path, and that they replace or remove the cached tree that plans routes.
func TestWorkspaceTreeInvalidation(t *testing.T) {
	trees, relays := treeTestRegistry([]*Session{treeTestEdge("a", "b")}, "b")
	first := trees.controllers["b"]
	activate := func() *treeRoute {
		t.Helper()
		route, err := trees.plan(relays["b"], 1, "a")
		if err != nil {
			t.Fatal(err)
		}
		release, err := route.activate()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
		return route
	}
	route := activate()
	// An unrelated pair session doesn't interrupt tree work.
	unrelated := &controller{session: treeTestEdge("x", "y")}
	unrelated.session.Labels, unrelated.session.Tree = nil, nil
	trees.update(unrelated, false)
	if err := route.check(context.Background()); err != nil {
		t.Fatal("unrelated session invalidated the route:", err)
	}
	// A second tree overlapping a node of the path invalidates the route, and
	// the cached tree refuses new routes until the overlap is removed.
	overlap := &controller{session: treeTestEdge("b", "c")}
	overlap.session.Labels["io.mutagen.project"] = "another-project"
	overlap.session.Tree.Identifier = "another-project"
	trees.update(overlap, false)
	if route.check(context.Background()) == nil {
		t.Fatal("overlapping tree didn't invalidate existing route")
	} else if _, err := trees.plan(relays["b"], 1, "a"); err == nil {
		t.Fatal("overlapping tree didn't invalidate the cached tree")
	}
	trees.update(overlap, true)
	route = activate()
	// Removing the last edge invalidates its routes and membership state.
	trees.update(first, true)
	if route.check(context.Background()) == nil || trees.cached["project"] != nil {
		t.Fatal("empty tree retained membership state")
	}
}
