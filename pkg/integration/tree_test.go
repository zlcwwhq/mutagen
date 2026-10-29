package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/integration/protocols/netpipe"
	"github.com/mutagen-io/mutagen/pkg/project"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// treeFixture exercises the real coordinator, CLI, synchronization engine,
// persistent session state, and agent-style command channels. Watching is off
// so assertions depend on the explicit path synchronization, not background
// propagation. The fourth node is an unavailable branch outside the path.
type treeFixture struct {
	manager  *synchronization.Manager
	roots    map[string]string
	sessions map[string]string
}

func newTreeFixture(t *testing.T, local ...bool) *treeFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("commands require a POSIX shell")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Keep the data directory out of t.TempDir: the test name and nested
	// directory it adds, combined with macOS's long temporary root, would push
	// the command registry's socket paths past Unix socket path limits.
	data, err := os.MkdirTemp("", "mt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(data); err != nil {
			t.Error("unable to remove data directory:", err)
		}
	})
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	t.Chdir(base)
	f := &treeFixture{roots: make(map[string]string), sessions: make(map[string]string)}
	for _, node := range []string{"a", "b", "c", "d"} {
		f.roots[node] = filepath.Join(base, node)
		if err := os.MkdirAll(filepath.Join(f.roots[node], "ignored"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(f.roots["a"], "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"input", "contended"} {
		if err := os.WriteFile(filepath.Join(f.roots["a"], "sub", name), []byte("base"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	prologue := filepath.Join(base, "prologue.sh")
	if err := os.WriteFile(prologue, []byte("export TREE_PROLOGUE=node-b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "mutagen.yml")
	config := fmt.Sprintf(`tree:
  remote: true
  sync:
    watch: {mode: no-watch}
    ignore: {paths: [ignored]}
  nodes:
    a: {endpoint: ./a}
    b: {endpoint: ./b, parent: a, remotePrologue: %q}
    c: {endpoint: ./c, parent: b}
    d: {endpoint: ./d, parent: a}
`, prologue)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	configuration, err := project.LoadConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	specifications, err := project.NewSessionSpecifications(configuration, nil, "tree-test", false)
	if err != nil {
		t.Fatal(err)
	}
	f.manager, err = synchronization.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.manager.Terminate(context.Background(), &selection.Selection{All: true}, "")
		f.manager.Shutdown()
	})
	for _, s := range specifications.Synchronization {
		for _, endpoint := range []*url.URL{s.Alpha, s.Beta} {
			if len(local) == 0 && (endpoint.Path == f.roots["b"] || endpoint.Path == f.roots["c"]) {
				endpoint.Protocol = netpipe.Protocol_Netpipe
			}
		}
	}
	// All URL changes precede creation: incident edges share immutable node
	// URLs, which live controllers may read immediately after Create returns.
	for _, s := range specifications.Synchronization {
		id, err := f.manager.Create(context.Background(), s.Alpha, s.Beta, s.Configuration, s.ConfigurationAlpha, s.ConfigurationBeta, s.Name, s.Labels, s.Tree, s.Name == "tree-d", "")
		if err != nil {
			t.Fatal(err)
		}
		f.sessions[s.Name] = id
	}
	f.ready(t)
	return f
}

func (f *treeFixture) ready(t *testing.T) {
	t.Helper()
	// connected reports whether a node's workspace is located as that node,
	// its daemon reports the other connected nodes (all but the paused branch)
	// as available, and the submission endpoint of each of its edges to an
	// adjacent node is registered, so that submissions pinned to either edge
	// succeed, as the CLI's are.
	connected := func(node string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		root := f.roots[node]
		connection, source, err := (&remote.Selection{Workspace: true}).Submit(ctx, root, &remote.Submission{Operation: remote.OperationNodes})
		if err != nil {
			return false
		}
		graph, err := remote.ListNodes(ctx, connection)
		if err != nil || graph.Source != node {
			return false
		}
		for _, other := range graph.Nodes {
			if other.Current || other.Name == "d" {
				continue
			} else if !other.Available {
				return false
			} else if len(other.Path) == 2 {
				connection, _, err := (&remote.Selection{Target: source, Resolve: true, Node: other.Name}).Submit(ctx, root, &remote.Submission{Operation: remote.OperationResolve})
				if err != nil {
					return false
				}
				connection.Close()
			}
		}
		return true
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := true
		for _, node := range []string{"a", "b", "c"} {
			ready = ready && connected(node)
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("tree command channels did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkspaceTreeCommands(t *testing.T) {
	f := newTreeFixture(t)
	code, output, diagnostic := runRemote(t, filepath.Join(f.roots["a"], "sub"), "--node", "c", "-e", "MUTAGEN_REMOTE_NODE=spoof", "sh", "-c", `cat input; printf ':%s' "$MUTAGEN_REMOTE_NODE"; printf result > result`)
	if code != 0 || output != "base:c" {
		t.Fatalf("node command failed: %d %q %s", code, output, diagnostic)
	}
	if result, err := os.ReadFile(filepath.Join(f.roots["a"], "sub", "result")); err != nil || string(result) != "result" {
		t.Fatalf("result didn't return over path: %q %v", result, err)
	}
	for _, from := range []string{"a", "c"} {
		code, output, diagnostic = runRemote(t, filepath.Join(f.roots[from], "sub"), "--node", "b", "sh", "-c", `printf '%s:%s:' "$MUTAGEN_REMOTE_NODE" "$TREE_PROLOGUE"; pwd -P`)
		if code != 0 || strings.TrimSpace(output) != "b:node-b:"+filepath.Join(f.roots["b"], "sub") {
			t.Fatalf("node execution depended on edge: %d %q %s", code, output, diagnostic)
		}
	}
	code, output, diagnostic = runRemote(t, filepath.Join(f.roots["c"], "sub"), "--node", "a", "pwd", "-P")
	if code != 0 || strings.TrimSpace(output) != filepath.Join(f.roots["a"], "sub") {
		t.Fatalf("reverse path failed: %d %q %s", code, output, diagnostic)
	}
	code, _, diagnostic = runRemote(t, f.roots["a"], "--node", "missing", "true")
	if code != 125 || !strings.Contains(diagnostic, "unknown node") {
		t.Fatalf("unknown node not refused: %d %s", code, diagnostic)
	}
	code, _, diagnostic = runRemote(t, f.roots["a"], "--node", "d", "true")
	if code != 255 {
		t.Fatalf("paused target not refused: %d %s", code, diagnostic)
	}
	// Enabling the other branch exercises a path through a common ancestor.
	if err := f.manager.Resume(context.Background(), &selection.Selection{Specifications: []string{f.sessions["tree-d"]}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runRemoteCommand(f.roots["d"], "true"); err != nil {
		t.Fatal(err)
	}
	code, output, diagnostic = runRemote(t, filepath.Join(f.roots["c"], "sub"), "--node", "d", "pwd", "-P")
	if code != 0 || strings.TrimSpace(output) != filepath.Join(f.roots["d"], "sub") {
		t.Fatalf("sibling path failed: %d %q %s", code, output, diagnostic)
	}

}

func TestWorkspaceTreeRestart(t *testing.T) {
	// Use real local URLs here: the in-memory test transport intentionally
	// isn't accepted by validation when persisted sessions are loaded.
	f := newTreeFixture(t, true)
	// Membership survives a daemon restart, including the paused branch.
	f.manager.Shutdown()
	var err error
	f.manager, err = synchronization.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.ready(t)
	code, output, diagnostic := runRemote(t, f.roots["c"], "--node", "a", "sh", "-c", `printf '%s' "$MUTAGEN_REMOTE_NODE"`)
	if code != 0 || output != "a" {
		t.Fatalf("routing after restart failed: %d %q %s", code, output, diagnostic)
	}
}

func TestWorkspaceTreeConflicts(t *testing.T) {
	f := newTreeFixture(t)
	if code, _, diagnostic := runRemote(t, f.roots["a"], "--node", "c", "true"); code != 0 {
		t.Fatal(diagnostic)
	}
	for _, node := range []string{"a", "c"} {
		if err := os.WriteFile(filepath.Join(f.roots[node], "sub", "input"), []byte(node), 0600); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(f.roots["c"], "ignored", "should-not-run")
	code, _, diagnostic := runRemote(t, f.roots["a"], "--node", "c", "touch", marker)
	if code != 125 || !strings.Contains(diagnostic, "command not started") || !strings.Contains(diagnostic, "b <-> c") {
		t.Fatalf("conflicted command not refused: %d %s", code, diagnostic)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command ran despite preflight conflict")
	}
	// --no-flush skips the path's synchronizations along with their checks.
	code, output, diagnostic := runRemote(t, f.roots["a"], "--node", "c", "--no-flush", "sh", "-c", `cat sub/input && touch "$1"`, "sh", marker)
	if code != 0 || output != "c" || strings.Contains(diagnostic, "b <-> c") {
		t.Fatalf("unsynchronized command not run on the conflicted target: %d %q %s", code, output, diagnostic)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("unsynchronized command didn't run:", err)
	}
	code, output, diagnostic = runRemote(t, f.roots["a"], "--node", "c", "--conflicts")
	if code != 0 || !strings.Contains(output, "Edge b <-> c:") || !strings.Contains(output, "sub/input") || strings.Contains(output, "--session") {
		t.Fatalf("path conflicts not labeled by their actual nodes: %d %q %s", code, output, diagnostic)
	}
	code, _, diagnostic = runRemote(t, f.roots["a"], "--node", "c", "--conflict-apply", "--take", "remote", "sub/input")
	if code != 125 || !strings.Contains(diagnostic, "adjacent node") {
		t.Fatalf("nonadjacent conflict resolution accepted: %d %s", code, diagnostic)
	}
	code, _, diagnostic = runRemote(t, f.roots["b"], "--node", "c", "--conflict-checkout", "sub/input")
	if code != 0 {
		t.Fatalf("node checkout failed: %d %s", code, diagnostic)
	}
	code, _, diagnostic = runRemote(t, f.roots["b"], "--node", "a", "--conflict-discard", "sub/input")
	if code != 125 || !strings.Contains(diagnostic, "different node pair") {
		t.Fatalf("discard ignored selected node: %d %s", code, diagnostic)
	}
	code, _, diagnostic = runRemote(t, f.roots["b"], "--node", "c", "--conflict-discard", "sub/input")
	if code != 0 {
		t.Fatalf("checkout lost after wrong-node discard: %d %s", code, diagnostic)
	}
	code, _, diagnostic = runRemote(t, f.roots["b"], "--node", "c", "--conflict-apply", "--take", "remote", "sub/input")
	if code != 0 {
		t.Fatalf("node-based conflict resolution failed: %d %s", code, diagnostic)
	}
	code, output, diagnostic = runRemote(t, f.roots["a"], "--node", "c", "cat", "sub/input")
	if code != 0 || output != "c" {
		t.Fatalf("resolved content didn't propagate: %d %q %s", code, output, diagnostic)
	}
}

func TestWorkspaceTreeNodeDiscovery(t *testing.T) {
	f := newTreeFixture(t)
	if code, _, diagnostic := runRemote(t, f.roots["a"], "--node", "c", "true"); code != 0 {
		t.Fatal(diagnostic)
	}
	if err := os.WriteFile(filepath.Join(f.roots["a"], "sub", "input"), []byte("not-flushed"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"a", "b", "c"} {
		code, output, diagnostic := runRemote(t, f.roots[from], "--nodes", "--json")
		if code != 0 {
			t.Fatalf("node query failed: %d %s", code, diagnostic)
		}
		var graph struct {
			Current string
			Nodes   []struct {
				Name               string
				Path               []string
				Current, Available bool
				Reason             string
			}
		}
		if err := json.Unmarshal([]byte(output), &graph); err != nil || graph.Current != from || len(graph.Nodes) != 4 {
			t.Fatalf("incomplete graph: %q %v", output, err)
		}
		if strings.Contains(strings.ToLower(output), "session") || strings.Contains(output, "sync_") {
			t.Fatalf("node listing exposed synchronization handles: %s", output)
		}
		for _, node := range graph.Nodes {
			if len(node.Path) == 0 || node.Path[0] != from || node.Path[len(node.Path)-1] != node.Name {
				t.Fatalf("wrong route: %+v", node)
			}
			if node.Name == "d" && (node.Available || !strings.Contains(node.Reason, "a <-> d")) {
				t.Fatalf("unavailable branch not identified: %+v", node)
			}
			if node.Current != (node.Name == from) {
				t.Fatal("incorrect current marker")
			}
		}
	}
	if contents, err := os.ReadFile(filepath.Join(f.roots["b"], "sub", "input")); err != nil || string(contents) != "base" {
		t.Fatalf("--nodes synchronized files: %q %v", contents, err)
	}
	if code, _, diagnostic := runRemote(t, f.roots["a"], "true"); code != 125 || !strings.Contains(diagnostic, "--node") {
		t.Fatalf("unspecified destination guessed: %d %s", code, diagnostic)
	}
}

func TestWorkspaceTreeDiscoveryPinsTopology(t *testing.T) {
	f := newTreeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, source, err := (&remote.Selection{Workspace: true}).Submit(ctx, f.roots["a"], &remote.Submission{Operation: remote.OperationNodes})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := remote.ListNodes(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Terminate(ctx, &selection.Selection{Specifications: []string{f.sessions["tree-d"]}}, ""); err != nil {
		t.Fatal(err)
	}
	connection, _, err = (&remote.Selection{Target: source}).Submit(ctx, f.roots["a"], &remote.Submission{Operation: remote.OperationTreeExecute, Node: "c", Revision: graph.Revision})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.roots["c"], "ignored", "not-run")
	_, err = remote.Execute(ctx, connection, &remote.Request{Arguments: []string{"touch", marker}}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "graph changed") {
		t.Fatalf("obsolete graph accepted: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command ran after graph changed")
	}
}

func TestWorkspaceTreeReturnFailure(t *testing.T) {
	f := newTreeFixture(t)
	code, _, diagnostic := runRemote(t, filepath.Join(f.roots["a"], "sub"), "--node", "c", "sh", "-c",
		`printf local > "$1"; printf remote > contended; printf x >> ../ignored/attempts; exit 7`, "tree-test", filepath.Join(f.roots["a"], "sub", "contended"))
	if code != 125 || !strings.Contains(diagnostic, "command exited with status 7") || !strings.Contains(diagnostic, "synchronization back failed") {
		t.Fatalf("return conflict not reported: %d %s", code, diagnostic)
	}
	if contents, err := os.ReadFile(filepath.Join(f.roots["c"], "ignored", "attempts")); err != nil || string(contents) != "x" {
		t.Fatalf("command retried or not run: %q %v", contents, err)
	}
}

func TestWorkspaceTreeRouteCancellation(t *testing.T) {
	testWorkspaceTreeCancellation(t, false)
}

func TestWorkspaceTreeUnrelatedTopologyChange(t *testing.T) {
	testWorkspaceTreeCancellation(t, true)
}

func testWorkspaceTreeCancellation(t *testing.T, topology bool) {
	t.Helper()
	f := newTreeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, _, err := (&remote.Selection{Workspace: true}).Submit(ctx, f.roots["c"], &remote.Submission{Operation: remote.OperationTreeExecute, Node: "a"})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := remote.Execute(ctx, connection, &remote.Request{Arguments: []string{"sh", "-c", "printf ready > ignored/running; while test ! -f ignored/continue; do sleep 0.02; done"}}, strings.NewReader(""), io.Discard, io.Discard)
		finished <- err
	}()
	for {
		if _, err := os.Stat(filepath.Join(f.roots["a"], "ignored", "running")); err == nil {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("command exited before cancellation: %v", err)
		case <-ctx.Done():
			t.Fatal("command didn't start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Neither mutation closes the submitting b-c channel. Removing the paused
	// branch leaves the entire execution path intact and must not cancel it.
	if topology {
		err = f.manager.Terminate(ctx, &selection.Selection{Specifications: []string{f.sessions["tree-d"]}}, "")
	} else {
		err = f.manager.Pause(ctx, &selection.Selection{Specifications: []string{f.sessions["tree-b"]}}, "")
	}
	if err != nil {
		t.Fatal(err)
	}
	if topology {
		if err := os.WriteFile(filepath.Join(f.roots["a"], "ignored", "continue"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case err := <-finished:
		if topology && err != nil {
			t.Fatalf("unrelated branch change cancelled command: %v", err)
		} else if !topology && err == nil {
			t.Fatal("interrupted tree command reported success")
		}
	case <-ctx.Done():
		t.Fatal("tree command did not finish")
	}
}
