package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

const (
	// waitRetryPeriod is the period for which remote --wait retries once the
	// connection to the executing endpoint is lost, such as while the session
	// reconnects.
	waitRetryPeriod = time.Minute
	// waitRetryInterval is the interval between those retries.
	waitRetryInterval = time.Second
	// maximumShownLeftovers is the maximum number of the processes that a job
	// left running that are shown for the job.
	maximumShownLeftovers = 5
	// groupListingCommand is the command suggested for listing processes with
	// their process groups where the processes that a job left running can't
	// be listed.
	groupListingCommand = "remote -- ps -A -o pid,pgid,args"
)

// executing names the endpoint that executes operations submitted through a
// target.
func executing(target *remote.Target) string {
	if target.Node != "" {
		return terminal.NeutralizeControlCharacters(target.Node)
	}
	if target.Alpha {
		return "beta"
	}
	return "alpha"
}

// shellQuote quotes an argument for display in a POSIX shell command line, if
// necessary.
func shellQuote(argument string) string {
	if argument != "" && strings.IndexFunc(argument, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("-_./=:,+@%", r))
	}) < 0 {
		return argument
	}
	return "'" + strings.ReplaceAll(argument, "'", `'\''`) + "'"
}

// lineNeutralizer escapes the characters that would break a line or a table
// cell.
var lineNeutralizer = strings.NewReplacer("\n", `\n`, "\t", `\t`)

// neutralizeLine neutralizes control characters, including those that would
// break a line or a table cell.
func neutralizeLine(value string) string {
	return lineNeutralizer.Replace(terminal.NeutralizeControlCharacters(value))
}

// describeCommand formats a job's command as a command line on one line, with
// control characters neutralized.
func describeCommand(job *remote.Job) string {
	var words []string
	if job.Script != "" {
		words = append(words, "--script", shellQuote(job.Script))
	}
	for _, argument := range job.Arguments {
		words = append(words, shellQuote(argument))
	}
	return neutralizeLine(strings.Join(words, " "))
}

// describeLog formats the location of a job's log on one line: a local path if
// it's inside the root, which is valid remotely as well, and otherwise its path
// on the executing system.
func describeLog(locator *locator, job *remote.Job) string {
	if job.LogPath != "" {
		return lineNeutralizer.Replace(locator.display(job.LogPath))
	}
	return neutralizeLine(job.Log)
}

// formatElapsed formats a job's running time compactly.
func formatElapsed(elapsed time.Duration) string {
	elapsed = elapsed.Round(time.Second)
	if elapsed < time.Minute {
		return fmt.Sprintf("%ds", int(elapsed.Seconds()))
	} else if elapsed < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(elapsed.Minutes()), int(elapsed.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(elapsed.Hours()), int(elapsed.Minutes())%60)
}

// describeState formats a job's state, including its exit status if it ended
// with one.
func describeState(job *remote.Job) string {
	switch job.State {
	case remote.JobExited, remote.JobKilled:
		return fmt.Sprintf("%s %d", job.State, job.ExitCode)
	}
	return string(job.State)
}

// startJob starts a command as a detached job over an established route,
// printing the job's identifier to stdout and how to follow it to stderr. It
// returns the exit code for failures to start the job along with the error.
func startJob(ctx context.Context, connection net.Conn, target *remote.Target, request *remote.Request, stdout, stderr io.Writer) (int, error) {
	return startJobWithEvents(ctx, connection, target, request, stdout, stderr, nil)
}

// startJobWithEvents renders a daemon-coordinated job's synchronization events.
func startJobWithEvents(ctx context.Context, connection net.Conn, target *remote.Target, request *remote.Request, stdout, stderr io.Writer, event func(*remote.CommandEvent)) (int, error) {
	job, code, err := remote.StartJobWithEvents(ctx, connection, request, event)
	if errors.Is(err, remote.ErrConnectionLost) {
		return 0, remote.ConnectionError(errors.New("the connection to the executing endpoint was lost while starting the job, which may be running; remote --jobs lists the jobs"))
	} else if err != nil {
		return code, err
	}
	if _, err := fmt.Fprintln(stdout, job.ID); err != nil {
		return 0, fmt.Errorf("job %s started, but its identifier couldn't be printed: %w", job.ID, err)
	}
	locator := newLocator(target)
	fmt.Fprintf(stderr, "remote: started job %[1]s on %[2]s with output in %[3]s; remote --wait %[1]s waits for it, and remote --kill %[1]s terminates it\n",
		job.ID, executing(target), describeLog(locator, job))
	if job.LogPath != "" {
		// The log isn't synchronized if the session ignores it in every form
		// in which scans could see its path, and remote --pull copies it
		// then. If the session ignores it in only some of them, it may not be
		// synchronized, but remote --pull doesn't write it either (see
		// remote.Ignores.ExcludedInEveryForm). Reading it, including with
		// remote --wait --tail, works in either case.
		if excluded := target.Ignores.ExcludedInEveryForm(job.LogPath, false); excluded != "" {
			fmt.Fprintf(stderr, "remote: warning: the session ignores %s, so the log isn't synchronized; read it with remote cat or remote --wait %s --tail <lines>, or copy it with remote --pull\n",
				locator.display(excluded), job.ID)
		} else if excluded := target.Ignores.ExcludedInAnyForm(job.LogPath, false); excluded != "" {
			fmt.Fprintf(stderr, "remote: warning: the session ignores %s in only some Unicode normalization forms of its path, so the log may not be synchronized; read it with remote cat or remote --wait %s --tail <lines>\n",
				locator.display(excluded), job.ID)
		}
	}
	return 0, nil
}

// listJobs lists the detached jobs started at the session's root on the
// executing endpoint.
func listJobs(router *router, output io.Writer) error {
	connection, target, err := router.dial(remote.OperationExecute)
	if err != nil {
		return err
	}
	jobs, hidden, err := remote.ListJobs(router.ctx, connection)
	if err != nil {
		return err
	}
	return printJobs(output, newLocator(target), executing(target), jobs, hidden)
}

// describeLeftovers describes the processes that an ended job left running, if
// any, on one line with control characters neutralized, showing at most
// maximumShownLeftovers of them.
func describeLeftovers(job *remote.Job) string {
	var processes []string
	for _, process := range job.Leftovers[:min(len(job.Leftovers), maximumShownLeftovers)] {
		processes = append(processes, fmt.Sprintf("%d %s", process.PID, neutralizeLine(process.Command)))
	}
	if more := len(job.Leftovers) - len(processes) + job.OmittedLeftovers; more > 0 {
		processes = append(processes, fmt.Sprintf("and %d more", more))
	}
	return strings.Join(processes, "; ")
}

// leftoverKillCommand returns the command that terminates the processes that an
// ended job left running, as listed by the executing endpoint.
func leftoverKillCommand(job *remote.Job) string {
	words := []string{"remote", "--", "kill"}
	for _, process := range job.Leftovers {
		words = append(words, strconv.Itoa(process.PID))
	}
	return strings.Join(words, " ")
}

// describeLeftoverReport reports the processes that an ended job left running,
// if any, or, where they can't be listed, that its process group may still
// have processes. The report is a warning for remote --wait, while for remote
// --kill it notes that remote --kill doesn't terminate them. It's empty if
// there's nothing to report.
func describeLeftoverReport(id string, job *remote.Job, kill bool) string {
	if len(job.Leftovers) > 0 {
		if kill {
			return fmt.Sprintf("remote: job %s left processes running, which remote --kill doesn't terminate: %s; %s terminates them\n",
				id, describeLeftovers(job), leftoverKillCommand(job))
		}
		return fmt.Sprintf("remote: warning: job %s left processes running: %s; %s terminates them\n",
			id, describeLeftovers(job), leftoverKillCommand(job))
	} else if job.GroupMayRemain {
		if kill {
			return fmt.Sprintf("remote: process group %d of job %s may still have processes that the job left running, which remote --kill doesn't terminate; %s lists processes with their groups\n",
				job.Group, id, groupListingCommand)
		}
		return fmt.Sprintf("remote: warning: process group %d of job %s may still have processes that the job left running; %s lists processes with their groups\n",
			job.Group, id, groupListingCommand)
	}
	return ""
}

// printJobs prints a job listing. Jobs that left processes running are followed
// by a line that lists them.
func printJobs(output io.Writer, locator *locator, endpoint string, jobs []*remote.Job, hidden int) error {
	if len(jobs) == 0 {
		fmt.Fprintf(output, "No jobs were started at this node's root on %s.\n", endpoint)
	} else {
		// Format the table separately, since lines without cells, such as
		// those listing leftover processes, would break its alignment. Each
		// job's row is one line, since its cells have line breaks neutralized.
		var buffer bytes.Buffer
		table := tabwriter.NewWriter(&buffer, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "ID\tSTATE\tELAPSED\tSTARTED\tLOG\tCOMMAND")
		for _, job := range jobs {
			elapsed := "-"
			if job.Elapsed > 0 || job.State != remote.JobLost {
				elapsed = formatElapsed(job.Elapsed)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", job.ID, describeState(job), elapsed,
				job.Started.Local().Format("2006-01-02 15:04:05"), describeLog(locator, job), describeCommand(job))
		}
		if err := table.Flush(); err != nil {
			return err
		}

		// Print the table, with each job's row followed by the processes that
		// the job left running, if any.
		var leftovers, groups bool
		rows := strings.SplitAfter(buffer.String(), "\n")
		var listing strings.Builder
		listing.WriteString(rows[0])
		for i, job := range jobs {
			listing.WriteString(rows[i+1])
			if len(job.Leftovers) > 0 {
				leftovers = true
				fmt.Fprintf(&listing, "  left running: %s\n", describeLeftovers(job))
			} else if job.GroupMayRemain {
				groups = true
				fmt.Fprintf(&listing, "  process group %d may still have processes\n", job.Group)
			}
		}
		if leftovers {
			listing.WriteString("(Processes that jobs left running aren't part of them, so remote --kill doesn't terminate them; remote -- kill PID... does.)\n")
		}
		if groups {
			fmt.Fprintf(&listing, "(%s lists processes with their groups.)\n", groupListingCommand)
		}
		if _, err := io.WriteString(output, listing.String()); err != nil {
			return err
		}
	}
	if hidden == 1 {
		fmt.Fprintf(output, "(1 job started at another root on %s isn't shown.)\n", endpoint)
	} else if hidden > 1 {
		fmt.Fprintf(output, "(%d jobs started at other roots on %s aren't shown.)\n", hidden, endpoint)
	}
	return nil
}

// errJobRunning indicates that a job was still running when the timeout of
// remote --wait expired, as reported by the executing endpoint shortly before
// it (see remote.WaitOptions.Deadline).
var errJobRunning = errors.New("the job is still running")

// waitJob waits for a detached job to end, retrying for a while if the
// connection to the executing endpoint is lost, synchronizes the session if
// requested, reports how the job ended, and returns the job's exit status. A
// requested log tail is printed once the job ends, or, if the timeout expires
// while it's running, before returning errJobRunning, while a followed log is
// printed as it's written.
func waitJob(router *router, options *options, output io.Writer) (int, error) {
	id := options.wait
	wait := &remote.WaitOptions{Warn: func(message string) { router.warn("%s", message) }}
	if deadline, ok := router.ctx.Deadline(); ok {
		wait.Deadline = deadline
	}
	if options.follow {
		wait.Follow = &remote.LogFollower{Output: output, Lines: options.tail}
		if !options.flags.Changed("tail") {
			wait.Follow.Lines = -1
		}
	} else {
		wait.Tail = options.tail
	}

	// If the followed log ends within a line, such as with a progress bar that
	// carriage returns redraw, then start what's printed afterward on a new
	// line of standard error, so that remote's reports start lines when both
	// streams are shown together, as on terminals, or merged for line-based
	// filters. The log itself is printed unchanged.
	ended := false
	endLine := func() {
		if !ended && wait.Follow != nil && wait.Follow.PartialLine() {
			fmt.Fprintln(router.stderr)
		}
		ended = true
	}
	defer endLine()

	// Wait for the job, retrying with the same follower, which resumes where
	// the previous attempt stopped.
	var job *remote.Job
	var tail *remote.JobLog
	var deadline time.Time
	for {
		started := time.Now()
		connection, _, err := router.dial(remote.OperationExecute)
		if err == nil {
			if job, tail, err = remote.WaitJobWithOptions(router.ctx, connection, id, wait); err == nil {
				break
			}
		}

		// Retry failures to reach the executing endpoint once it has been
		// reached, but not errors about the job or failures to locate the
		// session. An attempt that waited for longer than the retry period
		// before failing, such as one that followed a log for hours, starts
		// a new retry period.
		if router.ctx.Err() != nil || remote.ClassifyError(err) != remote.ErrorKindConnection || router.selection.Target == nil {
			return 0, err
		} else if deadline.IsZero() || time.Since(started) > waitRetryPeriod {
			deadline = time.Now().Add(waitRetryPeriod)
			router.warn("unable to wait for job %s, which keeps running, so remote retries for up to %v: %v", id, waitRetryPeriod, err)
		} else if time.Now().After(deadline) {
			return 0, fmt.Errorf("unable to wait for job %s, which may still be running: %w", id, err)
		}
		select {
		case <-time.After(waitRetryInterval):
		case <-router.ctx.Done():
			return 0, router.ctx.Err()
		}
	}
	endLine()

	// Print only the successful attempt's tail, so reconnecting can't repeat
	// partial output. A log failure doesn't replace the job's exit status.
	if tail != nil {
		if tail.Error != "" {
			router.warn("unable to read the log of job %s: %s", id, tail.Error)
		} else {
			if tail.Truncated {
				router.warn("the requested log tail exceeds 1 MiB; showing only the final 1 MiB, which may start within a line")
			}
			if _, err := output.Write(tail.Data); err != nil {
				router.warn("unable to print the log of job %s: %v", id, err)
			}
		}
	}

	// A job that's still running was returned because the timeout was about
	// to expire. It isn't synchronized, as with timeouts that expire while
	// waiting.
	if job.State == remote.JobRunning {
		return 0, errJobRunning
	}

	// Synchronize the job's changes. The job's status takes precedence over
	// failure to do so.
	if !options.noFlush {
		if _, _, err := router.synchronize(); err != nil {
			if router.ctx.Err() != nil {
				return 0, err
			}
			router.warn("unable to synchronize after the job, whose changes may not be present locally yet: %v", err)
		}
	}

	// Report how the job ended, and any processes that it left running.
	target := router.selection.Target
	log := describeLog(newLocator(target), job)
	code := job.ExitCode
	switch job.State {
	case remote.JobExited:
		fmt.Fprintf(router.stderr, "remote: job %s exited with status %d after %s; output in %s\n", id, job.ExitCode, formatElapsed(job.Elapsed), log)
	case remote.JobKilled:
		fmt.Fprintf(router.stderr, "remote: job %s was terminated by remote --kill (status %d); output in %s\n", id, job.ExitCode, log)
	default:
		fmt.Fprintf(router.stderr, "remote: job %s ended without recording its exit status, such as because it was killed with SIGKILL or %s restarted; output in %s\n", id, executing(target), log)
		code = failureExitCode
	}
	fmt.Fprint(router.stderr, describeLeftoverReport(id, job, false))
	return code, nil
}

// killJob terminates a detached job and reports its state.
func killJob(router *router, id string) error {
	connection, _, err := router.dial(remote.OperationExecute)
	if err != nil {
		return err
	}
	job, err := remote.KillJob(router.ctx, connection, id)
	if err != nil {
		return err
	}
	report, err := describeKill(id, job)
	if err != nil {
		return err
	}
	fmt.Fprint(router.stderr, report)
	return nil
}

// describeKill reports the state of a job returned by remote --kill, followed
// by any processes that the job left running, which remote --kill doesn't
// terminate.
func describeKill(id string, job *remote.Job) (string, error) {
	var report string
	switch job.State {
	case remote.JobKilled:
		report = fmt.Sprintf("remote: terminated job %s (status %d)\n", id, job.ExitCode)
	case remote.JobExited:
		if job.Group == 0 {
			// The executing endpoint predates the reporting of processes that
			// jobs left running, so whether there are any is unknown.
			report = fmt.Sprintf("remote: job %s had already exited with status %d; processes that it left running aren't terminated\n", id, job.ExitCode)
		} else {
			report = fmt.Sprintf("remote: job %s had already exited with status %d\n", id, job.ExitCode)
		}
	case remote.JobLost:
		report = fmt.Sprintf("remote: job %s had already ended without recording its exit status\n", id)
	default:
		return "", fmt.Errorf("job %s is still running", id)
	}
	return report + describeLeftoverReport(id, job, true), nil
}
