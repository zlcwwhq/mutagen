package synchronization

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// starEdgeTestManager creates a manager with its own data directory, whose
// sessions are terminated when the test ends. The data directory avoids
// t.TempDir, whose long paths could exceed socket path limits on macOS.
func starEdgeTestManager(t *testing.T) *Manager {
	t.Helper()
	data, err := os.MkdirTemp("", "mtg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	manager, err := NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.Terminate(context.Background(), &selection.Selection{All: true}, "")
		manager.Shutdown()
	})
	return manager
}

// starEdgeTestDirectory creates a short base directory for local workspaces,
// with symbolic links resolved.
func starEdgeTestDirectory(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("", "mse-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	return base
}

// starEdgeTestLocal creates a local synchronization URL.
func starEdgeTestLocal(path string) *url.URL {
	return &url.URL{Kind: url.Kind_Synchronization, Protocol: url.Protocol_Local, Path: path}
}

// starEdgeTestSSH creates an SSH synchronization URL.
func starEdgeTestSSH(host, path string) *url.URL {
	return &url.URL{Kind: url.Kind_Synchronization, Protocol: url.Protocol_SSH, Host: host, Path: path}
}

// starEdgeTestCreate creates an edge of the star named project, as project
// orchestration would, but directly through the manager.
func starEdgeTestCreate(m *Manager, alpha, beta *url.URL, center, node string, paused bool) (string, error) {
	return m.Create(
		context.Background(),
		alpha, beta,
		&Configuration{}, &Configuration{}, &Configuration{},
		"star-"+node,
		map[string]string{selection.ProjectLabelKey: "project"},
		&StarMembership{Identifier: "project", Center: center, Node: node},
		paused,
		"",
	)
}

// starEdgeTestSessions counts the manager's sessions and the sessions saved in
// its data directory, which must agree.
func starEdgeTestSessions(t *testing.T, m *Manager) int {
	t.Helper()
	_, states, err := m.List(context.Background(), &selection.Selection{All: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := pathForSession("")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	} else if len(contents) != len(states) {
		t.Fatalf("%d sessions saved, but %d registered", len(contents), len(states))
	}
	return len(states)
}

// TestStarEdgeCreationLocal verifies that the daemon validates star edges
// created directly through the manager against the star's existing edges,
// including a paused one, before saving any state: edges must agree on the
// center, nodes must have a single edge, and nodes must not overlap each other
// or the center, including through symbolic links. Sessions outside the star
// aren't checked in either direction.
func TestStarEdgeCreationLocal(t *testing.T) {
	m := starEdgeTestManager(t)
	base := starEdgeTestDirectory(t)
	directory := func(name string) string {
		path := filepath.Join(base, name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	center, gpu, server := directory("center"), directory("gpu"), directory("server")
	alias := filepath.Join(base, "alias")
	symlinks := true
	if err := os.Symlink(center, alias); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		symlinks = false
	}

	// Create a paused anchor edge.
	if _, err := starEdgeTestCreate(m, starEdgeTestLocal(center), starEdgeTestLocal(gpu), "center", "gpu", true); err != nil {
		t.Fatal("valid first edge rejected:", err)
	}

	// Inconsistent edges are refused without saving or registering anything.
	refusals := []struct {
		name, problem    string
		alpha, beta      string
		centerName, node string
	}{
		{"center-descendant", "nodes center and server overlap", center, filepath.Join(center, "sub"), "center", "server"},
		{"center-ancestor", "nodes center and server overlap", center, base, "center", "server"},
		{"center-same", "nodes center and server overlap", center, center, "center", "server"},
		{"node-descendant", "nodes gpu and server overlap", center, filepath.Join(gpu, "nested"), "center", "server"},
		{"node-same", "nodes gpu and server overlap", center, gpu, "center", "server"},
		{"center-endpoint", "alpha differs from the endpoint of center node center", directory("other"), server, "center", "server"},
		{"center-name", "star project has center node center, not hub", center, server, "hub", "server"},
		{"duplicate-node", "node gpu of star project already has an edge", center, server, "center", "gpu"},
	}
	if symlinks {
		refusals = append(refusals, struct {
			name, problem    string
			alpha, beta      string
			centerName, node string
		}{"center-symlink", "nodes center and server overlap", center, filepath.Join(alias, "new"), "center", "server"})
	}
	for _, test := range refusals {
		t.Run(test.name, func(t *testing.T) {
			_, err := starEdgeTestCreate(m, starEdgeTestLocal(test.alpha), starEdgeTestLocal(test.beta), test.centerName, test.node, true)
			if err == nil || !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("inconsistent edge not refused with %q: %v", test.problem, err)
			} else if count := starEdgeTestSessions(t, m); count != 1 {
				t.Fatalf("refused edge left state: %d sessions", count)
			}
		})
	}

	// A valid edge is accepted with the paused anchor. Sessions are created
	// paused here, since this package doesn't register endpoint protocols.
	if _, err := starEdgeTestCreate(m, starEdgeTestLocal(center), starEdgeTestLocal(server), "center", "server", true); err != nil {
		t.Fatal("valid second edge rejected:", err)
	} else if _, err := starEdgeTestCreate(m, starEdgeTestLocal(center), starEdgeTestLocal(directory("third")), "center", "server", true); err == nil ||
		!strings.Contains(err.Error(), "node server of star project already has an edge") {
		t.Fatal("duplicate of the second edge accepted:", err)
	}

	// Sessions outside the star may overlap its workspaces, and star edges may
	// overlap them.
	ordinary := filepath.Join(base, "ordinary")
	if _, err := m.Create(
		context.Background(),
		starEdgeTestLocal(filepath.Join(gpu, "data")), starEdgeTestLocal(ordinary),
		&Configuration{}, &Configuration{}, &Configuration{},
		"ordinary", nil, nil, true, "",
	); err != nil {
		t.Fatal("ordinary session overlapping a node rejected:", err)
	}
	if _, err := starEdgeTestCreate(m, starEdgeTestLocal(center), starEdgeTestLocal(filepath.Join(ordinary, "sub")), "center", "extra", true); err != nil {
		t.Fatal("edge overlapping an ordinary session rejected:", err)
	} else if count := starEdgeTestSessions(t, m); count != 4 {
		t.Fatalf("unexpected session count: %d", count)
	}
}

// TestStarEdgeCreationRemote verifies the overlap checks for remote endpoints,
// which compare paths on the same configured host without resolving the remote
// user's home directory.
func TestStarEdgeCreationRemote(t *testing.T) {
	m := starEdgeTestManager(t)
	center := starEdgeTestSSH("center", "~/project")
	if _, err := starEdgeTestCreate(m, center, starEdgeTestSSH("gpu", "~/project"), "center", "gpu", true); err != nil {
		t.Fatal("valid first edge rejected:", err)
	}
	for _, test := range []struct {
		name, problem string
		beta          *url.URL
	}{
		{"center-descendant", "nodes center and server overlap", starEdgeTestSSH("center", "~/project/data")},
		{"center-home", "nodes center and server overlap", starEdgeTestSSH("center", "~")},
		{"center-relative", "nodes center and server overlap", starEdgeTestSSH("center", "project/sub")},
		{"node-descendant", "nodes gpu and server overlap", starEdgeTestSSH("gpu", "~/project/nested")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := starEdgeTestCreate(m, center, test.beta, "center", "server", true); err == nil || !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("overlapping edge not refused with %q: %v", test.problem, err)
			}
		})
	}
	if _, err := starEdgeTestCreate(m, center, starEdgeTestSSH("center", "~/other"), "center", "server", true); err != nil {
		t.Fatal("distinct directory on the center's host rejected:", err)
	} else if count := starEdgeTestSessions(t, m); count != 2 {
		t.Fatalf("unexpected session count: %d", count)
	}
}

// TestStarEdgeReservations verifies that edges being created count as
// existing edges until they're registered, that an edge registered before its
// reservation is released isn't counted twice, that a failed creation frees
// its node, and that a star with a problem refuses new edges.
func TestStarEdgeReservations(t *testing.T) {
	stars := newCommandStars()
	var creations starCreations
	gpu := starTestEdge("gpu")
	releaseGPU, err := creations.reserve(stars, gpu)
	if err != nil {
		t.Fatal("valid edge rejected:", err)
	}

	// A reserved edge counts as an existing edge.
	duplicate := starTestEdge("gpu")
	duplicate.Identifier = "gpu-duplicate"
	duplicate.Beta.Host = "elsewhere"
	nested := starTestEdge("server")
	nested.Beta = &url.URL{Protocol: url.Protocol_SSH, Host: "gpu", Path: "~/project/nested"}
	moved := starTestEdge("server")
	moved.Alpha.Path = "~/elsewhere"
	for _, test := range []struct {
		edge    *Session
		problem string
	}{
		{duplicate, "node gpu of star project already has an edge (session gpu)"},
		{nested, "nodes gpu and server overlap"},
		{moved, "alpha differs from the endpoint of center node center"},
	} {
		if _, err := creations.reserve(stars, test.edge); err == nil || !strings.Contains(err.Error(), test.problem) {
			t.Fatalf("edge conflicting with a reservation not refused with %q: %v", test.problem, err)
		}
	}

	// An edge registered while still reserved is counted once, and remains
	// an existing edge once released.
	stars.update(&controller{session: gpu}, false)
	releaseServer, err := creations.reserve(stars, starTestEdge("server"))
	if err != nil {
		t.Fatal("edge rejected while another is being registered:", err)
	}
	releaseGPU()
	if _, err := creations.reserve(stars, duplicate); err == nil {
		t.Fatal("duplicate of a registered edge accepted")
	}

	// A failed creation frees its node.
	releaseServer()
	retry := starTestEdge("server")
	retry.Identifier = "server-retry"
	if release, err := creations.reserve(stars, retry); err != nil {
		t.Fatal("node of a failed creation still reserved:", err)
	} else {
		release()
	}

	// A star with a problem refuses new edges rather than blaming them.
	disagreeing := starTestEdge("server")
	disagreeing.Alpha.Path = "~/elsewhere"
	stars.update(&controller{session: disagreeing}, false)
	if _, err := creations.reserve(stars, starTestEdge("lab")); err == nil || !strings.Contains(err.Error(), "star project must be repaired before adding node lab") {
		t.Fatal("edge of a star with a problem accepted:", err)
	}

	// Membership is validated before the star, and edges need endpoints.
	invalid := starTestEdge("lab")
	invalid.Labels = nil
	if _, err := creations.reserve(stars, invalid); err == nil || !strings.Contains(err.Error(), "star must belong to its project") {
		t.Fatal("edge outside its project accepted:", err)
	}
	if err := validateStarEdge(&Session{Star: invalid.Star}, nil); err == nil {
		t.Fatal("edge without endpoints accepted")
	}
	if len(creations.pending) != 0 {
		t.Fatal("reservations leaked:", creations.pending)
	}
}

// TestStarEdgeConcurrentCreation verifies that concurrent creations of edges
// of one star can't both pass validation against snapshots that lack each
// other, whether they create the same node or overlapping nodes.
func TestStarEdgeConcurrentCreation(t *testing.T) {
	for _, test := range []struct {
		name string
		node func(int) string
	}{
		{"same-node", func(int) string { return "gpu" }},
		{"overlapping-nodes", func(i int) string { return "node" + string(rune('a'+i)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := starEdgeTestManager(t)
			base := starEdgeTestDirectory(t)
			center, shared := filepath.Join(base, "center"), filepath.Join(base, "shared")
			const creators = 8
			var ready, done sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, creators)
			ready.Add(creators)
			done.Add(creators)
			for i := 0; i < creators; i++ {
				go func(i int) {
					defer done.Done()
					ready.Done()
					<-start
					_, errs[i] = starEdgeTestCreate(m, starEdgeTestLocal(center), starEdgeTestLocal(shared), "center", test.node(i), true)
				}(i)
			}
			ready.Wait()
			close(start)
			done.Wait()
			var created int
			for _, err := range errs {
				if err == nil {
					created++
				} else if !strings.Contains(err.Error(), "already has an edge") && !strings.Contains(err.Error(), "overlap") {
					t.Fatal("unexpected creation failure:", err)
				}
			}
			if created != 1 {
				t.Fatalf("%d concurrent edges created, expected 1", created)
			} else if count := starEdgeTestSessions(t, m); count != 1 {
				t.Fatalf("%d sessions registered, expected 1", count)
			} else if len(m.starCreations.pending) != 0 {
				t.Fatal("reservations leaked:", m.starCreations.pending)
			}
		})
	}
}
