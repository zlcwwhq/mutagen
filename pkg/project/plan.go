package project

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/comparison"
	"github.com/mutagen-io/mutagen/pkg/forwarding"
	forwardingsvc "github.com/mutagen-io/mutagen/pkg/service/forwarding"
	synchronizationsvc "github.com/mutagen-io/mutagen/pkg/service/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
	forwardingurl "github.com/mutagen-io/mutagen/pkg/url/forwarding"
)

// projectSession is the subset of session behavior required for planning. It
// is implemented by forwarding and synchronization sessions.
type projectSession interface {
	comparable
	GetIdentifier() string
	GetName() string
	GetPaused() bool
}

// projectSpecification is the subset of creation specification behavior
// required for planning. It is implemented by forwarding and synchronization
// session creation specifications.
type projectSpecification interface {
	comparable
	GetName() string
	GetPaused() bool
}

// sessionChange describes a change to a single session. A nil current session
// indicates creation, a nil specification indicates removal, and both being
// non-nil indicates replacement. Unchanged sessions do not appear in plans.
type sessionChange[S projectSession, P projectSpecification] struct {
	// current is the existing session.
	current S
	// specification is the specification for the session to create. For
	// replacements, it preserves the paused state of the existing session.
	specification P
}

// name returns the name of the affected session.
func (c *sessionChange[S, P]) name() string {
	var none S
	if c.current != none {
		return c.current.GetName()
	}
	return c.specification.GetName()
}

// action describes the change.
func (c *sessionChange[S, P]) action() string {
	var noSession S
	var noSpecification P
	if c.current == noSession {
		return "Create"
	} else if c.specification == noSpecification {
		return "Remove"
	}
	return "Replace"
}

// planChanges matches specifications to existing sessions by name. Sessions
// that match their specification according to matches are left unchanged.
// Replacement specifications for paused sessions are substituted with paused
// copies created by pausedCopy, so specifications are never modified. Sessions
// whose names aren't declared are removed, including any duplicates, while
// duplicates of declared names are rejected because it's ambiguous which
// session should be retained.
func planChanges[S projectSession, P projectSpecification](
	kind string,
	sessions []S,
	specifications []P,
	matches func(S, P) bool,
	pausedCopy func(P) P,
) ([]*sessionChange[S, P], int, error) {
	// Index existing sessions by name.
	sessionsByName := make(map[string][]S)
	for _, session := range sessions {
		sessionsByName[session.GetName()] = append(sessionsByName[session.GetName()], session)
	}

	// Match specifications to existing sessions.
	var changes []*sessionChange[S, P]
	var unchanged int
	for _, specification := range specifications {
		name := specification.GetName()
		existing := sessionsByName[name]
		delete(sessionsByName, name)
		if len(existing) > 1 {
			identifiers := make([]string, len(existing))
			for i, session := range existing {
				identifiers[i] = session.GetIdentifier()
			}
			sort.Strings(identifiers)
			return nil, 0, fmt.Errorf(
				"multiple %s sessions named %q in project (%s): terminate all but one by identifier",
				kind, name, strings.Join(identifiers, ", "),
			)
		} else if len(existing) == 0 {
			changes = append(changes, &sessionChange[S, P]{specification: specification})
		} else if current := existing[0]; matches(current, specification) {
			unchanged++
		} else {
			if current.GetPaused() && !specification.GetPaused() {
				specification = pausedCopy(specification)
			}
			changes = append(changes, &sessionChange[S, P]{current, specification})
		}
	}

	// Remove sessions with undeclared names.
	for _, existing := range sessionsByName {
		for _, current := range existing {
			changes = append(changes, &sessionChange[S, P]{current: current})
		}
	}

	// Sort changes by name and then identifier, since removals are obtained
	// from a map and may share names.
	sort.Slice(changes, func(i, j int) bool {
		if iName, jName := changes[i].name(), changes[j].name(); iName != jName {
			return iName < jName
		}
		return changes[i].current.GetIdentifier() < changes[j].current.GetIdentifier()
	})

	// Success.
	return changes, unchanged, nil
}

// endpointMatches compares an existing endpoint URL with a freshly parsed one.
// Environment variables locked into Docker URLs come from the invoking shell
// rather than the project file, so they are excluded and retained sessions
// keep the environment from their creation. Local paths are compared by
// location because the same project directory can be reached through
// different paths, e.g. via symbolic links or a stale $PWD.
func endpointMatches(current, desired *url.URL) bool {
	if current == nil || desired == nil {
		return false
	} else if current.Kind != desired.Kind ||
		current.Protocol != desired.Protocol ||
		current.User != desired.User ||
		current.Host != desired.Host ||
		current.Port != desired.Port ||
		!comparison.StringMapsEqual(current.Parameters, desired.Parameters) {
		return false
	} else if current.Path == desired.Path {
		return true
	} else if current.Protocol != url.Protocol_Local {
		return false
	}

	// Synchronization paths are filesystem paths. Forwarding paths are only
	// filesystem paths for Unix domain sockets.
	if current.Kind == url.Kind_Forwarding {
		currentProtocol, currentAddress, currentErr := forwardingurl.Parse(current.Path)
		desiredProtocol, desiredAddress, desiredErr := forwardingurl.Parse(desired.Path)
		if currentErr != nil || desiredErr != nil ||
			currentProtocol != "unix" || desiredProtocol != "unix" {
			return false
		}
		return sameLocation(currentAddress, desiredAddress)
	}
	return sameLocation(current.Path, desired.Path)
}

// sameLocation reports whether two absolute paths name the same entry. Leaf
// names must match exactly, since an endpoint root may itself be a symbolic
// link, while parent directories are compared by file identity. Parents that
// do not exist yet are compared recursively using the same rule.
func sameLocation(first, second string) bool {
	if first == second {
		return true
	} else if filepath.Base(first) != filepath.Base(second) {
		return false
	}
	firstParent, secondParent := filepath.Dir(first), filepath.Dir(second)
	if firstParent == first || secondParent == second {
		return false
	}
	firstInfo, firstErr := os.Stat(firstParent)
	secondInfo, secondErr := os.Stat(secondParent)
	if firstErr == nil && secondErr == nil {
		return os.SameFile(firstInfo, secondInfo)
	} else if os.IsNotExist(firstErr) && os.IsNotExist(secondErr) {
		return sameLocation(firstParent, secondParent)
	}
	return false
}

// forwardingSessionMatches checks the session's declarative configuration.
// Paused state is managed separately by pause/resume and is not configuration.
func forwardingSessionMatches(session *forwarding.Session, specification *forwardingsvc.CreationSpecification) bool {
	return endpointMatches(session.Source, specification.Source) &&
		endpointMatches(session.Destination, specification.Destination) &&
		session.Configuration.Equal(specification.Configuration) &&
		session.ConfigurationSource.Equal(specification.ConfigurationSource) &&
		session.ConfigurationDestination.Equal(specification.ConfigurationDestination)
}

// synchronizationSessionMatches checks the session's declarative configuration.
func synchronizationSessionMatches(session *synchronization.Session, specification *synchronizationsvc.CreationSpecification) bool {
	// Star membership changes command routing even when an edge's filesystem
	// locations are unchanged.
	if !proto.Equal(session.Star, specification.Star) {
		return false
	}
	return endpointMatches(session.Alpha, specification.Alpha) &&
		endpointMatches(session.Beta, specification.Beta) &&
		session.Configuration.Equal(specification.Configuration) &&
		session.ConfigurationAlpha.Equal(specification.ConfigurationAlpha) &&
		session.ConfigurationBeta.Equal(specification.ConfigurationBeta)
}

// pausedForwardingSpecification returns a paused copy of a specification.
func pausedForwardingSpecification(specification *forwardingsvc.CreationSpecification) *forwardingsvc.CreationSpecification {
	specification = proto.Clone(specification).(*forwardingsvc.CreationSpecification)
	specification.Paused = true
	return specification
}

// pausedSynchronizationSpecification returns a paused copy of a specification.
func pausedSynchronizationSpecification(specification *synchronizationsvc.CreationSpecification) *synchronizationsvc.CreationSpecification {
	specification = proto.Clone(specification).(*synchronizationsvc.CreationSpecification)
	specification.Paused = true
	return specification
}

// Plan describes the changes required to reconcile a project's sessions with
// its session specifications.
type Plan struct {
	// forwarding contains forwarding session changes sorted by name.
	forwarding []*sessionChange[*forwarding.Session, *forwardingsvc.CreationSpecification]
	// synchronization contains synchronization session changes sorted by name.
	synchronization []*sessionChange[*synchronization.Session, *synchronizationsvc.CreationSpecification]
	// synchronizationSessions contains the project's existing synchronization
	// sessions, including those that will be retained, which may share
	// workspaces with those that will be terminated.
	synchronizationSessions []*synchronization.Session
	// flushOnCreate records flush-on-creation behavior by synchronization
	// session name.
	flushOnCreate map[string]bool
	// unchanged is the number of sessions that will be retained.
	unchanged int
}

// NewPlan computes a plan without modifying sessions or specifications. The
// states must be those of the project's sessions, i.e. those carrying the
// project label, and must already be validated. Sessions are matched by type
// and name. Duplicate names are rejected before any changes are applied if the
// session to retain would be ambiguous.
func NewPlan(
	specifications *SessionSpecifications,
	forwardingStates []*forwarding.State,
	synchronizationStates []*synchronization.State,
) (*Plan, error) {
	// Extract sessions.
	forwardingSessions := make([]*forwarding.Session, len(forwardingStates))
	for i, state := range forwardingStates {
		forwardingSessions[i] = state.Session
	}
	synchronizationSessions := make([]*synchronization.Session, len(synchronizationStates))
	for i, state := range synchronizationStates {
		synchronizationSessions[i] = state.Session
	}

	// Plan changes for both session types.
	forwardingChanges, forwardingUnchanged, err := planChanges(
		"forwarding", forwardingSessions, specifications.Forwarding,
		forwardingSessionMatches, pausedForwardingSpecification,
	)
	if err != nil {
		return nil, err
	}
	synchronizationChanges, synchronizationUnchanged, err := planChanges(
		"synchronization", synchronizationSessions, specifications.Synchronization,
		synchronizationSessionMatches, pausedSynchronizationSpecification,
	)
	if err != nil {
		return nil, err
	}

	// A star's retained edges keep the center's identity for new and replaced
	// edges. The initial matching pass uses declarative locations, while the
	// daemon compares the center's exact URLs across edges. Reconcile those
	// representations and validate the resulting stars before any sessions
	// are flushed, removed, or created.
	if err := preserveStarCenters(synchronizationSessions, synchronizationChanges); err != nil {
		return nil, err
	}

	// Success.
	return &Plan{
		forwarding:              forwardingChanges,
		synchronization:         synchronizationChanges,
		synchronizationSessions: synchronizationSessions,
		flushOnCreate:           specifications.FlushOnCreate,
		unchanged:               forwardingUnchanged + synchronizationUnchanged,
	}, nil
}

// pausedChangesOutcome describes what can happen to changes made since a
// synchronization session was paused once a session with fresh history (i.e.
// without an ancestor) synchronizes the same files, depending on its mode.
const pausedChangesOutcome = "changes made since it was paused may be overwritten (two-way-resolved, " +
	"one-way-replica) or become conflicts (two-way-safe, one-way-safe)"

// notes returns notes about paused synchronization sessions that will be
// terminated without being flushed, since flushing them would require resuming
// them. Replacements start with fresh history, as do sessions created under
// other names (now or later) for the same files as removed sessions.
func (p *Plan) notes() []string {
	var notes []string
	for _, change := range p.synchronization {
		if change.current == nil || !change.current.Paused {
			continue
		} else if change.specification != nil {
			notes = append(notes, fmt.Sprintf(
				"Synchronization session %q is paused and won't be flushed before it's replaced, so %s "+
					"once the replacement is resumed. Resume it before applying to synchronize them first.",
				change.current.Name, pausedChangesOutcome,
			))
		} else {
			notes = append(notes, fmt.Sprintf(
				"Synchronization session %q is paused and won't be flushed before it's removed, so if a new session "+
					"synchronizes the same files, %s. Resume it before applying to synchronize them first.",
				change.current.Name, pausedChangesOutcome,
			))
		}
	}
	return notes
}

// Print writes a description of the plan, followed by notes about paused
// synchronization sessions that won't be flushed before being terminated.
func (p *Plan) Print(writer io.Writer) {
	for _, change := range p.forwarding {
		fmt.Fprintf(writer, "%s forwarding session %q\n", change.action(), change.name())
	}
	for _, change := range p.synchronization {
		fmt.Fprintf(writer, "%s synchronization session %q\n", change.action(), change.name())
	}
	fmt.Fprintf(writer, "Unchanged sessions: %d\n", p.unchanged)
	for _, note := range p.notes() {
		fmt.Fprintln(writer, "Note:", note)
	}
}

// SessionOperations performs the session operations required to apply a plan.
// All sessions are specified by identifier.
type SessionOperations interface {
	// TerminateForwarding terminates forwarding sessions.
	TerminateForwarding(identifiers []string) error
	// TerminateSynchronization terminates synchronization sessions.
	TerminateSynchronization(identifiers []string) error
	// CreateForwarding creates a forwarding session.
	CreateForwarding(specification *forwardingsvc.CreationSpecification) error
	// CreateSynchronization creates a synchronization session and returns its
	// identifier.
	CreateSynchronization(specification *synchronizationsvc.CreationSpecification) (string, error)
	// FlushSynchronization flushes synchronization sessions.
	FlushSynchronization(identifiers []string) error
}

// currentIdentifiers returns the identifiers of removed and replaced sessions.
func currentIdentifiers[S projectSession, P projectSpecification](changes []*sessionChange[S, P]) []string {
	var none S
	var identifiers []string
	for _, change := range changes {
		if change.current != none {
			identifiers = append(identifiers, change.current.GetIdentifier())
		}
	}
	return identifiers
}

// sessionsShareWorkspaces reports whether any endpoint of one synchronization
// session overlaps any endpoint of another, i.e. whether they are equal or
// nested on the same configured system (see
// synchronization.WorkspaceEndpointsOverlap).
func sessionsShareWorkspaces(first, second *synchronization.Session) bool {
	for _, a := range []*url.URL{first.Alpha, first.Beta} {
		for _, b := range []*url.URL{second.Alpha, second.Beta} {
			if a != nil && b != nil && synchronization.WorkspaceEndpointsOverlap(a, b) {
				return true
			}
		}
	}
	return false
}

// flushRounds returns the flushes that must complete before any
// synchronization session is terminated, as rounds of running sessions, sorted
// by name and then identifier. Each round must be performed after the previous
// one has completed.
//
// Once a session is flushed, its endpoints agree on the content that it
// synchronizes, as of the flush's scans. That suffices for a session that
// shares no workspace with another session, but not for sessions that do: a
// change that a later flush brings into a shared workspace isn't propagated by
// a session that was flushed before. Sessions that take over files start with
// fresh history, so a file that then exists on only one side is copied to the
// other, which undoes deletions. Running sessions are therefore adjacent if
// any of their endpoints overlap (see sessionsShareWorkspaces), and every
// running session in a connected component that contains a session to be
// terminated, including retained sessions, is flushed in D+1 rounds, where D
// is the component's diameter: the largest distance between two of its
// sessions, counted in hops between adjacent sessions.
//
// Consider a change that is present in an endpoint of a session s before the
// first round. s is flushed during the first round, after which both of its
// endpoints hold the change. Suppose that every session k hops from s holds the
// change on both endpoints after round k+1. A session t that is k+1 hops from s
// is adjacent to a session k hops from s, which holds the change on both of its
// endpoints, including the workspace that it shares with t, once round k+1 has
// completed. The flush of t during round k+2 then carries the change to t's
// other endpoint. By induction, since no session is more than D hops from s,
// every session in the component holds the change on both endpoints after D+1
// rounds, regardless of the order of the flushes within each round. The bound
// is tight: a chain of k sessions has a diameter of k-1, and a change at one
// end needs k rounds if each round flushes the sessions starting from the
// other end. The edges of a star all share its center, so its diameter is 1
// and two rounds suffice, while a session that shares no workspace with
// another is flushed once. Flushes don't report whether they synchronized any
// changes, so rounds can't stop early.
//
// The argument assumes that each session carries the change between its
// endpoints. A session might not, because the change is ignored, outside the
// overlap of nested workspaces, or against a one-way mode. Where workspaces
// form a tree, such as a chain or a star, there's no path around such a
// session, so the change can't reach the sessions beyond it anyway, while the
// distances to the others are unchanged. Where workspaces form a cycle, such a
// session can force a path around the cycle that's longer than the diameter.
//
// Paused sessions are left out. They aren't flushed, since that would require
// resuming them, which is why the plan notes those that will be terminated.
// They don't carry changes while the plan is applied either, so paths through
// them must not shorten distances. Skipping retained paused sessions is safe
// because they keep their history: once resumed, they reconcile against it,
// so a file that exists on only one of their endpoints is propagated as a
// creation or deletion on that side rather than copied back.
func (p *Plan) flushRounds() [][]*synchronization.Session {
	// Identify the running sessions that will be terminated.
	terminated := make(map[*synchronization.Session]bool, len(p.synchronization))
	for _, change := range p.synchronization {
		if change.current != nil && !change.current.Paused {
			terminated[change.current] = true
		}
	}
	if len(terminated) == 0 {
		return nil
	}

	// Collect the running sessions in a deterministic order.
	var running []*synchronization.Session
	for _, session := range p.synchronizationSessions {
		if !session.Paused {
			running = append(running, session)
		}
	}
	sort.Slice(running, func(i, j int) bool {
		if running[i].Name != running[j].Name {
			return running[i].Name < running[j].Name
		}
		return running[i].Identifier < running[j].Identifier
	})

	// Connect running sessions that share workspaces.
	neighbors := make([][]int, len(running))
	for i := range running {
		for j := i + 1; j < len(running); j++ {
			if sessionsShareWorkspaces(running[i], running[j]) {
				neighbors[i] = append(neighbors[i], j)
				neighbors[j] = append(neighbors[j], i)
			}
		}
	}

	// Define a breadth-first search that returns the sessions reachable from a
	// session, which form its connected component, and the largest distance
	// to any of them. Sessions are reached in order of distance, so the last
	// one is the farthest.
	search := func(source int) ([]int, int) {
		distances := map[int]int{source: 0}
		reached := []int{source}
		for next := 0; next < len(reached); next++ {
			session := reached[next]
			for _, neighbor := range neighbors[session] {
				if _, ok := distances[neighbor]; !ok {
					distances[neighbor] = distances[session] + 1
					reached = append(reached, neighbor)
				}
			}
		}
		return reached, distances[reached[len(reached)-1]]
	}

	// Compute the number of rounds for each session from the diameter of its
	// connected component, if the component contains a session that will be
	// terminated. Sessions in other components aren't flushed.
	rounds := make([]int, len(running))
	visited := make([]bool, len(running))
	for i := range running {
		if visited[i] {
			continue
		}
		component, _ := search(i)
		var diameter int
		var affected bool
		for _, member := range component {
			visited[member] = true
			affected = affected || terminated[running[member]]
			if _, eccentricity := search(member); eccentricity > diameter {
				diameter = eccentricity
			}
		}
		if affected {
			for _, member := range component {
				rounds[member] = diameter + 1
			}
		}
	}

	// Assemble the rounds.
	var result [][]*synchronization.Session
	for round := 0; ; round++ {
		var sessions []*synchronization.Session
		for i, session := range running {
			if rounds[i] > round {
				sessions = append(sessions, session)
			}
		}
		if len(sessions) == 0 {
			return result
		}
		result = append(result, sessions)
	}
}

// Apply applies the plan. Running synchronization sessions that will be
// terminated are flushed first, together with the running sessions that share
// their workspaces, repeatedly, until changes have propagated across them (see
// flushRounds). Their replacements, as well as any sessions created under
// other names for the same files, start with fresh history and could otherwise
// overwrite (or turn into conflicts) changes that haven't been synchronized
// yet, or copy back files whose deletion hasn't reached every workspace. If a
// flush fails, then no session is terminated or created. Paused sessions are
// terminated without being flushed, since that would require resuming them,
// which is why the plan notes them. Removed and replaced sessions are then
// terminated before any creation so that forwarding listeners and
// synchronization roots are released, including when endpoints move between
// names. Operations are never performed with an empty set of sessions.
// Unchanged sessions are never paused, resumed, or terminated, and they're
// only flushed if they share workspaces with running sessions that will be
// terminated. Changes are not transactional: if an operation fails, completed
// changes remain in place.
func (p *Plan) Apply(operations SessionOperations) error {
	// Flush running synchronization sessions that will be terminated, together
	// with the running sessions that share their workspaces. Removed sessions
	// are flushed as well, since a session created under another name (by this
	// plan or a later one) may synchronize the same files, which can't be
	// reliably detected by comparing endpoint URLs. Sessions are flushed
	// individually so that a failure can identify the session.
	changes := make(map[*synchronization.Session]*sessionChange[*synchronization.Session, *synchronizationsvc.CreationSpecification])
	for _, change := range p.synchronization {
		if change.current != nil {
			changes[change.current] = change
		}
	}
	for _, round := range p.flushRounds() {
		for _, session := range round {
			if err := operations.FlushSynchronization([]string{session.Identifier}); err != nil {
				change := changes[session]
				if change == nil {
					return fmt.Errorf(
						"unable to flush synchronization session %q (%s), which shares workspaces with sessions that will be "+
							"replaced or removed: %w; replacing or removing them now could let sessions with fresh history "+
							"overwrite changes, or restore deleted files, that haven't propagated through it, so no sessions "+
							"were terminated or created: fix the session and apply again, or pause it with "+
							"\"mutagen sync pause %s\" and apply again: it keeps its history, so it synchronizes its changes "+
							"once resumed",
						session.Name, session.Identifier, err, session.Identifier,
					)
				}
				action := "replacing"
				risk := "replacing it now could overwrite its unsynchronized changes"
				if change.specification == nil {
					action = "removing"
					risk = "removing it now could let a new session for the same files overwrite its unsynchronized changes"
				}
				return fmt.Errorf(
					"unable to flush synchronization session %q (%s) before %s it: %w; %s, so no sessions were terminated or created: "+
						"fix the session and apply again, or terminate it with \"mutagen sync terminate %s\" and apply again "+
						"if its unsynchronized changes don't matter",
					session.Name, session.Identifier, action, err, risk, session.Identifier,
				)
			}
		}
	}

	// Terminate obsolete sessions.
	if identifiers := currentIdentifiers(p.forwarding); len(identifiers) > 0 {
		if err := operations.TerminateForwarding(identifiers); err != nil {
			return fmt.Errorf("unable to terminate forwarding sessions: %w", err)
		}
	}
	if identifiers := currentIdentifiers(p.synchronization); len(identifiers) > 0 {
		if err := operations.TerminateSynchronization(identifiers); err != nil {
			return fmt.Errorf("unable to terminate synchronization sessions: %w", err)
		}
	}

	// Create forwarding sessions.
	for _, change := range p.forwarding {
		if change.specification == nil {
			continue
		}
		if err := operations.CreateForwarding(change.specification); err != nil {
			return fmt.Errorf("unable to create forwarding session (%s): %w", change.specification.Name, err)
		}
	}

	// Create synchronization sessions and collect those that are running and
	// request a flush, so that paused sessions don't block application. If a
	// creation fails, the sessions created before it are still flushed, since
	// a subsequent application will consider them unchanged.
	var sessionsToFlush []string
	var creationErr error
	for _, change := range p.synchronization {
		if change.specification == nil {
			continue
		}
		session, err := operations.CreateSynchronization(change.specification)
		if err != nil {
			creationErr = fmt.Errorf("unable to create synchronization session (%s): %w", change.specification.Name, err)
			break
		}
		if !change.specification.Paused && p.flushOnCreate[change.specification.Name] {
			sessionsToFlush = append(sessionsToFlush, session)
		}
	}
	if len(sessionsToFlush) > 0 {
		if err := operations.FlushSynchronization(sessionsToFlush); err != nil {
			return errors.Join(creationErr, fmt.Errorf("unable to flush synchronization sessions: %w", err))
		}
	}

	// Done.
	return creationErr
}
