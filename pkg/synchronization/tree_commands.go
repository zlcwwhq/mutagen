package synchronization

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// cachedTree is the immutable topology and validation result of one group.
// It is built when membership changes, without holding the relay registry lock.
type cachedTree struct {
	// sessions is a stable identifier ordering of the persisted edges.
	sessions []*Session
	// parents and nodes describe a valid reconstructed tree, when possible.
	parents  map[string]*Session
	nodes    map[string]*url.URL
	problem  error
	revision string
	// bridges identifies edges whose removal disconnects their endpoints, even
	// when unrelated parts of the recorded graph no longer form a tree.
	bridges map[string]bool
}

// commandTrees indexes persisted membership and currently connected relays.
// The registry lock protects publication and live connections, never filesystem
// inspection. Membership updates are serialized separately so slow local path
// resolution cannot prevent unrelated relays from connecting or stopping.
type commandTrees struct {
	sync.Mutex
	// updateLock serializes snapshot construction without blocking relay changes.
	updateLock sync.Mutex
	// controllers includes paused and disconnected sessions.
	controllers map[string]*controller
	// relays records each currently running synchronization loop.
	relays map[string]*commandRelay
	// cached holds immutable validation results, never rebuilt by readers.
	cached map[string]*cachedTree
	// active holds running paths, whose lifetimes are independent of the cache.
	// Each is invalidated through its own changed channel (see treeRoute).
	active map[*treeRoute]struct{}
	// build constructs snapshots; tests can model slow filesystem inspection.
	build func(string, map[string]*controller) *cachedTree
}

// newCommandTrees creates the manager's runtime tree index.
func newCommandTrees() *commandTrees {
	return &commandTrees{
		controllers: make(map[string]*controller), relays: make(map[string]*commandRelay),
		cached: make(map[string]*cachedTree), active: make(map[*treeRoute]struct{}),
		build: buildTree,
	}
}

// buildTree validates a membership snapshot. All filesystem work is confined
// to this function and the overlap checks in update, outside the registry lock.
func buildTree(group string, controllers map[string]*controller) *cachedTree {
	result := &cachedTree{}
	for _, controller := range controllers {
		if controller.session.GetTree().GetIdentifier() == group {
			result.sessions = append(result.sessions, controller.session)
		}
	}
	sort.Slice(result.sessions, func(i, j int) bool { return result.sessions[i].Identifier < result.sessions[j].Identifier })
	result.revision = nodeRevision(result.sessions)
	result.bridges = treeBridges(result.sessions)
	result.parents, result.nodes, result.problem = workspaceTree(result.sessions)
	if result.problem != nil {
		return result
	}
	var identifiers []string
	for identifier := range controllers {
		identifiers = append(identifiers, identifier)
	}
	sort.Strings(identifiers)
	for _, name := range sortedTreeNodes(result.nodes) {
		for _, identifier := range identifiers {
			other := controllers[identifier].session
			if other.GetTree().GetIdentifier() == group {
				continue
			}
			if TreeEndpointsOverlap(result.nodes[name], other.Alpha) || TreeEndpointsOverlap(result.nodes[name], other.Beta) {
				result.problem = fmt.Errorf("session %s overlaps node %s of the workspace tree", other.Identifier, name)
				return result
			}
		}
	}
	return result
}

// treeBridges identifies edges that still provide a unique connection between
// their endpoints. This uses the complete current edge graph, including newly
// added nodes and disconnected components; a degraded tree has no usable
// parent map, but it still has enough information to protect active paths.
func treeBridges(sessions []*Session) map[string]bool {
	type link struct{ node, edge string }
	neighbors := make(map[string][]link)
	for _, session := range sessions {
		alpha, beta := session.Tree.Alpha, session.Tree.Beta
		neighbors[alpha] = append(neighbors[alpha], link{beta, session.Identifier})
		neighbors[beta] = append(neighbors[beta], link{alpha, session.Identifier})
	}
	bridges := make(map[string]bool)
	visited, lowest := make(map[string]int), make(map[string]int)
	clock := 0
	var visit func(string, string)
	visit = func(node, parentEdge string) {
		clock++
		visited[node], lowest[node] = clock, clock
		for _, next := range neighbors[node] {
			if next.edge == parentEdge {
				continue
			}
			if visited[next.node] == 0 {
				visit(next.node, next.edge)
				lowest[node] = min(lowest[node], lowest[next.node])
				if lowest[next.node] > visited[node] {
					bridges[next.edge] = true
				}
			} else {
				lowest[node] = min(lowest[node], visited[next.node])
			}
		}
	}
	for node := range neighbors {
		if visited[node] == 0 {
			visit(node, "")
		}
	}
	return bridges
}

// changedNode checks whether one newly recorded endpoint introduces an unsafe
// identity or configuration for an existing node. An ordinary incident edge
// with the same node settings is harmless even if some other branch is broken.
func changedNode(existing *Session, existingIndex int, added *Session, addedIndex int, overlap bool) bool {
	if existing.Tree.Identifier != added.GetTree().GetIdentifier() {
		return overlap
	}
	if existing.Tree.node(existingIndex) != added.Tree.node(addedIndex) {
		return overlap
	}
	oldEndpoint := []*url.URL{existing.Alpha, existing.Beta}[existingIndex]
	newEndpoint := []*url.URL{added.Alpha, added.Beta}[addedIndex]
	oldConfiguration := []*Configuration{existing.ConfigurationAlpha, existing.ConfigurationBeta}[existingIndex]
	newConfiguration := []*Configuration{added.ConfigurationAlpha, added.ConfigurationBeta}[addedIndex]
	return existing.Tree.Root != added.Tree.Root || !proto.Equal(oldEndpoint, newEndpoint) ||
		!existing.Configuration.Equal(added.Configuration) || !oldConfiguration.Equal(newConfiguration)
}

// update computes and atomically publishes the affected groups. Running routes
// retain their pinned edges when unrelated branches change; only changes that
// remove their edges or introduce a problem touching their nodes cancel them.
func (t *commandTrees) update(c *controller, remove bool) {
	if t == nil {
		return
	}
	t.updateLock.Lock()
	defer t.updateLock.Unlock()
	t.Lock()
	controllers := make(map[string]*controller, len(t.controllers)+1)
	for id, controller := range t.controllers {
		controllers[id] = controller
	}
	previous := make(map[string]*cachedTree, len(t.cached))
	for group, tree := range t.cached {
		previous[group] = tree
	}
	t.Unlock()
	if remove {
		delete(controllers, c.session.Identifier)
	} else {
		controllers[c.session.Identifier] = c
	}
	group := c.session.GetTree().GetIdentifier()
	affected := map[string]bool{}
	if group != "" {
		affected[group] = true
	}
	// Precompute filesystem overlaps and changes to node identity outside the
	// registry lock. Cache invalidation and running-path invalidation differ:
	// a harmless new incident edge needs a new graph, but must not cancel a
	// command solely because an unrelated existing problem remains.
	unsafe := make(map[string]map[string]bool)
	for key, tree := range previous {
		for _, edge := range tree.sessions {
			for index, name := range []string{edge.Tree.Alpha, edge.Tree.Beta} {
				endpoint := []*url.URL{edge.Alpha, edge.Beta}[index]
				for addedIndex, addedEndpoint := range []*url.URL{c.session.Alpha, c.session.Beta} {
					overlap := TreeEndpointsOverlap(endpoint, addedEndpoint)
					if overlap {
						affected[key] = true
					}
					if !remove && changedNode(edge, index, c.session, addedIndex, overlap) {
						if unsafe[key] == nil {
							unsafe[key] = make(map[string]bool)
						}
						unsafe[key][name] = true
					}
				}
			}
		}
		if tree.problem != nil {
			affected[key] = true
		}
	}

	updated := make(map[string]*cachedTree, len(affected))
	for key := range affected {
		updated[key] = t.build(key, controllers)
	}
	t.Lock()
	defer t.Unlock()
	t.controllers = controllers
	for key, tree := range updated {
		if len(tree.sessions) == 0 {
			delete(t.cached, key)
		} else {
			t.cached[key] = tree
		}
	}
	for route := range t.active {
		invalid := false
		for _, step := range route.steps {
			if controller := controllers[step.session.Identifier]; controller == nil || controller.session != step.session {
				invalid = true
				break
			}
		}
		if !invalid && !remove {
			if tree := updated[route.group]; tree != nil {
				invalid = route.affectedByAddition(tree, unsafe[route.group])
			}
		}
		if invalid {
			close(route.changed)
			delete(t.active, route)
		}
	}
}

// setRelay registers a synchronization loop, or removes it when it stops.
func (t *commandTrees) setRelay(c *controller, relay *commandRelay) {
	if t == nil {
		return
	}
	t.Lock()
	defer t.Unlock()
	if relay == nil {
		delete(t.relays, c.session.Identifier)
	} else {
		t.relays[c.session.Identifier] = relay
	}
}

// treeStep pins one synchronization loop and its command channels. A reconnect
// cannot silently replace any of them during a command invocation.
type treeStep struct {
	// session contains immutable topology and synchronization settings.
	session *Session
	// relay owns the pinned synchronization loop and flush request queue.
	relay *commandRelay
	// channels must both remain connected throughout the invocation.
	channels [2]remote.Channel
	// target is the next node's endpoint index along the forward path.
	target int
}

// treeRoute is the immutable plan for one invocation.
type treeRoute struct {
	// trees and group identify the registry that pins this path while it runs.
	trees    *commandTrees
	group    string
	revision string
	steps    []treeStep
	// nodes identifies path workspaces for scoped membership invalidation.
	nodes map[string]bool
	// changed is closed when a membership update invalidates the active route
	// (see update), which cancels the invocation.
	changed chan struct{}
}

// affectedByAddition checks the new issues introduced by an addition, using
// only maps prepared outside the registry lock. Existing unrelated problems
// neither cancel a path nor hide a new cycle crossing one of its edges.
func (r *treeRoute) affectedByAddition(current *cachedTree, unsafe map[string]bool) bool {
	for name := range r.nodes {
		if unsafe[name] {
			return true
		}
	}
	for _, step := range r.steps {
		if !current.bridges[step.session.Identifier] {
			return true
		}
	}
	return false
}

// activate registers an invocation after checking that its pinned sessions
// still belong to a valid graph. Discovery plans do not register invocations.
func (r *treeRoute) activate() (func(), error) {
	r.trees.Lock()
	defer r.trees.Unlock()
	current := r.trees.cached[r.group]
	if current == nil || current.problem != nil {
		return nil, errors.New("workspace tree changed before command preflight")
	}
	for _, step := range r.steps {
		if c := r.trees.controllers[step.session.Identifier]; c == nil || c.session != step.session {
			return nil, errors.New("workspace tree path changed before command preflight")
		}
	}
	r.trees.active[r] = struct{}{}
	return func() { r.trees.Lock(); delete(r.trees.active, r); r.trees.Unlock() }, nil
}

// check rejects an already-invalid route synchronously, before scheduling
// cancellation watchers or opening an executor.
func (r *treeRoute) check(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.changed:
		return remote.ConnectionError(errors.New("workspace tree topology changed"))
	default:
	}
	for _, step := range r.steps {
		if step.relay.ctx.Err() != nil {
			return remote.ConnectionError(errors.New("tree synchronization stopped"))
		}
		for _, channel := range step.channels {
			select {
			case <-channel.Done():
				return remote.ConnectionError(errors.New("tree endpoint disconnected"))
			default:
			}
		}
	}
	return nil
}

// plan finds the unique source-to-target path in the persisted tree. Source
// identity comes from the daemon's endpoint connection, not from the request.
func (t *commandTrees) plan(source *commandRelay, index int, target string) (*treeRoute, error) {
	t.Lock()
	defer t.Unlock()
	return t.planLocked(source, index, target)
}

// sortedTreeNodes returns node names in order, so that validation, routing,
// and discovery report the same node regardless of map iteration order.
func sortedTreeNodes(nodes map[string]*url.URL) []string {
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// treeLocked returns the cached immutable topology. The registry lock must be
// held; rebuilding and filesystem inspection only happen during update.
func (t *commandTrees) treeLocked(group string) ([]*Session, map[string]*Session, map[string]*url.URL, error) {
	if tree := t.cached[group]; tree != nil {
		return tree.sessions, tree.parents, tree.nodes, tree.problem
	}
	return nil, nil, nil, errors.New("empty workspace tree")
}

// planLocked plans a route while the membership lock is held. It refuses any
// problem with the tree's membership, whichever nodes the route would use.
func (t *commandTrees) planLocked(source *commandRelay, index int, target string) (*treeRoute, error) {
	c := source.controller
	group := c.session.GetTree().GetIdentifier()
	if group == "" || t.relays[c.session.Identifier] != source {
		return nil, errors.New("submission is not connected through a workspace tree")
	}
	_, parents, nodes, err := t.treeLocked(group)
	if err != nil {
		return nil, err
	}
	if nodes[target] == nil {
		return nil, fmt.Errorf("unknown tree node %q (available: %s)", target, strings.Join(sortedTreeNodes(nodes), ", "))
	}
	from := c.session.Tree.node(index)
	if from == target {
		return nil, errors.New("target is the current node; run the command locally")
	}

	// Walk both parent chains to their lowest common ancestor.
	ancestors := map[string]bool{from: true}
	for node := from; parents[node] != nil; {
		node = parents[node].Tree.Alpha
		ancestors[node] = true
	}
	var descent []*Session
	ancestor := target
	for !ancestors[ancestor] {
		edge := parents[ancestor]
		if edge == nil {
			return nil, errors.New("tree target is disconnected")
		}
		descent = append(descent, edge)
		ancestor = edge.Tree.Alpha
	}
	route := &treeRoute{trees: t, group: group, changed: make(chan struct{}), revision: t.cached[group].revision, nodes: make(map[string]bool)}
	appendStep := func(edge *Session, target int) error {
		relay := t.relays[edge.Identifier]
		if relay == nil || edge.Configuration.RemoteCommandMode != RemoteCommandMode_RemoteCommandModeBoth {
			return remote.ConnectionError(fmt.Errorf("edge %s <-> %s is paused, disconnected, or has commands disabled", edge.Tree.Alpha, edge.Tree.Beta))
		}
		relay.channelsLock.Lock()
		channels := relay.channels
		relay.channelsLock.Unlock()
		if channels[0] == nil || channels[1] == nil {
			return remote.ConnectionError(fmt.Errorf("edge %s <-> %s is not connected for commands", edge.Tree.Alpha, edge.Tree.Beta))
		}
		route.steps = append(route.steps, treeStep{edge, relay, channels, target})
		route.nodes[edge.Tree.Alpha], route.nodes[edge.Tree.Beta] = true, true
		return nil
	}
	for node := from; node != ancestor; {
		edge := parents[node]
		if err := appendStep(edge, 0); err != nil {
			return nil, err
		}
		node = edge.Tree.Alpha
	}
	for i := len(descent) - 1; i >= 0; i-- {
		if err := appendStep(descent[i], 1); err != nil {
			return nil, err
		}
	}
	return route, nil
}

// synchronize performs one strict cycle and emits the same structured progress
// and reports used by pair commands. Presentation belongs to the submitter.
func (r *treeRoute) synchronize(ctx context.Context, step treeStep, phase string, progress bool, event func(*remote.CommandEvent) error) error {
	if err := r.check(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop, stopped := make(chan struct{}), make(chan error, 1)
	if progress && step.relay.progress != nil {
		go func() {
			err := remote.MonitorProgress(func(sample *remote.Progress) error {
				return event(&remote.CommandEvent{Alpha: step.session.Tree.Alpha, Beta: step.session.Tree.Beta, Phase: phase, Progress: sample})
			}, step.relay.progress, progressSampleInterval, progressHeartbeatInterval, stop)
			if err != nil {
				cancel()
			}
			stopped <- err
		}()
	} else {
		stopped <- nil
	}
	err := step.relay.flush(ctx)
	close(stop)
	if progressError := <-stopped; progressError != nil {
		return progressError
	}
	var report *remote.Report
	if err == nil {
		report = step.relay.report()
		if report != nil {
			report.Resolution = step.relay.conflicts != nil
		}
	}
	completion := &remote.CommandEvent{Alpha: step.session.Tree.Alpha, Beta: step.session.Tree.Beta, Phase: phase, Report: report, Done: true}
	if err != nil {
		completion.Error = err.Error()
	}
	if eventError := event(completion); eventError != nil {
		return eventError
	}
	if err != nil {
		return fmt.Errorf("edge %s <-> %s: %w", step.session.Tree.Alpha, step.session.Tree.Beta, err)
	}
	if !report.Empty() {
		return fmt.Errorf("edge %s <-> %s has unsynchronized content; inspect with remote --node %s --conflicts from node %s", step.session.Tree.Alpha, step.session.Tree.Beta, step.session.Tree.Beta, step.session.Tree.Alpha)
	}
	return nil
}

// run performs strict path synchronization around a foreground command, or
// before starting an adjacent detached job. Jobs retain their normal lifecycle:
// starting one does not wait for it or synchronize back before it finishes.
// noFlush skips both path synchronizations, along with their checks for
// conflicts and other unsynchronized content, as --no-flush skips both flushes
// of a pair command. The invocation then neither synchronizes nor waits for
// synchronization: background synchronization continues as configured, with
// no guarantee that the command's changes propagate. The route itself is still
// pinned and checked, so the target remains the selected node of a valid tree.
func (r *treeRoute) run(ctx context.Context, request *remote.Request, stdin io.Reader, stdout, stderr io.Writer, noFlush, progress bool, event func(*remote.CommandEvent) error) (*remote.CommandResult, error) {
	if err := r.check(ctx); err != nil {
		return nil, err
	}
	if err := request.EnsureValid(); err != nil {
		return nil, err
	}
	if request.File != "" || request.Copy != "" || request.Archive != "" || request.Replace != nil || request.Job != nil && request.Job.Operation != remote.JobStart {
		return nil, errors.New("strict node routing only supports commands, scripts, and starting adjacent jobs")
	}
	if request.Job != nil && len(r.steps) != 1 {
		return nil, errors.New("starting a detached job requires an adjacent node")
	}
	release, err := r.activate()
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watch := func(done <-chan struct{}) {
		go func() {
			select {
			case <-done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	watch(r.changed)
	for _, step := range r.steps {
		watch(step.relay.ctx.Done())
		for _, channel := range step.channels {
			watch(channel.Done())
		}
		ignores := EffectiveIgnores(step.session.Configuration, step.session.Version)
		if excluded := ignores.ExcludedInAnyForm(request.Path, true); excluded != "" && request.Directory == "" {
			return nil, fmt.Errorf("submission directory is ignored by edge %s <-> %s: %s", step.session.Tree.Alpha, step.session.Tree.Beta, excluded)
		}
	}
	synchronized := r.steps
	if noFlush {
		synchronized = nil
	}
	for _, step := range synchronized {
		if err := r.synchronize(ctx, step, "before command", progress, event); err != nil {
			return nil, fmt.Errorf("command not started: %w", err)
		}
	}
	last := r.steps[len(r.steps)-1]
	if err := r.check(ctx); err != nil {
		return nil, err
	}
	openCtx, openCancel := context.WithTimeout(ctx, commandRouteOpenTimeout)
	connection, err := last.channels[last.target].Open(openCtx)
	openCancel()
	if err != nil {
		return nil, remote.ConnectionError(fmt.Errorf("unable to reach target node: %w", err))
	}
	response, executionError := remote.ForwardCommand(ctx, connection, request, stdin, stdout, stderr)
	if ctx.Err() != nil || errors.Is(executionError, remote.ErrConnectionLost) {
		if noFlush {
			return nil, remote.ConnectionError(errors.New("tree route was interrupted; the command may have run; it was not retried"))
		}
		return nil, remote.ConnectionError(errors.New("tree route was interrupted; the command may have run and its changes were not synchronized back; it was not retried"))
	}
	if request.Job != nil {
		return response, executionError
	}
	if response != nil && executionError == nil {
		executionError = response.ExecutionError()
	}
	for i := len(synchronized) - 1; i >= 0; i-- {
		if err := r.synchronize(ctx, synchronized[i], "after command", progress, event); err != nil {
			outcome := "command execution failed"
			if response != nil {
				outcome = fmt.Sprintf("command exited with status %d", response.ExitCode)
			}
			if executionError != nil {
				outcome += fmt.Sprintf(" (%v)", executionError)
			}
			return &remote.CommandResult{ExitCode: 125}, fmt.Errorf("%s, but synchronization back failed: %w", outcome, err)
		}
	}
	// Preserve executor metadata, including directory errors and exit status.
	if response != nil {
		return response, nil
	}
	return nil, executionError
}

// serveTree owns one node submission from planning through final reporting.
func (r *commandRelay) serveTree(ctx context.Context, submission io.ReadWriteCloser, source int, header *remote.Submission) {
	if r.trees == nil || r.controller == nil {
		remote.Reject(submission, errors.New("workspace tree routing is unavailable"))
		return
	}
	route, err := r.trees.plan(r, source, header.Node)
	if err != nil {
		remote.Reject(submission, err)
		return
	}
	if header.Revision != "" && route.revision != header.Revision {
		remote.Reject(submission, errNodeGraphChanged)
		return
	}
	remote.ServeForwarded(ctx, submission, func(ctx context.Context, request *remote.Request, stdin io.Reader, stdout, stderr io.Writer, event func(*remote.CommandEvent) error) (*remote.CommandResult, error) {
		return route.run(ctx, request, stdin, stdout, stderr, header.NoFlush, header.Progress, event)
	})
}
