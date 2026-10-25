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
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/ipc"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
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

	// Among distinct sessions with the same root, the session created first is
	// used, with ties broken by identifier, and the sessions are labeled for a
	// warning, unless one is selected explicitly.
	registerTest(t, &Endpoint{Session: "sync_other", Root: outer, SessionCreated: time.Now()}, "other", &accepted, &lock)
	if target, err := dial(filepath.Join(outer, "y"), ""); err != nil || target.Session != "sync_outer" ||
		!reflect.DeepEqual(target.Overlapping, []string{"outer-name", "sync_other"}) {
		t.Fatalf("incorrect selection among equally specific roots: %+v, %v", target, err)
	} else if target, err := dial(filepath.Join(outer, "y"), "sync_other"); err != nil || target.Session != "sync_other" || target.Overlapping != nil {
		t.Fatalf("explicit selection not used: %+v, %v", target, err)
	}
	registerTest(t, &Endpoint{Session: "sync_another", Root: outer}, "another", &accepted, &lock)
	if target, err := dial(filepath.Join(outer, "y"), ""); err != nil || target.Session != "sync_another" ||
		!reflect.DeepEqual(target.Overlapping, []string{"sync_another", "outer-name", "sync_other"}) {
		t.Fatalf("tie not broken by identifier: %+v, %v", target, err)
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

// dialTest submits an execution through the endpoint located by a selection
// for a directory and returns its target.
func dialTest(selection *Selection, directory string) (*Target, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, target, err := selection.Dial(ctx, directory, OperationExecute)
	if err == nil {
		connection.Close()
	}
	return target, err
}

// registerIncompatible registers a live endpoint as another version of Mutagen
// would. Its connections are accepted and closed, as its version handshake
// would fail: on Windows, clients can't connect to a named pipe until its
// server accepts, so liveness probes would otherwise wait for their deadline.
func registerIncompatible(t *testing.T, registry, session, root string) {
	t.Helper()
	base := filepath.Join(registry, session)
	listener, err := ipc.NewListener(base + socketExtension)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connection.Close()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	data, err := json.Marshal(&record{Version: "0.0.1", Session: session, Root: root})
	if err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(base+recordExtension, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDialRegistrationRefusals(t *testing.T) {
	// Named pipes have no backlog that refuses connections.
	if runtime.GOOS == "windows" {
		t.Skip()
	}

	// Where connections to a full backlog are refused, a refusal is only
	// reported once it persists, so that live registrations aren't pruned.
	// Sockets whose listeners are closed without removing them refuse
	// connections on all systems.
	ambiguous := ambiguousRefusals
	ambiguousRefusals = true
	t.Cleanup(func() { ambiguousRefusals = ambiguous })
	directory := filepath.Dir(useTestRegistry(t))
	path := filepath.Join(directory, "refused")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := dialRegistration(ctx, path); !isDead(err) {
		t.Fatal("persistent refusal not reported:", err)
	} else if time.Since(start) < refusalConfirmationTimeout {
		t.Fatal("refusal reported without retrying")
	}

	// A listener that accepts connections before the refusal persists is
	// reached. It's renamed into place, so that the path never goes missing.
	live, err := net.Listen("unix", filepath.Join(directory, "live"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	renamed := make(chan error, 1)
	time.AfterFunc(refusalConfirmationTimeout/5, func() {
		renamed <- os.Rename(filepath.Join(directory, "live"), path)
	})
	if connection, err := dialRegistration(ctx, path); err != nil {
		t.Fatal("transient refusal reported:", err)
	} else {
		connection.Close()
	}
	if err := <-renamed; err != nil {
		t.Fatal(err)
	}
}

func TestDialSessionDefault(t *testing.T) {
	registry := useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	outer, first, outside := filepath.Join(base, "outer"), filepath.Join(base, "first"), filepath.Join(base, "outside")
	inner := filepath.Join(outer, "inner")
	mkdirs(t, filepath.Join(inner, "x"), filepath.Join(outer, "y"), first, outside)
	var lock sync.Mutex
	var accepted []string
	registerTest(t, &Endpoint{Session: "sync_outer", Name: "outer", Root: outer, Alpha: true}, "outer", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_inner", Name: "inner", Root: inner}, "inner", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_first", Name: "first", Root: first}, "first", &accepted, &lock)

	// The default never redirects a submission from inside a session's root
	// to a session that doesn't contain it, but it selects among the sessions
	// that do, and outside every root it selects a session.
	for _, test := range []struct {
		directory, session, path string
		contained                bool
	}{
		{filepath.Join(outer, "y"), "sync_outer", "y", true},
		{filepath.Join(inner, "x"), "sync_inner", "x", true},
		{outside, "sync_first", "", false},
	} {
		if target, err := dialTest(&Selection{Default: "first"}, test.directory); err != nil ||
			target.Session != test.session || target.Path != test.path || target.Contained != test.contained {
			t.Errorf("incorrect selection from %s with a default: %+v, %v", test.directory, target, err)
		}
	}
	if target, err := dialTest(&Selection{Default: "outer"}, filepath.Join(inner, "x")); err != nil ||
		target.Session != "sync_outer" || target.Path != "inner/x" {
		t.Errorf("default didn't select among containing sessions: %+v, %v", target, err)
	} else if _, err := dialTest(&Selection{Default: "missing"}, outside); err == nil ||
		!strings.Contains(err.Error(), `MUTAGEN_REMOTE_SESSION="missing"`) {
		t.Error("missing default session not explained:", err)
	} else if target, err := dialTest(&Selection{Session: "first", Default: "outer"}, outside); err != nil || target.Session != "sync_first" {
		t.Errorf("explicit session didn't take precedence over the default: %+v, %v", target, err)
	}

	// Dial uses the environment variable as the default.
	t.Setenv(SessionVariable, "first")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for directory, expected := range map[string]string{outside: "sync_first", filepath.Join(outer, "y"): "sync_outer"} {
		if connection, target, err := Dial(ctx, directory, "", OperationExecute); err != nil || target.Session != expected {
			t.Errorf("environment variable applied incorrectly from %s: %+v, %v", directory, target, err)
		} else {
			connection.Close()
		}
	}

	// A default matching one of several equally specific containing sessions
	// selects it, while an unrelated default leaves the session created first
	// in use, with a warning.
	registerTest(t, &Endpoint{Session: "sync_other", Name: "other", Root: outer, SessionCreated: time.Now()}, "other", &accepted, &lock)
	if target, err := dialTest(&Selection{Default: "first"}, filepath.Join(outer, "y")); err != nil || target.Session != "sync_outer" ||
		!reflect.DeepEqual(target.Overlapping, []string{"outer", "other"}) {
		t.Errorf("incorrect selection with an unrelated default: %+v, %v", target, err)
	} else if target, err := dialTest(&Selection{Default: "other"}, filepath.Join(outer, "y")); err != nil || target.Session != "sync_other" ||
		target.Overlapping != nil {
		t.Errorf("default not used: %+v, %v", target, err)
	}

	// A root synchronized by another version of Mutagen isn't treated as being
	// outside every root.
	legacy := filepath.Join(base, "legacy")
	mkdirs(t, filepath.Join(legacy, "sub"))
	registerIncompatible(t, registry, "sync_legacy", legacy)
	if target, err := dialTest(&Selection{Default: "first"}, filepath.Join(legacy, "sub")); err == nil ||
		!strings.Contains(err.Error(), "0.0.1") {
		t.Errorf("default selected outside a root synchronized by another version: %+v, %v", target, err)
	}
}

func TestDialCorresponding(t *testing.T) {
	useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	outer, first, outside := filepath.Join(base, "outer"), filepath.Join(base, "first"), filepath.Join(base, "outside")
	inner := filepath.Join(outer, "inner")
	mkdirs(t, filepath.Join(inner, "x"), filepath.Join(outer, "y"), first, outside)
	var lock sync.Mutex
	var accepted []string
	registerTest(t, &Endpoint{Session: "sync_outer", Name: "outer", Root: outer, Alpha: true}, "outer", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_inner", Name: "inner", Root: inner}, "inner", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_first", Name: "first", Root: first}, "first", &accepted, &lock)

	// A session selected explicitly from inside another session's root isn't
	// used for operations corresponding to the directory, which would apply
	// to its root instead.
	if _, err := dialTest(&Selection{Session: "first", Corresponding: true}, filepath.Join(outer, "y")); err == nil ||
		!strings.Contains(err.Error(), "is synchronized by session outer (sync_outer), not first (sync_first)") ||
		!strings.Contains(err.Error(), "--cwd") {
		t.Error("explicit session redirected a corresponding submission:", err)
	} else if _, err := dialTest(&Selection{Session: "sync_first", Corresponding: true}, filepath.Join(inner, "x")); err == nil ||
		!strings.Contains(err.Error(), "sessions inner (sync_inner), outer (sync_outer)") {
		t.Error("nested containing sessions not described:", err)
	} else if _, err := dialTest(&Selection{Session: "missing", Corresponding: true}, filepath.Join(outer, "y")); err == nil ||
		!strings.Contains(err.Error(), `no connected session matching "missing"`) {
		t.Error("missing session not reported:", err)
	}

	// Other operations, sessions that contain the directory, and selections
	// outside every root are unaffected.
	for _, test := range []struct {
		selection       *Selection
		directory, path string
		session         string
		contained       bool
		description     string
	}{
		{&Selection{Session: "first"}, filepath.Join(outer, "y"), "", "sync_first", false, "non-corresponding operation"},
		{&Selection{Session: "first", Corresponding: true}, outside, "", "sync_first", false, "outside every root"},
		{&Selection{Session: "outer", Corresponding: true}, filepath.Join(inner, "x"), "inner/x", "sync_outer", true, "enclosing session"},
	} {
		if target, err := dialTest(test.selection, test.directory); err != nil || target.Session != test.session ||
			target.Path != test.path || target.Contained != test.contained {
			t.Errorf("incorrect selection (%s): %+v, %v", test.description, target, err)
		}
	}

	// Registrations of crashed processes don't count as synchronizing the
	// directory.
	stale := filepath.Join(base, "stale")
	mkdirs(t, stale)
	registration := registerTest(t, &Endpoint{Session: "sync_stale", Root: stale}, "stale", &accepted, &lock)
	registration.listener.(*registeredListener).Listener.Close()
	if target, err := dialTest(&Selection{Session: "first", Corresponding: true}, stale); err != nil || target.Session != "sync_first" {
		t.Errorf("stale registration prevented selection: %+v, %v", target, err)
	}
}

func TestDialPinned(t *testing.T) {
	useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "inner")
	mkdirs(t, filepath.Join(inner, "x"))
	var lock sync.Mutex
	var accepted []string
	registerTest(t, &Endpoint{Session: "sync_outer", Name: "outer", Root: outer, Alpha: true}, "outer", &accepted, &lock)
	registration := registerTest(t, &Endpoint{Session: "sync_inner", Name: "inner", Root: inner}, "inner", &accepted, &lock)
	directory := filepath.Join(inner, "x")
	target, err := dialTest(&Selection{}, directory)
	if err != nil || target.Session != "sync_inner" {
		t.Fatalf("incorrect initial selection: %+v, %v", target, err)
	}

	// Once the located endpoint disconnects, a pinned selection fails rather
	// than falling back to the enclosing session.
	registration.listener.Close()
	<-registration.done
	if fallback, err := dialTest(&Selection{}, directory); err != nil || fallback.Session != "sync_outer" {
		t.Fatalf("unpinned selection didn't fall back: %+v, %v", fallback, err)
	} else if _, err := dialTest(&Selection{Target: target}, directory); err == nil ||
		!strings.Contains(err.Error(), "session inner (sync_inner) is no longer connected") {
		t.Fatal("pinned selection fell back to another session:", err)
	}

	// A pinned selection follows the endpoint when it reconnects, and ignores
	// other selection criteria.
	registerTest(t, &Endpoint{Session: "sync_inner", Name: "inner", Root: inner}, "reconnected", &accepted, &lock)
	pinned := &Selection{Session: "outer", Default: "outer", Target: target}
	if reconnected, err := dialTest(pinned, directory); err != nil || reconnected.Session != "sync_inner" || reconnected.Path != "x" {
		t.Fatalf("pinned selection didn't follow the reconnected endpoint: %+v, %v", reconnected, err)
	}

	// Pinning distinguishes the endpoints of a session.
	registerTest(t, &Endpoint{Session: "sync_inner", Name: "inner", Root: filepath.Join(base, "beta"), Alpha: true}, "alpha", &accepted, &lock)
	if pinnedAlpha, err := dialTest(&Selection{Target: &Target{Session: "sync_inner", Alpha: true}}, directory); err != nil ||
		!pinnedAlpha.Alpha || pinnedAlpha.Contained {
		t.Fatalf("pinned selection reached the wrong endpoint: %+v, %v", pinnedAlpha, err)
	}
}

func TestDialExcluded(t *testing.T) {
	useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "inner")
	mkdirs(t, filepath.Join(inner, "x"), filepath.Join(outer, "y"))
	var lock sync.Mutex
	var accepted []string
	ignores := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"inner"}}
	registerTest(t, &Endpoint{Session: "sync_outer", Name: "outer", Root: outer, Alpha: true, Ignores: ignores}, "outer", &accepted, &lock)
	registration := registerTest(t, &Endpoint{Session: "sync_inner", Name: "inner", Root: inner}, "inner", &accepted, &lock)
	directory := filepath.Join(inner, "x")
	if target, err := dialTest(&Selection{Corresponding: true}, directory); err != nil || target.Session != "sync_inner" {
		t.Fatalf("incorrect nested selection: %+v, %v", target, err)
	}

	// Once the nested session disconnects, operations corresponding to the
	// directory aren't submitted through the enclosing session, which ignores
	// it, however that session is selected.
	registration.listener.Close()
	<-registration.done
	for _, selection := range []*Selection{
		{Corresponding: true},
		{Session: "outer", Corresponding: true},
		{Default: "inner", Corresponding: true},
	} {
		if _, err := dialTest(selection, directory); err == nil ||
			!strings.Contains(err.Error(), directory+" is inside "+inner+", which session outer (sync_outer) ignores") ||
			!strings.Contains(err.Error(), "use --cwd .") {
			t.Errorf("excluded directory not refused (%+v): %v", selection, err)
		}
	}
	if _, err := dialTest(&Selection{Corresponding: true}, inner); err == nil ||
		!strings.Contains(err.Error(), inner+" is ignored by session outer (sync_outer)") {
		t.Error("excluded root of the nested session not refused:", err)
	}

	// Other operations, synchronized directories, and pinned selections are
	// unaffected.
	if target, err := dialTest(&Selection{}, directory); err != nil || target.Session != "sync_outer" || target.Path != "inner/x" {
		t.Errorf("non-corresponding operation refused: %+v, %v", target, err)
	} else if target, err := dialTest(&Selection{Corresponding: true}, filepath.Join(outer, "y")); err != nil || target.Path != "y" {
		t.Errorf("synchronized directory refused: %+v, %v", target, err)
	} else if pinned, err := dialTest(&Selection{Target: target, Corresponding: true}, directory); err != nil || pinned.Path != "inner/x" {
		t.Errorf("pinned selection refused: %+v, %v", pinned, err)
	}
}

func TestDialOverlapping(t *testing.T) {
	registry := useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	outer, first := filepath.Join(base, "outer"), filepath.Join(base, "first")
	inner := filepath.Join(outer, "inner")
	directory := filepath.Join(inner, "x")
	mkdirs(t, directory, filepath.Join(outer, "y"), first)
	var lock sync.Mutex
	var accepted []string
	registerTest(t, &Endpoint{Session: "sync_outer", Name: "outer", Root: outer, Alpha: true}, "outer", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_inner", Name: "inner", Root: inner}, "inner", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_first", Name: "first", Root: first}, "first", &accepted, &lock)

	// Sessions that also synchronize the directory are described unless the
	// session was selected, explicitly or by a matching default.
	for _, test := range []struct {
		selection   *Selection
		directory   string
		overlapping []string
	}{
		{&Selection{}, directory, []string{"inner", "outer"}},
		{&Selection{Default: "first"}, directory, []string{"inner", "outer"}},
		{&Selection{Default: "inner"}, directory, nil},
		{&Selection{Session: "inner"}, directory, nil},
		{&Selection{Session: "outer"}, directory, nil},
		{&Selection{}, filepath.Join(outer, "y"), nil},
		{&Selection{Default: "first"}, base, nil},
		{&Selection{Target: &Target{Session: "sync_inner"}}, directory, nil},
	} {
		if target, err := dialTest(test.selection, test.directory); err != nil {
			t.Errorf("selection failed (%+v from %s): %v", test.selection, test.directory, err)
		} else if !reflect.DeepEqual(target.Overlapping, test.overlapping) {
			t.Errorf("incorrect overlapping sessions (%+v from %s): %#v", test.selection, test.directory, target.Overlapping)
		}
	}

	// Sessions whose ignores exclude the directory don't synchronize it, while
	// sessions registered by other versions of Mutagen do. Equally specific
	// sessions follow the order of preference.
	ignores := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"inner"}}
	registerTest(t, &Endpoint{Session: "sync_ignoring", Root: outer, Ignores: ignores}, "ignoring", &accepted, &lock)
	registerIncompatible(t, registry, "sync_legacy", outer)
	if target, err := dialTest(&Selection{}, directory); err != nil || !reflect.DeepEqual(target.Overlapping, []string{"inner", "sync_legacy", "outer"}) {
		t.Errorf("incorrect overlapping sessions: %+v, %v", target, err)
	}

	// Sessions sharing a name are labeled by identifier.
	registerTest(t, &Endpoint{Session: "sync_twin", Name: "outer", Root: first}, "twin", &accepted, &lock)
	if target, err := dialTest(&Selection{}, directory); err != nil || !reflect.DeepEqual(target.Overlapping, []string{"inner", "sync_legacy", "sync_outer"}) {
		t.Errorf("shared name not replaced by identifier: %+v, %v", target, err)
	}
}

func TestSessions(t *testing.T) {
	registry := useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	outer, first := filepath.Join(base, "outer"), filepath.Join(base, "first")
	inner := filepath.Join(outer, "inner")
	directory := filepath.Join(inner, "x")
	mkdirs(t, directory, first)
	var lock sync.Mutex
	var accepted []string
	sessions := func(selection *Selection, directory string) []Session {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := selection.Sessions(ctx, directory)
		if err != nil {
			t.Fatal("unable to list sessions:", err)
		}
		return result
	}
	if listed := sessions(&Selection{}, directory); len(listed) != 0 {
		t.Fatalf("sessions listed without registrations: %+v", listed)
	}

	// Containing sessions come first, most specific first and then in the order
	// of preference, and the session that's used is marked. Replaced and dead
	// registrations aren't listed.
	ignores := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"inner"}}
	registerTest(t, &Endpoint{Session: "sync_outer", Name: "outer", Root: outer, Alpha: true, Ignores: ignores}, "outer", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_inner", Name: "inner", Root: inner}, "inner", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_inner", Name: "inner", Root: inner}, "replacement", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_late", Name: "late", Root: inner, SessionCreated: time.Now()}, "late", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_first", Name: "first", Root: first}, "first", &accepted, &lock)
	stale := registerTest(t, &Endpoint{Session: "sync_stale", Root: base}, "stale", &accepted, &lock)
	stale.listener.(*registeredListener).Listener.Close()
	registerIncompatible(t, registry, "sync_legacy", filepath.Join(base, "legacy"))
	expected := []Session{
		{Session: "sync_inner", Name: "inner", Label: "inner", Root: inner, Version: mutagen.Version, Contained: true, Selected: true},
		{Session: "sync_late", Name: "late", Label: "late", Root: inner, Version: mutagen.Version, Contained: true},
		{Session: "sync_outer", Name: "outer", Label: "outer", Alpha: true, Root: outer, Version: mutagen.Version, Contained: true, Excluded: true},
		{Session: "sync_first", Name: "first", Label: "first", Root: first, Version: mutagen.Version},
		{Session: "sync_legacy", Label: "sync_legacy", Root: filepath.Join(base, "legacy"), Version: "0.0.1"},
	}
	if listed := sessions(&Selection{}, directory); !reflect.DeepEqual(listed, expected) {
		t.Errorf("incorrect sessions:\n%+v\nexpected:\n%+v", listed, expected)
	}

	// The selection determines the session that's marked, and none is marked
	// if it doesn't locate one.
	selected := func(listed []Session) []string {
		var result []string
		for _, session := range listed {
			if session.Selected {
				result = append(result, session.Session)
			}
		}
		return result
	}
	if marked := selected(sessions(&Selection{Default: "first"}, base)); !reflect.DeepEqual(marked, []string{"sync_first"}) {
		t.Errorf("default selection not marked: %v", marked)
	} else if marked := selected(sessions(&Selection{Default: "outer"}, directory)); !reflect.DeepEqual(marked, []string{"sync_outer"}) {
		t.Errorf("default selection among containing sessions not marked: %v", marked)
	} else if marked := selected(sessions(&Selection{}, base)); marked != nil {
		t.Errorf("session marked outside every root: %v", marked)
	}
}

func TestSynchronizing(t *testing.T) {
	// This test relies on symbolic links.
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	root, beta, other, outside := filepath.Join(base, "root"), filepath.Join(base, "beta"), filepath.Join(base, "other"), filepath.Join(base, "outside")
	mkdirs(t, filepath.Join(root, "src"), filepath.Join(root, "cache"), filepath.Join(beta, "src"), filepath.Join(other, "sub"), outside)
	for link, target := range map[string]string{
		"results":    "src",
		"cache/link": "../src",
		"out-beta":   filepath.Join(beta, "src"),
		"out-other":  filepath.Join(other, "sub"),
		"out-plain":  outside,
		"out-base":   base,
	} {
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(link))); err != nil {
			t.Fatal(err)
		}
	}
	var lock sync.Mutex
	var accepted []string
	ignores := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"results", "cache", "out-*"}}
	registerTest(t, &Endpoint{Session: "sync_own", Name: "own", Root: root, Alpha: true, Ignores: ignores}, "alpha", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_own", Name: "own", Root: beta, Ignores: ignores}, "beta", &accepted, &lock)
	registerTest(t, &Endpoint{Session: "sync_other", Name: "other", Root: other}, "other", &accepted, &lock)

	// Paths are related to the sessions as writes to them land: symbolic
	// links among their parents are resolved, whether or not the final
	// component exists, while the final component isn't, so that ignored
	// symbolic links that lead to synchronized content are replaced rather
	// than written through. Both endpoints of every session are considered,
	// including those of the session whose root contains the path as given.
	for _, test := range []struct {
		path      string
		directory bool
		sessions  []string
	}{
		{"cache/data", false, nil},
		{"cache/data", true, nil},
		{"results", false, nil},
		{"cache/link", false, nil},
		{"src/data", false, []string{"own (sync_own)"}},
		{"results/data", false, []string{"own (sync_own)"}},
		{"results/missing", true, []string{"own (sync_own)"}},
		{"cache/link/data", false, []string{"own (sync_own)"}},
		{"out-beta/data", false, []string{"own (sync_own)"}},
		{"out-other/data", false, []string{"other (sync_other)"}},
		{"out-plain/data", false, nil},
		{"out-base/root", true, []string{"own (sync_own)"}},
		{"out-base/outside", true, nil},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		sessions, err := Synchronizing(ctx, filepath.Join(root, filepath.FromSlash(test.path)), test.directory)
		cancel()
		if err != nil {
			t.Errorf("unable to determine the sessions synchronizing %s: %v", test.path, err)
		} else if strings.Join(sessions, ", ") != strings.Join(test.sessions, ", ") {
			t.Errorf("incorrect sessions synchronizing %s (directory: %t): %v", test.path, test.directory, sessions)
		}
	}

	// Paths whose parents don't exist can't be related.
	if _, err := Synchronizing(context.Background(), filepath.Join(root, "cache", "missing", "data"), false); err == nil {
		t.Error("path with a missing parent related")
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
	mkdirs(t, filepath.Join(root, "sub"), filepath.Join(root, "ignored"), filepath.Join(opposite, "sub"))

	// The remote executable is installed on initialization.
	var installed bool
	install := func() (string, error) {
		installed = true
		return filepath.Join(base, "agents", "remote"), nil
	}
	ignores := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"ignored"}}
	channel := testChannel(t, &Endpoint{Session: "sync_test", Root: root, Submit: true, Execute: true, Ignores: ignores}, install)
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
		} else if header, err := ReceiveSubmission(context.Background(), submission); err != nil || header.Operation != OperationExecute {
			t.Error("incorrect submission:", header, err)
			submission.Close()
			return
		}
		execution, server := net.Pipe()
		go Serve(context.Background(), server, opposite, "", false)
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

	// The registration records the session's ignores, so commands aren't
	// submitted from directories that the session doesn't synchronize.
	if _, _, err := (&Selection{Corresponding: true}).Dial(ctx, filepath.Join(root, "ignored"), OperationExecute); err == nil ||
		!strings.Contains(err.Error(), "is ignored by session sync_test") {
		t.Fatal("ignores not registered:", err)
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
		} else if header, err := ReceiveSubmission(context.Background(), submission); err != nil || header.Operation != OperationExecute {
			t.Error("incorrect submission:", header, err)
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
	// outcomes.
	type outcome struct {
		report *Report
		err    error
	}
	outcomes := make(chan outcome)
	go func() {
		for {
			submission, err := channel.Accept()
			if err != nil {
				return
			} else if header, err := ReceiveSubmission(context.Background(), submission); err != nil || header.Operation != OperationFlush {
				t.Error("incorrect submission:", header, err)
				submission.Close()
				continue
			}
			result := <-outcomes
			RespondToFlush(submission, result.report, result.err)
		}
	}()
	flush := func(ctx context.Context, result outcome) (*Report, error) {
		connection, _, err := Dial(ctx, root, "", OperationFlush)
		if err != nil {
			t.Fatal(err)
		}
		go func() { outcomes <- result }()
		return Flush(ctx, connection)
	}

	// Reports and failures are transmitted.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report := &Report{
		Conflicts: []Conflict{{
			Root:  "out",
			Alpha: []Change{{Path: "out", Old: "directory"}},
			Beta:  []Change{{Path: "out/log", Old: "file", New: "file"}},
		}},
		ExcludedConflicts: 2,
		Problems:          []Problem{{Alpha: true, Path: "secret", Error: "permission denied"}},
		ExcludedProblems:  1,
	}
	if received, err := flush(ctx, outcome{report: report}); err != nil || !reflect.DeepEqual(received, report) {
		t.Fatalf("report transmitted incorrectly: %+v, %v", received, err)
	} else if received, err := flush(ctx, outcome{}); err != nil || received == nil || !received.Empty() {
		t.Fatalf("missing report not treated as empty: %+v, %v", received, err)
	} else if _, err := flush(ctx, outcome{err: errors.New("synchronization stopped")}); err == nil || err.Error() != "synchronization stopped" {
		t.Fatal("flush failure not reported:", err)
	}

	// Cancellation abandons the wait.
	ctx, cancel = context.WithCancel(context.Background())
	connection, _, err := Dial(ctx, root, "", OperationFlush)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := Flush(ctx, connection); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not reported:", err)
	}
	select {
	case outcomes <- outcome{}:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned flush not received")
	}
}
