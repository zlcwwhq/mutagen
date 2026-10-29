package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/cmd/remote/cli"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestWorkspaceTreeRoutingPinsSource(t *testing.T) {
	f := newTreeFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source, err := (&remote.Selection{Workspace: true}).Locate(ctx, f.roots["b"])
	if err != nil {
		t.Fatal(err)
	}
	node, edge := "a", f.sessions["tree-b"]
	if source.Session == edge {
		node, edge = "c", f.sessions["tree-c"]
	}
	connection, target, err := (&remote.Selection{Target: source, Resolve: true, Node: node}).Submit(ctx, f.roots["b"], &remote.Submission{Operation: remote.OperationExecute})
	if err != nil {
		t.Fatal(err)
	}
	if target.SourceSession != source.Session || target.SourceAlpha != source.Alpha || target.Session != edge {
		t.Fatalf("destination replaced source registration: source=%+v selected=%+v", source, target)
	}
	if _, err := remote.Fetch(ctx, connection, ""); err != nil {
		t.Fatal(err)
	}
	connection, repeated, err := (&remote.Selection{Target: target}).Dial(ctx, f.roots["b"], remote.OperationExecute)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.SourceSession != source.Session || repeated.Session != edge || repeated.Alpha != target.Alpha {
		t.Fatalf("pinned submission changed identity: %+v", repeated)
	}
	if _, err := remote.Fetch(ctx, connection, ""); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceTreeConflictJSONShape(t *testing.T) {
	f := newTreeFixture(t)
	for node, count := range map[string]int{"b": 1, "c": 2} {
		code, output, diagnostic := runRemote(t, f.roots["a"], "--node", node, "--conflicts", "--json")
		if code != 0 {
			t.Fatalf("conflict listing failed: %d %s", code, diagnostic)
		}
		var graph struct {
			Edges []struct {
				Alpha, Beta string
				Listing     map[string]json.RawMessage
			}
		}
		if err := json.Unmarshal([]byte(output), &graph); err != nil || len(graph.Edges) != count {
			t.Fatalf("wrong path shape: %s %v", output, err)
		}
		for _, edge := range graph.Edges {
			for _, field := range []string{"Conflicts", "Pending", "RemotePending"} {
				if _, ok := edge.Listing[field]; !ok {
					t.Fatalf("edge-dependent schema missing %s: %s", field, output)
				}
			}
		}
	}
}

type slowTreeErrorOutput struct{ bytes.Buffer }

func (w *slowTreeErrorOutput) Write(data []byte) (int, error) {
	time.Sleep(time.Millisecond)
	return w.Buffer.Write(data)
}

// The remote stderr stream can still drain while return synchronization emits
// reports on the control stream. All writes must share one serialization lock.
func TestWorkspaceTreeCommandEventsSerializeStderr(t *testing.T) {
	f := newTreeFixture(t)
	t.Chdir(filepath.Join(f.roots["a"], "sub"))
	var output bytes.Buffer
	var diagnostic slowTreeErrorOutput
	code := cli.Run([]string{"--node", "c", "--progress", "sh", "-c", `printf local > "$1"; printf remote > contended; i=0; while [ "$i" -lt 3000 ]; do printf 'command stderr output line\n' >&2; i=$((i+1)); done`, "test", filepath.Join(f.roots["a"], "sub", "contended")}, strings.NewReader(""), &output, &diagnostic)
	if code != 125 || !strings.Contains(diagnostic.String(), "command stderr output line") || !strings.Contains(diagnostic.String(), "on node a") || !strings.Contains(diagnostic.String(), "synchronization back failed") {
		t.Fatalf("missing command output or edge report: %d %s", code, diagnostic.String())
	}
	if _, err := os.Stat(filepath.Join(f.roots["c"], "sub", "contended")); err != nil {
		t.Fatal(err)
	}
}
