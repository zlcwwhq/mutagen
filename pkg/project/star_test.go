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
	"github.com/mutagen-io/mutagen/pkg/url"
)

// TestStarSpecifications verifies the expansion of a star into one session
// per non-center node, with the center as alpha, shared settings, per-node
// modes, node prologues, and membership.
func TestStarSpecifications(t *testing.T) {
	t.Chdir(t.TempDir())
	specifications := testSpecifications(t, `
sync:
  defaults:
    ignore: {paths: [ignored]}
    configurationAlpha: {watch: {pollingInterval: 7}}
star:
  remote: true
  sync: {ignore: {paths: [shared]}}
  center: local
  nodes:
    local: {endpoint: ./local, remotePrologue: local.sh}
    server: {endpoint: "server:~/project"}
    gpu: {endpoint: "gpu:~/project", mode: one-way-safe, remotePrologue: gpu.sh}
`)
	if len(specifications.Synchronization) != 2 {
		t.Fatal("incorrect edge count")
	}
	gpu, server := specifications.Synchronization[0], specifications.Synchronization[1]
	if gpu.Name != "star-gpu" || server.Name != "star-server" {
		t.Fatal("incorrect session names:", gpu.Name, server.Name)
	}
	local, err := filepath.Abs("local")
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range []struct {
		name string
		mode core.SynchronizationMode
		host string
	}{
		{"star-gpu", core.SynchronizationMode_SynchronizationModeOneWaySafe, "gpu"},
		{"star-server", core.SynchronizationMode_SynchronizationModeTwoWaySafe, "server"},
	} {
		specification := gpu
		if edge.name == server.Name {
			specification = server
		}
		if specification.Alpha.Protocol != url.Protocol_Local || specification.Alpha.Path != local {
			t.Fatalf("%s: center isn't alpha: %v", edge.name, specification.Alpha)
		} else if specification.Beta.Host != edge.host || specification.Beta.Path != "~/project" {
			t.Fatalf("%s: node isn't beta: %v", edge.name, specification.Beta)
		} else if specification.Configuration.SynchronizationMode != edge.mode {
			t.Fatalf("%s: incorrect mode: %v", edge.name, specification.Configuration.SynchronizationMode)
		} else if specification.Configuration.RemoteCommandMode != synchronization.RemoteCommandMode_RemoteCommandModeBoth ||
			strings.Join(specification.Configuration.Ignores, ",") != "ignored,shared" {
			t.Fatalf("%s: incorrect shared configuration: %v", edge.name, specification.Configuration)
		} else if specification.ConfigurationAlpha.WatchPollingInterval != 0 || len(specification.Labels) != 1 || specification.Labels[LabelKey] != "project" {
			t.Fatalf("%s: pair overrides or project identity leaked", edge.name)
		} else if specification.ConfigurationAlpha.RemotePrologue != "local.sh" {
			t.Fatalf("%s: center prologue not attached to alpha", edge.name)
		} else if specification.Star.GetIdentifier() != "project" || specification.Star.GetCenter() != "local" || specification.Star.GetNode() != edge.host {
			t.Fatalf("%s: incorrect membership: %v", edge.name, specification.Star)
		}
	}
	if gpu.ConfigurationBeta.RemotePrologue != "gpu.sh" || server.ConfigurationBeta.RemotePrologue != "" {
		t.Fatal("node prologues not attached to their own edges")
	}

	// Reapplying keeps histories, but changed membership must not be mistaken
	// for an unchanged session solely because its paths match.
	_, states := testSessionStates(specifications)
	plan, err := NewPlan(specifications, nil, states)
	if err != nil || plan.unchanged != 2 {
		t.Fatalf("unchanged star not retained: %v", err)
	}
	states[0].Session = proto.Clone(states[0].Session).(*synchronization.Session)
	states[0].Session.Star.Center = "renamed"
	plan, err = NewPlan(specifications, nil, states)
	if err != nil || plan.unchanged != 1 || len(plan.synchronization) != 1 {
		t.Fatalf("membership change not replaced: %v", err)
	}
}

// TestStarModePrecedence verifies that a node's mode takes precedence over the
// star's shared mode, which takes precedence over the defaults, and that
// two-way-safe is used if nothing selects a mode.
func TestStarModePrecedence(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		name, defaults, shared, node string
		expected                     core.SynchronizationMode
	}{
		{"none", "", "", "", core.SynchronizationMode_SynchronizationModeTwoWaySafe},
		{"defaults", "two-way-resolved", "", "", core.SynchronizationMode_SynchronizationModeTwoWayResolved},
		{"shared", "two-way-resolved", "one-way-replica", "", core.SynchronizationMode_SynchronizationModeOneWayReplica},
		{"node", "two-way-resolved", "one-way-replica", "one-way-safe", core.SynchronizationMode_SynchronizationModeOneWaySafe},
		{"node-only", "", "", "two-way-resolved", core.SynchronizationMode_SynchronizationModeTwoWayResolved},
	} {
		t.Run(test.name, func(t *testing.T) {
			mode := func(value string) string {
				if value == "" {
					return ""
				}
				return "mode: " + value
			}
			specifications := testSpecifications(t, fmt.Sprintf(`
sync:
  defaults: {%s}
star:
  sync: {%s}
  center: a
  nodes:
    a: {endpoint: ./a}
    b: {endpoint: ./b, %s}
`, mode(test.defaults), mode(test.shared), mode(test.node)))
			if actual := specifications.Synchronization[0].Configuration.SynchronizationMode; actual != test.expected {
				t.Fatalf("incorrect mode: %v, expected %v", actual, test.expected)
			}
		})
	}
}

// TestStarValidation verifies that invalid stars are rejected before any
// session is created.
func TestStarValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("file", nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, yaml, message string }{
		{"center-missing", "star: {nodes: {a: {endpoint: ./a}, b: {endpoint: ./b}}}", "star.center is required"},
		{"center-unknown", "star: {center: c, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b}}}", `star.center "c" doesn't name a node`},
		{"single", "star: {center: a, nodes: {a: {endpoint: ./a}}}", "at least one node besides its center"},
		{"center-mode", "star: {center: a, nodes: {a: {endpoint: ./a, mode: one-way-safe}, b: {endpoint: ./b}}}", "mode can't be set on the center node a"},
		{"endpoint", "star: {center: a, nodes: {a: {endpoint: ./a}, b: {}}}", "node b has no endpoint"},
		{"name", "star: {center: a, nodes: {a: {endpoint: ./a}, -b: {endpoint: ./b}}}", "invalid star node name"},
		{"reserved", "star: {center: a, nodes: {a: {endpoint: ./a}, defaults: {endpoint: ./b}}}", "reserved"},
		{"file", "star: {center: a, nodes: {a: {endpoint: ./a}, b: {endpoint: ./file}}}", "must be a directory"},
		{"overlap", "star: {center: a, nodes: {a: {endpoint: ./a}, b: {endpoint: ./a/b}}}", "nodes a and b overlap"},
		{"overlap-nodes", "star: {center: a, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b}, c: {endpoint: ./b/c}}}", "nodes b and c overlap"},
		{"direction", "star: {center: a, sync: {remote: alpha}, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b}}}", "star.remote"},
		{"prologue", "star: {center: a, sync: {remotePrologue: x.sh}, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b}}}", "remotePrologue"},
		{"pair", "sync: {extra: {alpha: ./a/sub, beta: ./c}}\nstar: {center: a, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b}}}", "session extra overlaps node a"},
		{"session-name", "sync: {star-b: {alpha: ./c, beta: ./d}}\nstar: {center: a, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b}}}", "reserved by the star"},
		{"tree", "tree: {nodes: {a: {endpoint: ./a}, b: {endpoint: ./b, parent: a}}}", "declare a star (star:) instead"},
		{"tree-empty", "tree:", "declare a star (star:) instead"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewSessionSpecifications(testConfiguration(t, test.yaml), nil, "project", false)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("invalid star not rejected as expected: %v", err)
			}
		})
	}
	// Invalid modes are rejected when the file is loaded.
	path := filepath.Join(t.TempDir(), DefaultConfigurationFileName)
	if err := os.WriteFile(path, []byte("star: {center: a, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b, mode: sideways}}}"), 0600); err != nil {
		t.Fatal(err)
	} else if _, err := LoadConfiguration(path); err == nil {
		t.Fatal("invalid mode accepted")
	}
	// Enabling pair commands in defaults must not grant star-wide execution.
	specifications := testSpecifications(t, "sync: {defaults: {remote: true}}\nstar: {center: a, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b}}}")
	if specifications.Synchronization[0].Configuration.RemoteCommandMode != synchronization.RemoteCommandMode_RemoteCommandModeDisabled {
		t.Fatal("star commands enabled implicitly")
	}
}

// TestStarDefaultPrologue verifies that the prologue of the global or project
// synchronization defaults doesn't reach the configuration shared by star
// edges, which underlies every node's merged endpoint configuration and would
// run it on each node without its own. Node prologues, including the center's,
// must still attach to their own endpoints, and pairs keep the defaults.
func TestStarDefaultPrologue(t *testing.T) {
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
star:
  remote: true
  center: local
  nodes:
    local: {endpoint: ./local, remotePrologue: local.sh}
    server: {endpoint: ./server, remotePrologue: server.sh}
    gpu: {endpoint: ./gpu}
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
				"star-server": {"", "local.sh", "server.sh"},
				"star-gpu":    {"", "local.sh", ""},
			}
			if !reflect.DeepEqual(prologues, expected) {
				t.Fatalf("incorrect prologues: %v, expected %v", prologues, expected)
			}
		})
	}
}

// TestStarOverlappingEndpoints checks that overlapping remote paths are
// rejected during planning, before any synchronization session can start
// copying files.
func TestStarOverlappingEndpoints(t *testing.T) {
	for _, test := range []struct{ name, first, second string }{
		{"relative-root", "host:.", "host:sub"},
		{"home-relative", "host:work", "host:~/work/sub"},
		{"windows", `host:C:\work`, `host:C:\work\sub`},
		{"windows-mixed", `host:C:\work`, "host:c:/work/sub"},
		{"container", "docker://container/~/work", "docker://container/~/work/sub"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := &Configuration{Star: &StarConfiguration{
				Center: "a",
				Nodes: map[string]StarNode{
					"a": {Endpoint: test.first},
					"b": {Endpoint: test.second},
				},
			}}
			if _, err := NewSessionSpecifications(configuration, nil, "project", false); err == nil || !strings.Contains(err.Error(), "overlap") {
				t.Fatal("overlapping star nodes were not rejected:", err)
			}
			// Other sessions must use the same overlap rules as star nodes.
			configuration.Star.Nodes["b"] = StarNode{Endpoint: "other:work"}
			configuration.Synchronization = map[string]SynchronizationConfiguration{
				"extra": {Alpha: test.second, Beta: "third:work"},
			}
			if _, err := NewSessionSpecifications(configuration, nil, "project", false); err == nil || !strings.Contains(err.Error(), "overlap") {
				t.Fatal("overlapping ordinary session was not rejected:", err)
			}
		})
	}
}

func TestStarMissingSymlinkDescendant(t *testing.T) {
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
	configuration := &Configuration{Star: &StarConfiguration{
		Center: "a",
		Nodes: map[string]StarNode{
			"a": {Endpoint: root},
			"b": {Endpoint: filepath.Join(alias, "new", "nested")},
		},
	}}
	if _, err := NewSessionSpecifications(configuration, nil, "project", false); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatal("overlap through a missing symlink descendant was not rejected:", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Fatal("project validation created a directory:", err)
	}
}

// TestStarPlanRetainsCenterTransport verifies that adding a node or replacing
// one edge can't move the center to a different Docker daemon merely because
// the invoking shell's captured environment changed, while non-center nodes,
// which have a single edge each, capture the current environment.
func TestStarPlanRetainsCenterTransport(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "new-edge"
		if replace {
			name = "replaced-edge"
		}
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("MUTAGEN_ALPHA_DOCKER_HOST", "tcp://original:2375")
			t.Setenv("MUTAGEN_BETA_DOCKER_HOST", "tcp://original:2375")
			configuration := &Configuration{Star: &StarConfiguration{
				Remote: true,
				Center: "a",
				Nodes: map[string]StarNode{
					"a": {Endpoint: "docker://container/work"},
					"b": {Endpoint: "./b"},
					"c": {Endpoint: "./c"},
				},
			}}
			initial, err := NewSessionSpecifications(configuration, nil, "project", false)
			if err != nil {
				t.Fatal(err)
			}
			_, states := testSessionStates(initial)
			t.Setenv("MUTAGEN_ALPHA_DOCKER_HOST", "tcp://changed:2375")
			t.Setenv("MUTAGEN_BETA_DOCKER_HOST", "tcp://changed:2375")
			if replace {
				node := configuration.Star.Nodes["c"]
				node.RemotePrologue = "changed.sh"
				configuration.Star.Nodes["c"] = node
			} else {
				configuration.Star.Nodes["d"] = StarNode{Endpoint: "docker://other/work"}
			}
			desired, err := NewSessionSpecifications(configuration, nil, "project", false)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := NewPlan(desired, nil, states)
			if err != nil {
				t.Fatal(err)
			}
			expectedUnchanged := len(states)
			if replace {
				expectedUnchanged--
			}
			if len(plan.synchronization) != 1 || plan.unchanged != expectedUnchanged {
				t.Fatalf("unrelated histories were replaced: %+v", plan)
			}
			change := plan.synchronization[0]
			if change.specification.Alpha.Environment["DOCKER_HOST"] != "tcp://original:2375" {
				t.Fatal("new edge changed the center's Docker daemon")
			} else if !replace && change.specification.Beta.Environment["DOCKER_HOST"] != "tcp://changed:2375" {
				t.Fatal("new node didn't capture the current environment")
			}
			for _, specification := range desired.Synchronization {
				if specification.Name == change.specification.Name && specification.Alpha.Environment["DOCKER_HOST"] != "tcp://changed:2375" {
					t.Fatal("planning modified the caller's specifications")
				}
			}

			// The actual sessions after applying the plan must form a valid
			// star, and applying the same declarations again must retain them.
			var final []*synchronization.Session
			var finalStates []*synchronization.State
			for _, state := range states {
				if state.Session != change.current {
					final = append(final, state.Session)
					finalStates = append(finalStates, state)
				}
			}
			created := specificationSession(change.specification)
			created.Identifier = "new-" + created.Name
			final = append(final, created)
			finalStates = append(finalStates, &synchronization.State{Session: created})
			if err := synchronization.ValidateStar(final, nil); err != nil {
				t.Fatal("plan produced an invalid star:", err)
			}
			repeated, err := NewPlan(desired, nil, finalStates)
			if err != nil || len(repeated.synchronization) != 0 || repeated.unchanged != len(final) {
				t.Fatal("reapplying changed the reconciled star:", err)
			}
		})
	}
}

// TestStarPlanNewTransportWithoutRetainedEdges verifies that a complete star
// replacement still captures the current environment, just as ordinary pairs
// do. Center preservation only applies while the star has a retained edge.
func TestStarPlanNewTransportWithoutRetainedEdges(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MUTAGEN_ALPHA_DOCKER_HOST", "tcp://original:2375")
	configuration := &Configuration{Star: &StarConfiguration{
		Center: "a",
		Nodes: map[string]StarNode{
			"a": {Endpoint: "docker://container/work"},
			"b": {Endpoint: "./b"},
		},
	}}
	initial, err := NewSessionSpecifications(configuration, nil, "project", false)
	if err != nil {
		t.Fatal(err)
	}
	_, states := testSessionStates(initial)
	before := proto.Clone(states[0].Session)
	t.Setenv("MUTAGEN_ALPHA_DOCKER_HOST", "tcp://changed:2375")
	configuration.Star.Remote = true
	desired, err := NewSessionSpecifications(configuration, nil, "project", false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(desired, nil, states)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.synchronization) != 1 || plan.synchronization[0].specification.Alpha.Environment["DOCKER_HOST"] != "tcp://changed:2375" {
		t.Fatal("fully replaced star retained old transport environment")
	}
	if !proto.Equal(before, states[0].Session) {
		t.Fatal("planning changed an existing session")
	}
}

// TestStarPlanRejectsAmbiguousRetainedCenter ensures that a star whose
// retained edges already disagree on the center isn't silently redirected to
// whichever retained edge happens to be visited first. Different captured
// environments can identify different Docker daemons even when their container
// names and paths match.
func TestStarPlanRejectsAmbiguousRetainedCenter(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MUTAGEN_ALPHA_DOCKER_HOST", "tcp://original:2375")
	configuration := &Configuration{Star: &StarConfiguration{
		Center: "a",
		Nodes: map[string]StarNode{
			"a": {Endpoint: "docker://container/work"},
			"b": {Endpoint: "./b"},
			"c": {Endpoint: "./c"},
		},
	}}
	specifications, err := NewSessionSpecifications(configuration, nil, "project", false)
	if err != nil {
		t.Fatal(err)
	}
	_, states := testSessionStates(specifications)
	states[1].Session.Alpha = proto.Clone(states[1].Session.Alpha).(*url.URL)
	states[1].Session.Alpha.Environment["DOCKER_HOST"] = "tcp://other:2375"
	before := proto.Clone(states[1].Session)
	if _, err := NewPlan(specifications, nil, states); err == nil || !strings.Contains(err.Error(), "disagree on the endpoint of center node a") || !strings.Contains(err.Error(), "mutagen sync list -l") {
		t.Fatal("ambiguous center was not refused with recovery guidance:", err)
	}
	if !proto.Equal(before, states[1].Session) {
		t.Fatal("planning changed an ambiguous existing endpoint")
	}
}

// TestStarPlanRemovesTreeEdges verifies that sessions of a former workspace
// tree, which load as plain pairs of the project, are removed when the
// project's star is applied.
func TestStarPlanRemovesTreeEdges(t *testing.T) {
	t.Chdir(t.TempDir())
	specifications := testSpecifications(t, "star: {center: a, nodes: {a: {endpoint: ./a}, b: {endpoint: ./b}}}")
	_, states := testSessionStates(specifications)
	tree := proto.Clone(states[0].Session).(*synchronization.Session)
	tree.Identifier, tree.Name, tree.Star = "sync-tree-b", "tree-b", nil
	states[0] = &synchronization.State{Session: tree}
	plan, err := NewPlan(specifications, nil, states)
	if err != nil {
		t.Fatal(err)
	}
	output := testPlanOutput(plan)
	if !strings.Contains(output, `Create synchronization session "star-b"`) || !strings.Contains(output, `Remove synchronization session "tree-b"`) {
		t.Fatalf("tree edge not replaced by star edge:\n%s", output)
	}
}
