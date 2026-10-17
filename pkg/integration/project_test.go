package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
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
		s.Name, s.Labels, s.Paused, "",
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

// TestProjectApply tests applying project configuration to real sessions. This
// includes creation with flushing, idempotency against the session state
// reported by the managers, replacement with paused state retention, and
// removal.
func TestProjectApply(t *testing.T) {
	// Allow this test to run in parallel.
	t.Parallel()

	// Create a project identifier and a selection for its sessions, and ensure
	// that the project's sessions are terminated when we're done.
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

	// Create synchronization roots with content.
	directory := t.TempDir()
	for _, root := range []string{"alpha", "extra-alpha"} {
		if err := os.Mkdir(filepath.Join(directory, root), 0700); err != nil {
			t.Fatal("unable to create synchronization root:", err)
		} else if err := os.WriteFile(filepath.Join(directory, root, "file"), []byte(root), 0600); err != nil {
			t.Fatal("unable to create synchronization content:", err)
		}
	}

	// Define a helper that applies project configuration and returns the
	// printed plan.
	apply := func(contents string) string {
		t.Helper()
		path := filepath.Join(directory, project.DefaultConfigurationFileName)
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal("unable to write project configuration:", err)
		}
		configuration, err := project.LoadConfiguration(path)
		if err != nil {
			t.Fatal("unable to load project configuration:", err)
		}
		specifications, err := project.NewSessionSpecifications(configuration, nil, projectIdentifier, false)
		if err != nil {
			t.Fatal("unable to compute session specifications:", err)
		}
		forwardingStates, synchronizationStates, err := listProjectSessions(ctx, projectSelection)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := project.NewPlan(specifications, forwardingStates, synchronizationStates)
		if err != nil {
			t.Fatal("unable to compute plan:", err)
		}
		output := &bytes.Buffer{}
		plan.Print(output)
		if err := plan.Apply(&managerSessionOperations{ctx}); err != nil {
			t.Fatal("unable to apply plan:", err)
		}
		return output.String()
	}

	// Define a helper that reads synchronized content.
	readContent := func(root string) string {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(directory, root, "file"))
		if err != nil {
			t.Fatal("unable to read synchronized content:", err)
		}
		return string(content)
	}

	// Define a helper that indexes project sessions by name.
	sessions := func() (map[string]*forwarding.Session, map[string]*synchronization.Session) {
		t.Helper()
		forwardingStates, synchronizationStates, err := listProjectSessions(ctx, projectSelection)
		if err != nil {
			t.Fatal(err)
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
`, filepath.Join(directory, "alpha"), filepath.Join(directory, "beta"))
	if output := apply(initial); output != "Create forwarding session \"service\"\n"+
		"Create synchronization session \"code\"\n"+
		"Unchanged sessions: 0\n" {
		t.Fatalf("unexpected initial plan:\n%s", output)
	}
	if readContent("beta") != "alpha" {
		t.Fatal("initial synchronization did not complete")
	}
	initialForwarding, initialSynchronization := sessions()

	// Reapply the configuration. The sessions reported by the managers must
	// match their specifications, so nothing may change.
	if output := apply(initial); output != "Unchanged sessions: 2\n" {
		t.Fatalf("unexpected plan for unchanged configuration:\n%s", output)
	}
	forwardingSessions, synchronizationSessions := sessions()
	if forwardingSessions["service"].Identifier != initialForwarding["service"].Identifier ||
		synchronizationSessions["code"].Identifier != initialSynchronization["code"].Identifier {
		t.Fatal("unchanged session was recreated")
	}

	// Pause the synchronization session, then move its beta root and add a
	// session. The replacement must retain the paused state.
	if err := synchronizationManager.Pause(ctx, &selection.Selection{
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
		filepath.Join(directory, "alpha"), filepath.Join(directory, "replacement"),
		filepath.Join(directory, "extra-alpha"), filepath.Join(directory, "extra-beta"),
	)
	if output := apply(changed); output != "Replace synchronization session \"code\"\n"+
		"Create synchronization session \"extra\"\n"+
		"Unchanged sessions: 1\n" {
		t.Fatalf("unexpected plan for changed configuration:\n%s", output)
	}
	forwardingSessions, synchronizationSessions = sessions()
	if forwardingSessions["service"].Identifier != initialForwarding["service"].Identifier {
		t.Fatal("unchanged forwarding session was recreated")
	} else if code := synchronizationSessions["code"]; code.Identifier == initialSynchronization["code"].Identifier || !code.Paused {
		t.Fatal("synchronization session was not replaced or did not retain paused state")
	} else if readContent("extra-beta") != "extra-alpha" {
		t.Fatal("added session was not flushed")
	}

	// Remove all sessions.
	if output := apply("{}\n"); output != "Remove forwarding session \"service\"\n"+
		"Remove synchronization session \"code\"\n"+
		"Remove synchronization session \"extra\"\n"+
		"Unchanged sessions: 0\n" {
		t.Fatalf("unexpected plan for removal:\n%s", output)
	}
	if forwardingSessions, synchronizationSessions = sessions(); len(forwardingSessions) != 0 || len(synchronizationSessions) != 0 {
		t.Fatal("removed sessions still exist")
	}
}
