package integration

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

// TestWorkspaceTreeDetachedPreflight exercises the strict daemon path directly
// so job startup cannot evade the same conflict checks as foreground execution.
func TestWorkspaceTreeDetachedPreflight(t *testing.T) {
	f := newTreeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if code, _, diagnostic := runRemote(t, f.roots["a"], "--node", "c", "true"); code != 0 {
		t.Fatal(diagnostic)
	}
	for _, node := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(f.roots[node], "sub", "input"), []byte(node), 0600); err != nil {
			t.Fatal(err)
		}
	}
	connection, _, err := (&remote.Selection{Workspace: true}).Submit(ctx, f.roots["a"], &remote.Submission{Operation: remote.OperationTreeExecute, Node: "b", Progress: true})
	if err != nil {
		t.Fatal(err)
	}
	var events []*remote.CommandEvent
	job, _, err := remote.StartJobWithEvents(ctx, connection, &remote.Request{
		Arguments: []string{"sh", "-c", "printf ran > ignored/job"}, Job: &remote.JobRequest{Operation: remote.JobStart},
	}, func(event *remote.CommandEvent) { events = append(events, event) })
	if err == nil || job != nil || !strings.Contains(err.Error(), "command not started") {
		t.Fatalf("conflicted job was not refused: %+v %v", job, err)
	}
	if _, err := os.Stat(filepath.Join(f.roots["b"], "ignored", "job")); !os.IsNotExist(err) {
		t.Fatal("job ran despite conflicts")
	}
	if len(events) == 0 || events[len(events)-1].Report.Empty() || !events[len(events)-1].Done {
		t.Fatal("strict refusal did not carry its structured report")
	}
}

// TestWorkspaceTreeDetachedResult preserves job metadata across daemon
// coordination and leaves synchronization back to the subsequent job wait.
func TestWorkspaceTreeDetachedResult(t *testing.T) {
	f := newTreeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, source, err := (&remote.Selection{Workspace: true}).Submit(ctx, f.roots["a"], &remote.Submission{Operation: remote.OperationTreeExecute, Node: "b"})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := remote.StartJobWithEvents(ctx, connection, &remote.Request{
		Arguments: []string{"sh", "-c", "printf ran > ignored/job; exit 7"}, Job: &remote.JobRequest{Operation: remote.JobStart},
	}, nil)
	if err != nil || job == nil {
		t.Fatalf("strict job start failed: %+v %v", job, err)
	}
	connection, _, err = (&remote.Selection{Target: source}).Submit(ctx, f.roots["a"], &remote.Submission{Operation: remote.OperationExecute})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := remote.WaitJob(ctx, connection, job.ID)
	if err != nil || finished.ExitCode != 7 {
		t.Fatalf("job result was lost: %+v %v", finished, err)
	}
}

// TestWorkspaceTreeEventsKeepCommandStderr verifies that progress uses control
// metadata rather than polluting the remote command's stderr stream.
func TestWorkspaceTreeEventsKeepCommandStderr(t *testing.T) {
	f := newTreeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _, err := (&remote.Selection{Workspace: true}).Submit(ctx, f.roots["a"], &remote.Submission{Operation: remote.OperationTreeExecute, Node: "c", Progress: true})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	var events []*remote.CommandEvent
	code, err := remote.ExecuteWithEvents(ctx, connection, &remote.Request{Arguments: []string{"sh", "-c", "printf command-stderr >&2"}}, strings.NewReader(""), io.Discard, &stderr,
		func(event *remote.CommandEvent) { events = append(events, event) })
	if code != 0 || err != nil || stderr.String() != "command-stderr" {
		t.Fatalf("command stderr changed: %d %q %v", code, stderr.String(), err)
	}
	completions := 0
	for _, event := range events {
		if event.Done {
			completions++
		}
	}
	if completions != 4 {
		t.Fatalf("received %d synchronization completions, want 4", completions)
	}
}
