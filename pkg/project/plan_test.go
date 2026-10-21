package project

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/forwarding"
	forwardingsvc "github.com/mutagen-io/mutagen/pkg/service/forwarding"
	synchronizationsvc "github.com/mutagen-io/mutagen/pkg/service/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// testSessionStates constructs the states of sessions created from
// specifications, as returned by the daemon.
func testSessionStates(specifications *SessionSpecifications) ([]*forwarding.State, []*synchronization.State) {
	var forwardingStates []*forwarding.State
	for _, s := range specifications.Forwarding {
		forwardingStates = append(forwardingStates, &forwarding.State{Session: &forwarding.Session{
			Identifier: "forward-" + s.Name, Name: s.Name, Labels: s.Labels,
			Source: s.Source, Destination: s.Destination,
			Configuration: s.Configuration, ConfigurationSource: s.ConfigurationSource,
			ConfigurationDestination: s.ConfigurationDestination, Paused: s.Paused,
		}})
	}
	var synchronizationStates []*synchronization.State
	for _, s := range specifications.Synchronization {
		synchronizationStates = append(synchronizationStates, &synchronization.State{Session: &synchronization.Session{
			Identifier: "sync-" + s.Name, Name: s.Name, Labels: s.Labels,
			Alpha: s.Alpha, Beta: s.Beta, Configuration: s.Configuration,
			ConfigurationAlpha: s.ConfigurationAlpha, ConfigurationBeta: s.ConfigurationBeta,
			Paused: s.Paused,
		}})
	}
	return forwardingStates, synchronizationStates
}

// testPlanOutput returns the printed form of a plan.
func testPlanOutput(plan *Plan) string {
	var output bytes.Buffer
	plan.Print(&output)
	return output.String()
}

// TestPlan verifies all actions, deterministic output, and retention of paused
// state by replacements without modification of desired specifications.
func TestPlan(t *testing.T) {
	current := testSpecifications(t, `
forward:
  defaults: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
  keep: {}
  replace: {}
  remove: {}
sync:
  defaults: {alpha: ./alpha, beta: ./beta}
  keep: {}
  replace: {}
  remove: {}
`)
	desired := testSpecifications(t, `
forward:
  defaults: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
  replace: {destination: "tcp:127.0.0.1:8002"}
  keep: {}
  add: {}
sync:
  defaults: {alpha: ./alpha, beta: ./beta}
  replace: {beta: ./other}
  keep: {flushOnCreate: true}
  add: {}
`)
	forwardingStates, synchronizationStates := testSessionStates(current)
	for _, state := range forwardingStates {
		state.Session.Paused = true
	}
	for _, state := range synchronizationStates {
		state.Session.Paused = true
	}
	plan, err := NewPlan(desired, forwardingStates, synchronizationStates)
	if err != nil {
		t.Fatal(err)
	}
	const expected = `Create forwarding session "add"
Remove forwarding session "remove"
Replace forwarding session "replace"
Create synchronization session "add"
Remove synchronization session "remove"
Replace synchronization session "replace"
Unchanged sessions: 2
Note: Synchronization session "remove" is paused and won't be flushed before it's removed, so if a new session synchronizes the same files, changes made since it was paused may be overwritten (two-way-resolved, one-way-replica) or become conflicts (two-way-safe, one-way-safe). Resume it before applying to synchronize them first.
Note: Synchronization session "replace" is paused and won't be flushed before it's replaced, so changes made since it was paused may be overwritten (two-way-resolved, one-way-replica) or become conflicts (two-way-safe, one-way-safe) once the replacement is resumed. Resume it before applying to synchronize them first.
`
	if output := testPlanOutput(plan); output != expected {
		t.Fatalf("unexpected plan:\n%s", output)
	}
	if plan.forwarding[1].current.Identifier != "forward-remove" ||
		plan.synchronization[1].current.Identifier != "sync-remove" {
		t.Error("removal did not retain exact session identifiers")
	}
	if !plan.forwarding[2].specification.Paused || !plan.synchronization[2].specification.Paused {
		t.Error("replacement did not retain paused state")
	}
	if plan.forwarding[0].specification.Paused || plan.synchronization[0].specification.Paused {
		t.Error("creation inherited paused state")
	}
	for _, s := range desired.Forwarding {
		if s.Paused {
			t.Error("planning modified desired forwarding specification:", s.Name)
		}
	}
	for _, s := range desired.Synchronization {
		if s.Paused {
			t.Error("planning modified desired synchronization specification:", s.Name)
		}
	}
}

// TestPlanNotes verifies that paused synchronization sessions that will be
// replaced or removed are noted, even if the plan doesn't create sessions, and
// that running, unchanged, and forwarding sessions are not.
func TestPlanNotes(t *testing.T) {
	current := testSpecifications(t, `
forward:
  defaults: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
  remove: {}
  replace: {}
sync:
  defaults: {alpha: ./alpha, beta: ./beta}
  keep: {}
  remove: {}
  replace: {}
`)
	desired := testSpecifications(t, `
forward:
  defaults: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
  replace: {destination: "tcp:127.0.0.1:8002"}
sync:
  defaults: {alpha: ./alpha, beta: ./beta}
  keep: {}
  replace: {beta: ./other}
`)
	forwardingStates, synchronizationStates := testSessionStates(current)
	for _, state := range forwardingStates {
		state.Session.Paused = true
	}

	// Running synchronization sessions and paused forwarding sessions are not
	// noted.
	plan, err := NewPlan(desired, forwardingStates, synchronizationStates)
	if err != nil {
		t.Fatal(err)
	} else if notes := plan.notes(); len(notes) > 0 {
		t.Error("running or forwarding sessions were noted:", notes)
	}

	// Paused synchronization sessions are noted when replaced or removed, but
	// not when unchanged.
	for _, state := range synchronizationStates {
		state.Session.Paused = true
	}
	plan, err = NewPlan(desired, forwardingStates, synchronizationStates)
	if err != nil {
		t.Fatal(err)
	} else if notes := plan.notes(); len(notes) != 2 ||
		!strings.HasPrefix(notes[0], `Synchronization session "remove" is paused and won't be flushed before it's removed,`) ||
		!strings.HasPrefix(notes[1], `Synchronization session "replace" is paused and won't be flushed before it's replaced,`) {
		t.Error("unexpected notes for paused sessions:", notes)
	}
}

// TestPlanConfigurationChanges verifies comparison of all endpoint and
// configuration components, including endpoint parameters.
func TestPlanConfigurationChanges(t *testing.T) {
	specifications := testSpecifications(t, `
forward:
  example: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
sync:
  example: {alpha: ./alpha, beta: ./beta}
`)
	forwardingStates, synchronizationStates := testSessionStates(specifications)
	forwardingOriginal := forwardingStates[0].Session
	synchronizationOriginal := synchronizationStates[0].Session
	if !forwardingSessionMatches(forwardingOriginal, specifications.Forwarding[0]) ||
		!synchronizationSessionMatches(synchronizationOriginal, specifications.Synchronization[0]) {
		t.Fatal("unchanged sessions do not match")
	}
	for name, mutate := range map[string]func(*forwarding.Session){
		"source":        func(s *forwarding.Session) { s.Source.Path = "tcp:127.0.0.1:9000" },
		"destination":   func(s *forwarding.Session) { s.Destination.Path = "tcp:127.0.0.1:9001" },
		"configuration": func(s *forwarding.Session) { s.Configuration.SocketPermissionMode = 0600 },
		"source configuration": func(s *forwarding.Session) {
			s.ConfigurationSource.SocketPermissionMode = 0600
		},
		"destination configuration": func(s *forwarding.Session) {
			s.ConfigurationDestination.SocketPermissionMode = 0600
		},
	} {
		t.Run("forwarding/"+name, func(t *testing.T) {
			changed := proto.Clone(forwardingOriginal).(*forwarding.Session)
			mutate(changed)
			if forwardingSessionMatches(changed, specifications.Forwarding[0]) {
				t.Fatal("configuration change was not detected")
			}
		})
	}
	for name, mutate := range map[string]func(*synchronization.Session){
		"alpha":         func(s *synchronization.Session) { s.Alpha.Path += "-changed" },
		"beta":          func(s *synchronization.Session) { s.Beta.Path += "-changed" },
		"configuration": func(s *synchronization.Session) { s.Configuration.MaximumEntryCount = 100 },
		"remote commands": func(s *synchronization.Session) {
			s.Configuration.RemoteCommandMode = synchronization.RemoteCommandMode_RemoteCommandModeBoth
		},
		"alpha configuration": func(s *synchronization.Session) {
			s.ConfigurationAlpha.WatchPollingInterval = 10
		},
		"beta configuration": func(s *synchronization.Session) {
			s.ConfigurationBeta.WatchPollingInterval = 10
		},
		"beta remote prologue": func(s *synchronization.Session) {
			s.ConfigurationBeta.RemotePrologue = "prologue.sh"
		},
		"parameters": func(s *synchronization.Session) {
			s.Beta.Parameters = map[string]string{"parameter": "value"}
		},
	} {
		t.Run("synchronization/"+name, func(t *testing.T) {
			changed := proto.Clone(synchronizationOriginal).(*synchronization.Session)
			mutate(changed)
			if synchronizationSessionMatches(changed, specifications.Synchronization[0]) {
				t.Fatal("configuration change was not detected")
			}
		})
	}
}

// TestEndpointMatching verifies that environment captured from the invoking
// shell and alternate paths to the same local location are not changes.
func TestEndpointMatching(t *testing.T) {
	// Docker environment variables come from the shell, not the project file.
	docker := &url.URL{Kind: url.Kind_Synchronization, Protocol: url.Protocol_Docker, Host: "container", Path: "/path"}
	withEnvironment := proto.Clone(docker).(*url.URL)
	withEnvironment.Environment = map[string]string{"DOCKER_CONTEXT": "remote"}
	if !endpointMatches(withEnvironment, docker) {
		t.Error("Docker environment difference was treated as a change")
	}
	moved := proto.Clone(docker).(*url.URL)
	moved.Path = "/other"
	if endpointMatches(moved, docker) {
		t.Error("Docker path change was not detected")
	}

	// Local paths may reach the same directory through a symbolic link.
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("unable to create symbolic link:", err)
	}
	for _, test := range []struct {
		kind             url.Kind
		current, desired string
		expected         bool
	}{
		{url.Kind_Synchronization, filepath.Join(link, "alpha"), filepath.Join(target, "alpha"), true},
		{url.Kind_Synchronization, filepath.Join(link, "missing", "alpha"), filepath.Join(target, "missing", "alpha"), true},
		{url.Kind_Synchronization, filepath.Join(link, "alpha"), filepath.Join(target, "beta"), false},
		{url.Kind_Synchronization, filepath.Join(root, "alpha"), filepath.Join(target, "alpha"), false},
		{url.Kind_Synchronization, link, target, false},
		{url.Kind_Forwarding, "unix:" + filepath.Join(link, "socket"), "unix:" + filepath.Join(target, "socket"), true},
		{url.Kind_Forwarding, "tcp:127.0.0.1:8000", "tcp:127.0.0.1:8001", false},
	} {
		current := &url.URL{Kind: test.kind, Protocol: url.Protocol_Local, Path: test.current}
		desired := &url.URL{Kind: test.kind, Protocol: url.Protocol_Local, Path: test.desired}
		if endpointMatches(current, desired) != test.expected {
			t.Errorf("incorrect match result for %s and %s", test.current, test.desired)
		}
	}
}

// TestPlanDuplicateNames verifies that undeclared duplicates are removed, that
// declared duplicates are rejected with their identifiers, and that names are
// scoped by session type.
func TestPlanDuplicateNames(t *testing.T) {
	specifications := testSpecifications(t, `
forward:
  same: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
sync:
  same: {alpha: ./alpha, beta: ./beta}
`)
	forwardingStates, synchronizationStates := testSessionStates(specifications)
	plan, err := NewPlan(specifications, forwardingStates, synchronizationStates)
	if err != nil || plan.unchanged != 2 {
		t.Fatal("same name across different session types was not accepted")
	}
	duplicate := proto.Clone(synchronizationStates[0]).(*synchronization.State)
	duplicate.Session.Identifier = "sync-same-duplicate"
	synchronizationStates = append(synchronizationStates, duplicate)
	if _, err := NewPlan(specifications, forwardingStates, synchronizationStates); err == nil ||
		!strings.Contains(err.Error(), "sync-same, sync-same-duplicate") {
		t.Fatal("declared duplicate names were not rejected with identifiers:", err)
	}
	undeclared := testSpecifications(t, `
forward:
  same: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
`)
	plan, err = NewPlan(undeclared, forwardingStates, synchronizationStates)
	if err != nil {
		t.Fatal("undeclared duplicate names were rejected:", err)
	} else if len(plan.synchronization) != 2 ||
		plan.synchronization[0].current.Identifier != "sync-same" ||
		plan.synchronization[1].current.Identifier != "sync-same-duplicate" ||
		plan.synchronization[0].specification != nil || plan.synchronization[1].specification != nil {
		t.Fatal("undeclared duplicate names were not removed")
	}
}

// errTestFlush is the error returned by testSessionOperations when a simulated
// flush fails.
var errTestFlush = errors.New("flush failed")

// testSessionOperations records session operations and simulates failures.
type testSessionOperations struct {
	// operations records the operations performed.
	operations []string
	// failCreation specifies synchronization sessions whose creation fails.
	failCreation map[string]bool
	// failFlush specifies synchronization sessions, by identifier, whose flush
	// fails.
	failFlush map[string]bool
}

func (o *testSessionOperations) TerminateForwarding(identifiers []string) error {
	o.operations = append(o.operations, "terminate forwarding "+strings.Join(identifiers, ","))
	return nil
}

func (o *testSessionOperations) TerminateSynchronization(identifiers []string) error {
	o.operations = append(o.operations, "terminate synchronization "+strings.Join(identifiers, ","))
	return nil
}

func (o *testSessionOperations) CreateForwarding(specification *forwardingsvc.CreationSpecification) error {
	o.operations = append(o.operations, fmt.Sprintf("create forwarding %s paused=%t", specification.Name, specification.Paused))
	return nil
}

func (o *testSessionOperations) CreateSynchronization(specification *synchronizationsvc.CreationSpecification) (string, error) {
	o.operations = append(o.operations, fmt.Sprintf("create synchronization %s paused=%t", specification.Name, specification.Paused))
	if o.failCreation[specification.Name] {
		return "", errors.New("creation failed")
	}
	return "new-" + specification.Name, nil
}

func (o *testSessionOperations) FlushSynchronization(identifiers []string) error {
	o.operations = append(o.operations, "flush synchronization "+strings.Join(identifiers, ","))
	for _, identifier := range identifiers {
		if o.failFlush[identifier] {
			return errTestFlush
		}
	}
	return nil
}

// TestPlanApply verifies that running synchronization sessions that will be
// terminated are flushed individually before any session is terminated, that
// paused and unchanged sessions are never flushed, that obsolete sessions are
// terminated before creation, that replacements retain paused state, and that
// running sessions created by the plan are flushed after creation.
func TestPlanApply(t *testing.T) {
	current := testSpecifications(t, `
forward:
  defaults: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
  keep: {}
  replace: {}
  remove: {}
sync:
  defaults: {alpha: ./alpha, beta: ./beta}
  keep: {}
  replace: {}
  replace-paused: {}
  remove: {}
  remove-paused: {}
`)
	desired := testSpecifications(t, `
forward:
  defaults: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
  keep: {}
  replace: {destination: "tcp:127.0.0.1:8002"}
  add: {}
sync:
  defaults: {alpha: ./alpha, beta: ./beta, flushOnCreate: true}
  keep: {}
  replace: {beta: ./other}
  replace-paused: {beta: ./other}
  add: {}
`)
	forwardingStates, synchronizationStates := testSessionStates(current)
	for _, state := range synchronizationStates {
		state.Session.Paused = strings.HasSuffix(state.Session.Name, "-paused")
	}
	plan, err := NewPlan(desired, forwardingStates, synchronizationStates)
	if err != nil {
		t.Fatal(err)
	}
	operations := &testSessionOperations{}
	if err := plan.Apply(operations); err != nil {
		t.Fatal(err)
	}
	expected := []string{
		"flush synchronization sync-remove",
		"flush synchronization sync-replace",
		"terminate forwarding forward-remove,forward-replace",
		"terminate synchronization sync-remove,sync-remove-paused,sync-replace,sync-replace-paused",
		"create forwarding add paused=false",
		"create forwarding replace paused=false",
		"create synchronization add paused=false",
		"create synchronization replace paused=false",
		"create synchronization replace-paused paused=true",
		"flush synchronization new-add,new-replace",
	}
	if strings.Join(operations.operations, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("unexpected operations:\n%s", strings.Join(operations.operations, "\n"))
	}

	// Applying a plan without changes must not perform any operations.
	forwardingStates, synchronizationStates = testSessionStates(desired)
	if plan, err = NewPlan(desired, forwardingStates, synchronizationStates); err != nil {
		t.Fatal(err)
	}
	operations = &testSessionOperations{}
	if err := plan.Apply(operations); err != nil || len(operations.operations) > 0 {
		t.Fatal("plan without changes performed operations:", operations.operations)
	}
}

// TestPlanApplyFlushFailure verifies that if a running synchronization session
// can't be flushed before it's terminated, then no session is terminated or
// created, and that the error identifies the session, wraps the cause, and
// explains how to proceed.
func TestPlanApplyFlushFailure(t *testing.T) {
	current := testSpecifications(t, `
forward:
  remove: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
sync:
  defaults: {alpha: ./alpha, beta: ./beta}
  remove: {}
  replace: {}
`)
	desired := testSpecifications(t, `
sync:
  defaults: {alpha: ./alpha, beta: ./beta, flushOnCreate: true}
  add: {}
  replace: {beta: ./other}
`)
	forwardingStates, synchronizationStates := testSessionStates(current)
	for _, test := range []struct {
		// failing is the identifier of the session whose flush fails.
		failing string
		// operations are the expected operations.
		operations []string
		// message is the expected error message.
		message string
	}{
		{
			"sync-remove",
			[]string{"flush synchronization sync-remove"},
			`unable to flush synchronization session "remove" (sync-remove) before removing it: flush failed; ` +
				`removing it now could let a new session for the same files overwrite its unsynchronized changes, ` +
				`so no sessions were terminated or created: fix the session and apply again, or terminate it with ` +
				`"mutagen sync terminate sync-remove" and apply again if its unsynchronized changes don't matter`,
		},
		{
			"sync-replace",
			[]string{"flush synchronization sync-remove", "flush synchronization sync-replace"},
			`unable to flush synchronization session "replace" (sync-replace) before replacing it: flush failed; ` +
				`replacing it now could overwrite its unsynchronized changes, so no sessions were terminated or ` +
				`created: fix the session and apply again, or terminate it with "mutagen sync terminate sync-replace" ` +
				`and apply again if its unsynchronized changes don't matter`,
		},
	} {
		plan, err := NewPlan(desired, forwardingStates, synchronizationStates)
		if err != nil {
			t.Fatal(err)
		}
		operations := &testSessionOperations{failFlush: map[string]bool{test.failing: true}}
		err = plan.Apply(operations)
		if err == nil {
			t.Fatal("flush failure was not reported")
		} else if err.Error() != test.message {
			t.Error("unexpected flush failure message:", err)
		} else if !errors.Is(err, errTestFlush) {
			t.Error("flush failure does not wrap its cause")
		}
		if strings.Join(operations.operations, "\n") != strings.Join(test.operations, "\n") {
			t.Errorf("unexpected operations:\n%s", strings.Join(operations.operations, "\n"))
		}
	}
}

// TestPlanApplyPartialFailure verifies that sessions created before a failed
// creation are still flushed, since subsequent plans consider them unchanged,
// and that both creation and flush failures are reported.
func TestPlanApplyPartialFailure(t *testing.T) {
	specifications := testSpecifications(t, `
sync:
  defaults: {alpha: ./alpha, beta: ./beta, flushOnCreate: true}
  first: {}
  second: {}
  third: {}
`)
	for _, failFlush := range []bool{false, true} {
		plan, err := NewPlan(specifications, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		operations := &testSessionOperations{
			failCreation: map[string]bool{"second": true},
			failFlush:    map[string]bool{"new-first": failFlush},
		}
		err = plan.Apply(operations)
		if err == nil || !strings.Contains(err.Error(), "unable to create synchronization session (second)") {
			t.Fatal("creation failure was not reported:", err)
		} else if failFlush != strings.Contains(err.Error(), "unable to flush synchronization sessions") {
			t.Fatal("incorrect flush failure reporting:", err)
		}
		expected := []string{
			"create synchronization first paused=false",
			"create synchronization second paused=false",
			"flush synchronization new-first",
		}
		if strings.Join(operations.operations, "\n") != strings.Join(expected, "\n") {
			t.Fatalf("unexpected operations:\n%s", strings.Join(operations.operations, "\n"))
		}
	}
}
