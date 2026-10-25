//go:build !windows && !aix

package remote

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a buffer that's safe for concurrent use, through which tests
// watch streamed output.
type syncBuffer struct {
	// lock serializes access to data.
	lock sync.Mutex
	// data is the output written so far.
	data bytes.Buffer
}

// Write implements io.Writer.Write.
func (b *syncBuffer) Write(data []byte) (int, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.data.Write(data)
}

// String returns the output written so far.
func (b *syncBuffer) String() string {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.data.String()
}

// waitFor waits until the output is the specified value, failing the test if
// that takes longer than 10 seconds.
func (b *syncBuffer) waitFor(t *testing.T, value string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); b.String() != value; {
		if time.Now().After(deadline) {
			t.Fatalf("output didn't become %q: %q", value, b.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// warningRecorder records the warnings that waits issue.
type warningRecorder struct {
	// lock serializes access to warnings.
	lock sync.Mutex
	// warnings are the warnings issued so far.
	warnings []string
}

// warn records a warning.
func (r *warningRecorder) warn(warning string) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.warnings = append(r.warnings, warning)
}

// take returns the warnings issued since it was last called.
func (r *warningRecorder) take() []string {
	r.lock.Lock()
	defer r.lock.Unlock()
	warnings := r.warnings
	r.warnings = nil
	return warnings
}

// releasedJob is a job command that exits with status 5 once the file given as
// its first argument exists, without writing output.
const releasedJob = `while [ ! -e "$1" ]; do sleep 0.02; done; exit 5`

// appendLog appends data to a job's log, as the job's processes would.
func appendLog(t *testing.T, path, data string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

// followInBackground starts a wait with the specified options and returns a
// function that cancels it, abandoning the wait as a lost connection would,
// and a channel that receives its outcome.
func followInBackground(ctx context.Context, root, id string, options *WaitOptions) (context.CancelFunc, <-chan waitOutcome) {
	attempt, cancel := context.WithCancel(ctx)
	outcome := make(chan waitOutcome, 1)
	go func() {
		job, log, err := WaitJobWithOptions(attempt, jobRoute(ctx, root), id, options)
		outcome <- waitOutcome{job, log, err}
	}()
	return cancel, outcome
}

// waitOutcome is the result of a wait started by followInBackground.
type waitOutcome struct {
	// job is the job returned by the wait, if any.
	job *Job
	// log is the log tail returned by the wait, if any.
	log *JobLog
	// err is the error returned by the wait, if any.
	err error
}

func TestWaitJobFollow(t *testing.T) {
	useTestJobs(t)
	root := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	job := startTestJob(t, root, "", "sh", "-c", `printf 'one\n'; while [ ! -e "$1" ]; do sleep 0.02; done; printf 'two\nthree'; exit 7`, "sh", release)

	// The log is streamed from its beginning while the job runs.
	var output syncBuffer
	var warnings warningRecorder
	follower := &LogFollower{Output: &output, Lines: -1}
	options := &WaitOptions{Follow: follower, Warn: warnings.warn}
	abandon, outcome := followInBackground(ctx, root, job.ID, options)
	output.waitFor(t, "one\n")

	// Abandoning the wait, as a lost connection would, and resuming it once
	// the job has ended prints the rest of the log exactly once.
	abandon()
	if result := <-outcome; result.err == nil || result.job != nil {
		t.Fatalf("abandoned wait returned a result: %+v", result)
	} else if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	} else if ended := waitTestJob(t, root, job.ID); ended.ExitCode != 7 {
		t.Fatalf("incorrect job status: %+v", ended)
	}
	ended, log, err := WaitJobWithOptions(ctx, jobRoute(ctx, root), job.ID, options)
	if err != nil || ended.State != JobExited || ended.ExitCode != 7 || log != nil {
		t.Fatalf("incorrect resumed wait result: %+v, %+v, %v", ended, log, err)
	} else if output.String() != "one\ntwo\nthree" || !follower.PartialLine() {
		t.Fatalf("incorrect followed output: %q (partial line: %t)", output.String(), follower.PartialLine())
	} else if w := warnings.take(); len(w) != 0 {
		t.Fatalf("unexpected warnings: %q", w)
	}

	// Following an ended job prints its whole log, and a fresh follower of the
	// same log starts over, without repeating it twice.
	output = syncBuffer{}
	options.Follow = &LogFollower{Output: &output, Lines: -1}
	if ended, _, err := WaitJobWithOptions(ctx, jobRoute(ctx, root), job.ID, options); err != nil || ended.ExitCode != 7 || output.String() != "one\ntwo\nthree" {
		t.Fatalf("incorrect follow of an ended job: %+v, %v, %q", ended, err, output.String())
	}
}

func TestWaitJobFollowTail(t *testing.T) {
	useTestJobs(t)
	root := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	job := startTestJob(t, root, "", "sh", "-c", releasedJob, "sh", release)
	appendLog(t, job.Log, "1\n2\n3\n4\n")

	// Following starts with the requested final lines, or at the log's end for
	// zero lines, and long tails are limited to 1 MiB, with a warning.
	var two, zero, all syncBuffer
	var warnings warningRecorder
	_, twoOutcome := followInBackground(ctx, root, job.ID, &WaitOptions{Follow: &LogFollower{Output: &two, Lines: 2}, Warn: warnings.warn})
	_, zeroOutcome := followInBackground(ctx, root, job.ID, &WaitOptions{Follow: &LogFollower{Output: &zero, Lines: 0}, Warn: warnings.warn})
	two.waitFor(t, "3\n4\n")
	appendLog(t, job.Log, "5\n")
	two.waitFor(t, "3\n4\n5\n")
	zero.waitFor(t, "5\n")
	if w := warnings.take(); len(w) != 0 {
		t.Fatalf("unexpected warnings: %q", w)
	}
	long := strings.Repeat("x", MaximumJobLogSize+10)
	appendLog(t, job.Log, long)
	_, allOutcome := followInBackground(ctx, root, job.ID, &WaitOptions{Follow: &LogFollower{Output: &all, Lines: 1}, Warn: warnings.warn})
	all.waitFor(t, long[10:])
	if w := warnings.take(); len(w) != 1 || !strings.Contains(w[0], "exceeds 1 MiB") {
		t.Fatalf("incorrect warnings for a long tail: %q", w)
	}

	// Each follower receives the output that follows, and the job's status.
	appendLog(t, job.Log, "\nend\n")
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		outcome  <-chan waitOutcome
		output   *syncBuffer
		expected string
	}{
		{twoOutcome, &two, "3\n4\n5\n" + long + "\nend\n"},
		{zeroOutcome, &zero, "5\n" + long + "\nend\n"},
		{allOutcome, &all, long[10:] + "\nend\n"},
	} {
		if result := <-test.outcome; result.err != nil || result.job.ExitCode != 5 || result.log != nil {
			t.Fatalf("incorrect wait result: %+v", result)
		} else if test.output.String() != test.expected {
			t.Fatalf("incorrect followed output: %d bytes instead of %d", len(test.output.String()), len(test.expected))
		}
	}
	if w := warnings.take(); len(w) != 0 {
		t.Fatalf("unexpected warnings: %q", w)
	}
}

func TestWaitJobFollowTruncation(t *testing.T) {
	useTestJobs(t)
	root := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	job := startTestJob(t, root, "", "sh", "-c", releasedJob, "sh", release)
	appendLog(t, job.Log, "first line\n")

	// A log that's truncated while it's followed, as by logrotate's
	// copytruncate, is followed from its beginning again, with a warning.
	var output syncBuffer
	var warnings warningRecorder
	options := &WaitOptions{Follow: &LogFollower{Output: &output, Lines: -1}, Warn: warnings.warn}
	abandon, outcome := followInBackground(ctx, root, job.ID, options)
	output.waitFor(t, "first line\n")
	if err := os.Truncate(job.Log, 0); err != nil {
		t.Fatal(err)
	}
	appendLog(t, job.Log, "new\n")
	output.waitFor(t, "first line\nnew\n")
	if w := warnings.take(); len(w) != 1 || !strings.Contains(w[0], "became shorter than the 11 bytes followed so far") {
		t.Fatalf("incorrect truncation warnings: %q", w)
	}

	// A log that's truncated while the wait is interrupted is followed from its
	// beginning once the wait is resumed.
	abandon()
	<-outcome
	if err := os.Truncate(job.Log, 0); err != nil {
		t.Fatal(err)
	}
	appendLog(t, job.Log, "x\n")
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ended, _, err := WaitJobWithOptions(ctx, jobRoute(ctx, root), job.ID, options)
	if err != nil || ended.ExitCode != 5 || output.String() != "first line\nnew\nx\n" {
		t.Fatalf("incorrect resumed wait: %+v, %v, %q", ended, err, output.String())
	} else if w := warnings.take(); len(w) != 1 || !strings.Contains(w[0], "became shorter than the 4 bytes followed so far") {
		t.Fatalf("incorrect truncation warnings: %q", w)
	}
}

func TestWaitJobFollowReplacement(t *testing.T) {
	useTestJobs(t)
	root := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	script := `printf 'one\n'; while [ ! -e "$1" ]; do sleep 0.02; done; printf 'two\n'; while [ ! -e "$1.again" ]; do sleep 0.02; done; exit 5`
	job := startTestJob(t, root, "", "sh", "-c", script, "sh", release)

	// The log that a wait opened keeps being followed even if another file
	// replaces it at its path, since the job keeps writing to the file that
	// it opened.
	var output syncBuffer
	var warnings warningRecorder
	options := &WaitOptions{Follow: &LogFollower{Output: &output, Lines: -1}, Warn: warnings.warn}
	abandon, outcome := followInBackground(ctx, root, job.ID, options)
	output.waitFor(t, "one\n")
	if err := os.Rename(job.Log, job.Log+".old"); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(job.Log, []byte("another file that's longer\n"), 0600); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "one\ntwo\n")

	// A resumed wait doesn't follow another file at the log's path from the
	// offset that it reached in the job's, but keeps waiting for the job.
	abandon()
	<-outcome
	if err := os.WriteFile(release+".again", nil, 0600); err != nil {
		t.Fatal(err)
	}
	ended, _, err := WaitJobWithOptions(ctx, jobRoute(ctx, root), job.ID, options)
	if err != nil || ended.ExitCode != 5 || output.String() != "one\ntwo\n" {
		t.Fatalf("incorrect resumed wait: %+v, %v, %q", ended, err, output.String())
	} else if w := warnings.take(); len(w) != 1 || !strings.Contains(w[0], "was replaced since remote last read it") {
		t.Fatalf("incorrect replacement warnings: %q", w)
	}

	// Once following has stopped, retried waits don't follow or warn again.
	if ended, _, err := WaitJobWithOptions(ctx, jobRoute(ctx, root), job.ID, options); err != nil || ended.ExitCode != 5 || output.String() != "one\ntwo\n" {
		t.Fatalf("incorrect retried wait: %+v, %v, %q", ended, err, output.String())
	} else if w := warnings.take(); len(w) != 0 {
		t.Fatalf("retried wait warned again: %q", w)
	}
}

func TestWaitJobFollowUnreadable(t *testing.T) {
	useTestJobs(t)
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, test := range []struct {
		name, log, warning string
		replace            func(t *testing.T, path string)
	}{
		{"device", "/dev/null", "/dev/null is not a regular file", nil},
		{"missing", "", "no such file or directory", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", "", "is not a regular file", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			} else if err := mkfifo(path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Logs that can't be followed are reported once, without affecting
			// the wait or the job's status.
			release := filepath.Join(t.TempDir(), "release")
			job := startTestJob(t, root, test.log, "sh", "-c", `printf 'output\n'; `+releasedJob, "sh", release)
			if test.replace != nil {
				waitForLines(t, job.Log, 1)
				test.replace(t, job.Log)
			}
			var output syncBuffer
			var warnings warningRecorder
			options := &WaitOptions{Follow: &LogFollower{Output: &output, Lines: -1}, Warn: warnings.warn}
			_, outcome := followInBackground(ctx, root, job.ID, options)
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				warnings.lock.Lock()
				count := len(warnings.warnings)
				warnings.lock.Unlock()
				if count > 0 {
					break
				} else if time.Now().After(deadline) {
					t.Fatal("unreadable log wasn't reported")
				}
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if result := <-outcome; result.err != nil || result.job.ExitCode != 5 || output.String() != "" {
				t.Fatalf("unreadable log changed the wait: %+v, %q", result, output.String())
			} else if w := warnings.take(); len(w) != 1 || !strings.Contains(w[0], "unable to follow the log of job "+job.ID+": ") || !strings.Contains(w[0], test.warning) {
				t.Fatalf("incorrect warnings: %q", w)
			}
		})
	}
}

func TestWaitJobDeadline(t *testing.T) {
	useTestJobs(t)
	root := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	job := startTestJob(t, root, "", "sh", "-c", releasedJob, "sh", release)
	appendLog(t, job.Log, "a\nb\n")

	// A job that's still running when the deadline approaches is returned,
	// with its log's current tail, in time for the deadline.
	// With a second left, the executing endpoint waits for half of it.
	start := time.Now()
	deadline := start.Add(time.Second)
	waiting, stop := context.WithDeadline(ctx, deadline)
	defer stop()
	running, log, err := WaitJobWithOptions(waiting, jobRoute(ctx, root), job.ID, &WaitOptions{Tail: 1, Deadline: deadline})
	if err != nil || running.State != JobRunning || log == nil || log.Error != "" || string(log.Data) != "b\n" {
		t.Fatalf("incorrect result before the deadline: %+v, %+v, %v", running, log, err)
	} else if elapsed := time.Since(start); elapsed < 400*time.Millisecond || elapsed >= time.Second {
		t.Fatalf("result returned after %v instead of about half a second", elapsed)
	}

	// Waits that don't read tails, such as those that follow logs, whose output
	// has already been received, don't stop waiting early, and end with the
	// context instead.
	for _, follow := range []bool{false, true} {
		var output syncBuffer
		start = time.Now()
		deadline = start.Add(500 * time.Millisecond)
		waiting, stop = context.WithDeadline(ctx, deadline)
		defer stop()
		options := &WaitOptions{Deadline: deadline}
		if follow {
			options.Tail, options.Follow = 1, &LogFollower{Output: &output, Lines: -1}
		}
		if ended, log, err := WaitJobWithOptions(waiting, jobRoute(ctx, root), job.ID, options); err == nil || ended != nil || log != nil || time.Now().Before(deadline) {
			t.Fatalf("wait without a tail returned early: %+v, %+v, %v", ended, log, err)
		} else if follow && output.String() != "a\nb\n" {
			t.Fatalf("incorrect followed output: %q", output.String())
		}
	}

	// Jobs that end before the deadline are returned as they are without one.
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	if ended, log, err := WaitJobWithOptions(ctx, jobRoute(ctx, root), job.ID, &WaitOptions{Tail: 1, Deadline: deadline}); err != nil || ended.ExitCode != 5 || string(log.Data) != "b\n" {
		t.Fatalf("incorrect result for an ended job: %+v, %+v, %v", ended, log, err)
	}
}

func TestWaitJobOldClient(t *testing.T) {
	useTestJobs(t)
	root := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	job := startTestJob(t, root, "", "sh", "-c", `printf 'one\ntwo\n'; `+releasedJob, "sh", release)
	waitForLines(t, job.Log, 2)

	// Clients that predate following and deadlines don't send them, so a wait
	// for a running job doesn't return before the job ends, even with a tail,
	// and the tail is sent as raw output once it does.
	request := &Request{Job: &JobRequest{Operation: JobWait, ID: job.ID, Tail: 1}}
	waiting, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	var abandoned syncBuffer
	if response, err := execute(waiting, jobRoute(ctx, root), request, strings.NewReader(""), &abandoned, io.Discard); err == nil || abandoned.String() != "" {
		t.Fatalf("wait for a running job returned early: %+v, %v, %q", response, err, abandoned.String())
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	response, err := execute(ctx, jobRoute(ctx, root), request, strings.NewReader(""), &output, io.Discard)
	if err != nil || response.Job == nil || response.Job.ExitCode != 5 || response.JobLog == nil || output.String() != "two\n" {
		t.Fatalf("incorrect result for an old client: %+v, %v, %q", response, err, output.String())
	}
}
