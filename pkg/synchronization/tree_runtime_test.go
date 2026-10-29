package synchronization

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/url"
)

// TestTreeRouteMembershipInvalidation protects long running commands from
// unrelated project edits without permitting changes to their actual path.
func TestTreeRouteMembershipInvalidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*commandTrees)
		cancel bool
	}{
		{"remove-unrelated-leaf", func(trees *commandTrees) { trees.update(trees.controllers["d"], true) }, false},
		{"add-unrelated-leaf", func(trees *commandTrees) { trees.update(&controller{session: treeTestEdge("a", "e")}, false) }, false},
		{"remove-path", func(trees *commandTrees) { trees.update(trees.controllers["b"], true) }, true},
		{"overlap-unrelated-node", func(trees *commandTrees) { trees.update(treeRuntimeExternal("d"), false) }, false},
		{"overlap-path-node", func(trees *commandTrees) { trees.update(treeRuntimeExternal("b"), false) }, true},
		{"cycle-through-path", func(trees *commandTrees) {
			edge := treeTestEdge("c", "b")
			edge.Identifier = "cycle"
			trees.update(&controller{session: edge}, false)
		}, true},
		{"inconsistent-path-node", func(trees *commandTrees) {
			edge := treeTestEdge("b", "e")
			edge.Alpha.Path = "~/different"
			trees.update(&controller{session: edge}, false)
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			trees, relays := treeTestRegistry([]*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c"), treeTestEdge("a", "d")}, "b", "c")
			route, err := trees.plan(relays["c"], 1, "a")
			if err != nil {
				t.Fatal(err)
			}
			release, err := route.activate()
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			test.change(trees)
			cancelled := route.check(context.Background()) != nil
			if cancelled != test.cancel {
				t.Fatalf("route cancellation = %t, want %t", cancelled, test.cancel)
			}
		})
	}
}

func treeRuntimeExternal(node string) *controller {
	return &controller{session: &Session{
		Identifier: "external-" + node,
		Alpha:      &url.URL{Protocol: url.Protocol_SSH, Host: node, Path: "~/project/sub"},
		Beta:       &url.URL{Protocol: url.Protocol_SSH, Host: "outside", Path: "~/other"},
	}}
}

// TestTreeRouteInvalidationAfterDegradation ensures an invocation that survived
// an unrelated problem still notices later overlapping changes on its path.
func TestTreeRouteInvalidationAfterDegradation(t *testing.T) {
	trees, relays := treeTestRegistry([]*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c"), treeTestEdge("a", "d"), treeTestEdge("d", "e")}, "b", "c")
	route, err := trees.plan(relays["c"], 1, "a")
	if err != nil {
		t.Fatal(err)
	}
	release, err := route.activate()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	trees.update(trees.controllers["d"], true)
	if trees.cached["project"].problem == nil || route.check(context.Background()) != nil {
		t.Fatal("unrelated degradation cancelled the route")
	}
	trees.update(treeRuntimeExternal("b"), false)
	if route.check(context.Background()) == nil {
		t.Fatal("path overlap after degradation did not cancel the route")
	}
}

// TestTreeDiscoveryUsesCachedValidation changes a path alias after validating
// membership. Discovery must reuse that validation, while the next membership
// update re-evaluates path identity and detects the new overlap.
func TestTreeDiscoveryUsesCachedValidation(t *testing.T) {
	base := t.TempDir()
	first, second, alias := filepath.Join(base, "first"), filepath.Join(base, "second"), filepath.Join(base, "alias")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	edge := treeTestEdge("a", "b")
	edge.Alpha = &url.URL{Protocol: url.Protocol_Local, Path: first}
	edge.Beta = &url.URL{Protocol: url.Protocol_Local, Path: alias}
	trees, relays := treeTestRegistry([]*Session{edge}, "b")
	cached := trees.cached["project"]
	if cached.problem != nil {
		t.Fatal(cached.problem)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		graph, err := trees.nodes(relays["b"], 0)
		if err != nil || graph.Problem != "" || trees.cached["project"] != cached {
			t.Fatalf("discovery rebuilt immutable membership: %v %+v", err, graph)
		}
	}
	updated := proto.Clone(edge).(*Session)
	trees.update(&controller{session: updated}, false)
	if trees.cached["project"].problem == nil {
		t.Fatal("membership update failed to revalidate local endpoints")
	}
}

// TestTreeValidationDoesNotLockRelays models a slow filesystem operation in
// validation and verifies that relay lifecycle changes remain independent.
func TestTreeValidationDoesNotLockRelays(t *testing.T) {
	trees := newCommandTrees()
	started, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	trees.build = func(group string, controllers map[string]*controller) *cachedTree {
		close(started)
		<-resume
		return buildTree(group, controllers)
	}
	c := &controller{session: treeTestEdge("a", "b")}
	go func() { trees.update(c, false); close(finished) }()
	<-started
	relayChanged := make(chan struct{})
	go func() { trees.setRelay(c, &commandRelay{}); close(relayChanged) }()
	select {
	case <-relayChanged:
	case <-time.After(5 * time.Second):
		close(resume)
		<-finished
		t.Fatal("filesystem validation blocked relay registration")
	}
	close(resume)
	<-finished
}

// TestTreeQueriesDoNotRebuild counts static validation independently from the
// number of nodes discovered or paths planned from the cached group.
func TestTreeQueriesDoNotRebuild(t *testing.T) {
	trees, relays := treeTestRegistry([]*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c"), treeTestEdge("a", "d")}, "b", "c", "d")
	builds := 0
	trees.build = func(group string, controllers map[string]*controller) *cachedTree {
		builds++
		return buildTree(group, controllers)
	}
	trees.update(&controller{session: treeTestEdge("a", "e")}, false)
	if builds != 1 {
		t.Fatalf("membership update rebuilt %d groups, want 1", builds)
	}
	for i := 0; i < 3; i++ {
		if _, err := trees.nodes(relays["c"], 1); err != nil {
			t.Fatal(err)
		}
		if _, err := trees.plan(relays["c"], 1, "d"); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 {
		t.Fatalf("read-only queries repeated static validation: %d builds", builds)
	}
}

// TestTreeRouteIndirectCycle invalidates an execution path when a new edge
// between other branches creates an alternative path through its workspaces.
func TestTreeRouteIndirectCycle(t *testing.T) {
	trees, relays := treeTestRegistry([]*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c"), treeTestEdge("a", "d"), treeTestEdge("d", "e"), treeTestEdge("c", "f")}, "b", "c")
	route, err := trees.plan(relays["c"], 1, "b")
	if err != nil {
		t.Fatal(err)
	}
	release, err := route.activate()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	edge := treeTestEdge("e", "f")
	edge.Identifier = "cycle"
	trees.update(&controller{session: edge}, false)
	if route.check(context.Background()) == nil {
		t.Fatal("cycle crossing the path did not cancel it")
	}
}

// TestTreeRouteUnrelatedProblemThenSafeLeaf ensures a harmless new edge does
// not inherit the cancellation consequence of an existing unrelated overlap.
func TestTreeRouteUnrelatedProblemThenSafeLeaf(t *testing.T) {
	trees, relays := treeTestRegistry([]*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c"), treeTestEdge("a", "d")}, "b", "c")
	route, err := trees.plan(relays["c"], 1, "b")
	if err != nil {
		t.Fatal(err)
	}
	release, err := route.activate()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	trees.update(treeRuntimeExternal("d"), false)
	if err := route.check(context.Background()); err != nil {
		t.Fatal("unrelated overlap cancelled early:", err)
	}
	trees.update(&controller{session: treeTestEdge("b", "e")}, false)
	if err := route.check(context.Background()); err != nil {
		t.Fatal("adding a safe leaf cancelled the path because of a pre-existing unrelated problem:", err)
	}
}

// TestTreeRouteCycleAfterNewBranchesAndDegradation uses all latest edges when
// checking for a new cycle, even if the group has no valid parent map.
func TestTreeRouteCycleAfterNewBranchesAndDegradation(t *testing.T) {
	trees, relays := treeTestRegistry([]*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c"), treeTestEdge("a", "d"), treeTestEdge("d", "e"), treeTestEdge("c", "f")}, "b", "c")
	route, err := trees.plan(relays["c"], 1, "b")
	if err != nil {
		t.Fatal(err)
	}
	release, err := route.activate()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	trees.update(&controller{session: treeTestEdge("a", "x")}, false)
	trees.update(&controller{session: treeTestEdge("f", "y")}, false)
	trees.update(trees.controllers["d"], true)
	if err := route.check(context.Background()); err != nil {
		t.Fatal("unrelated changes cancelled early:", err)
	}
	added := treeTestEdge("x", "y")
	added.Identifier = "cycle"
	trees.update(&controller{session: added}, false)
	if route.check(context.Background()) == nil {
		t.Fatal("new cycle through x-a-b-c-f-y-x did not cancel active b-c path")
	}
}
