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
			Identifier: "sync-" + s.Name, Name: s.Name, Labels: s.Labels, Star: s.Star,
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
	// failFlushAt specifies synchronization sessions, by identifier, whose
	// flush fails only when it's requested for the specified time, counting
	// from one.
	failFlushAt map[string]int
	// flushes counts the flush requests by identifier.
	flushes map[string]int
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
	if o.flushes == nil {
		o.flushes = make(map[string]int)
	}
	for _, identifier := range identifiers {
		o.flushes[identifier]++
		if o.failFlush[identifier] || o.failFlushAt[identifier] == o.flushes[identifier] {
			return errTestFlush
		}
	}
	return nil
}

// TestPlanApply verifies that running synchronization sessions that will be
// terminated, and that share no workspace with another session, are flushed
// individually and once before any session is terminated, that paused and
// unchanged sessions are never flushed if they share no workspace with a
// running session that will be terminated, that obsolete sessions are
// terminated before creation, that replacements retain paused state, and that
// running sessions created by the plan are flushed after creation.
func TestPlanApply(t *testing.T) {
	t.Chdir(t.TempDir())
	current := testSpecifications(t, `
forward:
  defaults: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
  keep: {}
  replace: {}
  remove: {}
sync:
  keep: {alpha: ./keep/alpha, beta: ./keep/beta}
  replace: {alpha: ./replace/alpha, beta: ./replace/beta}
  replace-paused: {alpha: ./replace-paused/alpha, beta: ./replace-paused/beta}
  remove: {alpha: ./remove/alpha, beta: ./remove/beta}
  remove-paused: {alpha: ./remove-paused/alpha, beta: ./remove-paused/beta}
`)
	desired := testSpecifications(t, `
forward:
  defaults: {source: "tcp:127.0.0.1:8000", destination: "tcp:127.0.0.1:8001"}
  keep: {}
  replace: {destination: "tcp:127.0.0.1:8002"}
  add: {}
sync:
  defaults: {flushOnCreate: true}
  keep: {alpha: ./keep/alpha, beta: ./keep/beta}
  replace: {alpha: ./replace/alpha, beta: ./replace/other}
  replace-paused: {alpha: ./replace-paused/alpha, beta: ./replace-paused/other}
  add: {alpha: ./add/alpha, beta: ./add/beta}
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

// testApply computes a plan that reconciles the sessions created from a
// current project configuration with a desired one and applies it using
// operations. If prepare is non-nil, then it can modify the states of the
// current synchronization sessions first. Relative paths are resolved against
// the current directory.
func testApply(t *testing.T, current, desired string, prepare func([]*synchronization.State), operations *testSessionOperations) error {
	t.Helper()
	_, states := testSessionStates(testSpecifications(t, current))
	if prepare != nil {
		prepare(states)
	}
	plan, err := NewPlan(testSpecifications(t, desired), nil, states)
	if err != nil {
		t.Fatal(err)
	}
	return plan.Apply(operations)
}

// testPause returns a function that pauses the named sessions.
func testPause(names ...string) func([]*synchronization.State) {
	return func(states []*synchronization.State) {
		for _, state := range states {
			for _, name := range names {
				if state.Session.Name == name {
					state.Session.Paused = true
				}
			}
		}
	}
}

// testCheckOperations verifies the operations performed.
func testCheckOperations(t *testing.T, operations *testSessionOperations, expected []string) {
	t.Helper()
	if strings.Join(operations.operations, "\n") != strings.Join(expected, "\n") {
		t.Errorf("unexpected operations:\n%s", strings.Join(operations.operations, "\n"))
	}
}

// TestPlanApplyConvergesStar verifies that replacing every edge of a star
// flushes all of its running edges in two rounds before any session is
// terminated, however many there are, since they all share the center and
// their workspaces form a tree, and that paused edges aren't flushed.
func TestPlanApplyConvergesStar(t *testing.T) {
	t.Chdir(t.TempDir())
	const star = `
star:
  center: local
  sync: {ignore: {paths: [%s]}}
  nodes:
    local: {endpoint: ./local}
    server: {endpoint: ./server}
    gpu: {endpoint: ./gpu}
    cloud: {endpoint: ./cloud}
    offline: {endpoint: ./offline}
`
	operations := &testSessionOperations{}
	if err := testApply(t, fmt.Sprintf(star, "old"), fmt.Sprintf(star, "new"), testPause("star-offline"), operations); err != nil {
		t.Fatal(err)
	}
	var expected []string
	for range 2 {
		for _, name := range []string{"cloud", "gpu", "server"} {
			expected = append(expected, "flush synchronization sync-star-"+name)
		}
	}
	testCheckOperations(t, operations, append(expected,
		"terminate synchronization sync-star-cloud,sync-star-gpu,sync-star-offline,sync-star-server",
		"create synchronization star-cloud paused=false",
		"create synchronization star-gpu paused=false",
		"create synchronization star-offline paused=true",
		"create synchronization star-server paused=false",
	))
}

// TestPlanApplyConvergesChain verifies that replacing a chain of three
// sessions, each sharing a workspace with the next, flushes them in three
// rounds, and that the flush order is deterministic, i.e. independent of the
// order of the sessions' states.
func TestPlanApplyConvergesChain(t *testing.T) {
	t.Chdir(t.TempDir())
	const chain = `
sync:
  defaults: {maxEntryCount: %d}
  c: {alpha: ./w2, beta: ./w3}
  a: {alpha: ./w0, beta: ./w1}
  b: {alpha: ./w1, beta: ./w2}
`
	for _, prepare := range []func([]*synchronization.State){
		nil,
		func(states []*synchronization.State) {
			for i, j := 0, len(states)-1; i < j; i, j = i+1, j-1 {
				states[i], states[j] = states[j], states[i]
			}
		},
		func(states []*synchronization.State) {
			states[0], states[1] = states[1], states[0]
		},
	} {
		operations := &testSessionOperations{}
		if err := testApply(t, fmt.Sprintf(chain, 100), fmt.Sprintf(chain, 200), prepare, operations); err != nil {
			t.Fatal(err)
		}
		testCheckOperations(t, operations, []string{
			"flush synchronization sync-a",
			"flush synchronization sync-b",
			"flush synchronization sync-c",
			"flush synchronization sync-a",
			"flush synchronization sync-b",
			"flush synchronization sync-c",
			"flush synchronization sync-a",
			"flush synchronization sync-b",
			"flush synchronization sync-c",
			"terminate synchronization sync-a,sync-b,sync-c",
			"create synchronization a paused=false",
			"create synchronization b paused=false",
			"create synchronization c paused=false",
		})
	}
}

// TestPlanApplyWorkspaceTopologies verifies that a component whose
// workspaces form a tree is flushed in D+1 rounds, where D is its diameter,
// even if it has more sessions, and that a component whose workspaces form a
// cycle is flushed in as many rounds as it has sessions, even if its diameter
// is shorter. A session whose endpoints overlap, or two sessions between the
// same workspaces, count as cycles.
func TestPlanApplyWorkspaceTopologies(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		// name is the scenario name.
		name string
		// configuration is the project configuration, which includes a
		// placeholder for a maximum entry count that's changed to replace
		// every session.
		configuration string
		// sessions are the names of the sessions, in name order.
		sessions []string
		// rounds is the expected number of rounds.
		rounds int
	}{
		{
			// No session in a cycle of five is more than two hops from
			// another, but if the session that closes the cycle ignores a
			// file, then the file's deletion on w1 has to traverse all four
			// of the other sessions, and each round only carries it through
			// one of them, since they're flushed starting from the other end.
			name: "cycle",
			configuration: `
sync:
  defaults: {maxEntryCount: %d}
  a: {alpha: ./w4, beta: ./w5}
  b: {alpha: ./w3, beta: ./w4}
  c: {alpha: ./w2, beta: ./w3}
  d: {alpha: ./w1, beta: ./w2}
  e: {alpha: ./w5, beta: ./w1, ignore: {paths: [file]}}
`,
			sessions: []string{"a", "b", "c", "d", "e"},
			rounds:   5,
		},
		{
			// Three sessions share w0, and a fourth extends one of them, so
			// no session is more than two hops from another.
			name: "branching tree",
			configuration: `
sync:
  defaults: {maxEntryCount: %d}
  a: {alpha: ./w0, beta: ./w1}
  b: {alpha: ./w0, beta: ./w2}
  c: {alpha: ./w0, beta: ./w3}
  d: {alpha: ./w3, beta: ./w4}
`,
			sessions: []string{"a", "b", "c", "d"},
			rounds:   3,
		},
		{
			// All three sessions share w0, but the endpoints of c overlap.
			name: "self-loop",
			configuration: `
sync:
  defaults: {maxEntryCount: %d}
  a: {alpha: ./w0, beta: ./w1}
  b: {alpha: ./w0, beta: ./w2}
  c: {alpha: ./w0, beta: ./w0/sub}
`,
			sessions: []string{"a", "b", "c"},
			rounds:   3,
		},
		{
			// All three sessions share w0, but a and b both connect w0 and
			// w1, which b's endpoint is nested in.
			name: "parallel sessions",
			configuration: `
sync:
  defaults: {maxEntryCount: %d}
  a: {alpha: ./w0, beta: ./w1}
  b: {alpha: ./w0, beta: ./w1/sub}
  c: {alpha: ./w0, beta: ./w2}
`,
			sessions: []string{"a", "b", "c"},
			rounds:   3,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			operations := &testSessionOperations{}
			if err := testApply(t,
				fmt.Sprintf(test.configuration, 100), fmt.Sprintf(test.configuration, 200), nil, operations,
			); err != nil {
				t.Fatal(err)
			}
			var expected []string
			for range test.rounds {
				for _, name := range test.sessions {
					expected = append(expected, "flush synchronization sync-"+name)
				}
			}
			var identifiers []string
			for _, name := range test.sessions {
				identifiers = append(identifiers, "sync-"+name)
			}
			expected = append(expected, "terminate synchronization "+strings.Join(identifiers, ","))
			for _, name := range test.sessions {
				expected = append(expected, "create synchronization "+name+" paused=false")
			}
			testCheckOperations(t, operations, expected)
		})
	}
}

// TestPlanApplyIsolatedSession verifies that a replaced session that shares no
// workspace with another session is flushed once, and that running retained
// sessions that don't share its workspaces aren't flushed, even if they share
// workspaces with each other.
func TestPlanApplyIsolatedSession(t *testing.T) {
	t.Chdir(t.TempDir())
	operations := &testSessionOperations{}
	if err := testApply(t, `
sync:
  replace: {alpha: ./a, beta: ./b}
  keep-first: {alpha: ./c, beta: ./d}
  keep-second: {alpha: ./d, beta: ./e}
`, `
sync:
  replace: {alpha: ./a, beta: ./b, maxEntryCount: 100}
  keep-first: {alpha: ./c, beta: ./d}
  keep-second: {alpha: ./d, beta: ./e}
`, nil, operations); err != nil {
		t.Fatal(err)
	}
	testCheckOperations(t, operations, []string{
		"flush synchronization sync-replace",
		"terminate synchronization sync-replace",
		"create synchronization replace paused=false",
	})
}

// testRetainedCurrent and testRetainedDesired declare a replaced session that
// shares a workspace with a retained session, whose endpoint is nested in it.
const (
	testRetainedCurrent = `
sync:
  keep: {alpha: ./w0, beta: ./w1/sub}
  replace: {alpha: ./w1, beta: ./w2}
`
	testRetainedDesired = `
sync:
  keep: {alpha: ./w0, beta: ./w1/sub}
  replace: {alpha: ./w1, beta: ./w2, maxEntryCount: 100}
`
)

// TestPlanApplyConvergesRetainedSessions verifies that a running retained
// session that shares a workspace with a replaced session, including through
// nested endpoints, is flushed with it, but never terminated or created.
func TestPlanApplyConvergesRetainedSessions(t *testing.T) {
	t.Chdir(t.TempDir())
	operations := &testSessionOperations{}
	if err := testApply(t, testRetainedCurrent, testRetainedDesired, nil, operations); err != nil {
		t.Fatal(err)
	}
	testCheckOperations(t, operations, []string{
		"flush synchronization sync-keep",
		"flush synchronization sync-replace",
		"flush synchronization sync-keep",
		"flush synchronization sync-replace",
		"terminate synchronization sync-replace",
		"create synchronization replace paused=false",
	})
}

// TestPlanApplyPausedSessions verifies that paused sessions are neither
// flushed, whether retained or replaced, nor counted in the number of rounds:
// a chain of four running sessions is flushed in four rounds, even though two
// paused sessions close it into a cycle of six sessions.
func TestPlanApplyPausedSessions(t *testing.T) {
	t.Chdir(t.TempDir())
	operations := &testSessionOperations{}
	if err := testApply(t, `
sync:
  a: {alpha: ./w0, beta: ./w1}
  b: {alpha: ./w1, beta: ./w2}
  c: {alpha: ./w2, beta: ./w3}
  d: {alpha: ./w3, beta: ./w4}
  shortcut: {alpha: ./w4, beta: ./w0}
  stale: {alpha: ./w4, beta: ./w0}
`, `
sync:
  a: {alpha: ./w0, beta: ./w1}
  b: {alpha: ./w1, beta: ./w2}
  c: {alpha: ./w2, beta: ./w3}
  d: {alpha: ./w3, beta: ./w4, maxEntryCount: 100}
  shortcut: {alpha: ./w4, beta: ./w0}
  stale: {alpha: ./w4, beta: ./w0, maxEntryCount: 100}
`, testPause("shortcut", "stale"), operations); err != nil {
		t.Fatal(err)
	}
	var expected []string
	for range 4 {
		for _, name := range []string{"a", "b", "c", "d"} {
			expected = append(expected, "flush synchronization sync-"+name)
		}
	}
	testCheckOperations(t, operations, append(expected,
		"terminate synchronization sync-d,sync-stale",
		"create synchronization d paused=false",
		"create synchronization stale paused=true",
	))

	// A replaced session whose workspaces only paused sessions share is
	// flushed once.
	operations = &testSessionOperations{}
	if err := testApply(t, testRetainedCurrent, testRetainedDesired, testPause("keep"), operations); err != nil {
		t.Fatal(err)
	}
	testCheckOperations(t, operations, []string{
		"flush synchronization sync-replace",
		"terminate synchronization sync-replace",
		"create synchronization replace paused=false",
	})
}

// TestPlanApplyFlushFailureInLaterRound verifies that if a flush fails in a
// later round, then no session is terminated or created, and that the error
// identifies the session, whether it will be replaced or retained, wraps the
// cause, and explains how to proceed.
func TestPlanApplyFlushFailureInLaterRound(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		// failing is the identifier of the session whose second flush fails.
		failing string
		// operations are the expected operations.
		operations []string
		// message is the expected error message.
		message string
	}{
		{
			"sync-replace",
			[]string{
				"flush synchronization sync-keep",
				"flush synchronization sync-replace",
				"flush synchronization sync-keep",
				"flush synchronization sync-replace",
			},
			`unable to flush synchronization session "replace" (sync-replace) before replacing it: flush failed; ` +
				`replacing it now could overwrite its unsynchronized changes, so no sessions were terminated or ` +
				`created: fix the session and apply again, or terminate it with "mutagen sync terminate sync-replace" ` +
				`and apply again if its unsynchronized changes don't matter`,
		},
		{
			"sync-keep",
			[]string{
				"flush synchronization sync-keep",
				"flush synchronization sync-replace",
				"flush synchronization sync-keep",
			},
			`unable to flush synchronization session "keep" (sync-keep), which shares workspaces with sessions that ` +
				`will be replaced or removed: flush failed; replacing or removing them now could let sessions with ` +
				`fresh history overwrite changes, or restore deleted files, that haven't propagated through it, so no ` +
				`sessions were terminated or created: fix the session and apply again, or pause it with ` +
				`"mutagen sync pause sync-keep" and apply again: it keeps its history, so it synchronizes its changes ` +
				`once resumed`,
		},
	} {
		operations := &testSessionOperations{failFlushAt: map[string]int{test.failing: 2}}
		err := testApply(t, testRetainedCurrent, testRetainedDesired, nil, operations)
		if err == nil {
			t.Fatal("flush failure was not reported")
		} else if err.Error() != test.message {
			t.Error("unexpected flush failure message:", err)
		} else if !errors.Is(err, errTestFlush) {
			t.Error("flush failure does not wrap its cause")
		}
		testCheckOperations(t, operations, test.operations)
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
