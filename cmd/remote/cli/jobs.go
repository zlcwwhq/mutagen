package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
)

// executing names the endpoint that executes operations submitted through a
// target.
func executing(target *remote.Target) string {
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
	job, code, err := remote.StartJob(ctx, connection, request)
	if errors.Is(err, remote.ErrConnectionLost) {
		return 0, errors.New("the connection to the executing endpoint was lost while starting the job, which may be running; remote --jobs lists the jobs")
	} else if err != nil {
		return code, err
	}
	fmt.Fprintln(stdout, job.ID)
	locator := newLocator(target)
	fmt.Fprintf(stderr, "remote: started job %[1]s on %[2]s with output in %[3]s; remote --wait %[1]s waits for it, and remote --kill %[1]s terminates it\n",
		job.ID, executing(target), describeLog(locator, job))
	if job.LogPath != "" {
		if excluded := target.Ignores.Excluded(job.LogPath, false); excluded != "" {
			fmt.Fprintf(stderr, "remote: warning: the session ignores %s, so the log isn't synchronized; read it with remote cat or copy it with remote --pull\n",
				locator.display(excluded))
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

// printJobs prints a job listing.
func printJobs(output io.Writer, locator *locator, endpoint string, jobs []*remote.Job, hidden int) error {
	if len(jobs) == 0 {
		fmt.Fprintf(output, "No jobs were started at this session's root on %s.\n", endpoint)
	} else {
		table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
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
	}
	if hidden == 1 {
		fmt.Fprintf(output, "(1 job started at another root on %s isn't shown.)\n", endpoint)
	} else if hidden > 1 {
		fmt.Fprintf(output, "(%d jobs started at other roots on %s aren't shown.)\n", hidden, endpoint)
	}
	return nil
}

// waitJob waits for a detached job to end, retrying for a while if the
// connection to the executing endpoint is lost, synchronizes the session if
// requested, reports how the job ended, and returns the job's exit status.
func waitJob(router *router, id string, synchronize bool) (int, error) {
	var job *remote.Job
	var deadline time.Time
	for {
		connection, _, err := router.dial(remote.OperationExecute)
		if err == nil {
			if job, err = remote.WaitJob(router.ctx, connection, id); err == nil {
				break
			}
		}

		// Retry failures to reach the executing endpoint once it has been
		// reached, but not errors about the job or failures to locate the
		// session.
		var jobError *remote.JobError
		if router.ctx.Err() != nil || errors.As(err, &jobError) || router.selection.Target == nil {
			return 0, err
		} else if deadline.IsZero() {
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

	// Synchronize the job's changes. The job's status takes precedence over
	// failure to do so.
	if synchronize {
		if _, _, err := router.synchronize(); err != nil {
			if router.ctx.Err() != nil {
				return 0, err
			}
			router.warn("unable to synchronize after the job, whose changes may not be present locally yet: %v", err)
		}
	}

	// Report how the job ended.
	target := router.selection.Target
	log := describeLog(newLocator(target), job)
	switch job.State {
	case remote.JobExited:
		fmt.Fprintf(router.stderr, "remote: job %s exited with status %d after %s; output in %s\n", id, job.ExitCode, formatElapsed(job.Elapsed), log)
	case remote.JobKilled:
		fmt.Fprintf(router.stderr, "remote: job %s was terminated by remote --kill (status %d); output in %s\n", id, job.ExitCode, log)
	default:
		fmt.Fprintf(router.stderr, "remote: job %s ended without recording its exit status, such as because it was killed with SIGKILL or %s restarted; output in %s\n", id, executing(target), log)
		return failureExitCode, nil
	}
	return job.ExitCode, nil
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
	switch job.State {
	case remote.JobKilled:
		fmt.Fprintf(router.stderr, "remote: terminated job %s (status %d)\n", id, job.ExitCode)
	case remote.JobExited:
		fmt.Fprintf(router.stderr, "remote: job %s had already exited with status %d\n", id, job.ExitCode)
	case remote.JobLost:
		fmt.Fprintf(router.stderr, "remote: job %s had already ended without recording its exit status\n", id)
	default:
		return fmt.Errorf("job %s is still running", id)
	}
	return nil
}
