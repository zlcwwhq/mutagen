package synchronization

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/rsync"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
)

const (
	// commandRouteOpenTimeout bounds the time spent opening a route to an
	// executing endpoint.
	commandRouteOpenTimeout = 30 * time.Second
)

var (
	// progressSampleInterval is the interval at which the progress of a
	// session is sampled for each submission that waits for a synchronization
	// cycle and requested progress (see remote.ReportProgress). It's a
	// variable so that tests can shorten it.
	progressSampleInterval = time.Second
	// progressHeartbeatInterval is the longest interval between progress
	// messages while the progress doesn't change. It's a variable so that
	// tests can shorten it.
	progressHeartbeatInterval = 10 * time.Second
)

// CommandProtocolHandler is an optional extension of ProtocolHandler for
// protocols that support command routing between endpoints.
type CommandProtocolHandler interface {
	// ConnectCommands connects to an endpoint's command facilities using the
	// connection parameters in the provided URL. Connection must not require
	// interactive prompting.
	ConnectCommands(
		ctx context.Context,
		logger *logging.Logger,
		url *urlpkg.URL,
		endpoint *remote.Endpoint,
	) (remote.Channel, error)
}

// errFlushTerminated indicates that synchronization stopped before a flush
// requested by a submission completed.
var errFlushTerminated = remote.ConnectionError(errors.New("synchronization stopped before the flush completed"))

// errResolutionUnsupported indicates that the session's synchronization mode
// doesn't support conflict resolution.
var errResolutionUnsupported = errors.New("conflict resolution requires the two-way-safe or one-way-safe synchronization mode")

// commandRelay routes commands submitted on each endpoint's system to the
// opposite endpoint. Routes always pass through the daemon, so neither endpoint
// needs connectivity to the other, regardless of where the daemon runs. It also
// performs flushes requested by submissions.
type commandRelay struct {
	// stars is the daemon's index of sessions and relays, which describes the
	// nodes that a submission can reach (see resolveSubmission).
	stars *commandStars
	// controller is the session's controller.
	controller *controller
	// ctx is cancelled when the relay stops.
	ctx context.Context
	// logger is the relay logger.
	logger *logging.Logger
	// flushRequests is the synchronization loop's flush request channel.
	flushRequests chan chan error
	// report reports content that the session couldn't synchronize.
	report func() *remote.Report
	// progress samples the session's progress for submissions that request
	// it, or is nil if progress isn't reported.
	progress func() *remote.Progress
	// conflicts lists the session's conflicts and checkouts for resolution, or
	// is nil if the session's synchronization mode doesn't support resolution,
	// in which case claim and release are nil too.
	conflicts func() (*remote.Listing, error)
	// claim records a checkout by alpha or beta (see claimCheckout).
	claim func(alpha bool, path, token string) (remote.CheckoutStatus, error)
	// release removes a checkout held by alpha or beta (see releaseCheckout).
	release func(alpha bool, path, token string) (remote.CheckoutStatus, error)
	// channelsLock guards channels.
	channelsLock sync.Mutex
	// channels holds the connected alpha and beta channels (or nil).
	channels [2]remote.Channel
	// routes tracks the accept loops and the routes that they accept, each of
	// which only uses this relay's state, since operations never cross another
	// edge than the one through which they were submitted. Accept loops are
	// added by the channel maintainers, which stop before routes are waited
	// for, and routes by their accept loops while those are tracked.
	routes sync.WaitGroup
}

// endpointName returns the name of an endpoint by relay index.
func endpointName(index int) string {
	if index == 0 {
		return "alpha"
	}
	return "beta"
}

// startCommandRelay starts routing commands if enabled for the session. It
// returns a function that stops routing and waits for all channels, routes,
// and commands to terminate.
func (c *controller) startCommandRelay(ctx context.Context) func() {
	alphaExecutes, betaExecutes := executingEndpoints(c.session)
	if !alphaExecutes && !betaExecutes {
		return func() {}
	}

	// The relay submits flush requests directly to the synchronization loop
	// rather than using flush, whose lifecycle lock may be held by an operation
	// waiting for the relay to stop. The request channel is fixed while the
	// synchronization loop runs.
	relay := &commandRelay{
		logger:        c.logger.Sublogger("commands"),
		flushRequests: c.flushRequests,
		report:        c.remoteReport,
		progress:      c.remoteProgress,
	}

	// Resolution writes the same result to both endpoints, which is only
	// consistent with the safe modes, since other modes resolve conflicts by
	// giving alpha precedence.
	synchronizationMode := c.session.Configuration.SynchronizationMode
	if synchronizationMode.IsDefault() {
		synchronizationMode = c.session.Version.DefaultSynchronizationMode()
	}
	if synchronizationMode == core.SynchronizationMode_SynchronizationModeTwoWaySafe ||
		synchronizationMode == core.SynchronizationMode_SynchronizationModeOneWaySafe {
		hashingAlgorithm := c.session.Configuration.HashingAlgorithm
		if hashingAlgorithm.IsDefault() {
			hashingAlgorithm = c.session.Version.DefaultHashingAlgorithm()
		}
		algorithm, _ := hashingAlgorithm.MarshalText()
		relay.conflicts = func() (*remote.Listing, error) {
			return c.remoteConflicts(string(algorithm))
		}
		relay.claim = c.claimCheckout
		relay.release = c.releaseCheckout
	}

	// Each endpoint submits commands if and only if its opposite executes.
	// Prologues are endpoint-specific, since they're interpreted on the
	// executing endpoint's system.
	prologues := [2]string{
		c.mergedAlphaConfiguration.RemotePrologue,
		c.mergedBetaConfiguration.RemotePrologue,
	}

	// Submissions only run commands in directories that the session
	// synchronizes. Ignores can't be endpoint-specific, so both endpoints'
	// merged configurations specify the same ones.
	ignores := EffectiveIgnores(c.mergedAlphaConfiguration, c.session.Version)
	ctx, cancel := context.WithCancel(ctx)
	relay.stars, relay.controller, relay.ctx = c.stars, c, ctx
	c.stars.setRelay(c, relay)
	var maintainers sync.WaitGroup
	urls := [2]*urlpkg.URL{c.session.Alpha, c.session.Beta}
	for index, url := range urls {
		executionHost, executionRoot := commandExecutionLocation(urls[1-index])
		// Star endpoints are named by their nodes, and the endpoints of other
		// sessions form an implicit pair of nodes named alpha and beta.
		names := []string{"alpha", "beta"}
		if c.session.Star != nil {
			names = []string{c.session.Star.Center, c.session.Star.Node}
		}
		endpoint := &remote.Endpoint{
			Star:           c.session.GetStar().GetIdentifier(),
			Node:           names[index],
			PeerNode:       names[1-index],
			Session:        c.session.Identifier,
			Name:           c.session.Name,
			Root:           url.Path,
			ExecutionHost:  executionHost,
			ExecutionRoot:  executionRoot,
			Alpha:          index == 0,
			SessionCreated: c.session.CreationTime.AsTime(),
			Submit:         (index == 0 && betaExecutes) || (index == 1 && alphaExecutes),
			Execute:        (index == 0 && alphaExecutes) || (index == 1 && betaExecutes),
			Prologue:       prologues[index],
			Ignores:        ignores,
		}
		maintainers.Add(1)
		go func() {
			defer maintainers.Done()
			relay.maintain(ctx, index, url, endpoint)
		}()
	}
	return func() {
		// Routes observe the cancellation through the relay's context. The
		// relay's channels close before the wait, which ends routes spliced to
		// them.
		cancel()
		c.stars.setRelay(c, nil)
		maintainers.Wait()
		relay.routes.Wait()
	}
}

// commandExecutionLocation describes a configured execution endpoint without
// revealing credentials or transport configuration. Local endpoints run on the
// daemon's system, whose hostname also identifies it to remote submitters.
func commandExecutionLocation(url *urlpkg.URL) (string, string) {
	host := url.Host
	if url.Protocol == urlpkg.Protocol_Local {
		host, _ = os.Hostname()
	}
	return host, url.Path
}

// maintain keeps an endpoint's channel connected until cancelled. Channel
// failures do not affect synchronization.
func (r *commandRelay) maintain(ctx context.Context, index int, url *urlpkg.URL, endpoint *remote.Endpoint) {
	logger := r.logger.Sublogger(endpointName(index))
	handler, ok := ProtocolHandlers[url.Protocol].(CommandProtocolHandler)
	if !ok {
		logger.Warn("Remote commands are not supported for", url.Protocol, "endpoints")
		return
	}
	for {
		channel, err := handler.ConnectCommands(ctx, logger, url, endpoint)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("Unable to connect command channel:", err)
		} else {
			logger.Debug("Command channel connected")
			r.channelsLock.Lock()
			r.channels[index] = channel
			r.channelsLock.Unlock()
			if endpoint.Submit {
				r.routes.Add(1)
				go func() {
					defer r.routes.Done()
					r.accept(ctx, index, channel)
				}()
			}
			select {
			case <-channel.Done():
				logger.Warn("Command channel disconnected")
			case <-ctx.Done():
			}
			r.channelsLock.Lock()
			r.channels[index] = nil
			r.channelsLock.Unlock()
			channel.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(autoReconnectInterval):
		}
	}
}

// accept routes commands submitted through a channel to the opposite endpoint
// until the channel closes.
func (r *commandRelay) accept(ctx context.Context, index int, channel remote.Channel) {
	for {
		submission, err := channel.Accept()
		if err != nil {
			return
		}
		r.routes.Add(1)
		go func() {
			defer r.routes.Done()
			r.route(ctx, submission, 1-index)
		}()
	}
}

// route handles a submission by performing a flush or by connecting it to the
// executor of the specified endpoint, which is always the opposite endpoint of
// this relay's session: a submission that selects a node on another edge is
// refused (see resolveSubmission).
func (r *commandRelay) route(ctx context.Context, submission io.ReadWriteCloser, target int) {
	header, err := remote.ReceiveSubmission(ctx, submission)
	if err != nil {
		remote.Reject(submission, err)
		return
	}
	if header.Resolve || header.Node != "" {
		route, err := r.resolveSubmission(1-target, header)
		if err != nil {
			remote.Reject(submission, err)
			return
		} else if header.Resolve {
			if err := remote.RespondWithRoute(submission, route); err != nil {
				submission.Close()
				return
			}
		}
	}
	r.routeResolved(ctx, submission, target, header)
}

// routeResolved executes an operation after its destination is resolved.
func (r *commandRelay) routeResolved(ctx context.Context, submission io.ReadWriteCloser, target int, header *remote.Submission) {
	var err error
	operation := header.Operation
	if operation == remote.OperationResolve {
		submission.Close()
		return
	}
	if operation == remote.OperationNodes {
		graph, err := r.stars.nodes(r, 1-target)
		remote.RespondWithNodes(submission, graph, err)
		return
	} else if operation == remote.OperationConflicts {
		r.logger.Debug("Forcing synchronization cycle for conflict listing")
		stop := context.AfterFunc(ctx, func() { submission.Close() })
		defer stop()
		var listing *remote.Listing
		if r.conflicts == nil {
			err = errResolutionUnsupported
		} else if err = r.flushReporting(ctx, submission, header.Progress); err == nil {
			listing, err = r.conflicts()
		}
		if err := remote.RespondWithConflicts(submission, listing, err); err != nil {
			r.logger.Debug("Unable to send conflicts:", err)
		}
		return
	} else if operation == remote.OperationClaim || operation == remote.OperationRelease {
		// Checkouts belong to the submitting endpoint, which is the opposite
		// of the target.
		alpha := target == 1
		path, token, err := remote.ReceiveCheckout(ctx, submission)
		var status remote.CheckoutStatus
		if err == nil && r.claim == nil {
			err = errResolutionUnsupported
		} else if err == nil && operation == remote.OperationClaim {
			status, err = r.claim(alpha, path, token)
		} else if err == nil {
			status, err = r.release(alpha, path, token)
		}
		if err := remote.RespondToCheckout(submission, status, err); err != nil {
			r.logger.Debug("Unable to send checkout status:", err)
		}
		return
	} else if operation == remote.OperationFlush {
		r.logger.Debug("Forcing synchronization cycle for submission")
		stop := context.AfterFunc(ctx, func() { submission.Close() })
		defer stop()
		var report *remote.Report
		if err = r.flushReporting(ctx, submission, header.Progress); err == nil {
			report = r.report()
			report.Resolution = r.conflicts != nil
		}
		if err := remote.RespondToFlush(submission, report, err); err != nil {
			r.logger.Debug("Unable to send synchronization report:", err)
		}
		return
	}
	r.channelsLock.Lock()
	channel := r.channels[target]
	r.channelsLock.Unlock()
	if channel == nil {
		remote.Reject(submission, remote.ConnectionError(fmt.Errorf("%s is not connected for remote commands", endpointName(target))))
		return
	}
	openCtx, cancel := context.WithTimeout(ctx, commandRouteOpenTimeout)
	execution, err := channel.Open(openCtx)
	cancel()
	if err != nil {
		remote.Reject(submission, remote.ConnectionError(fmt.Errorf("unable to reach %s: %w", endpointName(target), err)))
		return
	}
	remote.Splice(submission, execution)
}

// flush forces a synchronization cycle and waits for it to complete, failing if
// the context (and thus synchronization) is cancelled first.
func (r *commandRelay) flush(ctx context.Context) error {
	request := make(chan error, 1)
	select {
	case r.flushRequests <- request:
	case <-ctx.Done():
		return errFlushTerminated
	}
	select {
	case err := <-request:
		return err
	case <-ctx.Done():
		return errFlushTerminated
	}
}

// flushReporting performs a flush (see flush) for a submission, sending it
// progress messages while it waits if it requested them (see
// remote.ReportProgress). Messages stop before it returns, so that the final
// response follows them. Submissions that don't request progress, such as
// those of earlier versions, which expect a single response, receive none.
func (r *commandRelay) flushReporting(ctx context.Context, submission io.Writer, progress bool) error {
	if !progress || r.progress == nil {
		return r.flush(ctx)
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		remote.ReportProgress(submission, r.progress, progressSampleInterval, progressHeartbeatInterval, stop)
	}()
	err := r.flush(ctx)
	close(stop)
	<-stopped
	return err
}

// remoteProgress samples the session's synchronization status and staging
// progress for submissions that wait for a synchronization cycle and requested
// progress (see remote.Submission.Progress). The staging endpoint's progress
// is copied while the state lock is held, since it's updated in place.
func (c *controller) remoteProgress() *remote.Progress {
	c.stateLock.Lock()
	defer c.stateLock.UnlockWithoutNotify()
	status, _ := c.state.Status.MarshalText()
	progress := &remote.Progress{Status: string(status)}
	var staging *rsync.ReceiverState
	if c.state.Status == Status_StagingAlpha {
		staging = c.state.GetAlphaState().GetStagingProgress()
	} else if c.state.Status == Status_StagingBeta {
		staging = c.state.GetBetaState().GetStagingProgress()
	}
	if staging != nil {
		progress.Staging = &remote.StagingProgress{
			ReceivedFiles:    staging.ReceivedFiles,
			ExpectedFiles:    staging.ExpectedFiles,
			ReceivedSize:     staging.TotalReceivedSize,
			Path:             staging.Path,
			FileReceivedSize: staging.ReceivedSize,
			FileExpectedSize: staging.ExpectedSize,
		}
	}
	return progress
}

// remoteReport reports the content that the session couldn't synchronize in
// its last synchronization cycle, sorted and truncated as for session listings.
func (c *controller) remoteReport() *remote.Report {
	// Conflicts and problems are immutable and their lists are replaced rather
	// than modified, so copying the lists yields a consistent snapshot.
	c.stateLock.Lock()
	conflicts := core.CopyConflicts(c.state.Conflicts)
	problems := []struct {
		alpha    bool
		problems []*core.Problem
		maximum  int
	}{
		{true, core.CopyProblems(c.state.AlphaState.ScanProblems), maximumListScanProblems},
		{true, core.CopyProblems(c.state.AlphaState.TransitionProblems), maximumListTransitionProblems},
		{false, core.CopyProblems(c.state.BetaState.ScanProblems), maximumListScanProblems},
		{false, core.CopyProblems(c.state.BetaState.TransitionProblems), maximumListTransitionProblems},
	}
	c.stateLock.UnlockWithoutNotify()

	// Convert conflicts.
	report := &remote.Report{}
	core.SortConflicts(conflicts)
	if len(conflicts) > maximumListConflicts {
		report.ExcludedConflicts = uint64(len(conflicts) - maximumListConflicts)
		conflicts = conflicts[:maximumListConflicts]
	}
	for _, conflict := range conflicts {
		report.Conflicts = append(report.Conflicts, remote.Conflict{
			Root:  conflict.Root,
			Alpha: remoteChanges(conflict.AlphaChanges),
			Beta:  remoteChanges(conflict.BetaChanges),
		})
	}

	// Convert problems.
	for _, list := range problems {
		core.SortProblems(list.problems)
		if len(list.problems) > list.maximum {
			report.ExcludedProblems += uint64(len(list.problems) - list.maximum)
			list.problems = list.problems[:list.maximum]
		}
		for _, problem := range list.problems {
			report.Problems = append(report.Problems, remote.Problem{
				Alpha: list.alpha,
				Path:  problem.Path,
				Error: problem.Error,
			})
		}
	}
	return report
}

// remoteConflicts lists all of the session's conflicts and problems as of its
// last synchronization cycle for resolution, determining how remote can
// resolve each conflict, along with the session's checkouts. The digests of
// conflicting files and of their common ancestor are reported with the textual
// name of the session's hashing algorithm.
func (c *controller) remoteConflicts(algorithm string) (*remote.Listing, error) {
	// Conflicts and problems are immutable and their lists are replaced rather
	// than modified, so the lists can be read once the lock is released.
	c.stateLock.Lock()
	conflicts := slices.Clone(c.state.Conflicts)
	problems := []struct {
		alpha    bool
		problems []*core.Problem
	}{
		{true, c.state.AlphaState.ScanProblems},
		{true, c.state.AlphaState.TransitionProblems},
		{false, c.state.BetaState.ScanProblems},
		{false, c.state.BetaState.TransitionProblems},
	}
	c.stateLock.UnlockWithoutNotify()
	core.SortConflicts(conflicts)
	listing := &remote.Listing{Conflicts: make([]remote.ConflictRoot, len(conflicts))}
	for c, conflict := range conflicts {
		listing.Conflicts[c] = resolution(conflict, algorithm)
	}
	for _, list := range problems {
		for _, problem := range list.problems {
			listing.Problems = append(listing.Problems, remote.Problem{Alpha: list.alpha, Path: problem.Path, Error: problem.Error})
		}
	}
	checkouts, err := c.checkouts()
	if err != nil {
		return nil, err
	}
	listing.Checkouts = checkouts
	return listing, nil
}

// resolution determines how remote can resolve a conflict in a safe
// synchronization mode, in which it writes the same result to both endpoints,
// which synchronization then considers to agree.
//
// Conflicts between regular files consist of a single change at the conflict
// root on each side, and the result can be any file, such as a merge of both
// copies. Their changes' old entries are the common ancestor, and their new
// entries are the copies, whose digests bind the ancestor to the copies that
// it's the ancestor of.
//
// Conflicts involving directories require manual resolution, as do other
// conflicts at the root, which change its type. For other conflicts, such as
// between a file and a symbolic link, the result is one of the copies, which
// can't be content that can't be synchronized (such as a named pipe), and
// replacing the other copy requires reading it.
func resolution(conflict *core.Conflict, algorithm string) remote.ConflictRoot {
	root := remote.ConflictRoot{
		Path: conflict.Root, AlphaChanges: remoteChanges(conflict.AlphaChanges),
		BetaChanges: remoteChanges(conflict.BetaChanges),
	}
	files, directory := true, false
	for _, changes := range [][]*core.Change{conflict.AlphaChanges, conflict.BetaChanges} {
		if len(changes) != 1 || changes[0].Path != conflict.Root ||
			changes[0].New == nil || changes[0].New.Kind != core.EntryKind_File {
			files = false
		}
		for _, change := range changes {
			// A change below the conflict root also identifies a directory,
			// even if no change describes the directory itself.
			if change.Path != conflict.Root || (change.New != nil && change.New.Kind == core.EntryKind_Directory) {
				directory = true
			}
		}
	}
	if files {
		root.Files, root.AlphaTakable, root.BetaTakable = true, true, true
		root.Algorithm = algorithm
		root.AlphaDigest = fmt.Sprintf("%x", conflict.AlphaChanges[0].New.Digest)
		root.BetaDigest = fmt.Sprintf("%x", conflict.BetaChanges[0].New.Digest)
		if base := conflict.AlphaChanges[0].Old; base != nil && base.Kind == core.EntryKind_File {
			root.BaseDigest = fmt.Sprintf("%x", base.Digest)
		}
		return root
	} else if conflict.Root == "" || directory {
		return root
	}
	alphaUnsynchronizable := containsKind(conflict.AlphaChanges, core.EntryKind_Untracked, core.EntryKind_Problematic)
	betaUnsynchronizable := containsKind(conflict.BetaChanges, core.EntryKind_Untracked, core.EntryKind_Problematic)
	alphaUnreadable := containsKind(conflict.AlphaChanges, core.EntryKind_Problematic)
	betaUnreadable := containsKind(conflict.BetaChanges, core.EntryKind_Problematic)
	root.AlphaTakable = !alphaUnsynchronizable && !betaUnreadable
	root.BetaTakable = !betaUnsynchronizable && !alphaUnreadable
	return root
}

// containsKind determines whether any of the new entries of a list of changes
// contains an entry of one of the specified kinds.
func containsKind(changes []*core.Change, kinds ...core.EntryKind) bool {
	var contains func(*core.Entry) bool
	contains = func(entry *core.Entry) bool {
		if entry == nil {
			return false
		} else if slices.Contains(kinds, entry.Kind) {
			return true
		}
		for _, content := range entry.Contents {
			if contains(content) {
				return true
			}
		}
		return false
	}
	return slices.ContainsFunc(changes, func(change *core.Change) bool {
		return contains(change.New)
	})
}

// remoteChanges converts changes for remote reports.
func remoteChanges(changes []*core.Change) []remote.Change {
	result := make([]remote.Change, len(changes))
	for c, change := range changes {
		result[c] = remote.Change{
			Path: change.Path,
			Old:  describeRemoteEntry(change.Old),
			New:  describeRemoteEntry(change.New),
		}
	}
	return result
}

// describeRemoteEntry describes the kind of an entry for remote reports.
func describeRemoteEntry(entry *core.Entry) string {
	if entry == nil {
		return ""
	}
	switch entry.Kind {
	case core.EntryKind_Directory:
		return "directory"
	case core.EntryKind_File:
		if entry.Executable {
			return "executable file"
		}
		return "file"
	case core.EntryKind_SymbolicLink:
		return "symbolic link"
	case core.EntryKind_Untracked:
		return "untracked content"
	case core.EntryKind_Problematic:
		return fmt.Sprintf("problematic content (%s)", entry.Problem)
	default:
		return "unknown content"
	}
}
