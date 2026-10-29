package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dustin/go-humanize"
	"golang.org/x/term"

	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// ProgressDelay is how long remote waits for a synchronization before showing
// its progress, so that fast synchronizations show none. It's exported only so
// that tests in other packages can shorten it, and isn't configuration.
var ProgressDelay = 3 * time.Second

var (
	// progressLineInterval is the longest interval between progress lines on
	// output that isn't a terminal while the phase doesn't change. It's a
	// variable so that tests can shorten it.
	progressLineInterval = 10 * time.Second
	// progressRefreshInterval is the interval at which progress output is
	// refreshed: a terminal's line is redrawn, so that the elapsed time stays
	// current, and other output gets a line once progressLineInterval has
	// elapsed since the last one. It's a variable so that tests can shorten
	// it.
	progressRefreshInterval = time.Second
)

// defaultTerminalWidth is the width assumed for terminals whose width can't be
// determined.
const defaultTerminalWidth = 80

// progressOutput is where synchronization progress is shown (see --progress).
type progressOutput struct {
	// writer receives progress. Progress isn't shown if it's nil.
	writer io.Writer
	// terminal indicates whether writer is a terminal, on which progress is
	// shown on a single line updated in place, rather than as separate lines.
	terminal bool
	// width is the terminal's width in columns.
	width int
}

// newProgressOutput determines where synchronization progress is shown, if
// anywhere: on standard error if enabled (from --progress) is true, or, if
// it's nil, only if standard error is a terminal, following git's convention.
func newProgressOutput(stderr io.Writer, enabled *bool) *progressOutput {
	isTerminal, width := terminalWidth(stderr)
	if enabled != nil && !*enabled || enabled == nil && !isTerminal {
		return &progressOutput{}
	}
	return &progressOutput{writer: stderr, terminal: isTerminal, width: width}
}

// terminalWidth determines whether output is a terminal and, if it is, its
// width in columns.
func terminalWidth(output io.Writer) (bool, int) {
	file, ok := output.(interface{ Fd() uintptr })
	if !ok {
		return false, 0
	}
	descriptor := int(file.Fd())
	if !term.IsTerminal(descriptor) {
		return false, 0
	}
	width, _, err := term.GetSize(descriptor)
	if err != nil || width <= 0 {
		width = defaultTerminalWidth
	}
	return true, width
}

// progressPhase accumulates the time that a synchronization spent with one
// status, over every period in which the session had that status.
type progressPhase struct {
	// status is the session's status (see remote.Progress.Status).
	status string
	// duration is the time spent in the phase.
	duration time.Duration
	// staged indicates whether staging progress was received for the phase.
	staged bool
	// files and size are the number of files staged and their total size, as
	// of the last staging progress received for each staging operation (see
	// recordStaging), which may understate the size.
	files, size uint64
}

// progressDisplay shows the progress of a synchronization that remote waits
// for (see remote.FlushWithProgress), starting once ProgressDelay has elapsed,
// and finishes with a summary of where the synchronization spent its time.
// Daemons that predate progress messages send none, so only the elapsed time
// is shown for them. A nil display shows nothing.
type progressDisplay struct {
	// output is the progress output.
	output *progressOutput
	// locator maps paths for display.
	locator *locator
	// alpha indicates whether the submitting endpoint is alpha.
	alpha bool
	// now returns the current time. Tests can replace it.
	now func() time.Time
	// start is the time at which the synchronization started.
	start time.Time
	// stop is closed to stop refreshing, which closes stopped.
	stop, stopped chan struct{}
	// lock serializes updates, refreshes, and output.
	lock sync.Mutex
	// visible indicates whether progress is being shown.
	visible bool
	// latest is the latest progress received, if any.
	latest *remote.Progress
	// phaseStart is the time at which the latest progress's status began.
	phaseStart time.Time
	// staging is the latest staging progress received in the current period
	// of the latest status, if staged is true.
	staging remote.StagingProgress
	// staged indicates whether staging is set.
	staged bool
	// phases are the phases seen, in the order in which they began.
	phases []*progressPhase
	// lastLine is the time at which the last line was printed, on output that
	// isn't a terminal.
	lastLine time.Time
	// shown is the length of the line shown on a terminal, if any.
	shown int
}

// newProgressDisplay creates a display for a synchronization that has started
// through the specified target, or returns nil if progress isn't shown. It
// must be finished with finish.
func newProgressDisplay(output *progressOutput, target *remote.Target) *progressDisplay {
	if output.writer == nil {
		return nil
	}
	return &progressDisplay{
		output:  output,
		locator: newLocator(target),
		alpha:   target.Alpha,
		now:     time.Now,
	}
}

// begin records the start of the synchronization and starts refreshing the
// display. It returns the function that receives progress messages.
func (d *progressDisplay) begin() func(*remote.Progress) {
	if d == nil {
		return nil
	}
	d.start = d.now()
	d.stop, d.stopped = make(chan struct{}), make(chan struct{})
	go d.refresh()
	return d.update
}

// refresh shows progress once ProgressDelay has elapsed and refreshes it until
// stopped.
func (d *progressDisplay) refresh() {
	defer close(d.stopped)
	delay := time.NewTimer(ProgressDelay)
	defer delay.Stop()
	select {
	case <-delay.C:
	case <-d.stop:
		return
	}
	d.lock.Lock()
	now := d.now()
	if !d.visible {
		d.visible = true
		d.show(now)
	}
	d.lock.Unlock()
	ticker := time.NewTicker(progressRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-d.stop:
			return
		}
		d.lock.Lock()
		now := d.now()
		if d.output.terminal {
			d.draw(now)
		} else if now.Sub(d.lastLine) >= progressLineInterval {
			d.print(now)
		}
		d.lock.Unlock()
	}
}

// update records a progress message and shows it if progress is visible: on a
// terminal, the line is redrawn, while other output only gets a line when the
// phase changes, so that each line is meaningful on its own.
func (d *progressDisplay) update(progress *remote.Progress) {
	d.lock.Lock()
	defer d.lock.Unlock()
	now := d.now()
	changed := d.latest == nil || d.latest.Status != progress.Status
	if changed {
		// The session is only briefly between cycles while a submission waits
		// (see statusLabels), so the time until the following sample, which
		// can be up to the daemon's sampling interval, belongs to the
		// following phase, and a change to it doesn't merit a line. Staging
		// completes unless synchronization stops.
		d.endPhase(now, !stopping(progress.Status))
		if d.latest == nil || d.latest.Status != watchingStatus {
			d.phaseStart = now
		}
		if progress.Status != watchingStatus {
			d.phase(progress.Status)
		} else {
			changed = false
		}
	}
	if staging := progress.Staging; staging != nil {
		// A sample of another staging operation, which began after the last
		// sample of the previous one, completes the previous one.
		if d.staged && (staging.ExpectedFiles != d.staging.ExpectedFiles ||
			staging.ReceivedFiles < d.staging.ReceivedFiles || staging.ReceivedSize < d.staging.ReceivedSize) {
			d.recordStaging(true)
		}
		d.staging, d.staged = *staging, true
	}
	d.latest = progress
	if !d.visible && now.Sub(d.start) >= ProgressDelay {
		d.visible = true
		d.show(now)
	} else if d.visible && (d.output.terminal || changed) {
		d.show(now)
	}
}

// phase returns the phase for a status, adding it if it's new.
func (d *progressDisplay) phase(status string) *progressPhase {
	for _, phase := range d.phases {
		if phase.status == status {
			return phase
		}
	}
	phase := &progressPhase{status: status}
	d.phases = append(d.phases, phase)
	return phase
}

// stopping determines whether a status indicates that synchronization stopped,
// rather than continuing with another phase.
func stopping(status string) bool {
	return status == "disconnected" || strings.HasPrefix(status, "halted-") || strings.HasPrefix(status, "connecting-")
}

// recordStaging adds the staging operation of the latest staging progress to
// its phase. Samples are taken periodically, so the latest one may not include
// the files received last. If the operation completed and the latest sample
// was taken during the last file, then the rest of that file is included, so
// that the size is exact for operations that only staged one file, or whose
// last file dominated.
func (d *progressDisplay) recordStaging(completed bool) {
	if !d.staged {
		return
	}
	phase := d.phase(d.latest.Status)
	phase.staged = true
	phase.files += d.staging.ExpectedFiles
	phase.size += d.staging.ReceivedSize
	if completed && d.staging.Path != "" && d.staging.ReceivedFiles+1 == d.staging.ExpectedFiles &&
		d.staging.FileReceivedSize < d.staging.FileExpectedSize {
		phase.size += d.staging.FileExpectedSize - d.staging.FileReceivedSize
	}
	d.staged = false
}

// endPhase adds the current period of the latest status to its phase, unless
// the session is between cycles (see update), and records its staging
// operation, if any (see recordStaging).
func (d *progressDisplay) endPhase(now time.Time, completed bool) {
	if d.latest == nil || d.latest.Status == watchingStatus {
		return
	}
	d.recordStaging(completed)
	d.phase(d.latest.Status).duration += now.Sub(d.phaseStart)
}

// show shows the current progress, on a single line on a terminal.
func (d *progressDisplay) show(now time.Time) {
	if d.output.terminal {
		d.draw(now)
	} else {
		d.print(now)
	}
}

// print prints the current progress as a line.
func (d *progressDisplay) print(now time.Time) {
	fmt.Fprintln(d.output.writer, d.describe(now))
	d.lastLine = now
}

// singleLine replaces the control characters that remain in a description
// once paths and statuses are neutralized for terminals, such as newlines in
// paths, so that progress always occupies a single line.
func singleLine(description string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, description)
}

// draw replaces the terminal's line with the current progress, truncated to
// the terminal's width and padded to cover the line that it replaces.
func (d *progressDisplay) draw(now time.Time) {
	line := d.describe(now)
	if limit := d.output.width - 1; limit > 0 && utf8.RuneCountInString(line) > limit {
		line = string([]rune(line)[:limit])
	}
	length := utf8.RuneCountInString(line)
	fmt.Fprint(d.output.writer, "\r"+line+strings.Repeat(" ", max(d.shown-length, 0)))
	d.shown = length
}

// clear clears the terminal's line, if progress is shown on it.
func (d *progressDisplay) clear() {
	if d.shown > 0 {
		fmt.Fprint(d.output.writer, "\r"+strings.Repeat(" ", d.shown)+"\r")
		d.shown = 0
	}
}

// describe describes the current progress, such as "remote: synchronizing for
// 13s: staging on alpha (local), 0 of 1 files, 1.2 MiB; out.bin: 1.2 MiB of
// 5.0 MiB".
func (d *progressDisplay) describe(now time.Time) string {
	description := "remote: synchronizing for " + formatElapsed(now.Sub(d.start))
	if d.latest == nil || d.latest.Status == "" {
		return description
	}
	description += ": " + d.label(d.latest.Status)
	if staging := d.latest.Staging; staging != nil {
		description += fmt.Sprintf(", %d of %d files, %s", staging.ReceivedFiles, staging.ExpectedFiles, humanize.IBytes(staging.ReceivedSize))
		if staging.Path != "" {
			description += fmt.Sprintf("; %s: %s of %s", d.locator.display(staging.Path),
				humanize.IBytes(staging.FileReceivedSize), humanize.IBytes(staging.FileExpectedSize))
		}
	}
	return singleLine(description)
}

// watchingStatus is the status of a session that's between cycles, waiting for
// changes. While a submission waits for a cycle, the session has this status
// only briefly, before the cycle starts or once it ends, since a flush request
// starts a cycle at once.
const watchingStatus = "watching"

// statusLabels describe the session statuses (see remote.Progress.Status).
var statusLabels = map[string]string{
	"disconnected":               "disconnected",
	"halted-on-root-emptied":     "halted",
	"halted-on-root-deletion":    "halted",
	"halted-on-root-type-change": "halted",
	"connecting-alpha":           "connecting to alpha",
	"connecting-beta":            "connecting to beta",
	watchingStatus:               "between cycles",
	"scanning":                   "scanning",
	"waiting-for-rescan":         "waiting to rescan",
	"reconciling":                "reconciling",
	"staging-alpha":              "staging on alpha",
	"staging-beta":               "staging on beta",
	"transitioning":              "applying changes",
	"saving":                     "saving",
}

// statusEndpoints identify the statuses that refer to an endpoint, which is
// alpha if the value is true and beta otherwise.
var statusEndpoints = map[string]bool{
	"connecting-alpha": true,
	"connecting-beta":  false,
	"staging-alpha":    true,
	"staging-beta":     false,
}

// label describes a session's status (see statusLabels), naming endpoints from
// the submitting endpoint's perspective as well. Statuses that remote doesn't
// know, such as those added by later daemons, are shown as they are.
func (d *progressDisplay) label(status string) string {
	label, ok := statusLabels[status]
	if !ok {
		return terminal.NeutralizeControlCharacters(status)
	} else if alpha, ok := statusEndpoints[status]; ok && alpha == d.alpha {
		label += " (local)"
	} else if ok {
		label += " (remote)"
	}
	return label
}

// finish stops showing progress and, if it was shown or the synchronization
// took longer than ProgressDelay, prints a summary of where it spent its
// time, such as "remote: synchronized in 44s: scanning 31s; staging on alpha
// (local) 12s, 1 file, 5.0 MiB", given the context of the wait and the error
// that ended it, if any.
func (d *progressDisplay) finish(ctx context.Context, err error) {
	if d == nil {
		return
	}
	close(d.stop)
	<-d.stopped
	d.lock.Lock()
	defer d.lock.Unlock()
	now := d.now()
	d.endPhase(now, ctx.Err() == nil && err == nil)
	elapsed := now.Sub(d.start)
	if !d.visible && elapsed < ProgressDelay {
		return
	}
	d.clear()
	var summary string
	if ctx.Err() != nil {
		summary = "stopped waiting for synchronization after " + formatElapsed(elapsed)
	} else if err != nil {
		summary = "synchronization failed after " + formatElapsed(elapsed)
	} else {
		summary = "synchronized in " + formatElapsed(elapsed)
	}
	if phases := d.describePhases(); phases != "" {
		summary += ": " + phases
	}
	fmt.Fprintln(d.output.writer, "remote: "+singleLine(summary))
}

// describePhases describes the time spent in each phase that took at least a
// second (once rounded) or staged files, in the order in which they began.
func (d *progressDisplay) describePhases() string {
	var phases []string
	for _, phase := range d.phases {
		if phase.duration < time.Second/2 && phase.files == 0 {
			continue
		}
		description := d.label(phase.status) + " " + formatElapsed(phase.duration)
		if phase.staged {
			files := "files"
			if phase.files == 1 {
				files = "file"
			}
			description += fmt.Sprintf(", %d %s, %s", phase.files, files, humanize.IBytes(phase.size))
		}
		phases = append(phases, description)
	}
	return strings.Join(phases, "; ")
}
