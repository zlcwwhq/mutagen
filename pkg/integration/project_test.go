package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/forwarding"
	"github.com/mutagen-io/mutagen/pkg/identifier"
	"github.com/mutagen-io/mutagen/pkg/project"
	"github.com/mutagen-io/mutagen/pkg/selection"
	forwardingsvc "github.com/mutagen-io/mutagen/pkg/service/forwarding"
	synchronizationsvc "github.com/mutagen-io/mutagen/pkg/service/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
)

// managerSessionOperations implements project.SessionOperations using the
// session managers of the integration testing daemon.
type managerSessionOperations struct {
	// ctx is the context for operations.
	ctx context.Context
}

func (o *managerSessionOperations) TerminateForwarding(identifiers []string) error {
	return forwardingManager.Terminate(o.ctx, &selection.Selection{Specifications: identifiers}, "")
}

func (o *managerSessionOperations) TerminateSynchronization(identifiers []string) error {
	return synchronizationManager.Terminate(o.ctx, &selection.Selection{Specifications: identifiers}, "")
}

func (o *managerSessionOperations) CreateForwarding(s *forwardingsvc.CreationSpecification) error {
	_, err := forwardingManager.Create(o.ctx,
		s.Source, s.Destination,
		s.Configuration, s.ConfigurationSource, s.ConfigurationDestination,
		s.Name, s.Labels, s.Paused, "",
	)
	return err
}

func (o *managerSessionOperations) CreateSynchronization(s *synchronizationsvc.CreationSpecification) (string, error) {
	return synchronizationManager.Create(o.ctx,
		s.Alpha, s.Beta,
		s.Configuration, s.ConfigurationAlpha, s.ConfigurationBeta,
		s.Name, s.Labels, s.Star, s.Paused, "",
	)
}

func (o *managerSessionOperations) FlushSynchronization(identifiers []string) error {
	// Sessions only accept flush requests once their synchronization loops
	// have started, which happens asynchronously after creation. When going
	// through the daemon, the latency of the flush request covers this, but
	// here we need to wait explicitly.
	selection := &selection.Selection{Specifications: identifiers}
	ctx, cancel := context.WithTimeout(o.ctx, time.Minute)
	defer cancel()
	var previousStateIndex uint64
	for ready := false; !ready; {
		stateIndex, states, err := synchronizationManager.List(ctx, selection, previousStateIndex)
		if err != nil {
			return fmt.Errorf("unable to wait for synchronization: %w", err)
		}
		previousStateIndex, ready = stateIndex, true
		for _, state := range states {
			ready = ready && state.Status >= synchronization.Status_Watching
		}
	}
	return synchronizationManager.Flush(o.ctx, selection, "", false)
}

// listProjectSessions lists the states of a project's sessions.
func listProjectSessions(ctx context.Context, projectSelection *selection.Selection) ([]*forwarding.State, []*synchronization.State, error) {
	_, forwardingStates, err := forwardingManager.List(ctx, projectSelection, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to list forwarding sessions: %w", err)
	}
	_, synchronizationStates, err := synchronizationManager.List(ctx, projectSelection, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to list synchronization sessions: %w", err)
	}
	return forwardingStates, synchronizationStates, nil
}

// projectFixture applies configuration for a project in a temporary directory
// to real sessions on behalf of a test. The project's sessions are terminated
// when the test completes.
type projectFixture struct {
	// t is the test using the fixture.
	t *testing.T
	// ctx is the context for operations.
	ctx context.Context
	// directory is the project directory.
	directory string
	// identifier is the project identifier.
	identifier string
	// selection selects the project's sessions.
	selection *selection.Selection
}

// newProjectFixture creates a project fixture.
func newProjectFixture(t *testing.T) *projectFixture {
	t.Helper()

	// Create the project directory. Since cleanup functions run in reverse
	// order, this ensures that it's removed after the project's sessions are
	// terminated.
	directory := t.TempDir()

	// Create a project identifier and a selection for its sessions, and ensure
	// that the project's sessions are terminated when the test completes.
	ctx := context.Background()
	projectIdentifier, err := identifier.New(identifier.PrefixProject)
	if err != nil {
		t.Fatal("unable to create project identifier:", err)
	}
	projectSelection := &selection.Selection{
		LabelSelector: fmt.Sprintf("%s=%s", project.LabelKey, projectIdentifier),
	}
	t.Cleanup(func() {
		forwardingManager.Terminate(ctx, projectSelection, "")
		synchronizationManager.Terminate(ctx, projectSelection, "")
	})

	// Done.
	return &projectFixture{
		t:          t,
		ctx:        ctx,
		directory:  directory,
		identifier: projectIdentifier,
		selection:  projectSelection,
	}
}

// apply applies project configuration and returns the printed plan.
func (f *projectFixture) apply(contents string) string {
	f.t.Helper()
	path := filepath.Join(f.directory, project.DefaultConfigurationFileName)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		f.t.Fatal("unable to write project configuration:", err)
	}
	configuration, err := project.LoadConfiguration(path)
	if err != nil {
		f.t.Fatal("unable to load project configuration:", err)
	}
	specifications, err := project.NewSessionSpecifications(configuration, nil, f.identifier, false)
	if err != nil {
		f.t.Fatal("unable to compute session specifications:", err)
	}
	forwardingStates, synchronizationStates, err := listProjectSessions(f.ctx, f.selection)
	if err != nil {
		f.t.Fatal(err)
	}
	plan, err := project.NewPlan(specifications, forwardingStates, synchronizationStates)
	if err != nil {
		f.t.Fatal("unable to compute plan:", err)
	}
	output := &bytes.Buffer{}
	plan.Print(output)
	if err := plan.Apply(&managerSessionOperations{f.ctx}); err != nil {
		f.t.Fatal("unable to apply plan:", err)
	}
	return output.String()
}

// sessions returns the project's sessions indexed by name.
func (f *projectFixture) sessions() (map[string]*forwarding.Session, map[string]*synchronization.Session) {
	f.t.Helper()
	forwardingStates, synchronizationStates, err := listProjectSessions(f.ctx, f.selection)
	if err != nil {
		f.t.Fatal(err)
	}
	forwardingSessions := make(map[string]*forwarding.Session)
	for _, state := range forwardingStates {
		forwardingSessions[state.Session.Name] = state.Session
	}
	synchronizationSessions := make(map[string]*synchronization.Session)
	for _, state := range synchronizationStates {
		synchronizationSessions[state.Session.Name] = state.Session
	}
	return forwardingSessions, synchronizationSessions
}

// writeContent writes content to a file at a path relative to the project
// directory.
func (f *projectFixture) writeContent(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.directory, path), []byte(content), 0600); err != nil {
		f.t.Fatal("unable to write content:", err)
	}
}

// readContent reads the content of a file at a path relative to the project
// directory.
func (f *projectFixture) readContent(path string) string {
	f.t.Helper()
	content, err := os.ReadFile(filepath.Join(f.directory, path))
	if err != nil {
		f.t.Fatal("unable to read content:", err)
	}
	return string(content)
}

// TestProjectApply tests applying project configuration to real sessions. This
// includes creation with flushing, idempotency against the session state
// reported by the managers, replacement with paused state retention, and
// removal.
func TestProjectApply(t *testing.T) {
	// Allow this test to run in parallel.
	t.Parallel()

	// Create the project.
	fixture := newProjectFixture(t)

	// Create synchronization roots with content.
	for _, root := range []string{"alpha", "extra-alpha"} {
		if err := os.Mkdir(filepath.Join(fixture.directory, root), 0700); err != nil {
			t.Fatal("unable to create synchronization root:", err)
		}
		fixture.writeContent(filepath.Join(root, "file"), root)
	}

	// Create the initial sessions. With watching disabled, content is only
	// synchronized if sessions are flushed on creation.
	initial := fmt.Sprintf(`
forward:
  service: {source: "tcp:127.0.0.1:0", destination: "tcp:127.0.0.1:65000"}
sync:
  defaults:
    watch: {mode: no-watch}
    flushOnCreate: true
  code: {alpha: '%s', beta: '%s'}
`, filepath.Join(fixture.directory, "alpha"), filepath.Join(fixture.directory, "beta"))
	if output := fixture.apply(initial); output != "Create forwarding session \"service\"\n"+
		"Create synchronization session \"code\"\n"+
		"Unchanged sessions: 0\n" {
		t.Fatalf("unexpected initial plan:\n%s", output)
	}
	if fixture.readContent(filepath.Join("beta", "file")) != "alpha" {
		t.Fatal("initial synchronization did not complete")
	}
	initialForwarding, initialSynchronization := fixture.sessions()

	// Reapply the configuration. The sessions reported by the managers must
	// match their specifications, so nothing may change.
	if output := fixture.apply(initial); output != "Unchanged sessions: 2\n" {
		t.Fatalf("unexpected plan for unchanged configuration:\n%s", output)
	}
	forwardingSessions, synchronizationSessions := fixture.sessions()
	if forwardingSessions["service"].Identifier != initialForwarding["service"].Identifier ||
		synchronizationSessions["code"].Identifier != initialSynchronization["code"].Identifier {
		t.Fatal("unchanged session was recreated")
	}

	// Pause the synchronization session, then move its beta root and add a
	// session. The replacement must retain the paused state, and the plan
	// must note that the paused session won't be flushed.
	if err := synchronizationManager.Pause(fixture.ctx, &selection.Selection{
		Specifications: []string{initialSynchronization["code"].Identifier},
	}, ""); err != nil {
		t.Fatal("unable to pause session:", err)
	}
	changed := fmt.Sprintf(`
forward:
  service: {source: "tcp:127.0.0.1:0", destination: "tcp:127.0.0.1:65000"}
sync:
  defaults:
    watch: {mode: no-watch}
    flushOnCreate: true
  code: {alpha: '%s', beta: '%s'}
  extra: {alpha: '%s', beta: '%s'}
`,
		filepath.Join(fixture.directory, "alpha"), filepath.Join(fixture.directory, "replacement"),
		filepath.Join(fixture.directory, "extra-alpha"), filepath.Join(fixture.directory, "extra-beta"),
	)
	if output := fixture.apply(changed); !strings.HasPrefix(output, "Replace synchronization session \"code\"\n"+
		"Create synchronization session \"extra\"\n"+
		"Unchanged sessions: 1\n"+
		"Note: Synchronization session \"code\" is paused and won't be flushed before it's replaced,") ||
		strings.Count(output, "\n") != 4 {
		t.Fatalf("unexpected plan for changed configuration:\n%s", output)
	}
	forwardingSessions, synchronizationSessions = fixture.sessions()
	if forwardingSessions["service"].Identifier != initialForwarding["service"].Identifier {
		t.Fatal("unchanged forwarding session was recreated")
	} else if code := synchronizationSessions["code"]; code.Identifier == initialSynchronization["code"].Identifier || !code.Paused {
		t.Fatal("synchronization session was not replaced or did not retain paused state")
	} else if fixture.readContent(filepath.Join("extra-beta", "file")) != "extra-alpha" {
		t.Fatal("added session was not flushed")
	}

	// Remove all sessions. The running session is flushed before it's
	// removed, which synchronizes an unsynchronized change on its beta
	// endpoint, while the paused session isn't, which the plan notes.
	fixture.writeContent(filepath.Join("extra-beta", "file"), "extra-beta")
	if output := fixture.apply("{}\n"); !strings.HasPrefix(output, "Remove forwarding session \"service\"\n"+
		"Remove synchronization session \"code\"\n"+
		"Remove synchronization session \"extra\"\n"+
		"Unchanged sessions: 0\n"+
		"Note: Synchronization session \"code\" is paused and won't be flushed before it's removed,") ||
		strings.Count(output, "\n") != 5 {
		t.Fatalf("unexpected plan for removal:\n%s", output)
	}
	if forwardingSessions, synchronizationSessions = fixture.sessions(); len(forwardingSessions) != 0 || len(synchronizationSessions) != 0 {
		t.Fatal("removed sessions still exist")
	} else if fixture.readContent(filepath.Join("extra-alpha", "file")) != "extra-beta" {
		t.Fatal("removed session was not flushed")
	}
}

// TestProjectApplyFlushesTerminatedSessions tests that changes that a running
// synchronization session hasn't synchronized yet survive its replacement and
// its renaming. Both start a session with fresh history over the same roots,
// which in two-way-resolved mode resolves every difference between the
// endpoints in favor of alpha, so apply must flush the session first. Remote
// command settings don't affect synchronization, but changing them still
// replaces the session.
func TestProjectApplyFlushesTerminatedSessions(t *testing.T) {
	// Allow this test to run in parallel.
	t.Parallel()

	// Create the project and a synchronization root with content.
	fixture := newProjectFixture(t)
	if err := os.Mkdir(filepath.Join(fixture.directory, "alpha"), 0700); err != nil {
		t.Fatal("unable to create synchronization root:", err)
	}
	fixture.writeContent(filepath.Join("alpha", "results"), "initial")

	// Define a helper that computes the project configuration for a session
	// with the specified name and additional settings. With watching
	// disabled, changes are only synchronized by flushes.
	configuration := func(name, settings string) string {
		return fmt.Sprintf(`
sync:
  %s:
    alpha: '%s'
    beta: '%s'
    mode: two-way-resolved
    watch: {mode: no-watch}
    flushOnCreate: true
%s`,
			name, filepath.Join(fixture.directory, "alpha"), filepath.Join(fixture.directory, "beta"), settings,
		)
	}

	// Create the session and perform the initial synchronization.
	if output := fixture.apply(configuration("code", "")); output != "Create synchronization session \"code\"\n"+
		"Unchanged sessions: 0\n" {
		t.Fatalf("unexpected initial plan:\n%s", output)
	} else if fixture.readContent(filepath.Join("beta", "results")) != "initial" {
		t.Fatal("initial synchronization did not complete")
	}
	_, initial := fixture.sessions()

	// Modify the file on beta without synchronizing the change, then change
	// only the session's remote command setting. Since the replacement is
	// flushed on creation, both endpoints must have the change afterwards.
	fixture.writeContent(filepath.Join("beta", "results"), "replacement")
	if output := fixture.apply(configuration("code", "    remote: beta\n")); output != "Replace synchronization session \"code\"\n"+
		"Unchanged sessions: 0\n" {
		t.Fatalf("unexpected plan for remote command setting change:\n%s", output)
	}
	_, replaced := fixture.sessions()
	if code := replaced["code"]; code == nil || code.Identifier == initial["code"].Identifier ||
		code.Configuration.RemoteCommandMode != synchronization.RemoteCommandMode_RemoteCommandModeBeta {
		t.Fatal("session was not replaced")
	}
	for _, root := range []string{"alpha", "beta"} {
		if content := fixture.readContent(filepath.Join(root, "results")); content != "replacement" {
			t.Errorf("unsynchronized change was lost on %s after replacement: found %q", root, content)
		}
	}

	// Repeat the experiment by renaming the session, which removes it and
	// creates another session over the same roots.
	fixture.writeContent(filepath.Join("beta", "results"), "rename")
	if output := fixture.apply(configuration("renamed", "    remote: beta\n")); output != "Remove synchronization session \"code\"\n"+
		"Create synchronization session \"renamed\"\n"+
		"Unchanged sessions: 0\n" {
		t.Fatalf("unexpected plan for renaming:\n%s", output)
	}
	for _, root := range []string{"alpha", "beta"} {
		if content := fixture.readContent(filepath.Join(root, "results")); content != "rename" {
			t.Errorf("unsynchronized change was lost on %s after renaming: found %q", root, content)
		}
	}
}

// TestProjectApplyConvergesSharedSessions tests that replacing sessions that
// share workspaces doesn't resurrect a deleted file. A flush only makes the
// endpoints of its own session agree, so if a later flush brings a deletion
// into a shared workspace, a session that was flushed before doesn't propagate
// it. Since replacements start with fresh history, they would copy the stale
// file back to every workspace unless apply keeps flushing the sessions that
// share workspaces until the deletion has reached all of them. Sessions are
// flushed in name order, so the deletion is made at the end whose session is
// flushed last. Watching is disabled, so only flushes synchronize changes.
func TestProjectApplyConvergesSharedSessions(t *testing.T) {
	// Allow this test to run in parallel.
	t.Parallel()

	// Define the scenarios.
	for _, test := range []struct {
		// name is the scenario name.
		name string
		// roots are the synchronization roots, relative to the project
		// directory.
		roots []string
		// configuration computes the project configuration from the absolute
		// paths of the roots and additional synchronization settings, which
		// replace every session when they change.
		configuration func(roots map[string]string, settings string) string
		// deleted is the root where the file is deleted.
		deleted string
	}{
		{
			// A chain of two pairs that share the middle root. The deletion
			// on right reaches middle only when right is flushed, after left.
			name:  "chain",
			roots: []string{"left", "middle", "right"},
			configuration: func(roots map[string]string, settings string) string {
				return fmt.Sprintf(`
sync:
  defaults:
    watch: {mode: no-watch}
    flushOnCreate: true
%s
  left: {alpha: '%s', beta: '%s'}
  right: {alpha: '%s', beta: '%s'}
`, settings, roots["left"], roots["middle"], roots["middle"], roots["right"])
			},
			deleted: "right",
		},
		{
			// A star whose edges are all replaced. The deletion on server
			// reaches the center only when star-server is flushed, after
			// star-gpu.
			name:  "star",
			roots: []string{"local", "server", "gpu"},
			configuration: func(roots map[string]string, settings string) string {
				return fmt.Sprintf(`
star:
  sync:
    watch: {mode: no-watch}
%s
  center: local
  nodes:
    local: {endpoint: '%s'}
    server: {endpoint: '%s'}
    gpu: {endpoint: '%s'}
`, settings, roots["local"], roots["server"], roots["gpu"])
			},
			deleted: "server",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Allow scenarios to run in parallel.
			t.Parallel()

			// Create the project and the synchronization roots, each with
			// the same file, so that the initial sessions agree on it.
			fixture := newProjectFixture(t)
			roots := make(map[string]string, len(test.roots))
			for _, root := range test.roots {
				roots[root] = filepath.Join(fixture.directory, root)
				if err := os.Mkdir(roots[root], 0700); err != nil {
					t.Fatal("unable to create synchronization root:", err)
				}
				fixture.writeContent(filepath.Join(root, "file"), "content")
			}

			// Define a helper that flushes all of the project's sessions in
			// name order.
			flush := func() {
				t.Helper()
				_, sessions := fixture.sessions()
				names := make([]string, 0, len(sessions))
				for name := range sessions {
					names = append(names, name)
				}
				sort.Strings(names)
				var identifiers []string
				for _, name := range names {
					identifiers = append(identifiers, sessions[name].Identifier)
					logSessionOnFailure(t, sessions[name].Identifier)
				}
				if err := (&managerSessionOperations{fixture.ctx}).FlushSynchronization(identifiers); err != nil {
					t.Fatal("unable to flush sessions:", err)
				}
			}

			// Create the sessions and synchronize them, which records the
			// file in their histories.
			fixture.apply(test.configuration(roots, ""))
			flush()

			// Delete the file without synchronizing the deletion, then
			// replace every session by changing their ignores.
			if err := os.Remove(filepath.Join(roots[test.deleted], "file")); err != nil {
				t.Fatal("unable to delete file:", err)
			}
			output := fixture.apply(test.configuration(roots, "    ignore: {paths: [unused]}"))
			if strings.Count(output, "Replace synchronization session") != len(test.roots)-1 ||
				!strings.HasSuffix(output, "Unchanged sessions: 0\n") {
				t.Fatalf("unexpected plan for replacement:\n%s", output)
			}

			// Synchronize the replacements, which start with fresh history,
			// and verify that the deletion reached every root.
			flush()
			for _, root := range test.roots {
				if _, err := os.Lstat(filepath.Join(roots[root], "file")); err == nil {
					t.Errorf("deleted file was restored on %s", root)
				} else if !os.IsNotExist(err) {
					t.Errorf("unable to inspect file on %s: %v", root, err)
				}
			}
		})
	}
}
