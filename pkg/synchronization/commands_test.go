package synchronization

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/state"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
)

func TestCommandRelayFlush(t *testing.T) {
	// Flushes complete once the synchronization loop responds.
	relay := &commandRelay{flushRequests: make(chan chan error, 1)}
	go func() { (<-relay.flushRequests) <- nil }()
	if err := relay.flush(context.Background()); err != nil {
		t.Fatal("flush failed:", err)
	}

	// Flushes fail once synchronization stops, rather than waiting for a
	// response that won't arrive, both after their request is queued and while
	// waiting to queue it (behind the request left by the first case).
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if err := relay.flush(ctx); err != errFlushTerminated {
		t.Fatal("queued flush not terminated:", err)
	} else if err := relay.flush(ctx); err != errFlushTerminated {
		t.Fatal("unqueued flush not terminated:", err)
	}
}

func TestRemoteReport(t *testing.T) {
	// Create a state with more conflicts and problems than are reported, in
	// reverse order.
	file := &core.Entry{Kind: core.EntryKind_File}
	directory := &core.Entry{Kind: core.EntryKind_Directory}
	var conflicts []*core.Conflict
	for i := maximumListConflicts; i >= 0; i-- {
		path := fmt.Sprintf("conflict%02d", i)
		conflicts = append(conflicts, &core.Conflict{
			Root:         path,
			AlphaChanges: []*core.Change{{Path: path, Old: directory}},
			BetaChanges: []*core.Change{{
				Path: path + "/file",
				Old:  file,
				New:  &core.Entry{Kind: core.EntryKind_File, Executable: true},
			}},
		})
	}
	var scanProblems []*core.Problem
	for i := maximumListScanProblems; i >= 0; i-- {
		scanProblems = append(scanProblems, &core.Problem{Path: fmt.Sprintf("scan%02d", i), Error: "unreadable"})
	}
	c := &controller{
		stateLock: state.NewTrackingLock(state.NewTracker()),
		state: &State{
			Conflicts:  conflicts,
			AlphaState: &EndpointState{ScanProblems: scanProblems},
			BetaState:  &EndpointState{TransitionProblems: []*core.Problem{{Path: "transition", Error: "denied"}}},
		},
	}

	// Lists are sorted, truncated, and converted without modifying the state.
	report := c.remoteReport()
	if len(report.Conflicts) != maximumListConflicts || report.ExcludedConflicts != 1 {
		t.Fatalf("conflicts truncated incorrectly: %d, %d", len(report.Conflicts), report.ExcludedConflicts)
	} else if expected := (remote.Conflict{
		Root:  "conflict00",
		Alpha: []remote.Change{{Path: "conflict00", Old: "directory"}},
		Beta:  []remote.Change{{Path: "conflict00/file", Old: "file", New: "executable file"}},
	}); !reflect.DeepEqual(report.Conflicts[0], expected) {
		t.Fatalf("conflict converted incorrectly: %+v", report.Conflicts[0])
	} else if len(report.Problems) != maximumListScanProblems+1 || report.ExcludedProblems != 1 {
		t.Fatalf("problems truncated incorrectly: %d, %d", len(report.Problems), report.ExcludedProblems)
	} else if first, last := report.Problems[0], report.Problems[maximumListScanProblems]; !reflect.DeepEqual(first, remote.Problem{Alpha: true, Path: "scan00", Error: "unreadable"}) ||
		!reflect.DeepEqual(last, remote.Problem{Path: "transition", Error: "denied"}) {
		t.Fatalf("problems converted incorrectly: %+v, %+v", first, last)
	} else if c.state.Conflicts[0].Root != fmt.Sprintf("conflict%02d", maximumListConflicts) || c.state.AlphaState.ScanProblems[0].Path != fmt.Sprintf("scan%02d", maximumListScanProblems) {
		t.Fatal("state modified")
	}

	// Resolution listings use the same actual changes as warnings, including
	// directory descendants and type changes, without deriving history from
	// the current copies' types.
	listed := resolution(conflicts[len(conflicts)-1], "sha1")
	if !reflect.DeepEqual(listed.Changes(), report.Conflicts[0]) {
		t.Fatalf("listing and warning disagree: %+v, %+v", listed.Changes(), report.Conflicts[0])
	}

	// Sessions without conflicts or problems produce empty reports.
	c.state = &State{AlphaState: &EndpointState{}, BetaState: &EndpointState{}}
	if report := c.remoteReport(); !report.Empty() {
		t.Fatalf("non-empty report: %+v", report)
	}
}

func TestRemoteFileConflictDigests(t *testing.T) {
	// Conflicts between two files report both copies' digests, with their
	// common ancestor's digest if they have one, so that checkouts can verify
	// that they download the listed copies. Other conflicts report none.
	file := func(digest byte) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_File, Digest: []byte{digest}}
	}
	link := &core.Entry{Kind: core.EntryKind_SymbolicLink, Target: "target"}
	wrap := func(entry *core.Entry) *core.Entry {
		if entry == nil {
			return &core.Entry{Kind: core.EntryKind_Directory}
		}
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"conflict": entry}}
	}
	for _, mode := range []core.SynchronizationMode{
		core.SynchronizationMode_SynchronizationModeTwoWaySafe,
		core.SynchronizationMode_SynchronizationModeOneWaySafe,
	} {
		for _, test := range []struct {
			ancestor, alpha, beta *core.Entry
			expected              remote.ConflictRoot
		}{
			{file(1), file(2), file(3), remote.ConflictRoot{AlphaDigest: "02", BetaDigest: "03", BaseDigest: "01", Algorithm: "sha1"}},
			{nil, file(2), file(3), remote.ConflictRoot{AlphaDigest: "02", BetaDigest: "03", Algorithm: "sha1"}},
			{file(1), link, file(3), remote.ConflictRoot{}},
		} {
			_, _, _, conflicts := core.Reconcile(wrap(test.ancestor), wrap(test.alpha), wrap(test.beta), mode)
			if len(conflicts) != 1 {
				t.Fatalf("unexpected conflicts: %+v", conflicts)
			}
			root := resolution(conflicts[0], "sha1")
			if root.AlphaDigest != test.expected.AlphaDigest || root.BetaDigest != test.expected.BetaDigest ||
				root.BaseDigest != test.expected.BaseDigest || root.Algorithm != test.expected.Algorithm {
				t.Fatalf("mode %d: unexpected digests for %+v: %+v", mode, conflicts[0], root)
			}
		}
	}
}

func TestRemoteTakableCopies(t *testing.T) {
	// Either copy of a conflict other than between two files can be taken in
	// both safe modes, since it's written to the other endpoint, unless it
	// can't be synchronized.
	file := func(digest byte) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_File, Digest: []byte{digest}}
	}
	link := &core.Entry{Kind: core.EntryKind_SymbolicLink, Target: "target"}
	untracked := &core.Entry{Kind: core.EntryKind_Untracked}
	wrap := func(entry *core.Entry) *core.Entry {
		if entry == nil {
			return &core.Entry{Kind: core.EntryKind_Directory}
		}
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"conflict": entry}}
	}
	for _, mode := range []core.SynchronizationMode{
		core.SynchronizationMode_SynchronizationModeTwoWaySafe,
		core.SynchronizationMode_SynchronizationModeOneWaySafe,
	} {
		for _, test := range []struct {
			ancestor, alpha, beta     *core.Entry
			alphaTakable, betaTakable bool
		}{
			{file(1), link, file(3), true, true},
			{file(1), file(3), link, true, true},
			{nil, file(2), untracked, true, false},
		} {
			_, _, _, conflicts := core.Reconcile(wrap(test.ancestor), wrap(test.alpha), wrap(test.beta), mode)
			if len(conflicts) != 1 {
				t.Fatalf("mode %d: unexpected conflicts: %+v", mode, conflicts)
			} else if root := resolution(conflicts[0], "sha1"); root.Files ||
				root.AlphaTakable != test.alphaTakable || root.BetaTakable != test.betaTakable {
				t.Fatalf("mode %d: unexpected resolution of %+v: %+v", mode, conflicts[0], root)
			}
		}
	}
}

func TestRemoteDirectoryConflicts(t *testing.T) {
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	changed := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{2}}
	directory := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"child": file}}
	modified := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"child": changed}}
	wrap := func(entry *core.Entry) *core.Entry {
		if entry == nil {
			return &core.Entry{Kind: core.EntryKind_Directory}
		}
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"conflict": entry}}
	}
	for _, mode := range []core.SynchronizationMode{
		core.SynchronizationMode_SynchronizationModeTwoWaySafe,
		core.SynchronizationMode_SynchronizationModeOneWaySafe,
	} {
		for _, modifiedDirectory := range []bool{false, true} {
			for _, directoryOnAlpha := range []bool{false, true} {
				t.Run(fmt.Sprintf("mode=%d/modified=%t/alpha=%t", mode, modifiedDirectory, directoryOnAlpha), func(t *testing.T) {
					ancestor, alpha, beta := (*core.Entry)(nil), directory, file
					if modifiedDirectory {
						ancestor, alpha = directory, modified
					}
					if !directoryOnAlpha {
						alpha, beta = beta, alpha
					}
					_, _, _, conflicts := core.Reconcile(wrap(ancestor), wrap(alpha), wrap(beta), mode)
					if len(conflicts) != 1 {
						t.Fatalf("unexpected conflicts: %+v", conflicts)
					} else if root := resolution(conflicts[0], "sha1"); root.Path != "conflict" || root.Resolvable() {
						t.Fatalf("directory conflict reported as resolvable: %+v", root)
					}
				})
			}
		}
	}
}

func TestCommandExecutionLocation(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		protocol                 urlpkg.Protocol
		host, path, expectedHost string
	}{
		{urlpkg.Protocol_Local, "", "/local/project", hostname},
		{urlpkg.Protocol_SSH, "ssh-alias", "project", "ssh-alias"},
		{urlpkg.Protocol_SSH, "ssh-alias", "~other/project", "ssh-alias"},
		{urlpkg.Protocol_Docker, "container", "/root/project", "container"},
	} {
		url := &urlpkg.URL{Protocol: test.protocol, Host: test.host, Path: test.path,
			User: "private-user", Environment: map[string]string{"SECRET": "private"},
			Parameters: map[string]string{"secret": "private"}}
		host, root := commandExecutionLocation(url)
		if host != test.expectedHost || root != test.path {
			t.Fatalf("incorrect execution metadata: %q, %q", host, root)
		}
	}
}

// relayTestChannel is a command channel through which a test submits commands
// to a relay, and whose executor sends output to each route that the relay
// opens at once, without reading its input. Closing the channel closes its
// routes, as closing an agent's channel does.
type relayTestChannel struct {
	// submissions delivers submissions to Accept.
	submissions chan io.ReadWriteCloser
	// done is closed when the channel is closed.
	done chan struct{}
	// lock guards closed and executors.
	lock sync.Mutex
	// closed indicates whether the channel is closed.
	closed bool
	// executors holds the executor's ends of the opened routes.
	executors []net.Conn
}

// newRelayTestChannel creates a connected test channel.
func newRelayTestChannel() *relayTestChannel {
	return &relayTestChannel{submissions: make(chan io.ReadWriteCloser), done: make(chan struct{})}
}

func (c *relayTestChannel) Accept() (io.ReadWriteCloser, error) {
	select {
	case submission := <-c.submissions:
		return submission, nil
	case <-c.done:
		return nil, net.ErrClosed
	}
}

func (c *relayTestChannel) Open(context.Context) (io.ReadWriteCloser, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.closed {
		return nil, net.ErrClosed
	}
	route, executor := net.Pipe()
	c.executors = append(c.executors, executor)
	go executor.Write(make([]byte, 64*1024))
	return route, nil
}

func (c *relayTestChannel) Done() <-chan struct{} {
	return c.done
}

func (c *relayTestChannel) Close() error {
	c.lock.Lock()
	defer c.lock.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
		for _, executor := range c.executors {
			executor.Close()
		}
	}
	return nil
}

// startRelayTest starts a relay for the specified session's controller, which
// is indexed in the specified star index, with the specified channels, and
// accepts submissions through alpha's channel, as a relay's maintainer does.
// Cancelling the returned context stops the relay (see stopRelayTest).
func startRelayTest(stars *commandStars, c *controller, channels [2]remote.Channel) (*commandRelay, context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	relay := &commandRelay{
		stars:         stars,
		controller:    c,
		ctx:           ctx,
		flushRequests: make(chan chan error, 1),
		report:        func() *remote.Report { return &remote.Report{} },
		progress:      func() *remote.Progress { return &remote.Progress{Status: "scanning"} },
		channels:      channels,
	}
	stars.setRelay(c, relay)
	relay.routes.Add(1)
	go func() {
		defer relay.routes.Done()
		relay.accept(ctx, 0, channels[0])
	}()
	return relay, ctx, cancel
}

// stopRelayTest stops a relay as the function returned by startCommandRelay
// does, closing its channels as its maintainers do, and reports whether its
// routes terminate within a timeout.
func stopRelayTest(relay *commandRelay, cancel context.CancelFunc) bool {
	cancel()
	relay.stars.setRelay(relay.controller, nil)
	stopped := make(chan struct{})
	go func() {
		for _, channel := range relay.channels {
			channel.Close()
		}
		relay.routes.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

// TestCommandRelayStopUnblocksRoutes verifies that stopping a relay, which
// waits for its routes, ends every route blocked on a submitter that stopped
// reading, such as while writing a response or forwarding output, which
// closing the relay's channels can't interrupt. Submitters are connected over
// pipes, so that a route blocks on the first message that isn't read.
func TestCommandRelayStopUnblocksRoutes(t *testing.T) {
	for _, test := range []struct {
		// description describes the blocked message.
		description string
		// frames are the frames written by the submitter: the header and any
		// request that follows it.
		frames []any
		// flush indicates whether the synchronization loop completes a flush.
		flush bool
	}{
		{"node listing", []any{&remote.Submission{Operation: remote.OperationNodes}}, false},
		{"selected route", []any{&remote.Submission{Operation: remote.OperationResolve, Resolve: true, Node: "gpu"}}, false},
		{"selection rejection", []any{&remote.Submission{Operation: remote.OperationExecute, Node: "unknown"}}, false},
		{"header rejection", []any{&struct{ Operation uint8 }{255}}, false},
		{"checkout status", []any{&remote.Submission{Operation: remote.OperationClaim}, &struct{ Path, Token string }{"a", "token"}}, false},
		{"flush report", []any{&remote.Submission{Operation: remote.OperationFlush}}, true},
		{"flush progress", []any{&remote.Submission{Operation: remote.OperationFlush, Progress: true}}, false},
		{"conflict listing", []any{&remote.Submission{Operation: remote.OperationConflicts}}, false},
		{"command output", []any{&remote.Submission{Operation: remote.OperationExecute}}, false},
	} {
		t.Run(test.description, func(t *testing.T) {
			// Start a relay at a star's center, whose synchronization loop
			// completes a flush if requested, and whose executor sends output
			// to each route at once.
			stars := newCommandStars()
			c := &controller{session: starTestEdge("gpu")}
			stars.update(c, false)
			submitting := newRelayTestChannel()
			relay, ctx, cancel := startRelayTest(stars, c, [2]remote.Channel{submitting, newRelayTestChannel()})
			defer cancel()
			if test.flush {
				go func() {
					select {
					case request := <-relay.flushRequests:
						request <- nil
					case <-ctx.Done():
					}
				}()
			}

			// Submit, and read only the length of the first message that the
			// route sends, which leaves the route blocked sending the rest.
			// The submitter's closure releases a route that isn't stopped.
			client, server := net.Pipe()
			t.Cleanup(func() { client.Close() })
			submitting.submissions <- server
			go func() {
				for _, frame := range test.frames {
					data, err := json.Marshal(frame)
					if err != nil {
						panic(err)
					} else if binary.Write(client, binary.BigEndian, uint32(len(data))) != nil {
						return
					} else if _, err := client.Write(data); err != nil {
						return
					}
				}
			}()
			client.SetReadDeadline(time.Now().Add(5 * time.Second))
			var size uint32
			if err := binary.Read(client, binary.BigEndian, &size); err != nil {
				t.Fatal("route sent nothing:", err)
			}

			// Stopping the relay ends the route.
			if !stopRelayTest(relay, cancel) {
				t.Fatal("stopping the relay didn't end the blocked route")
			}
		})
	}
}

// TestCommandRelayStopUnblocksLocalSubmission verifies that stopping a relay
// ends a route blocked on a local submission, which arrives over a Unix
// socket, whose submitter stopped reading a node listing that exceeds the
// socket's buffers, although closing the local channel doesn't close the
// submissions that it delivered.
func TestCommandRelayStopUnblocksLocalSubmission(t *testing.T) {
	// Isolate the command registry in a directory whose path is short enough
	// for Unix sockets, which rules out the test name and nested directory
	// added by t.TempDir.
	directory, err := os.MkdirTemp("", "mtg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	if directory, err = filepath.EvalSymlinks(directory); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUTAGEN_DATA_DIRECTORY", directory)
	root := filepath.Join(directory, "center")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}

	// Index a star whose center's node listing exceeds the socket's buffers,
	// and start the relay of one of its edges, which registers the center's
	// workspace for submissions.
	stars := newCommandStars()
	var c *controller
	for i := 0; i < 4000; i++ {
		edge := &controller{session: starTestEdge(fmt.Sprintf("node%04d", i))}
		stars.update(edge, false)
		if i == 0 {
			c = edge
		}
	}
	channel, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: c.session.Identifier, Root: root, Alpha: true, Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	relay, _, cancel := startRelayTest(stars, c, [2]remote.Channel{channel, newRelayTestChannel()})
	defer cancel()

	// Query the nodes, and read only the listing's length, which leaves the
	// route blocked sending the rest. The submitter's closure releases a route
	// that isn't stopped.
	ctx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	connection, _, err := (&remote.Selection{}).Dial(ctx, root, remote.OperationNodes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	var size uint32
	if err := binary.Read(connection, binary.BigEndian, &size); err != nil {
		t.Fatal("node listing not sent:", err)
	} else if size < 512*1024 {
		t.Fatal("node listing too small to exceed socket buffers:", size)
	}

	// Stopping the relay ends the route.
	if !stopRelayTest(relay, cancel) {
		t.Fatal("stopping the relay didn't end the blocked route")
	}
}
