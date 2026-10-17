package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// useTestRegistry isolates the command registry in a temporary data directory.
func useTestRegistry(t *testing.T) string {
	t.Helper()
	// Avoid the test name and nested directory added by t.TempDir: combined
	// with macOS's long temporary root, they exceed Unix socket path limits.
	directory, err := os.MkdirTemp("", "mr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error("unable to remove test registry:", err)
		}
	})
	t.Setenv("MUTAGEN_DATA_DIRECTORY", directory)
	return filepath.Join(directory, registryDirectoryName)
}

// testRegistration registers an endpoint whose submissions are recorded under
// the specified label. Version handshakes are performed by the listener.
type testRegistration struct {
	listener net.Listener
	done     chan struct{}
}

// registerTest registers an endpoint and serves its submissions by recording
// the label of the accepting registration.
func registerTest(t *testing.T, endpoint *Endpoint, label string, accepted *[]string, lock *sync.Mutex) *testRegistration {
	t.Helper()
	listener, err := register(endpoint, endpoint.Root)
	if err != nil {
		t.Fatal(err)
	}
	registration := &testRegistration{listener, make(chan struct{})}
	go func() {
		defer close(registration.done)
		serveSubmissions(nil, listener, func(connection net.Conn) {
			lock.Lock()
			*accepted = append(*accepted, label)
			lock.Unlock()
			connection.Close()
		})
	}()
	t.Cleanup(func() {
		listener.Close()
		<-registration.done
	})
	return registration
}

// mkdirs creates directories for a test.
func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDialSelection(t *testing.T) {
	registry := useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "inner")
	outside := filepath.Join(base, "outside")
	mkdirs(t, filepath.Join(inner, "x"), filepath.Join(outer, "y"), outside)
	var lock sync.Mutex
	var accepted []string
	registerTest(t, &Endpoint{Session: "sync_outer", Name: "outer-name", Root: outer, Alpha: true}, "outer", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_inner", Root: inner}, "inner", &accepted, &lock)
	dial := func(directory, session string) (*Target, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		connection, target, err := Dial(ctx, directory, session, OperationExecute)
		if err == nil {
			connection.Close()
		}
		return target, err
	}

	// The most specific containing root wins.
	if target, err := dial(filepath.Join(inner, "x"), ""); err != nil || target.Session != "sync_inner" || target.Path != "x" || target.Alpha || !target.Contained {
		t.Fatalf("incorrect nested selection: %+v, %v", target, err)
	} else if target, err := dial(filepath.Join(outer, "y"), ""); err != nil || target.Session != "sync_outer" || target.Path != "y" || !target.Alpha {
		t.Fatalf("incorrect outer selection: %+v, %v", target, err)
	} else if target, err := dial(outer, ""); err != nil || target.Path != "" {
		t.Fatalf("incorrect root selection: %+v, %v", target, err)
	}

	// Sessions must contain the directory unless explicitly selected.
	if _, err := dial(outside, ""); err == nil {
		t.Fatal("directory outside all roots accepted")
	} else if _, err := dial(outer+"-sibling", ""); err == nil {
		t.Fatal("sibling prefix matched")
	} else if target, err := dial(outside, "outer-name"); err != nil || target.Session != "sync_outer" || target.Path != "" || target.Contained {
		t.Fatalf("incorrect explicit selection by name: %+v, %v", target, err)
	} else if target, err := dial(filepath.Join(inner, "x"), "sync_outer"); err != nil || target.Path != "inner/x" || !target.Contained {
		t.Fatalf("incorrect explicit selection by identifier: %+v, %v", target, err)
	} else if _, err := dial(outer, "missing"); err == nil {
		t.Fatal("unknown session accepted")
	}

	// A replacement registration for the same endpoint supersedes the original,
	// such as when a lingering executor has not yet noticed disconnection.
	registerTest(t, &Endpoint{Session: "sync_inner", Root: inner}, "replacement", &accepted, &lock)
	if _, err := dial(inner, ""); err != nil {
		t.Fatal("replacement registration failed:", err)
	}
	time.Sleep(100 * time.Millisecond)
	lock.Lock()
	last := accepted[len(accepted)-1]
	lock.Unlock()
	if last != "replacement" {
		t.Fatal("older registration selected:", last)
	}

	// Distinct sessions with the same root are ambiguous.
	registerTest(t, &Endpoint{Session: "sync_other", Root: outer}, "other", &accepted, &lock)
	if _, err := dial(filepath.Join(outer, "y"), ""); err == nil || !strings.Contains(err.Error(), "multiple sessions") {
		t.Fatal("ambiguous sessions accepted:", err)
	} else if target, err := dial(filepath.Join(outer, "y"), "sync_other"); err != nil || target.Session != "sync_other" {
		t.Fatalf("explicit selection did not resolve ambiguity: %+v, %v", target, err)
	}

	// If both endpoints of a session are on this system, then selecting the
	// session outside their roots is ambiguous regardless of root lengths,
	// while invocation inside a root selects that endpoint.
	short, long := filepath.Join(base, "a"), filepath.Join(base, "longer-beta")
	mkdirs(t, short, long)
	registerTest(t, &Endpoint{Session: "sync_both", Name: "both", Root: short, Alpha: true}, "both-alpha", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_both", Name: "both", Root: long}, "both-beta", &accepted, &lock)
	if target, err := dial(outside, "both"); err == nil || !strings.Contains(err.Error(), "both endpoints") {
		t.Fatalf("ambiguous endpoints accepted: %+v, %v", target, err)
	} else if target, err := dial(short, "both"); err != nil || !target.Alpha {
		t.Fatalf("incorrect alpha selection: %+v, %v", target, err)
	} else if target, err := dial(long, "sync_both"); err != nil || target.Alpha {
		t.Fatalf("incorrect beta selection: %+v, %v", target, err)
	}

	// Distinct sessions sharing a name are similarly ambiguous.
	registerTest(t, &Endpoint{Session: "sync_named", Name: "both", Root: inner}, "named", &accepted, &lock)
	if target, err := dial(outside, "both"); err == nil || !strings.Contains(err.Error(), "multiple sessions match") {
		t.Fatalf("ambiguous sessions accepted: %+v, %v", target, err)
	}

	// Registrations of crashed processes are pruned.
	stale := registerTest(t, &Endpoint{Session: "sync_stale", Root: outside}, "stale", &accepted, &lock)
	stale.listener.(*registeredListener).Listener.Close()
	if _, err := dial(outside, ""); err == nil {
		t.Fatal("stale registration accepted")
	}
	registrations, err := loadRegistrations(registry)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range registrations {
		if r.Session == "sync_stale" {
			t.Fatal("stale registration not pruned")
		}
	}

	// Registrations from other versions are explained.
	data, _ := json.Marshal(&record{Version: "0.0.1", Session: "sync_old", Root: outside})
	if err := os.WriteFile(filepath.Join(registry, "old"+recordExtension), data, 0600); err != nil {
		t.Fatal(err)
	} else if _, err := dial(outside, ""); err == nil || !strings.Contains(err.Error(), "0.0.1") {
		t.Fatal("version mismatch not explained:", err)
	}
}

// testChannel connects an agent-style channel to an in-memory executor.
func testChannel(t *testing.T, endpoint *Endpoint, install ExecutableInstaller) Channel {
	t.Helper()
	client, server := net.Pipe()
	return testChannelOver(t, client, server, endpoint, install)
}

// testChannelOver connects an agent-style channel to an in-memory executor
// over the specified halves of a connection.
func testChannelOver(t *testing.T, client, server io.ReadWriteCloser, endpoint *Endpoint, install ExecutableInstaller) Channel {
	t.Helper()
	served := make(chan error, 1)
	go func() { served <- ServeChannel(context.Background(), nil, server, install) }()
	channel, err := NewChannel(context.Background(), nil, client, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		channel.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("executor did not terminate")
		}
	})
	return channel
}

// inspectDirectory runs the inspect helper over a route and returns its
// working directory.
func inspectDirectory(t *testing.T, route io.ReadWriteCloser, path string) string {
	t.Helper()
	request := helperRequest(t, "inspect")
	request.Directory, request.Path = "", path
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if code, err := Execute(ctx, route, request, strings.NewReader(""), &output, io.Discard); err != nil || code != 0 {
		t.Fatalf("execution failed: %d, %v", code, err)
	}
	var result []json.RawMessage
	var directory string
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(result[0], &directory)
	return directory
}

func TestAgentChannel(t *testing.T) {
	useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	root, opposite := filepath.Join(base, "root"), filepath.Join(base, "opposite")
	mkdirs(t, filepath.Join(root, "sub"), filepath.Join(opposite, "sub"))

	// The remote executable is installed on initialization.
	var installed bool
	install := func() (string, error) {
		installed = true
		return filepath.Join(base, "agents", "remote"), nil
	}
	channel := testChannel(t, &Endpoint{Session: "sync_test", Root: root, Submit: true, Execute: true}, install)
	if !installed {
		t.Fatal("remote executable not installed")
	}

	// Commands opened by the daemon run in the endpoint's root.
	route, err := channel.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	} else if directory := inspectDirectory(t, route, "sub"); directory != filepath.Join(root, "sub") {
		t.Fatal("incorrect executor directory:", directory)
	}

	// Local submissions reach the daemon, which routes them elsewhere (here,
	// to an executor for the opposite root).
	go func() {
		submission, err := channel.Accept()
		if err != nil {
			return
		} else if operation, err := ReceiveSubmission(context.Background(), submission); err != nil || operation != OperationExecute {
			t.Error("incorrect submission:", operation, err)
			submission.Close()
			return
		}
		execution, server := net.Pipe()
		go Serve(context.Background(), server, opposite, "")
		Splice(submission, execution)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, target, err := Dial(ctx, filepath.Join(root, "sub"), "", OperationExecute)
	if err != nil {
		t.Fatal(err)
	} else if directory := inspectDirectory(t, connection, target.Path); directory != filepath.Join(opposite, "sub") {
		t.Fatal("incorrect submission directory:", directory)
	}
}

func TestAgentChannelWithoutExecution(t *testing.T) {
	useTestRegistry(t)
	root := t.TempDir()

	// Endpoints that don't submit commands don't need the remote executable.
	install := func() (string, error) {
		t.Error("remote executable installed for non-submitting endpoint")
		return "", errors.New("unexpected installation")
	}
	channel := testChannel(t, &Endpoint{Session: "sync_test", Root: root}, install)
	route, err := channel.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Execute(ctx, route, helperRequest(t, "exit"), strings.NewReader(""), io.Discard, io.Discard); err == nil || err.Error() != errExecutionDisabled.Error() {
		t.Fatal("execution not refused:", err)
	}

	// Endpoints that don't submit commands don't register.
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := Dial(ctx, root, "", OperationExecute); err == nil {
		t.Fatal("non-submitting endpoint registered")
	}
}

func TestAgentChannelInstallationFailure(t *testing.T) {
	useTestRegistry(t)
	root := t.TempDir()
	install := func() (string, error) {
		return "", errors.New("installation failed")
	}
	testChannel(t, &Endpoint{Session: "sync_test", Root: root, Submit: true}, install)

	// Submission still works with a separately installed executable.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if connection, _, err := Dial(ctx, root, "", OperationExecute); err != nil {
		t.Fatal(err)
	} else {
		connection.Close()
	}
}

func TestAgentChannelInitializationCancellation(t *testing.T) {
	// Cancellation interrupts an executor that stops responding.
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var endpoint Endpoint
		readMessage(server, &endpoint)
		time.AfterFunc(50*time.Millisecond, cancel)
	}()
	results := make(chan error, 1)
	go func() {
		_, err := NewChannel(ctx, nil, client, &Endpoint{Session: "sync_test", Root: t.TempDir(), Submit: true})
		results <- err
	}()
	select {
	case err := <-results:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("incorrect initialization error:", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initialization not cancelled")
	}

	// The connection is closed on cancellation.
	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, server); err != nil {
		t.Fatal("connection not closed:", err)
	}
}

// unclosableConnection emulates standard input/output, which can't be closed
// to interrupt blocked operations.
type unclosableConnection struct {
	net.Conn
}

// Close implements net.Conn.Close.
func (c *unclosableConnection) Close() error {
	return errors.New("closing not allowed")
}

func TestServeChannelInitializationCancellation(t *testing.T) {
	registry := useTestRegistry(t)
	root := t.TempDir()

	// Cancellation terminates an executor whose daemon stops responding while
	// it awaits the request or reports initialization after registering.
	for _, stage := range []string{"request", "response"} {
		daemon, executor := net.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		stalled := make(chan struct{})
		go func() {
			defer close(stalled)
			if stage == "request" {
				return
			}
			writeMessage(daemon, &Endpoint{Session: "sync_test", Root: root, Submit: true})
		}()
		served := make(chan error, 1)
		go func() {
			served <- ServeChannel(ctx, nil, &unclosableConnection{executor}, nil)
		}()
		<-stalled
		time.AfterFunc(50*time.Millisecond, cancel)
		select {
		case err := <-served:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("incorrect executor error (stage: %s): %v", stage, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("executor not cancelled (stage: %s)", stage)
		}

		// A registration completed by abandoned initialization is removed.
		if stage == "response" {
			var response initializeResponse
			if err := readMessage(daemon, &response); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for registrations, _ := loadRegistrations(registry); len(registrations) > 0; registrations, _ = loadRegistrations(registry) {
				if time.Now().After(deadline) {
					t.Fatal("abandoned registration not removed")
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		daemon.Close()
		executor.Close()
		cancel()
	}
}

// useLivenessInterval sets the agent connection liveness interval for a test.
func useLivenessInterval(t *testing.T, interval time.Duration) {
	t.Helper()
	previous := livenessInterval
	livenessInterval = interval
	t.Cleanup(func() { livenessInterval = previous })
}

// throttledConnection emulates a slow link by delaying writes.
type throttledConnection struct {
	net.Conn
}

// Write implements net.Conn.Write.
func (c *throttledConnection) Write(data []byte) (int, error) {
	var written int
	for len(data) > 0 {
		time.Sleep(10 * time.Millisecond)
		count, err := c.Conn.Write(data[:min(len(data), 64)])
		written += count
		if err != nil {
			return written, err
		}
		data = data[count:]
	}
	return written, nil
}

func TestAgentChannelLiveness(t *testing.T) {
	useTestRegistry(t)
	useLivenessInterval(t, 250*time.Millisecond)
	root := t.TempDir()

	// Idle channels are kept alive by heartbeats.
	client, server := net.Pipe()
	channel := testChannelOver(t, &throttledConnection{client}, &throttledConnection{server}, &Endpoint{Session: "sync_test", Root: root, Execute: true}, nil)
	time.Sleep(4 * livenessInterval)
	select {
	case <-channel.Done():
		t.Fatal("idle channel closed")
	default:
	}

	// Slow links remain alive while heartbeats are queued behind command I/O
	// that takes several liveness intervals to transfer in each direction.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	route, err := channel.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := bytes.Repeat([]byte("slow"), 2048)
	var output bytes.Buffer
	if code, err := Execute(ctx, route, helperRequest(t, "copy"), bytes.NewReader(input), &output, io.Discard); err != nil || code != 0 {
		t.Fatalf("execution over slow link failed: %d, %v", code, err)
	} else if !bytes.Equal(output.Bytes(), input) {
		t.Fatal("incorrect output over slow link")
	}

	// Channels whose executors stop sending data are closed.
	daemon, executor := net.Pipe()
	go func(executor net.Conn) {
		var endpoint Endpoint
		readMessage(executor, &endpoint)
		writeMessage(executor, &initializeResponse{})
		io.Copy(io.Discard, executor)
	}(executor)
	stalled, err := NewChannel(context.Background(), nil, daemon, &Endpoint{Session: "sync_stalled", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	select {
	case <-stalled.Done():
	case <-time.After(10 * livenessInterval):
		t.Fatal("stalled channel not closed")
	}

	// Executors whose daemons stop sending data terminate.
	daemon, executor = net.Pipe()
	defer daemon.Close()
	served := make(chan error, 1)
	go func(executor net.Conn) { served <- ServeChannel(context.Background(), nil, executor, nil) }(executor)
	var response initializeResponse
	if err := writeMessage(daemon, &Endpoint{Session: "sync_stalled", Root: root}); err != nil {
		t.Fatal(err)
	} else if err := readMessage(daemon, &response); err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, daemon)
	select {
	case err := <-served:
		if !errors.Is(err, errLivenessTimeout) {
			t.Fatal("incorrect executor error:", err)
		}
	case <-time.After(10 * livenessInterval):
		t.Fatal("stalled executor did not terminate")
	}
}

func TestChannelPrologue(t *testing.T) {
	// Prologues are sourced by a POSIX shell.
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	useTestRegistry(t)

	// Both kinds of channel resolve relative prologues against the root.
	root := resolveSymbolicLinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "prologue.sh"), []byte("export REMOTE_VALUE=from-prologue\n"), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := &Endpoint{Session: "sync_test", Root: root, Execute: true, Prologue: "prologue.sh"}
	local, err := NewLocalChannel(nil, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	for name, channel := range map[string]Channel{"local": local, "agent": testChannel(t, endpoint, nil)} {
		for _, requested := range []bool{true, false} {
			route, err := channel.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			request := &Request{Arguments: []string{"sh", "-c", `printf %s "$REMOTE_VALUE"`}, Prologue: requested}
			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			code, err := Execute(ctx, route, request, strings.NewReader(""), &output, io.Discard)
			cancel()
			if expected := map[bool]string{true: "from-prologue"}[requested]; err != nil || code != 0 || output.String() != expected {
				t.Errorf("incorrect %s channel output with prologue %t: %d, %q, %v", name, requested, code, output.String(), err)
			}
		}
	}

	// Endpoints that don't execute commands ignore their prologue.
	if _, err := NewLocalChannel(nil, &Endpoint{Session: "sync_test", Root: root, Prologue: "~nonexistent-user/prologue.sh"}); err != nil {
		t.Fatal("prologue resolved for non-executing endpoint:", err)
	}
}

func TestLocalChannel(t *testing.T) {
	registry := useTestRegistry(t)
	root := resolveSymbolicLinks(t.TempDir())
	mkdirs(t, filepath.Join(root, "sub"))
	channel, err := NewLocalChannel(nil, &Endpoint{Session: "sync_test", Root: root, Submit: true, Execute: true})
	if err != nil {
		t.Fatal(err)
	}

	// Route a local submission back to the same endpoint.
	go func() {
		submission, err := channel.Accept()
		if err != nil {
			return
		} else if operation, err := ReceiveSubmission(context.Background(), submission); err != nil || operation != OperationExecute {
			t.Error("incorrect submission:", operation, err)
			submission.Close()
			return
		}
		execution, err := channel.Open(context.Background())
		if err != nil {
			Reject(submission, err)
			return
		}
		Splice(submission, execution)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, target, err := Dial(ctx, filepath.Join(root, "sub"), "", OperationExecute)
	if err != nil {
		t.Fatal(err)
	} else if directory := inspectDirectory(t, connection, target.Path); directory != filepath.Join(root, "sub") {
		t.Fatal("incorrect directory:", directory)
	}

	// Closure removes the registration and fails subsequent operations.
	if err := channel.Close(); err != nil {
		t.Fatal(err)
	} else if registrations, _ := loadRegistrations(registry); len(registrations) != 0 {
		t.Fatal("registration not removed")
	} else if _, err := channel.Accept(); err == nil {
		t.Fatal("accept succeeded after closure")
	} else if _, err := channel.Open(context.Background()); err == nil {
		t.Fatal("open succeeded after closure")
	}
}

func TestFlush(t *testing.T) {
	useTestRegistry(t)
	root := t.TempDir()
	channel, err := NewLocalChannel(nil, &Endpoint{Session: "sync_test", Root: root, Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { channel.Close() })

	// Respond to flush submissions as the daemon would, with the specified
	// results.
	results := make(chan error)
	go func() {
		for {
			submission, err := channel.Accept()
			if err != nil {
				return
			} else if operation, err := ReceiveSubmission(context.Background(), submission); err != nil || operation != OperationFlush {
				t.Error("incorrect submission:", operation, err)
				submission.Close()
				continue
			}
			Respond(submission, <-results)
		}
	}()
	flush := func(ctx context.Context, result error) error {
		connection, _, err := Dial(ctx, root, "", OperationFlush)
		if err != nil {
			t.Fatal(err)
		}
		go func() { results <- result }()
		return Flush(ctx, connection)
	}

	// Completion and failure are reported.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := flush(ctx, nil); err != nil {
		t.Fatal("flush failed:", err)
	} else if err := flush(ctx, errors.New("synchronization stopped")); err == nil || err.Error() != "synchronization stopped" {
		t.Fatal("flush failure not reported:", err)
	}

	// Cancellation abandons the wait.
	ctx, cancel = context.WithCancel(context.Background())
	connection, _, err := Dial(ctx, root, "", OperationFlush)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := Flush(ctx, connection); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not reported:", err)
	}
	select {
	case results <- nil:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned flush not received")
	}
}
