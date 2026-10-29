package synchronization

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

// Selecting another edge of an internal node must retain its lifecycle, even
// though the original submission arrived through a different connected edge.
func TestNodeRedirectCancelsWithSelectedRelay(t *testing.T) {
	data, err := os.MkdirTemp("", "mr-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	edges := []*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c")}
	for _, edge := range edges {
		edge.Version = DefaultVersion
	}
	_, relays := treeTestRegistry(edges, "b", "c")
	source, selected := relays["b"], relays["c"]
	selectedContext, errCancelContext := context.WithCancel(context.Background())
	selected.ctx = selectedContext
	defer errCancelContext()
	selected.flushRequests = make(chan chan error)
	selected.report = func() *remote.Report { return &remote.Report{} }
	channel, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: "b", Tree: "project", Node: "b", PeerNode: "a", Root: t.TempDir(), Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	source.channels[1] = channel
	done := make(chan struct{})
	go func() {
		defer close(done)
		submission, err := channel.Accept()
		if err == nil {
			source.route(context.Background(), submission, 0)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, target, err := (&remote.Selection{Target: &remote.Target{SourceSession: "b"}, Resolve: true, Node: "c"}).Submit(ctx, t.TempDir(), &remote.Submission{Operation: remote.OperationFlush})
	if err != nil {
		t.Fatal(err)
	}
	if target.SourceSession != "b" || target.Session != "c" || target.SourceAlpha || !target.Alpha {
		t.Fatalf("source registration was replaced by selected edge: %+v", target)
	}
	select {
	case <-selected.flushRequests:
	case <-ctx.Done():
		t.Fatal("redirected flush did not start")
	}
	errCancelContext()
	if _, err := remote.Flush(ctx, connection); err == nil {
		t.Fatal("selected relay cancellation was ignored")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("redirected operation retained a stopped relay")
	}
	if ctx.Err() != nil {
		t.Fatal("operation only stopped at its caller deadline")
	}
}

// A route redirected to another edge uses that edge's relay, so the relay must
// track it: stopping the relay waits for the route, and once the relay stops
// admitting routes, a redirect to it is refused as a connection failure rather
// than using its state. The source relay tracks its own routes throughout.
func TestNodeRedirectTrackedBySelectedRelay(t *testing.T) {
	data, err := os.MkdirTemp("", "mr-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	edges := []*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c")}
	for _, edge := range edges {
		edge.Version = DefaultVersion
	}
	_, relays := treeTestRegistry(edges, "b", "c")
	source, selected := relays["b"], relays["c"]
	selected.flushRequests = make(chan chan error)
	selected.report = func() *remote.Report { return &remote.Report{} }
	channel, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: "b", Tree: "project", Node: "b", PeerNode: "a", Root: t.TempDir(), Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	source.channels[1] = channel

	// Accept submissions as the source relay's maintenance does. Neither
	// relay's context is cancelled, so routes only end when they complete.
	if !source.admit() {
		t.Fatal("running relay refused its accept loop")
	}
	go func() {
		defer source.routes.Done()
		source.accept(context.Background(), 1, channel)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	submit := func() (net.Conn, error) {
		connection, _, err := (&remote.Selection{Target: &remote.Target{SourceSession: "b"}, Resolve: true, Node: "c"}).Submit(ctx, t.TempDir(), &remote.Submission{Operation: remote.OperationFlush})
		return connection, err
	}
	connection, err := submit()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	var request chan error
	select {
	case request = <-selected.flushRequests:
	case <-ctx.Done():
		t.Fatal("redirected flush did not start")
	}

	// Stopping the selected relay waits for the redirected route in flight.
	selected.refuseRoutes()
	stopped := make(chan struct{})
	go func() {
		selected.routes.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stopping the selected relay didn't wait for a redirected route")
	case <-time.After(100 * time.Millisecond):
	}

	// A redirect to the stopping relay is refused before its route response.
	if refused, err := submit(); err == nil {
		refused.Close()
		t.Fatal("redirect to a stopping relay was admitted")
	} else if remote.ClassifyError(err) != remote.ErrorKindConnection || !strings.Contains(err.Error(), "stopped synchronizing") {
		t.Fatal("redirect to a stopping relay wasn't refused as a connection failure:", err)
	}

	// The stop completes once the route does.
	request <- nil
	if _, err := remote.Flush(ctx, connection); err != nil {
		t.Fatal("redirected flush failed:", err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("stopping the selected relay outlived its routes")
	}

	// The source relay tracked both submissions and its accept loop, and
	// releases them all once its channel closes.
	channel.Close()
	source.refuseRoutes()
	drained := make(chan struct{})
	go func() {
		source.routes.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal("source relay retained completed routes")
	}
}

// An initial selection without a revision pins the acknowledged snapshot
// before a later execution plan can observe replacement path edges.
func TestNodeResolutionPinsInitialRevision(t *testing.T) {
	_, relays := treeTestRegistry([]*Session{treeTestEdge("a", "b"), treeTestEdge("b", "c")}, "b", "c")
	header := &remote.Submission{Operation: remote.OperationTreeExecute, Node: "c", Resolve: true, Command: true}
	_, _, route, err := relays["b"].resolveSubmission(0, header)
	if err != nil {
		t.Fatal(err)
	}
	if header.Revision == "" || header.Revision != route.Revision {
		t.Fatalf("acknowledged revision was not pinned: header=%+v route=%+v", header, route)
	}
}

// A shared node name in a malformed tree cannot authorize requests from a
// different workspace root on another incident edge.
func TestNodeRedirectRejectsDifferentSourceWorkspace(t *testing.T) {
	first, second := treeTestEdge("a", "b"), treeTestEdge("b", "c")
	first.Version, second.Version = DefaultVersion, DefaultVersion
	first.Beta.Path = "/first-source"
	second.Alpha.Path = "/second-source"
	_, relays := treeTestRegistry([]*Session{first, second}, "b", "c")
	source := relays["b"]
	if _, _, _, err := source.resolveSubmission(1, &remote.Submission{Operation: remote.OperationExecute, Node: "c", Resolve: true}); err == nil || !strings.Contains(err.Error(), "different source workspace") {
		t.Fatalf("different roots authorized by node name: %v", err)
	}
	if _, _, _, err := source.resolveSubmission(1, &remote.Submission{Operation: remote.OperationResolve, Node: "a", Resolve: true}); err != nil {
		t.Fatalf("own edge was blocked by unrelated source identity: %v", err)
	}
}
