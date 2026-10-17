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
		"alpha configuration": func(s *synchronization.Session) {
			s.ConfigurationAlpha.WatchPollingInterval = 10
		},
		"beta configuration": func(s *synchronization.Session) {
			s.ConfigurationBeta.WatchPollingInterval = 10
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

// testSessionOperations records session operations and simulates failures.
type testSessionOperations struct {
	// operations records the operations performed.
	operations []string
	// failCreation specifies synchronization sessions whose creation fails.
	failCreation map[string]bool
	// failFlush indicates that flushing fails.
	failFlush bool
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
	if o.failFlush {
		return errors.New("flush failed")
	}
	return nil
}

// TestPlanApply verifies that obsolete sessions are terminated before creation,
// that replacements retain paused state, and that only running sessions
// created by the plan are flushed.
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
  remove: {}
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
  add: {}
`)
	forwardingStates, synchronizationStates := testSessionStates(current)
	for _, state := range synchronizationStates {
		state.Session.Paused = state.Session.Name == "replace"
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
		"terminate forwarding forward-remove,forward-replace",
		"terminate synchronization sync-remove,sync-replace",
		"create forwarding add paused=false",
		"create forwarding replace paused=false",
		"create synchronization add paused=false",
		"create synchronization replace paused=true",
		"flush synchronization new-add",
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
		operations := &testSessionOperations{failCreation: map[string]bool{"second": true}, failFlush: failFlush}
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
