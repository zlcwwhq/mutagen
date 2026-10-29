package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// treeListing is the JSON node listing, as decoded by agents.
type treeListing struct {
	Current string
	Problem string
	Nodes   []struct {
		Name                         string
		Path                         []string
		Current, Available, Adjacent bool
		Reason                       string
	}
}

// listTree runs remote --nodes --json from a directory.
func listTree(t *testing.T, directory string) treeListing {
	t.Helper()
	code, output, diagnostic := runRemote(t, directory, "--nodes", "--json")
	if code != 0 {
		t.Fatalf("node query failed: %d %s", code, diagnostic)
	}
	var listing treeListing
	if err := json.Unmarshal([]byte(output), &listing); err != nil {
		t.Fatalf("invalid node listing: %q %v", output, err)
	}
	return listing
}

// checkAdjacentOperations verifies that operations using only the connected
// c <-> b edge still work from c, although strict tree commands are refused
// for every node, including b, because of the reported tree problem.
func checkAdjacentOperations(t *testing.T, f *treeFixture, problem, job string) {
	t.Helper()
	from := f.roots["c"]
	code, output, diagnostic := runRemote(t, from, "--nodes")
	if code != 0 || !strings.Contains(output, "Tree problem: ") || !strings.Contains(output, problem) || !strings.Contains(output, "b: adjacent operations only") {
		t.Fatalf("degraded node listing failed: %d %q %s", code, output, diagnostic)
	}
	listing := listTree(t, from)
	if listing.Current != "c" || !strings.Contains(listing.Problem, problem) {
		t.Fatalf("tree problem not listed: %+v", listing)
	}
	for _, node := range listing.Nodes {
		if node.Name == "b" && (node.Available || !node.Adjacent) {
			t.Fatalf("connected adjacent node misreported: %+v", node)
		} else if node.Name != "b" && (node.Adjacent || node.Available != node.Current) {
			t.Fatalf("node misreported: %+v", node)
		}
	}
	for _, arguments := range [][]string{{"--conflicts"}, {"--conflicts", "--json"}, {"--flush"}, {"--ignores"}} {
		if code, output, diagnostic := runRemote(t, from, append([]string{"--node", "b"}, arguments...)...); code != 0 {
			t.Fatalf("adjacent operation %v failed: %d %q %s", arguments, code, output, diagnostic)
		}
	}
	// --no-flush skips synchronization, never the tree's validation.
	for _, arguments := range [][]string{{"--detach", "true"}, {"--detach", "--no-flush", "true"}} {
		code, output, diagnostic = runRemote(t, from, append([]string{"--node", "b"}, arguments...)...)
		if code != 125 || !strings.Contains(diagnostic, problem) {
			t.Fatalf("detached command %v bypassed tree preflight: %d %q %s", arguments, code, output, diagnostic)
		}
	}
	if code, output, diagnostic := runRemote(t, from, "--node", "b", "--jobs"); code != 0 || !strings.Contains(output, job) {
		t.Fatalf("detached job not listed: %d %q %s", code, output, diagnostic)
	}
	if code, _, diagnostic := runRemote(t, from, "--node", "b", "--wait", job); code != 3 {
		t.Fatalf("detached job not awaited: %d %s", code, diagnostic)
	}
	if contents, err := os.ReadFile(filepath.Join(f.roots["b"], "ignored", "job")); err != nil || string(contents) != "b" {
		t.Fatalf("detached job didn't run on b: %q %v", contents, err)
	}

	// Strict tree commands are refused before anything runs, for adjacent
	// and distant targets alike, and say why.
	marker := filepath.Join(f.roots["b"], "ignored", "not-run")
	for _, options := range [][]string{nil, {"--no-flush"}} {
		arguments := append(append([]string{"--node", "b"}, options...), "touch", marker)
		code, _, diagnostic = runRemote(t, from, arguments...)
		if code != 125 || !strings.Contains(diagnostic, "tree commands are unavailable") || !strings.Contains(diagnostic, problem) {
			t.Fatalf("strict command %v not refused: %d %s", options, code, diagnostic)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("refused tree command %v ran", options)
		}
		arguments = append(append([]string{"--node", "a"}, options...), "true")
		if code, _, diagnostic = runRemote(t, from, arguments...); code != 125 || !strings.Contains(diagnostic, problem) {
			t.Fatalf("distant strict command %v not refused: %d %s", options, code, diagnostic)
		}
	}
	if code, _, diagnostic = runRemote(t, from, "--node", "a", "--jobs"); code != 125 || !strings.Contains(diagnostic, "adjacent node") {
		t.Fatalf("distant adjacent operation not refused: %d %s", code, diagnostic)
	}
	if code, _, diagnostic = runRemote(t, from, "true"); code != 125 || !strings.Contains(diagnostic, "--node") {
		t.Fatalf("destination guessed despite several nodes: %d %s", code, diagnostic)
	}
}

// TestWorkspaceTreeMissingEdge verifies that a terminated edge, which leaves
// the persisted tree disconnected, refuses strict tree commands without
// blocking discovery or operations on healthy adjacent edges.
func TestWorkspaceTreeMissingEdge(t *testing.T) {
	f := newTreeFixture(t)
	job := startDegradedTreeJob(t, f)
	ctx := context.Background()
	if err := f.manager.Terminate(ctx, &selection.Selection{Specifications: []string{f.sessions["tree-b"]}}, ""); err != nil {
		t.Fatal(err)
	}
	checkAdjacentOperations(t, f, "node b is disconnected from tree root a", job)
	// The root still reaches its other edge's node for adjacent operations
	// once that edge is connected, despite the problem elsewhere.
	if err := f.manager.Resume(ctx, &selection.Selection{Specifications: []string{f.sessions["tree-d"]}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runRemoteCommand(f.roots["d"], "true"); err != nil {
		t.Fatal(err)
	}
	if code, _, diagnostic := runRemote(t, f.roots["a"], "--node", "d", "--jobs"); code != 0 {
		t.Fatalf("root's adjacent operation failed: %d %s", code, diagnostic)
	}
}

// TestWorkspaceTreeExternalOverlap verifies that a session outside the tree
// overlapping one of its nodes refuses strict tree commands, while adjacent
// operations continue, and that removing it restores strict commands.
func TestWorkspaceTreeExternalOverlap(t *testing.T) {
	f := newTreeFixture(t)
	job := startDegradedTreeJob(t, f)
	ctx := context.Background()
	alpha := filepath.Join(f.roots["a"], "ignored", "external")
	beta := filepath.Join(filepath.Dir(f.roots["a"]), "external")
	for _, directory := range []string{alpha, beta} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	external, err := f.manager.Create(ctx,
		&url.URL{Protocol: url.Protocol_Local, Path: alpha}, &url.URL{Protocol: url.Protocol_Local, Path: beta},
		&synchronization.Configuration{}, &synchronization.Configuration{}, &synchronization.Configuration{},
		"external", nil, nil, true, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	checkAdjacentOperations(t, f, "overlaps node a of the workspace tree", job)
	if err := f.manager.Terminate(ctx, &selection.Selection{Specifications: []string{external}}, ""); err != nil {
		t.Fatal(err)
	}
	if listing := listTree(t, f.roots["c"]); listing.Problem != "" {
		t.Fatalf("problem retained after removing the overlap: %+v", listing)
	}
	if code, output, diagnostic := runRemote(t, f.roots["c"], "--node", "a", "sh", "-c", `printf '%s' "$MUTAGEN_REMOTE_NODE"`); code != 0 || output != "a" {
		t.Fatalf("strict command not restored: %d %q %s", code, output, diagnostic)
	}
}

// Existing jobs remain controllable after the tree degrades, while new starts
// must pass the same strict synchronization checks as foreground commands.
func startDegradedTreeJob(t *testing.T, f *treeFixture) string {
	t.Helper()
	code, output, diagnostic := runRemote(t, f.roots["c"], "--node", "b", "--detach", "sh", "-c", `printf '%s' "$MUTAGEN_REMOTE_NODE" > ignored/job; exit 3`)
	if code != 0 || strings.TrimSpace(output) == "" {
		t.Fatalf("job not started: %d %q %s", code, output, diagnostic)
	}
	return strings.TrimSpace(output)
}
