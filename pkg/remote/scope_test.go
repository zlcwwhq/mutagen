package remote

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// registerScopeTest registers the scope of a synchronization endpoint (see
// RegisterScope) for the duration of a test.
func registerScopeTest(t *testing.T, session string, alpha bool, root string, ignores *Ignores) io.Closer {
	t.Helper()
	scope, err := RegisterScope(session, alpha, root, ignores)
	if err != nil {
		t.Fatal("unable to register scope:", err)
	}
	t.Cleanup(func() { scope.Close() })
	return scope
}

// loadScopes loads the scope registrations.
func loadScopes(t *testing.T) []*registration {
	t.Helper()
	directory, err := filesystem.Mutagen(false, scopeDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	registrations, err := loadRegistrations(directory)
	if err != nil {
		t.Fatal(err)
	}
	return registrations
}

func TestRegisterScope(t *testing.T) {
	registry := useTestRegistry(t)
	root := resolveSymbolicLinks(t.TempDir())
	mkdirs(t, filepath.Join(root, "sub"))

	// Scopes are recorded with normalized roots in their own registry, rather
	// than the command registry, so that older versions of remote never
	// mistake them for endpoints that accept submissions.
	ignores := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"results"}}
	scope := registerScopeTest(t, "sync_scope", true, filepath.Join(root, "sub", ".."), ignores)
	if scopes := loadScopes(t); len(scopes) != 1 {
		t.Fatalf("incorrect scope registrations: %d", len(scopes))
	} else if r := scopes[0]; r.Version != mutagen.Version || r.Session != "sync_scope" || !r.Alpha || r.Root != root ||
		r.Name != "" || r.Ignores == nil || !slices.Equal(r.Ignores.Patterns, ignores.Patterns) {
		t.Errorf("incorrect scope registration: %+v", r.record)
	} else if registrations, _ := loadRegistrations(registry); len(registrations) != 0 {
		t.Error("scope registered in the command registry")
	}

	// Closure removes the registration.
	if err := scope.Close(); err != nil {
		t.Fatal("unable to close scope registration:", err)
	} else if scopes := loadScopes(t); len(scopes) != 0 {
		t.Error("scope registration not removed")
	}
}

func TestSynchronizingScopes(t *testing.T) {
	useTestRegistry(t)
	base := resolveSymbolicLinks(t.TempDir())
	outer, other, otherBeta, dead := filepath.Join(base, "outer"), filepath.Join(base, "other"), filepath.Join(base, "other-beta"), filepath.Join(base, "dead")
	results, nested := filepath.Join(outer, "results"), filepath.Join(outer, "cache", "nested")
	mkdirs(t, filepath.Join(outer, "src"), filepath.Join(results, "data"), nested, other, otherBeta, dead)
	var lock sync.Mutex
	var accepted []string

	// The outer session runs remote commands and ignores results and cache,
	// in which the results and nested sessions, which don't, synchronize
	// content. The other session runs remote commands from its beta endpoint
	// only, and only its alpha endpoint's scope is registered.
	ignores := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"results", "cache"}}
	registerTest(t, &Endpoint{Session: "sync_outer", Name: "outer", Root: outer, Alpha: true, Ignores: ignores}, "outer", &accepted, &lock)
	registerScopeTest(t, "sync_outer", true, outer, ignores)
	registerScopeTest(t, "sync_results", false, results, &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"*.tmp"}})
	registerScopeTest(t, "sync_nested", true, nested, nil)
	registerTest(t, &Endpoint{Session: "sync_other", Name: "other", Root: otherBeta}, "other", &accepted, &lock)
	registerScopeTest(t, "sync_other", true, other, nil)
	synchronizing := func(path string, directory bool) []string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sessions, err := Synchronizing(ctx, path, directory)
		if err != nil {
			t.Fatalf("unable to determine the sessions synchronizing %s: %v", path, err)
		}
		return sessions
	}

	// Sessions known only from their scopes are related to paths as those
	// known from command registrations are: by roots that contain the path
	// unless their ignores exclude it, and, for directories, by roots beneath
	// it. Sessions are listed once, labeled by name if a command registration
	// of the same session provides one, and by identifier otherwise.
	for _, test := range []struct {
		path      string
		directory bool
		sessions  []string
	}{
		{filepath.Join(outer, "src", "x"), false, []string{"outer (sync_outer)"}},
		{filepath.Join(results, "data", "x"), false, []string{"sync_results"}},
		{filepath.Join(results, "data"), true, []string{"sync_results"}},
		{results, true, []string{"sync_results"}},
		{filepath.Join(results, "data", "x.tmp"), false, nil},
		{filepath.Join(outer, "cache"), true, []string{"sync_nested"}},
		{filepath.Join(outer, "cache"), false, nil},
		{filepath.Join(outer, "cache", "x"), false, nil},
		{filepath.Join(other, "x"), false, []string{"other (sync_other)"}},
		{filepath.Join(base, "x"), false, nil},
	} {
		if sessions := synchronizing(test.path, test.directory); !slices.Equal(sessions, test.sessions) {
			t.Errorf("incorrect sessions synchronizing %s (directory: %t): %v", test.path, test.directory, sessions)
		}
	}

	// Dead scope registrations are pruned.
	stale := registerScopeTest(t, "sync_dead", true, dead, nil)
	stale.(*scopeRegistration).listener.(*registeredListener).Listener.Close()
	if sessions := synchronizing(filepath.Join(dead, "x"), false); len(sessions) != 0 {
		t.Errorf("dead scope counted as synchronizing: %v", sessions)
	} else if slices.ContainsFunc(loadScopes(t), func(r *registration) bool { return r.Session == "sync_dead" }) {
		t.Error("dead scope registration not pruned")
	}

	// Scopes don't accept submissions, so they're never selected: submissions
	// from the results session's root are made through the outer session, and
	// selecting the results session fails.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if connection, target, err := (&Selection{}).Dial(ctx, filepath.Join(results, "data"), OperationExecute); err != nil {
		t.Error("unable to dial from inside a scope:", err)
	} else {
		connection.Close()
		if target.Session != "sync_outer" || target.Path != "results/data" {
			t.Errorf("incorrect target: %+v", target)
		}
	}
	if _, _, err := (&Selection{Target: &Target{SourceSession: "sync_results", SourceAlpha: true}}).Dial(ctx, filepath.Join(results, "data"), OperationExecute); err == nil ||
		!strings.Contains(err.Error(), "no longer connected") {
		t.Error("session known only from its scope selected:", err)
	}
	if target, err := (&Selection{}).Locate(ctx, nested); err != nil || target.Session != "sync_outer" {
		t.Errorf("incorrect session located inside a scope: %+v, %v", target, err)
	}
}
