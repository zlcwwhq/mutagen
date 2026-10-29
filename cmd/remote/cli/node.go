package cli

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// selection preserves the original source registration while the daemon selects
// the destination. Internal edge identities are never used to choose a source.
func (o *options) selection(corresponding bool) *remote.Selection {
	return &remote.Selection{Workspace: true, WorkspaceOutside: o.node != "", Corresponding: corresponding, Node: o.node, Resolve: true, Target: o.target}
}

// locateSource fixes the local registration used to choose an orchestration
// policy. Destination selection stays in the daemon, and the corresponding
// directory check remains enabled for the first actual submission.
func (o *options) locateSource(ctx context.Context, directory string, corresponding bool) (*remote.Selection, *remote.Target, error) {
	selection := o.selection(corresponding)
	source, err := selection.Locate(ctx, directory)
	if err != nil {
		return nil, nil, err
	}
	selection.Target = source
	return selection, source, nil
}

// runNode submits one command, or the start of a detached job, to the tree
// coordinator through the selection that locateSource pinned to the source
// registration. That selection retains Corresponding, so an ignored submission
// directory is refused locally with the same guidance as for pairs (see
// remote.Selection.Corresponding). The daemon checks every edge on the path as
// well, as a backstop, but the edges of a valid tree share their ignores, and
// the daemon refuses commands in an invalid one, so it accepts nothing that
// this refuses.
//
// The submission declares Command, which makes the daemon own both
// synchronization phases: a failed preflight stops the command, and a failed
// return synchronization can't look successful. --no-flush skips both phases,
// along with their conflict checks, as it skips both flushes for a pair (see
// remote.Submission.NoFlush). This is a contract with the remote executable
// rather than a security boundary, since a client that omits Command takes the
// lenient adjacent path (see remote.Submission.Command) and could run a
// command on an adjacent node without that synchronization anyway.
func runNode(ctx context.Context, directory string, options *options, request *remote.Request, selection *remote.Selection, stdin io.Reader, stdout, stderr io.Writer, interrupted func(string) (int, bool)) int {
	progress := newProgressOutput(stderr, options.progress)
	connection, target, err := selection.Submit(ctx, directory, &remote.Submission{
		Operation: remote.OperationTreeExecute, Command: true, Detached: options.detach, NoFlush: options.noFlush, Progress: progress.writer != nil,
	})
	var code int
	if err == nil {
		events := newCommandEvents(ctx, stderr, progress, target)
		defer events.finish()

		request.Path = target.Path
		request.Prologue = target.Contained && !options.noPrologue
		if request.Job != nil {
			code, err = startJobWithEvents(ctx, connection, target, request, stdout, stderr, events.receive)
		} else {
			code, err = remote.ExecuteWithEvents(ctx, connection, request, stdin, stdout, stderr, events.receive)
		}
	}
	activity := "running the tree command and synchronizing its path"
	if options.noFlush {
		activity = "running the tree command"
	}
	if status, ok := interrupted(activity); ok {
		return status
	} else if err != nil {
		printCommandError(stderr, err, request.Directory)
		if code != 0 {
			return code
		}
		return errorExitCode(err)
	}
	return code
}

// commandEvents reuses ordinary progress and report rendering for daemon-led
// synchronization, with one report deduplication set per actual edge.
type commandEvents struct {
	ctx      context.Context
	output   io.Writer
	progress *progressOutput
	source   *remote.Target
	display  *progressDisplay
	reports  map[string]*reporter
}

func newCommandEvents(ctx context.Context, output io.Writer, progress *progressOutput, source *remote.Target) *commandEvents {
	return &commandEvents{ctx: ctx, output: output, progress: progress, source: source, reports: make(map[string]*reporter)}
}
func (e *commandEvents) finish() {
	if e.display != nil {
		e.display.finish(e.ctx, e.ctx.Err())
		e.display = nil
	}
}
func (e *commandEvents) receive(event *remote.CommandEvent) {
	key := event.Alpha + "\x00" + event.Beta
	target := *e.source
	target.Tree, target.Alpha, target.SourceNode, target.Node = true, true, event.Alpha, event.Beta
	if event.Progress != nil {
		if e.display == nil {
			e.display = newProgressDisplay(e.progress, &target)
			if e.display != nil {
				e.display.edge = terminal.NeutralizeControlCharacters(event.Alpha + " <-> " + event.Beta + " (" + event.Phase + ")")
				e.display.alphaNode = terminal.NeutralizeControlCharacters(event.Alpha)
				e.display.betaNode = terminal.NeutralizeControlCharacters(event.Beta)
				e.display.begin()
			}
		}
		if e.display != nil {
			e.display.update(event.Progress)
		}
	}
	if !event.Done {
		return
	}
	if e.display != nil {
		var err error
		if event.Error != "" {
			err = errors.New(event.Error)
		}
		e.display.finish(e.ctx, err)
		e.display = nil
	}
	if e.reports[key] == nil {
		e.reports[key] = newReporter(e.output, &target)
	}
	e.reports[key].print(event.Report)
}

// commandErrorOutput serializes command stderr with daemon event rendering.
type commandErrorOutput struct {
	lock   sync.Mutex
	writer io.Writer
}

func (w *commandErrorOutput) Write(data []byte) (int, error) {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.writer.Write(data)
}

// Fd preserves terminal detection when the underlying output exposes a file.
func (w *commandErrorOutput) Fd() uintptr {
	if file, ok := w.writer.(interface{ Fd() uintptr }); ok {
		return file.Fd()
	}
	return ^uintptr(0)
}
