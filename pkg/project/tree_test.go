package project

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/configuration/global"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

func TestWorkspaceTreeSpecifications(t *testing.T) {
	t.Chdir(t.TempDir())
	specifications := testSpecifications(t, `
sync:
  defaults:
    ignore: {paths: [ignored]}
    configurationAlpha: {watch: {pollingInterval: 7}}
tree:
  remote: true
  nodes:
    local: {endpoint: ./local}
    server: {endpoint: ./server, parent: local, remotePrologue: server.sh}
    gpu: {endpoint: ./gpu, parent: server, remotePrologue: gpu.sh}
`)
	if len(specifications.Synchronization) != 2 {
		t.Fatal("incorrect edge count")
	}
	gpu, server := specifications.Synchronization[0], specifications.Synchronization[1]
	if gpu.Name != "tree-gpu" || gpu.Tree.GetAlpha() != "server" || server.Tree.GetAlpha() != "local" {
		t.Fatal("incorrect tree topology")
	}
	for _, edge := range specifications.Synchronization {
		if edge.Configuration.SynchronizationMode != core.SynchronizationMode_SynchronizationModeTwoWaySafe || edge.Configuration.RemoteCommandMode != synchronization.RemoteCommandMode_RemoteCommandModeBoth || strings.Join(edge.Configuration.Ignores, ",") != "ignored" {
			t.Fatal("incorrect shared configuration")
		}
		if edge.ConfigurationAlpha.WatchPollingInterval != 0 || edge.Tree.GetIdentifier() != "project" || edge.Tree.GetRoot() != "local" || len(edge.Labels) != 1 {
			t.Fatal("pair overrides or project identity leaked")
		}
	}
	if gpu.ConfigurationAlpha.RemotePrologue != server.ConfigurationBeta.RemotePrologue || gpu.ConfigurationBeta.RemotePrologue != "gpu.sh" {
		t.Fatal("node execution configuration changed with edge")
	}

	// Reapplying keeps histories, but changed membership must not be mistaken
	// for an unchanged session solely because its paths match.
	_, states := testSessionStates(specifications)
	plan, err := NewPlan(specifications, nil, states)
	if err != nil || plan.unchanged != 2 {
		t.Fatalf("unchanged tree not retained: %v", err)
	}
	states[0].Session = proto.Clone(states[0].Session).(*synchronization.Session)
	states[0].Session.Tree.Beta = "renamed"
	plan, err = NewPlan(specifications, nil, states)
	if err != nil || plan.unchanged != 1 || len(plan.synchronization) != 1 {
		t.Fatalf("membership change not replaced: %v", err)
	}
}

func TestWorkspaceTreeValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, test := range []struct{ name, yaml string }{
		{"single", "tree: {nodes: {a: {endpoint: ./a}}}"},
		{"roots", "tree: {nodes: {a: {endpoint: ./a}, b: {endpoint: ./b}}}"},
		{"no-root", "tree: {nodes: {a: {endpoint: ./a, parent: b}, b: {endpoint: ./b, parent: a}}}"},
		{"cycle", "tree: {nodes: {a: {endpoint: ./a}, b: {endpoint: ./b, parent: c}, c: {endpoint: ./c, parent: b}}}"},
		{"parent", "tree: {nodes: {a: {endpoint: ./a}, b: {endpoint: ./b, parent: missing}}}"},
		{"self", "tree: {nodes: {a: {endpoint: ./a}, b: {endpoint: ./b, parent: b}}}"},
		{"endpoint", "tree: {nodes: {a: {endpoint: ./a}, b: {parent: a}}}"},
		{"overlap", "tree: {nodes: {a: {endpoint: ./a}, b: {endpoint: ./a/b, parent: a}}}"},
		{"unsafe", "tree: {sync: {mode: two-way-resolved}, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b, parent: a}}}"},
		{"direction", "tree: {sync: {remote: alpha}, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b, parent: a}}}"},
		{"pair", "sync: {extra: {alpha: ./a, beta: ./c}}\ntree: {nodes: {a: {endpoint: ./a}, b: {endpoint: ./b, parent: a}}}"},
		{"name", "sync: {tree-b: {alpha: ./c, beta: ./d}}\ntree: {nodes: {a: {endpoint: ./a}, b: {endpoint: ./b, parent: a}}}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewSessionSpecifications(testConfiguration(t, test.yaml), nil, "project", false); err == nil {
				t.Fatal("invalid tree accepted")
			}
		})
	}
	// Enabling pair commands in defaults must not grant tree-wide execution.
	specifications := testSpecifications(t, "sync: {defaults: {remote: true}}\ntree: {nodes: {a: {endpoint: ./a}, b: {endpoint: ./b, parent: a}}}")
	if specifications.Synchronization[0].Configuration.RemoteCommandMode != synchronization.RemoteCommandMode_RemoteCommandModeDisabled {
		t.Fatal("tree commands enabled implicitly")
	}
}

// TestWorkspaceTreeDefaultPrologue verifies that the prologue of the global or
// project synchronization defaults doesn't reach the configuration shared by
// tree edges, which underlies every node's merged endpoint configuration and
// would run it on each node without its own. Node prologues must still attach
// to their own endpoint on every incident edge, and pairs keep the defaults.
func TestWorkspaceTreeDefaultPrologue(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "global.yml")
	if err := os.WriteFile(path, []byte("sync: {defaults: {remotePrologue: global.sh}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	globalConfiguration, err := global.LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, defaults, expected string }{
		{"global", "", "global.sh"},
		{"project", "remotePrologue: defaults.sh", "defaults.sh"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := testConfiguration(t, fmt.Sprintf(`
sync:
  defaults: {%s}
  pair: {alpha: ./c, beta: ./d}
tree:
  remote: true
  nodes:
    local: {endpoint: ./local}
    server: {endpoint: ./server, parent: local, remotePrologue: server.sh}
    gpu: {endpoint: ./gpu, parent: server}
`, test.defaults))
			specifications, err := NewSessionSpecifications(configuration, globalConfiguration, "project", false)
			if err != nil {
				t.Fatal(err)
			}
			// Record the shared, alpha, and beta prologues of each session.
			prologues := make(map[string][3]string)
			for _, s := range specifications.Synchronization {
				prologues[s.Name] = [3]string{
					s.Configuration.RemotePrologue,
					s.ConfigurationAlpha.RemotePrologue,
					s.ConfigurationBeta.RemotePrologue,
				}
			}
			expected := map[string][3]string{
				"pair":        {test.expected, "", ""},
				"tree-server": {"", "", "server.sh"},
				"tree-gpu":    {"", "server.sh", ""},
			}
			if !reflect.DeepEqual(prologues, expected) {
				t.Fatalf("incorrect prologues: %v, expected %v", prologues, expected)
			}
		})
	}
}

// TestWorkspaceTreeOverlappingEndpoints checks that path aliases are rejected
// during planning, before any synchronization sessions can start copying files.
func TestWorkspaceTreeOverlappingEndpoints(t *testing.T) {
	for _, test := range []struct{ name, first, second string }{
		{"relative-root", "host:.", "host:sub"},
		{"home-relative", "host:work", "host:~/work/sub"},
		{"windows", `host:C:\work`, `host:C:\work\sub`},
		{"windows-mixed", `host:C:\work`, "host:c:/work/sub"},
		{"container", "docker://container/~/work", "docker://container/~/work/sub"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := &Configuration{Tree: &TreeConfiguration{
				Nodes: map[string]TreeNode{
					"a": {Endpoint: test.first},
					"b": {Endpoint: test.second, Parent: "a"},
				},
			}}
			if _, err := NewSessionSpecifications(configuration, nil, "project", false); err == nil || !strings.Contains(err.Error(), "overlap") {
				t.Fatal("overlapping tree nodes were not rejected:", err)
			}
			// Ordinary sessions must use the same overlap rules as tree nodes.
			configuration.Tree.Nodes["b"] = TreeNode{Endpoint: "other:work", Parent: "a"}
			configuration.Synchronization = map[string]SynchronizationConfiguration{
				"extra": {Alpha: test.second, Beta: "third:work"},
			}
			if _, err := NewSessionSpecifications(configuration, nil, "project", false); err == nil || !strings.Contains(err.Error(), "overlap") {
				t.Fatal("overlapping ordinary session was not rejected:", err)
			}
		})
	}
}

func TestWorkspaceTreeMissingSymlinkDescendant(t *testing.T) {
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
	child := filepath.Join(alias, "new", "nested")
	configuration := &Configuration{Tree: &TreeConfiguration{
		Nodes: map[string]TreeNode{
			"a": {Endpoint: root},
			"b": {Endpoint: child, Parent: "a"},
		},
	}}
	if _, err := NewSessionSpecifications(configuration, nil, "project", false); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatal("overlap through a missing symlink descendant was not rejected:", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Fatal("project validation created a directory:", err)
	}
}
