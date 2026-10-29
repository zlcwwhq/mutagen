package project

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// TestTreePlanRetainsNodeTransport verifies that extending a tree or replacing
// one incident edge cannot move an existing node to a different Docker daemon
// merely because the invoking shell's captured environment changed.
func TestTreePlanRetainsNodeTransport(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "new-edge"
		if replace {
			name = "replaced-edge"
		}
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("MUTAGEN_ALPHA_DOCKER_HOST", "tcp://original:2375")
			configuration := &Configuration{Tree: &TreeConfiguration{
				Remote: true,
				Nodes: map[string]TreeNode{
					"a": {Endpoint: "./a"},
					"b": {Endpoint: "docker://container/work", Parent: "a"},
					"c": {Endpoint: "./c", Parent: "b"},
				},
			}}
			initial, err := NewSessionSpecifications(configuration, nil, "project", false)
			if err != nil {
				t.Fatal(err)
			}
			_, states := testSessionStates(initial)
			t.Setenv("MUTAGEN_ALPHA_DOCKER_HOST", "tcp://changed:2375")
			if replace {
				node := configuration.Tree.Nodes["c"]
				node.RemotePrologue = "changed.sh"
				configuration.Tree.Nodes["c"] = node
			} else {
				configuration.Tree.Nodes["d"] = TreeNode{Endpoint: "./d", Parent: "b"}
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
				t.Fatal("new edge changed the retained node's Docker daemon")
			}
			for _, specification := range desired.Synchronization {
				if specification.Name == change.specification.Name && specification.Alpha.Environment["DOCKER_HOST"] != "tcp://changed:2375" {
					t.Fatal("planning modified the caller's specifications")
				}
			}

			// The actual sessions after applying the plan must form a valid
			// tree, and applying the same declarations again must retain them.
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
			if err := synchronization.ValidateWorkspaceTreeSessions(final); err != nil {
				t.Fatal("plan produced an invalid tree:", err)
			}
			repeated, err := NewPlan(desired, nil, finalStates)
			if err != nil || len(repeated.synchronization) != 0 || repeated.unchanged != len(final) {
				t.Fatal("reapplying changed the reconciled tree:", err)
			}
		})
	}
}

// TestTreePlanNewTransportWithoutRetainedEdges verifies that a complete tree
// replacement still captures the current environment, just as ordinary pairs
// do. Endpoint preservation only applies while a node has a retained edge.
func TestTreePlanNewTransportWithoutRetainedEdges(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MUTAGEN_ALPHA_DOCKER_HOST", "tcp://original:2375")
	configuration := &Configuration{Tree: &TreeConfiguration{
		Nodes: map[string]TreeNode{
			"a": {Endpoint: "./a"},
			"b": {Endpoint: "docker://container/work", Parent: "a"},
		},
	}}
	initial, err := NewSessionSpecifications(configuration, nil, "project", false)
	if err != nil {
		t.Fatal(err)
	}
	_, states := testSessionStates(initial)
	before := proto.Clone(states[0].Session)
	t.Setenv("MUTAGEN_ALPHA_DOCKER_HOST", "tcp://changed:2375")
	configuration.Tree.Remote = true
	desired, err := NewSessionSpecifications(configuration, nil, "project", false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(desired, nil, states)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.synchronization) != 1 || plan.synchronization[0].specification.Beta.Environment["DOCKER_HOST"] != "tcp://changed:2375" {
		t.Fatal("fully replaced tree retained old transport environment")
	}
	if !proto.Equal(before, states[0].Session) {
		t.Fatal("planning changed an existing session")
	}
}

// TestTreePlanRejectsAmbiguousRetainedTransport ensures that an already
// inconsistent tree is not silently redirected to whichever retained edge
// happens to be visited first. Different captured environments can identify
// different Docker daemons even when their container names and paths match.
func TestTreePlanRejectsAmbiguousRetainedTransport(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MUTAGEN_ALPHA_DOCKER_HOST", "tcp://original:2375")
	configuration := &Configuration{Tree: &TreeConfiguration{
		Nodes: map[string]TreeNode{
			"a": {Endpoint: "./a"},
			"b": {Endpoint: "docker://container/work", Parent: "a"},
			"c": {Endpoint: "./c", Parent: "b"},
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
	if _, err := NewPlan(specifications, nil, states); err == nil || !strings.Contains(err.Error(), "inconsistent endpoints") || !strings.Contains(err.Error(), "mutagen sync list -l") {
		t.Fatal("ambiguous transport identity was not refused with recovery guidance:", err)
	}
	if !proto.Equal(before, states[1].Session) {
		t.Fatal("planning changed an ambiguous existing endpoint")
	}
}
