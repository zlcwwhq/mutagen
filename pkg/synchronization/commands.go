package synchronization

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	} else if operation == remote.OperationFlush {
		r.logger.Debug("Forcing synchronization cycle for submission")
		if err := r.flush(ctx); err != nil {
			remote.RespondToFlush(submission, nil, err)
		} else {
			remote.RespondToFlush(submission, r.report(), nil)
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
