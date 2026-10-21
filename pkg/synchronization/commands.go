package synchronization

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
)

const (
	// commandRouteOpenTimeout bounds the time spent opening a route to an
	// executing endpoint.
	commandRouteOpenTimeout = 30 * time.Second
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
var errFlushTerminated = errors.New("synchronization stopped before the flush completed")

// errResolutionUnsupported indicates that the session's synchronization mode
// doesn't support conflict resolution.
var errResolutionUnsupported = errors.New("conflict resolution requires the two-way-safe or one-way-safe synchronization mode")

// commandRelay routes commands submitted on each endpoint's system to the
// opposite endpoint. Routes always pass through the daemon, so neither endpoint
// needs connectivity to the other, regardless of where the daemon runs. It also
// performs flushes requested by submissions.
type commandRelay struct {
	// logger is the relay logger.
	logger *logging.Logger
	// flushRequests is the synchronization loop's flush request channel.
	flushRequests chan chan error
	// report reports content that the session couldn't synchronize.
	report func() *remote.Report
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
	// routes tracks active routes.
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
	mode := c.session.Configuration.RemoteCommandMode
	if mode.IsDefault() {
		mode = c.session.Version.DefaultRemoteCommandMode()
	}
	alphaExecutes, betaExecutes := mode.Executes()
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
	ctx, cancel := context.WithCancel(ctx)
	var maintainers sync.WaitGroup
	for index, url := range []*urlpkg.URL{c.session.Alpha, c.session.Beta} {
		endpoint := &remote.Endpoint{
			Session:  c.session.Identifier,
			Name:     c.session.Name,
			Root:     url.Path,
			Alpha:    index == 0,
			Submit:   (index == 0 && betaExecutes) || (index == 1 && alphaExecutes),
			Execute:  (index == 0 && alphaExecutes) || (index == 1 && betaExecutes),
			Prologue: prologues[index],
		}
		maintainers.Add(1)
		go func() {
			defer maintainers.Done()
			relay.maintain(ctx, index, url, endpoint)
		}()
	}
	return func() {
		cancel()
		maintainers.Wait()
		relay.routes.Wait()
	}
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
// executor of the specified endpoint.
func (r *commandRelay) route(ctx context.Context, submission io.ReadWriteCloser, target int) {
	operation, err := remote.ReceiveSubmission(ctx, submission)
	if err != nil {
		remote.Reject(submission, err)
		return
	} else if operation == remote.OperationConflicts {
		r.logger.Debug("Forcing synchronization cycle for conflict listing")
		stop := context.AfterFunc(ctx, func() { submission.Close() })
		defer stop()
		var listing *remote.Listing
		if r.conflicts == nil {
			err = errResolutionUnsupported
		} else if err = r.flush(ctx); err == nil {
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
		if err = r.flush(ctx); err == nil {
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
		remote.Reject(submission, fmt.Errorf("%s is not connected for remote commands", endpointName(target)))
		return
	}
	openCtx, cancel := context.WithTimeout(ctx, commandRouteOpenTimeout)
	execution, err := channel.Open(openCtx)
	cancel()
	if err != nil {
		remote.Reject(submission, fmt.Errorf("unable to reach %s: %w", endpointName(target), err))
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
	root := remote.ConflictRoot{Path: conflict.Root}
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
