package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/cmd/remote/cli"
	"github.com/mutagen-io/mutagen/pkg/integration/protocols/netpipe"
	"github.com/mutagen-io/mutagen/pkg/project"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// starFixture exercises the real daemon-side routing, CLI, synchronization
// engine, persistent session state, and agent-style command channels for a
// star whose center is local, with the nodes server (two-way-safe), gpu
// (one-way-safe), and offline (paused). Watching is off, so assertions depend
// on the explicit flushes around commands rather than background
// propagation.
type starFixture struct {
	manager  *synchronization.Manager
	base     string
	roots    map[string]string
	sessions map[string]string
}

// newStarFixture creates the star's sessions in a separate manager. Unless
// local is specified, the server and gpu nodes use the in-memory netpipe
// transport, as remote endpoints would use agents.
func newStarFixture(t *testing.T, local ...bool) *starFixture {
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
	data, err := os.MkdirTemp("", "ms-")
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
	f := &starFixture{base: base, roots: make(map[string]string), sessions: make(map[string]string)}
	for _, node := range []string{"local", "server", "gpu", "offline"} {
		f.roots[node] = filepath.Join(base, node)
		if err := os.MkdirAll(filepath.Join(f.roots[node], "ignored"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(f.roots["local"], "sub"), 0700); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(f.roots["local"], "sub", "input"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	prologue := filepath.Join(base, "prologue.sh")
	if err := os.WriteFile(prologue, []byte("export STAR_PROLOGUE=server\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "mutagen.yml")
	config := fmt.Sprintf(`star:
  remote: true
  sync:
    watch: {mode: no-watch}
    ignore: {paths: [ignored]}
  center: local
  nodes:
    local: {endpoint: ./local}
    server: {endpoint: ./server, remotePrologue: %q}
    gpu: {endpoint: ./gpu, mode: one-way-safe}
    offline: {endpoint: ./offline}
`, prologue)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	configuration, err := project.LoadConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	specifications, err := project.NewSessionSpecifications(configuration, nil, "star-test", false)
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
		if len(local) == 0 && (s.Beta.Path == f.roots["server"] || s.Beta.Path == f.roots["gpu"]) {
			s.Beta.Protocol = netpipe.Protocol_Netpipe
		}
		id, err := f.manager.Create(context.Background(), s.Alpha, s.Beta, s.Configuration, s.ConfigurationAlpha, s.ConfigurationBeta, s.Name, s.Labels, s.Star, s.Name == "star-offline", "")
		if err != nil {
			t.Fatal(err)
		}
		f.sessions[s.Name] = id
	}
	f.ready(t)
	return f
}

// available reports whether a node, identified by its workspace's directory,
// reports the expected nodes as available, without changing the current
// directory.
func (f *starFixture) available(directory, from string, expected ...string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := (&remote.Selection{Workspace: true}).Submit(ctx, directory, &remote.Submission{Operation: remote.OperationNodes})
	if err != nil {
		return false
	}
	graph, err := remote.ListNodes(ctx, connection)
	if err != nil || graph.Source != from {
		return false
	}
	for _, name := range expected {
		found := false
		for _, node := range graph.Nodes {
			found = found || node.Name == name && node.Available
		}
		if !found {
			return false
		}
	}
	return true
}

// ready waits until the center reports the connected nodes as available and
// each connected node reports the center as available.
func (f *starFixture) ready(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !f.available(f.roots["local"], "local", "server", "gpu") || !f.available(f.roots["server"], "server", "local") || !f.available(f.roots["gpu"], "gpu", "local") {
		if time.Now().After(deadline) {
			t.Fatal("star command channels did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// starListing is the JSON node listing of remote --nodes --json.
type starListing struct {
	Current string `json:"current"`
	Star    bool   `json:"star"`
	Problem string `json:"problem"`
	Nodes   []struct {
		Name      string   `json:"name"`
		Center    bool     `json:"center"`
		Host      string   `json:"host"`
		Root      string   `json:"root"`
		Path      []string `json:"path"`
		Mode      string   `json:"mode"`
		Current   bool     `json:"current"`
		Available bool     `json:"available"`
		Reason    string   `json:"reason"`
	} `json:"nodes"`
}

// listStar runs remote --nodes --json from a directory.
func listStar(t *testing.T, directory string) (starListing, string) {
	t.Helper()
	code, output, diagnostic := runRemote(t, directory, "--nodes", "--json")
	if code != 0 {
		t.Fatalf("node query failed: %d %s", code, diagnostic)
	}
	var listing starListing
	if err := json.Unmarshal([]byte(output), &listing); err != nil {
		t.Fatalf("invalid node listing: %q %v", output, err)
	}
	return listing, output
}

func TestStarCommands(t *testing.T) {
	f := newStarFixture(t)

	// The center reaches every node over the node's own edge, which is
	// synchronized before and after the command as a pair would be.
	code, output, diagnostic := runRemote(t, filepath.Join(f.roots["local"], "sub"), "--node", "server", "-e", "MUTAGEN_REMOTE_NODE=spoof", "sh", "-c",
		`cat input; printf ':%s:%s:' "$MUTAGEN_REMOTE_NODE" "$STAR_PROLOGUE"; pwd -P; printf result > result`)
	if code != 0 || output != "base:server:server:"+filepath.Join(f.roots["server"], "sub")+"\n" {
		t.Fatalf("command on node failed: %d %q %s", code, output, diagnostic)
	}
	if result, err := os.ReadFile(filepath.Join(f.roots["local"], "sub", "result")); err != nil || string(result) != "result" {
		t.Fatalf("result didn't return to the center: %q %v", result, err)
	}

	// A one-way edge flows from the center to its node only.
	code, output, diagnostic = runRemote(t, filepath.Join(f.roots["local"], "sub"), "--node", "gpu", "sh", "-c", `cat input; printf gpu > gpu-output`)
	if code != 0 || output != "base" {
		t.Fatalf("command on one-way node failed: %d %q %s", code, output, diagnostic)
	}
	if _, err := os.Stat(filepath.Join(f.roots["local"], "sub", "gpu-output")); !os.IsNotExist(err) {
		t.Fatal("one-way node's change reached the center:", err)
	}

	// Other nodes reach the center, which they don't need to name.
	for _, node := range []string{"server", "gpu"} {
		code, output, diagnostic = runRemote(t, filepath.Join(f.roots[node], "sub"), "sh", "-c", `printf '%s:' "$MUTAGEN_REMOTE_NODE"; pwd -P`)
		if code != 0 || output != "local:"+filepath.Join(f.roots["local"], "sub")+"\n" {
			t.Fatalf("command on the center from %s failed: %d %q %s", node, code, output, diagnostic)
		}
	}

	// Stars have no multi-hop routes.
	code, _, diagnostic = runRemote(t, f.roots["server"], "--node", "gpu", "true")
	if code != 125 || !strings.Contains(diagnostic, "no multi-hop") || !strings.Contains(diagnostic, "center node local") {
		t.Fatalf("node-to-node command not refused: %d %s", code, diagnostic)
	}

	// The center must name its destination, which must exist and be
	// connected.
	code, _, diagnostic = runRemote(t, f.roots["local"], "true")
	if code != 125 || !strings.Contains(diagnostic, "--node") {
		t.Fatalf("unspecified destination guessed: %d %s", code, diagnostic)
	}
	code, _, diagnostic = runRemote(t, f.roots["local"], "--node", "missing", "true")
	if code != 125 || !strings.Contains(diagnostic, "unknown node") {
		t.Fatalf("unknown node not refused: %d %s", code, diagnostic)
	}
	code, _, diagnostic = runRemote(t, f.roots["local"], "--node", "offline", "true")
	if code != 255 || !strings.Contains(diagnostic, "node offline is unavailable") {
		t.Fatalf("paused node not refused: %d %s", code, diagnostic)
	}

	// Detached jobs use the edge as for a pair.
	code, output, diagnostic = runRemote(t, f.roots["local"], "--node", "server", "--detach", "sh", "-c", `printf job > job-output`)
	if code != 0 {
		t.Fatalf("detached job not started: %d %s", code, diagnostic)
	}
	job := strings.TrimSpace(output)
	if code, _, diagnostic = runRemote(t, f.roots["local"], "--node", "server", "--wait", job); code != 0 {
		t.Fatalf("detached job wait failed: %d %s", code, diagnostic)
	}
	if contents, err := os.ReadFile(filepath.Join(f.roots["local"], "job-output")); err != nil || string(contents) != "job" {
		t.Fatalf("job output didn't return after waiting: %q %v", contents, err)
	}
	code, output, diagnostic = runRemote(t, f.roots["local"], "--node", "server", "--jobs")
	if code != 0 || !strings.Contains(output, job) {
		t.Fatalf("job not listed: %d %q %s", code, output, diagnostic)
	}
	if code, output, _ = runRemote(t, f.roots["local"], "--node", "gpu", "--jobs"); code != 0 || strings.Contains(output, job) {
		t.Fatalf("job listed on another node: %d %q", code, output)
	}

	// --no-flush skips synchronization as for a pair.
	if err := os.WriteFile(filepath.Join(f.roots["local"], "sub", "input"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	code, output, diagnostic = runRemote(t, filepath.Join(f.roots["local"], "sub"), "--node", "server", "--no-flush", "cat", "input")
	if code != 0 || output != "base" {
		t.Fatalf("--no-flush synchronized: %d %q %s", code, output, diagnostic)
	}
}

func TestStarNodeDiscovery(t *testing.T) {
	f := newStarFixture(t)
	if code, _, diagnostic := runRemote(t, f.roots["local"], "--node", "server", "true"); code != 0 {
		t.Fatal(diagnostic)
	}
	if err := os.WriteFile(filepath.Join(f.roots["local"], "sub", "input"), []byte("not-flushed"), 0600); err != nil {
		t.Fatal(err)
	}

	// The center lists every node, over its own edge.
	listing, output := listStar(t, filepath.Join(f.roots["local"], "sub"))
	if listing.Current != "local" || !listing.Star || listing.Problem != "" || len(listing.Nodes) != 4 {
		t.Fatalf("incomplete listing from the center: %s", output)
	}
	if strings.Contains(strings.ToLower(output), "session") || strings.Contains(output, "sync_") || strings.Contains(output, "parent") || strings.Contains(output, "adjacent") {
		t.Fatalf("node listing exposed synchronization handles or tree fields: %s", output)
	}
	modes := map[string]string{"gpu": "one-way-safe", "local": "", "offline": "two-way-safe", "server": "two-way-safe"}
	for _, node := range listing.Nodes {
		if node.Mode != modes[node.Name] || node.Center != (node.Name == "local") || node.Current != (node.Name == "local") {
			t.Fatalf("node misreported: %+v", node)
		} else if node.Name != "local" && strings.Join(node.Path, ",") != "local,"+node.Name {
			t.Fatalf("node not reached over a single edge: %+v", node)
		} else if node.Available != (node.Name != "offline") {
			t.Fatalf("incorrect availability: %+v", node)
		} else if node.Name == "server" && node.Root != f.roots["server"] {
			t.Fatalf("incorrect root: %+v", node)
		}
	}

	// Another node lists itself and the center.
	listing, output = listStar(t, f.roots["gpu"])
	if listing.Current != "gpu" || len(listing.Nodes) != 2 || listing.Nodes[0].Name != "gpu" || listing.Nodes[1].Name != "local" {
		t.Fatalf("incorrect listing from a node: %s", output)
	} else if center := listing.Nodes[1]; !center.Center || !center.Available || center.Mode != "one-way-safe" || strings.Join(center.Path, ",") != "gpu,local" {
		t.Fatalf("center misreported from a node: %s", output)
	}
	code, output, diagnostic := runRemote(t, f.roots["gpu"], "--nodes")
	if code != 0 || !strings.Contains(output, "Current node: gpu") || !strings.Contains(output, "local: center; available;") {
		t.Fatalf("incorrect text listing: %d %q %s", code, output, diagnostic)
	}
	if contents, err := os.ReadFile(filepath.Join(f.roots["server"], "sub", "input")); err != nil || string(contents) != "base" {
		t.Fatalf("--nodes synchronized files: %q %v", contents, err)
	}
}

func TestStarConflicts(t *testing.T) {
	f := newStarFixture(t)
	if code, _, diagnostic := runRemote(t, f.roots["local"], "--node", "server", "true"); code != 0 {
		t.Fatal(diagnostic)
	}
	for _, node := range []string{"local", "server"} {
		if err := os.WriteFile(filepath.Join(f.roots[node], "sub", "input"), []byte(node), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// Conflicts warn, as for a pair, rather than preventing the command.
	code, output, diagnostic := runRemote(t, f.roots["local"], "--node", "server", "cat", "sub/input")
	if code != 0 || output != "server" || !strings.Contains(diagnostic, "conflicts") || !strings.Contains(diagnostic, "sub/input") {
		t.Fatalf("conflicted command not run with a warning: %d %q %s", code, output, diagnostic)
	}

	// Listings keep the edges array, with a single edge.
	code, output, diagnostic = runRemote(t, f.roots["local"], "--node", "server", "--conflicts", "--json")
	if code != 0 {
		t.Fatalf("conflict listing failed: %d %s", code, diagnostic)
	}
	var listing struct {
		Edges []struct {
			Alpha, Beta, Source, Target string
			Listing                     struct {
				Conflicts []struct{ Path string }
			}
		}
	}
	if err := json.Unmarshal([]byte(output), &listing); err != nil || len(listing.Edges) != 1 {
		t.Fatalf("incorrect conflict listing: %q %v", output, err)
	} else if edge := listing.Edges[0]; edge.Alpha != "local" || edge.Beta != "server" || edge.Source != "local" || edge.Target != "server" ||
		len(edge.Listing.Conflicts) != 1 || edge.Listing.Conflicts[0].Path != "sub/input" {
		t.Fatalf("incorrect conflict edge: %+v", edge)
	}
	code, output, diagnostic = runRemote(t, f.roots["server"], "--conflicts", "--json")
	if err := json.Unmarshal([]byte(output), &listing); code != 0 || err != nil || len(listing.Edges) != 1 {
		t.Fatalf("incorrect conflict listing from a node: %d %q %s", code, output, diagnostic)
	} else if edge := listing.Edges[0]; edge.Alpha != "local" || edge.Beta != "server" || edge.Source != "server" || edge.Target != "local" {
		t.Fatalf("incorrect conflict edge from a node: %+v", edge)
	}

	// Conflicts are resolved over the edge, from either of its nodes.
	if code, _, diagnostic = runRemote(t, f.roots["local"], "--node", "server", "--conflict-apply", "--take", "remote", "sub/input"); code != 0 {
		t.Fatalf("conflict resolution failed: %d %s", code, diagnostic)
	}
	if contents, err := os.ReadFile(filepath.Join(f.roots["local"], "sub", "input")); err != nil || string(contents) != "server" {
		t.Fatalf("resolution didn't apply: %q %v", contents, err)
	}
	code, output, diagnostic = runRemote(t, f.roots["local"], "--node", "server", "--conflicts")
	if code != 0 || strings.TrimSpace(output) != "No conflicts." {
		t.Fatalf("conflict remained: %d %q %s", code, output, diagnostic)
	}
}

func TestStarRestart(t *testing.T) {
	// Use real local URLs here: the in-memory test transport intentionally
	// isn't accepted by validation when persisted sessions are loaded.
	f := newStarFixture(t, true)
	f.manager.Shutdown()
	var err error
	f.manager, err = synchronization.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.ready(t)
	_, states, err := f.manager.List(context.Background(), &selection.Selection{All: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if star := state.Session.Star; star == nil || star.Identifier != "star-test" || star.Center != "local" || "star-"+star.Node != state.Session.Name {
			t.Fatalf("membership not persisted: %v", star)
		}
	}
	code, output, diagnostic := runRemote(t, f.roots["server"], "sh", "-c", `printf '%s' "$MUTAGEN_REMOTE_NODE"`)
	if code != 0 || output != "local" {
		t.Fatalf("routing after restart failed: %d %q %s", code, output, diagnostic)
	}
	if listing, output := listStar(t, f.roots["local"]); len(listing.Nodes) != 4 || listing.Problem != "" {
		t.Fatalf("paused node lost after restart: %s", output)
	}
}

// TestStarApplyEquivalentCenter verifies that adding a node while the center
// is reached through an alternate local path keeps the retained center's exact
// URL. Otherwise apply succeeds but the edges disagree on the center, which
// refuses operations from it, and a repeated apply reports every session
// unchanged instead of repairing it.
func TestStarApplyEquivalentCenter(t *testing.T) {
	f := newStarFixture(t, true)
	alias := filepath.Join(f.base, "alias")
	if err := os.Symlink(f.base, alias); err != nil {
		t.Fatal(err)
	}
	configuration, err := project.LoadConfiguration(filepath.Join(f.base, "mutagen.yml"))
	if err != nil {
		t.Fatal(err)
	}
	configuration.Star.Nodes["local"] = project.StarNode{Endpoint: filepath.Join(alias, "local")}
	configuration.Star.Nodes["extra"] = project.StarNode{Endpoint: filepath.Join(f.base, "extra")}
	specifications, err := project.NewSessionSpecifications(configuration, nil, "star-test", false)
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
	if description.String() != "Create synchronization session \"star-extra\"\nUnchanged sessions: 3\n" {
		t.Fatalf("equivalent paths replaced histories: %s", description.String())
	}
	previous := synchronizationManager
	synchronizationManager = f.manager
	defer func() { synchronizationManager = previous }()
	if err := plan.Apply(&managerSessionOperations{ctx}); err != nil {
		t.Fatal(err)
	}
	_, states, err = f.manager.List(ctx, &selection.Selection{All: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var center *url.URL
	for _, state := range states {
		if center == nil {
			center = state.Session.Alpha
		} else if !proto.Equal(center, state.Session.Alpha) {
			t.Fatal("apply produced edges that disagree on the center")
		}
		if id := f.sessions[state.Session.Name]; id != "" && state.Session.Identifier != id {
			t.Fatal("retained edge lost its synchronization history")
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		listing, output := listStar(t, f.roots["local"])
		if listing.Problem != "" {
			t.Fatal("apply produced a star problem:", output)
		}
		available := false
		for _, node := range listing.Nodes {
			available = available || node.Name == "extra" && node.Available
		}
		if available {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("new node didn't connect:", output)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, output, diagnostic := runRemote(t, f.roots["local"], "--node", "extra", "sh", "-c", `printf '%s' "$MUTAGEN_REMOTE_NODE"`); code != 0 || output != "extra" {
		t.Fatalf("command on new node failed: %d %q %s", code, output, diagnostic)
	}
	repeated, err := project.NewPlan(specifications, nil, states)
	if err != nil {
		t.Fatal(err)
	}
	description.Reset()
	repeated.Print(&description)
	if strings.TrimSpace(description.String()) != "Unchanged sessions: 4" {
		t.Fatal("reapply changed the reconciled star:", description.String())
	}
}

// TestStarIndependentEdges verifies that a running command on one node doesn't
// depend on any other edge: it enters through its own edge at the center, even
// if another edge registered the center more recently, and the edge alone is
// pinned for its later submissions, so that pausing, terminating, or adding
// other nodes while it runs doesn't affect it or its post-command flush.
func TestStarIndependentEdges(t *testing.T) {
	f := newStarFixture(t)
	ctx := context.Background()
	_, states, err := f.manager.List(ctx, &selection.Selection{Specifications: []string{f.sessions["star-server"]}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	template := states[0].Session
	edge := func(node string) *selection.Selection {
		return &selection.Selection{Specifications: []string{f.sessions["star-"+node]}}
	}
	t.Chdir(f.roots["local"])
	for i, step := range []struct {
		node, other string
		terminate   bool
	}{{"gpu", "server", false}, {"server", "gpu", true}} {
		// Reconnect the other edge first, so that its registration of the
		// center is the most recent, which a submission must not prefer.
		if err := f.manager.Pause(ctx, edge(step.other), ""); err != nil {
			t.Fatal(err)
		} else if err := f.manager.Resume(ctx, edge(step.other), ""); err != nil {
			t.Fatal(err)
		}
		f.ready(t)

		// Start a command on the node that runs until it's released. The
		// server's output is synchronized back, which its post-command flush
		// must still do.
		script := `: > ignored/started; while [ ! -e ignored/release ]; do sleep 0.05; done; printf done`
		if step.node == "server" {
			script += `; printf done > result`
		}
		type result struct {
			code               int
			output, diagnostic string
		}
		done := make(chan result, 1)
		go func() {
			var output, errorOutput strings.Builder
			code := cli.Run([]string{"--node", step.node, "--timeout", "60", "sh", "-c", script}, strings.NewReader(""), &output, &errorOutput)
			done <- result{code, output.String(), errorOutput.String()}
		}()
		deadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(f.roots[step.node], "ignored", "started")); err == nil {
				break
			}
			select {
			case r := <-done:
				t.Fatalf("command on %s ended early: %d %q %s", step.node, r.code, r.output, r.diagnostic)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("command on %s didn't start", step.node)
			}
			time.Sleep(10 * time.Millisecond)
		}

		// Pause or terminate the other node's edge, and add a node.
		if step.terminate {
			err = f.manager.Terminate(ctx, edge(step.other), "")
		} else {
			err = f.manager.Pause(ctx, edge(step.other), "")
		}
		if err != nil {
			t.Fatal(err)
		}
		star := proto.Clone(template.Star).(*synchronization.StarMembership)
		star.Node = fmt.Sprintf("extra%d", i)
		extra := filepath.Join(f.base, star.Node)
		if err := os.Mkdir(extra, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := f.manager.Create(ctx, template.Alpha, &url.URL{Kind: url.Kind_Synchronization, Path: extra},
			template.Configuration, template.ConfigurationAlpha, template.ConfigurationBeta, "star-"+star.Node, template.Labels, star, false, ""); err != nil {
			t.Fatal(err)
		}
		deadline = time.Now().Add(15 * time.Second)
		for !f.available(f.roots["local"], "local", star.Node) {
			if time.Now().After(deadline) {
				t.Fatal("added node didn't connect")
			}
			time.Sleep(10 * time.Millisecond)
		}

		// Release the command, which must complete and synchronize back.
		if err := os.WriteFile(filepath.Join(f.roots[step.node], "ignored", "release"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-done:
			if r.code != 0 || r.output != "done" || r.diagnostic != "" {
				t.Fatalf("command on %s affected by changes to other edges: %d %q %s", step.node, r.code, r.output, r.diagnostic)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("command on %s didn't complete", step.node)
		}
		if step.node == "server" {
			if contents, err := os.ReadFile(filepath.Join(f.roots["local"], "result")); err != nil || string(contents) != "done" {
				t.Fatalf("post-command flush didn't synchronize the result: %q %v", contents, err)
			}
		} else if err := f.manager.Resume(ctx, edge(step.other), ""); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStarProblem verifies that edges disagreeing on the center's endpoint are
// reported by --nodes and refuse operations from the center with 125, while
// other nodes keep using their own edges, and that a terminated edge only
// removes its node.
func TestStarProblem(t *testing.T) {
	f := newStarFixture(t, true)
	other := filepath.Join(f.base, "other-center")
	extra := filepath.Join(f.base, "extra")
	_, states, err := f.manager.List(context.Background(), &selection.Selection{Specifications: []string{f.sessions["star-server"]}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	template := states[0].Session
	star := proto.Clone(template.Star).(*synchronization.StarMembership)
	star.Node = "extra"
	id, err := f.manager.Create(context.Background(),
		&url.URL{Kind: url.Kind_Synchronization, Path: other}, &url.URL{Kind: url.Kind_Synchronization, Path: extra},
		template.Configuration, template.ConfigurationAlpha, template.ConfigurationBeta, "star-extra", template.Labels, star, false, "")
	if err != nil {
		t.Fatal(err)
	}
	listing, output := listStar(t, f.roots["local"])
	if !strings.Contains(listing.Problem, "disagree on the endpoint of center node local") {
		t.Fatalf("star problem not reported: %s", output)
	}
	code, _, diagnostic := runRemote(t, f.roots["local"], "--node", "server", "true")
	if code != 125 || !strings.Contains(diagnostic, "until the star is repaired") {
		t.Fatalf("operation from the center not refused: %d %s", code, diagnostic)
	}
	code, output, diagnostic = runRemote(t, f.roots["server"], "sh", "-c", `printf '%s' "$MUTAGEN_REMOTE_NODE"`)
	if code != 0 || output != "local" {
		t.Fatalf("node's own edge refused: %d %q %s", code, output, diagnostic)
	}
	code, output, _ = runRemote(t, f.roots["server"], "--nodes")
	if code != 0 || !strings.Contains(output, "Star problem:") {
		t.Fatalf("star problem not reported from a node: %d %q", code, output)
	}

	// Terminating the inconsistent edge repairs the star, and terminating a
	// node's edge only removes that node.
	ctx := context.Background()
	if err := f.manager.Terminate(ctx, &selection.Selection{Specifications: []string{id, f.sessions["star-gpu"]}}, ""); err != nil {
		t.Fatal(err)
	}
	listing, output = listStar(t, f.roots["local"])
	if listing.Problem != "" || len(listing.Nodes) != 3 {
		t.Fatalf("terminated edges still affect the star: %s", output)
	}
	if code, _, diagnostic := runRemote(t, f.roots["local"], "--node", "server", "true"); code != 0 {
		t.Fatalf("repaired star refused a command: %d %s", code, diagnostic)
	}
}
