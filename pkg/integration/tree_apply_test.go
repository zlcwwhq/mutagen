package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/project"
	"github.com/mutagen-io/mutagen/pkg/selection"
)

// TestWorkspaceTreeApplyEquivalentEndpoint verifies that adding an incident
// edge through an alternate local path keeps the retained node's exact URL.
// Otherwise apply succeeds but runtime reconstruction refuses the entire tree,
// and a repeated apply reports every session unchanged instead of repairing it.
func TestWorkspaceTreeApplyEquivalentEndpoint(t *testing.T) {
	f := newTreeFixture(t, true)
	base := filepath.Dir(f.roots["a"])
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	configuration, err := project.LoadConfiguration(filepath.Join(base, "mutagen.yml"))
	if err != nil {
		t.Fatal(err)
	}
	b := configuration.Tree.Nodes["b"]
	b.Endpoint = filepath.Join(alias, "b")
	configuration.Tree.Nodes["b"] = b
	configuration.Tree.Nodes["e"] = project.TreeNode{Endpoint: filepath.Join(base, "e"), Parent: "b"}
	specifications, err := project.NewSessionSpecifications(configuration, nil, "tree-test", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, states, err := f.manager.List(ctx, &selection.Selection{All: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := project.NewPlan(specifications, nil, states)
	if err != nil {
		t.Fatal(err)
	}
	var description bytes.Buffer
	plan.Print(&description)
	if description.String() != "Create synchronization session \"tree-e\"\nUnchanged sessions: 3\n" {
		t.Fatalf("equivalent paths replaced histories: %s", description.String())
	}
	previous := synchronizationManager
	synchronizationManager = f.manager
	defer func() { synchronizationManager = previous }()
	if err := plan.Apply(&managerSessionOperations{ctx}); err != nil {
		t.Fatal(err)
	}
	if graph := listTree(t, f.roots["c"]); graph.Problem != "" {
		t.Fatal("apply produced an unroutable tree:", graph.Problem)
	}
	if code, _, diagnostic := runRemote(t, f.roots["c"], "--node", "a", "true"); code != 0 {
		t.Fatalf("tree command failed after apply: %d %s", code, diagnostic)
	}
	_, states, err = f.manager.List(ctx, &selection.Selection{All: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if id := f.sessions[state.Session.Name]; id != "" && state.Session.Identifier != id {
			t.Fatal("retained edge lost its synchronization history")
		}
	}
	repeated, err := project.NewPlan(specifications, nil, states)
	if err != nil {
		t.Fatal(err)
	}
	description.Reset()
	repeated.Print(&description)
	if strings.TrimSpace(description.String()) != "Unchanged sessions: 4" {
		t.Fatal("reapply changed the reconciled tree:", description.String())
	}
}
