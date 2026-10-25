//go:build !windows && !aix

package remote

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
)

// useTestJobs isolates a test's jobs in a test data directory and kills the
// process groups of any jobs that remain running when the test ends.
func useTestJobs(t *testing.T) string {
	t.Helper()
	useTestRegistry(t)
	directory, err := filesystem.Mutagen(true, jobsDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		entries, _ := os.ReadDir(directory)
		for _, entry := range entries {
			if record, _, err := loadJob(filepath.Join(directory, entry.Name())); err == nil {
				syscall.Kill(-record.PID, syscall.SIGKILL)
			}
		}
	})
	return directory
}

// jobRoute opens a route to an executor with the specified root.
func jobRoute(ctx context.Context, root string) net.Conn {
	client, server := net.Pipe()
	go Serve(ctx, server, root, "", false)
	return client
}

// trackedJobRoute opens a route to an executor with the specified root, like
// jobRoute, and waits for the executor to return when the test ends, so that an
// executor that outlives its request, such as one terminating a job whose kill
// request was cancelled, neither uses state that the test restores nor
// outlives the test.
func trackedJobRoute(t *testing.T, ctx context.Context, root string) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(ctx, server, root, "", false)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("executor didn't return")
		}
	})
	return client
}

// waitForLines waits until the file at the specified path has at least the
// specified number of complete lines, failing the test if that takes longer
// than 10 seconds, and returns its contents.
func waitForLines(t *testing.T, path string, lines int) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && strings.Count(string(data), "\n") >= lines {
			return string(data)
		} else if time.Now().After(deadline) {
			t.Fatalf("%s didn't reach %d lines: %q, %v", path, lines, data, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// killTestJob starts terminating a job started in the specified root through
// a tracked route (see trackedJobRoute) with the specified request context and
// returns a channel that receives the result.
func killTestJob(t *testing.T, ctx context.Context, root, id string) <-chan killOutcome {
	t.Helper()
	route := trackedJobRoute(t, context.Background(), root)
	outcome := make(chan killOutcome, 1)
	go func() {
		job, err := KillJob(ctx, route, id)
		outcome <- killOutcome{job, err, time.Now()}
	}()
	return outcome
}

// killOutcome is the result of a kill request started by killTestJob.
type killOutcome struct {
	// job is the job returned by the request, if any.
	job *Job
	// err is the error returned by the request, if any.
	err error
	// returned is the time at which the request returned.
	returned time.Time
}

// receiveKill waits for the result of a kill request started by killTestJob,
// failing the test if it doesn't return within 10 seconds.
func receiveKill(t *testing.T, outcome <-chan killOutcome) killOutcome {
	t.Helper()
	select {
	case result := <-outcome:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("kill request didn't return")
		return killOutcome{}
	}
}

// termCountingJob is a job command that appends a line to the file given as its
// first argument whenever it receives SIGTERM, but otherwise ignores it, and
// that creates the file with a .ready suffix once it does so. The sleep that it
// runs receives SIGTERM with the job's process group and exits, which lets the
// trap run at once.
const termCountingJob = `trap 'echo term >> "$1"' TERM; echo > "$1.ready"; while :; do sleep 0.1; done`

// startTestJob starts a job running the specified command in the specified
// root.
func startTestJob(t *testing.T, root, log string, arguments ...string) *Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := &Request{Arguments: arguments, Job: &JobRequest{Operation: JobStart, Log: log}}
	job, code, err := StartJob(ctx, jobRoute(ctx, root), request)
	if err != nil {
		t.Fatalf("unable to start job: %d, %v", code, err)
	} else if job.State != JobRunning || !validJobID(job.ID) {
		t.Fatalf("incorrect started job: %+v", job)
	}
	return job
}

// waitTestJob waits for a job started in the specified root to end.
func waitTestJob(t *testing.T, root, id string) *Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	job, err := WaitJob(ctx, jobRoute(ctx, root), id)
	if err != nil {
		t.Fatal("unable to wait for job:", err)
	}
	return job
}

// listTestJobs lists the jobs started in the specified root.
func listTestJobs(t *testing.T, root string) ([]*Job, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	jobs, hidden, err := ListJobs(ctx, jobRoute(ctx, root))
	if err != nil {
		t.Fatal("unable to list jobs:", err)
	}
	return jobs, hidden
}

// readLog reads a job's log.
func readLog(t *testing.T, job *Job) string {
	t.Helper()
	data, err := os.ReadFile(job.Log)
	if err != nil {
		t.Fatal("unable to read log:", err)
	}
	return string(data)
}

func TestJobExitStatusAndLog(t *testing.T) {
	directory := useTestJobs(t)
	root := t.TempDir()

	// Jobs record their exit status, write their output to their log, have no
	// input, and are identified to their commands. The job runs for longer
	// than the granularity of file timestamps, which can be coarser than that
	// of the clock and from which its running time is computed, so that its
	// running time is positive.
	job := startTestJob(t, root, "", "sh", "-c", `echo out; echo err >&2; cat; printf '%s\n' "$MUTAGEN_REMOTE_JOB" "$MUTAGEN_REMOTE_ENDPOINT"; sleep 0.1; exit 37`)
	if job.Log != filepath.Join(directory, job.ID, jobLogName) || job.LogPath != "" {
		t.Fatalf("incorrect default log: %q, %q", job.Log, job.LogPath)
	}
	ended := waitTestJob(t, root, job.ID)
	if ended.State != JobExited || ended.ExitCode != 37 || ended.Elapsed <= 0 {
		t.Fatalf("incorrect ended job: %+v", ended)
	} else if log := readLog(t, ended); log != "out\nerr\n"+job.ID+"\nbeta\n" {
		t.Fatalf("incorrect log: %q", log)
	}

	// Statuses of commands killed by signals use the shell's encoding.
	job = startTestJob(t, root, "", "sh", "-c", "kill -TERM $$")
	if ended := waitTestJob(t, root, job.ID); ended.State != JobExited || ended.ExitCode != 143 {
		t.Fatalf("incorrect signaled job: %+v", ended)
	}
}

func TestJobLogs(t *testing.T) {
	useTestJobs(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0700); err != nil {
		t.Fatal(err)
	}

	// Relative logs are resolved against the working directory, truncated,
	// and located relative to the root.
	log := filepath.Join(root, "sub", "out.log")
	if err := os.WriteFile(log, []byte("previous contents\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := &Request{Arguments: []string{"echo", "hi"}, Path: "sub", Job: &JobRequest{Operation: JobStart, Log: "out.log"}}
	job, _, err := StartJob(ctx, jobRoute(ctx, root), request)
	if err != nil {
		t.Fatal("unable to start job:", err)
	} else if job.Log != log || job.LogPath != "sub/out.log" {
		t.Fatalf("incorrect log: %q, %q", job.Log, job.LogPath)
	} else if waitTestJob(t, root, job.ID); readLog(t, job) != "hi\n" {
		t.Fatalf("incorrect log contents: %q", readLog(t, job))
	}

	// Logs that can't be opened prevent jobs from starting.
	request.Job.Log = "missing/out.log"
	if _, code, err := StartJob(ctx, jobRoute(ctx, root), request); code != 0 || err == nil || ClassifyError(err) != ErrorKindOperation || !strings.Contains(err.Error(), "log") {
		t.Fatalf("incorrect result for an inaccessible log: %d, %v", code, err)
	}

	// Default logs are private, while requested logs are created as a shell
	// would create a file to which it redirects output, subject to the umask,
	// as the reference file is.
	reference := filepath.Join(root, "reference")
	if file, err := os.OpenFile(reference, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666); err != nil {
		t.Fatal(err)
	} else {
		file.Close()
	}
	job = startTestJob(t, root, "", "true")
	requested := startTestJob(t, root, "new.log", "true")
	for _, test := range []struct {
		path, expected string
	}{{job.Log, ""}, {requested.Log, reference}} {
		expected := os.FileMode(0600)
		if test.expected != "" {
			info, err := os.Stat(test.expected)
			if err != nil {
				t.Fatal(err)
			}
			expected = info.Mode().Perm()
		}
		if info, err := os.Stat(test.path); err != nil {
			t.Fatal(err)
		} else if info.Mode().Perm() != expected {
			t.Errorf("incorrect permissions for %s: %v instead of %v", test.path, info.Mode().Perm(), expected)
		}
	}

	// Character devices, such as /dev/null, can serve as logs.
	if job := startTestJob(t, root, "/dev/null", "echo", "discarded"); job.Log != "/dev/null" {
		t.Fatal("incorrect log:", job.Log)
	} else if ended := waitTestJob(t, root, job.ID); ended.State != JobExited || ended.ExitCode != 0 {
		t.Fatalf("incorrect job logging to /dev/null: %+v", ended)
	}
}

func TestJobLogTypes(t *testing.T) {
	directory := useTestJobs(t)
	root := t.TempDir()
	fifo := filepath.Join(root, "fifo")
	if err := mkfifo(fifo); err != nil {
		t.Fatal(err)
	} else if err := os.Symlink("fifo", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	// Logs that are neither regular files nor character devices are refused
	// at once, without starting the job or leaving its directory behind:
	// opening a FIFO without a reader would block, and output written to a
	// FIFO with one would go to whichever process reads it.
	for _, test := range []struct {
		description, log string
		reader           bool
	}{
		{"FIFO without a reader", "fifo", false},
		{"FIFO with a reader", "fifo", true},
		{"symbolic link to a FIFO", "link", false},
	} {
		var reader *os.File
		if test.reader {
			var err error
			if reader, err = os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		request := &Request{Arguments: []string{"echo", "hi"}, Job: &JobRequest{Operation: JobStart, Log: test.log}}
		start := time.Now()
		_, code, err := StartJob(ctx, jobRoute(ctx, root), request)
		elapsed := time.Since(start)
		cancel()
		if reader != nil {
			reader.Close()
		}
		if code != 0 || err == nil || ClassifyError(err) != ErrorKindOperation || !strings.Contains(err.Error(), "regular file or character device") {
			t.Errorf("incorrect result for a log that's a %s: %d, %v (after %v)", test.description, code, err, elapsed)
		}
		if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
			t.Errorf("job directories left behind for a log that's a %s: %v, %v", test.description, entries, err)
		}
	}
}

func TestJobStartFailures(t *testing.T) {
	directory := useTestJobs(t)
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, test := range []struct {
		request *Request
		code    int
	}{
		{&Request{Arguments: []string{"nonexistent-remote-command"}}, 127},
		{&Request{Arguments: []string{"echo"}, Path: "missing"}, 126},
	} {
		test.request.Job = &JobRequest{Operation: JobStart}
		if _, code, err := StartJob(ctx, jobRoute(ctx, root), test.request); code != test.code || err == nil {
			t.Errorf("incorrect start failure: %d, %v", code, err)
		}
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("job directories left behind: %v, %v", entries, err)
	}
}

func TestJobKill(t *testing.T) {
	useTestJobs(t)
	useTerminationGracePeriod(t, 500*time.Millisecond)
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A job that handles SIGTERM records the status with which it exits. The
	// job is running when checked from the process that started it, which
	// would fail with locks owned by processes.
	marker := filepath.Join(t.TempDir(), "terminating")
	job := startTestJob(t, root, "", "sh", "-c", `trap 'echo > "$1"; exit 5' TERM; echo > "$1.ready"; while :; do sleep 0.1; done`, "sh", marker)
	for _, err := os.Stat(marker + ".ready"); err != nil; _, err = os.Stat(marker + ".ready") {
		time.Sleep(10 * time.Millisecond)
	}
	if jobs, _ := listTestJobs(t, root); len(jobs) != 1 || jobs[0].State != JobRunning {
		t.Fatalf("incorrect listing of a running job: %+v", jobs)
	}
	killed, err := KillJob(ctx, jobRoute(ctx, root), job.ID)
	if err != nil {
		t.Fatal("unable to kill job:", err)
	} else if killed.State != JobKilled || killed.ExitCode != 5 {
		t.Fatalf("incorrect killed job: %+v", killed)
	} else if _, err := os.Stat(marker); err != nil {
		t.Fatal("job didn't handle termination:", err)
	}

	// Killing a job that has ended reports its state.
	if again, err := KillJob(ctx, jobRoute(ctx, root), job.ID); err != nil || again.State != JobKilled || again.ExitCode != 5 {
		t.Fatalf("incorrect repeated kill: %+v, %v", again, err)
	}

	// A job that ignores SIGTERM is killed with SIGKILL once the grace period
	// expires, along with its supervisor, so its status is that of SIGKILL.
	ready := filepath.Join(t.TempDir(), "ready")
	job = startTestJob(t, root, "", "sh", "-c", `trap '' TERM; echo > "$1"; while :; do sleep 0.1; done`, "sh", ready)
	for _, err := os.Stat(ready); err != nil; _, err = os.Stat(ready) {
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	if killed, err := KillJob(ctx, jobRoute(ctx, root), job.ID); err != nil || killed.State != JobKilled || killed.ExitCode != 137 {
		t.Fatalf("incorrect killed job: %+v, %v", killed, err)
	} else if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatal("job killed before the grace period expired:", elapsed)
	}
}

func TestJobKillAtStart(t *testing.T) {
	directory := useTestJobs(t)
	useTerminationGracePeriod(t, 2*time.Second)
	root := t.TempDir()

	// A job killed as soon as it has started is terminated by SIGTERM, which
	// reaches its command, rather than killing its supervisor before it
	// catches signals or being caught before the command exists, in which
	// cases the job would be killed with SIGKILL. The job is killed directly,
	// rather than through a route, so that the kill follows the start closely
	// enough to hit those windows.
	for attempt := 1; attempt <= 20; attempt++ {
		started := startJob(root, "", false, &Request{Arguments: []string{"sleep", "60"}, Job: &JobRequest{Operation: JobStart}})
		if started.Job == nil {
			t.Fatal("unable to start job:", started.Error)
		}
		jobDirectory := filepath.Join(directory, started.Job.ID)
		record, _, err := loadJob(jobDirectory)
		if err != nil {
			t.Fatal("unable to load job:", err)
		}
		if killed, err := terminateJob(jobDirectory, record.PID); err != nil {
			t.Fatal("unable to kill job:", err)
		} else if killed.State != JobKilled || killed.ExitCode != 143 {
			t.Fatalf("incorrect job killed at start (attempt %d): %+v", attempt, killed)
		}
	}
}

func TestJobKillTerminatesGroup(t *testing.T) {
	useTestJobs(t)
	useTerminationGracePeriod(t, 500*time.Millisecond)
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start a job whose command exits on SIGTERM, leaving a child that ignores
	// it. The child reports its process identifier once it ignores SIGTERM.
	path := filepath.Join(t.TempDir(), "child")
	script := `sh -c 'trap "" TERM; echo $$ > "$1"; exec sleep 60' sh "$1" & wait`
	job := startTestJob(t, root, "", "sh", "-c", script, "sh", path)
	var child int
	for {
		if data, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(data), "\n") {
			if child, err = strconv.Atoi(strings.TrimSpace(string(data))); err != nil {
				t.Fatal("invalid child process identifier:", err)
			}
			break
		} else if ctx.Err() != nil {
			t.Fatal("child didn't start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The job is only reported as killed, with its command's status, once the
	// child has been killed with SIGKILL after the grace period, even though
	// the supervisor releases the lock as soon as the command exits.
	start := time.Now()
	if killed, err := KillJob(ctx, jobRoute(ctx, root), job.ID); err != nil {
		t.Fatal("unable to kill job:", err)
	} else if killed.State != JobKilled || killed.ExitCode != 143 {
		t.Fatalf("incorrect killed job: %+v", killed)
	} else if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatal("job reported as killed before the grace period expired:", elapsed)
	} else if err := syscall.Kill(child, 0); err != syscall.ESRCH {
		t.Fatal("child survived the job:", err)
	}
}

func TestJobKillOutlivesCancellation(t *testing.T) {
	useTestJobs(t)
	useTerminationGracePeriod(t, 2*time.Second)
	root := t.TempDir()

	// Start a job that takes a while to exit once it receives SIGTERM. The
	// checkpoint's sleep is a process in the job's group, so the checkpoint
	// only completes if the group receives no further signal.
	marker := filepath.Join(t.TempDir(), "checkpoint")
	script := `trap 'echo > "$1.term"; sleep 0.4; echo done > "$1"; exit 5' TERM; echo > "$1.ready"; while :; do sleep 0.1; done`
	job := startTestJob(t, root, "", "sh", "-c", script, "sh", marker)
	waitForLines(t, marker+".ready", 1)

	// Cancel the kill request once the job has received SIGTERM. The request
	// returns, while termination continues on the executing endpoint, so the
	// job exits on its own, with its own status, rather than being killed with
	// SIGKILL.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcome := killTestJob(t, ctx, root, job.ID)
	waitForLines(t, marker+".term", 1)
	cancel()
	receiveKill(t, outcome)
	if ended := waitTestJob(t, root, job.ID); ended.State != JobKilled || ended.ExitCode != 5 {
		t.Fatalf("incorrect job killed by a cancelled request: %+v", ended)
	} else if data, err := os.ReadFile(marker); err != nil || string(data) != "done\n" {
		t.Fatalf("job's checkpoint didn't complete: %q, %v", data, err)
	}

	// A job that ignores SIGTERM is still killed with SIGKILL once the grace
	// period expires, and not before.
	terms := filepath.Join(t.TempDir(), "terms")
	job = startTestJob(t, root, "", "sh", "-c", termCountingJob, "sh", terms)
	waitForLines(t, terms+".ready", 1)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	outcome = killTestJob(t, ctx, root, job.ID)
	waitForLines(t, terms, 1)
	cancel()
	receiveKill(t, outcome)
	ended := waitTestJob(t, root, job.ID)
	if elapsed := time.Since(start); ended.State != JobKilled || ended.ExitCode != 137 {
		t.Fatalf("incorrect job killed by a cancelled request: %+v", ended)
	} else if elapsed < 2*time.Second {
		t.Fatal("job killed before the grace period expired:", elapsed)
	} else if elapsed > 7*time.Second {
		t.Fatal("job killed long after the grace period expired:", elapsed)
	}
}

func TestJobKillConcurrent(t *testing.T) {
	useTestJobs(t)
	useTerminationGracePeriod(t, time.Second)
	root := t.TempDir()

	// Kill a job that ignores SIGTERM twice, starting the second kill once the
	// job has handled the first kill's SIGTERM, with which another SIGTERM
	// could otherwise coalesce.
	terms := filepath.Join(t.TempDir(), "terms")
	job := startTestJob(t, root, "", "sh", "-c", termCountingJob, "sh", terms)
	waitForLines(t, terms+".ready", 1)
	start := time.Now()
	first := killTestJob(t, context.Background(), root, job.ID)
	waitForLines(t, terms, 1)
	second := killTestJob(t, context.Background(), root, job.ID)

	// Both kills report the job as killed with SIGKILL once the first kill's
	// grace period expires, and the job only receives SIGTERM once. The second
	// kill's deadline is computed from the timestamp of the file that marks the
	// job as killed, which can lag the clock slightly where timestamps are
	// coarse, so its return is allowed a little leeway.
	for _, outcome := range []<-chan killOutcome{first, second} {
		result := receiveKill(t, outcome)
		if result.err != nil || result.job.State != JobKilled || result.job.ExitCode != 137 {
			t.Fatalf("incorrect concurrent kill: %+v, %v", result.job, result.err)
		} else if elapsed := result.returned.Sub(start); elapsed < time.Second-100*time.Millisecond {
			t.Fatal("concurrent kill returned before the grace period expired:", elapsed)
		}
	}
	if data, err := os.ReadFile(terms); err != nil || string(data) != "term\n" {
		t.Fatalf("incorrect SIGTERMs received by a job killed concurrently: %q, %v", data, err)
	}
}

func TestJobKillUnsignalableGroup(t *testing.T) {
	useTestJobs(t)
	useTerminationGracePeriod(t, 500*time.Millisecond)
	root := t.TempDir()
	signal := signalJobGroup
	t.Cleanup(func() { signalJobGroup = signal })

	// On Darwin, signaling a group whose members are all zombies fails with
	// EPERM, such as when a concurrent kill's SIGKILL has just ended the job,
	// which we simulate by reporting EPERM once SIGKILL has been delivered. The
	// job has ended, so it's reported as killed.
	signalJobGroup = func(pid int, s syscall.Signal) error {
		if err := signal(pid, s); err != nil || s != syscall.SIGKILL {
			return err
		}
		return syscall.EPERM
	}
	terms := filepath.Join(t.TempDir(), "terms")
	job := startTestJob(t, root, "", "sh", "-c", termCountingJob, "sh", terms)
	waitForLines(t, terms+".ready", 1)
	result := receiveKill(t, killTestJob(t, context.Background(), root, job.ID))
	if result.err != nil || result.job.State != JobKilled || result.job.ExitCode != 137 {
		t.Fatalf("incorrect kill of a job ended by SIGKILL that failed with EPERM: %+v, %v", result.job, result.err)
	}

	// If no signal reaches the job, then it's still running afterward, and the
	// kill reports EPERM.
	signalJobGroup = func(int, syscall.Signal) error {
		return syscall.EPERM
	}
	terms = filepath.Join(t.TempDir(), "terms")
	job = startTestJob(t, root, "", "sh", "-c", termCountingJob, "sh", terms)
	waitForLines(t, terms+".ready", 1)
	result = receiveKill(t, killTestJob(t, context.Background(), root, job.ID))
	if result.err == nil || !strings.Contains(result.err.Error(), syscall.EPERM.Error()) {
		t.Fatalf("incorrect kill of a job that couldn't be signaled: %+v, %v", result.job, result.err)
	}
}

func TestJobKillAfterInterruptedKill(t *testing.T) {
	directory := useTestJobs(t)
	useTerminationGracePeriod(t, 2*time.Second)
	root := t.TempDir()

	// A kill that finds a job already marked as killed, such as by a kill whose
	// executor exited while terminating the job, doesn't send SIGTERM again,
	// and sends SIGKILL once the grace period that started when the job was
	// marked expires: at once if it already has.
	for _, test := range []struct {
		age, minimum, maximum time.Duration
	}{
		{time.Second, 900 * time.Millisecond, 1900 * time.Millisecond},
		{time.Minute, 0, 900 * time.Millisecond},
	} {
		terms := filepath.Join(t.TempDir(), "terms")
		job := startTestJob(t, root, "", "sh", "-c", termCountingJob, "sh", terms)
		waitForLines(t, terms+".ready", 1)
		marked := time.Now().Add(-test.age)
		killed := filepath.Join(directory, job.ID, jobKilledName)
		if err := os.WriteFile(killed, nil, 0600); err != nil {
			t.Fatal(err)
		} else if err := os.Chtimes(killed, marked, marked); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		result := receiveKill(t, killTestJob(t, context.Background(), root, job.ID))
		if result.err != nil || result.job.State != JobKilled || result.job.ExitCode != 137 {
			t.Fatalf("incorrect kill of a marked job: %+v, %v", result.job, result.err)
		} else if elapsed := result.returned.Sub(start); elapsed < test.minimum || elapsed > test.maximum {
			t.Fatalf("incorrect time to kill a job marked %v ago: %v", test.age, elapsed)
		} else if _, err := os.Stat(terms); !os.IsNotExist(err) {
			t.Fatal("job marked as killed received SIGTERM again:", err)
		}
	}
}

func TestJobLost(t *testing.T) {
	directory := useTestJobs(t)
	root := t.TempDir()

	// A job whose supervisor is killed without remote is lost.
	job := startTestJob(t, root, "", "sleep", "60")
	record, _, err := loadJob(filepath.Join(directory, job.ID))
	if err != nil {
		t.Fatal(err)
	} else if err := syscall.Kill(-record.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if ended := waitTestJob(t, root, job.ID); ended.State != JobLost {
		t.Fatalf("incorrect state of a lost job: %+v", ended)
	}
}

func TestJobSurvivesExecutor(t *testing.T) {
	useTestJobs(t)
	root := t.TempDir()

	// Jobs keep running when their route is cancelled.
	executorCtx, cancelExecutor := context.WithCancel(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	marker := filepath.Join(t.TempDir(), "survived")
	request := &Request{Arguments: []string{"sh", "-c", `sleep 1; echo > "$1"`, "sh", marker}, Job: &JobRequest{Operation: JobStart}}
	job, _, err := StartJob(ctx, jobRoute(executorCtx, root), request)
	cancelExecutor()
	if err != nil {
		t.Fatal("unable to start job:", err)
	} else if ended := waitTestJob(t, root, job.ID); ended.State != JobExited || ended.ExitCode != 0 {
		t.Fatalf("incorrect ended job: %+v", ended)
	} else if _, err := os.Stat(marker); err != nil {
		t.Fatal("job didn't survive its route:", err)
	}

	// Jobs keep running when the executing process exits.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(t.TempDir(), "survived")
	starter := exec.Command(executable, "-test.run=^TestRemoteHelper$", "--", "start-job", "sh", "-c", `sleep 1; echo > "$1"; exit 3`, "sh", marker)
	starter.Dir = root
	starter.Env = append(os.Environ(), "MUTAGEN_REMOTE_TEST_HELPER=1")
	output, err := starter.Output()
	if err != nil {
		t.Fatalf("unable to start job from a helper: %v, %q", err, output)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if ended := waitTestJob(t, resolvedRoot, string(output)); ended.State != JobExited || ended.ExitCode != 3 {
		t.Fatalf("incorrect ended job: %+v", ended)
	} else if _, err := os.Stat(marker); err != nil {
		t.Fatal("job didn't survive its executor:", err)
	}
}

func TestJobListingAndLookup(t *testing.T) {
	useTestJobs(t)
	root, other := t.TempDir(), t.TempDir()
	first := startTestJob(t, root, "", "true")
	time.Sleep(10 * time.Millisecond)
	second := startTestJob(t, root, "", "sleep", "60")
	elsewhere := startTestJob(t, other, "", "true")

	// Listings show the jobs started at the root, newest first, and count the
	// others.
	waitTestJob(t, root, first.ID)
	jobs, hidden := listTestJobs(t, root)
	if len(jobs) != 2 || hidden != 1 || jobs[0].ID != second.ID || jobs[1].ID != first.ID {
		t.Fatalf("incorrect listing: %+v, %d", jobs, hidden)
	} else if jobs[0].State != JobRunning || jobs[1].State != JobExited {
		t.Fatalf("incorrect states: %s, %s", jobs[0].State, jobs[1].State)
	} else if strings.Join(jobs[0].Arguments, " ") != "sleep 60" {
		t.Fatalf("incorrect arguments: %q", jobs[0].Arguments)
	}

	// Jobs are only found at the root at which they started.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := WaitJob(ctx, jobRoute(ctx, root), elsewhere.ID); err == nil || !strings.Contains(err.Error(), "another synchronization root") {
		t.Fatal("job found at another root:", err)
	} else if _, err := WaitJob(ctx, jobRoute(ctx, root), "00000000"); err == nil || !strings.Contains(err.Error(), "no job") {
		t.Fatal("nonexistent job found:", err)
	}

	// Waiting is abandoned on cancellation, without affecting the job.
	waitCtx, cancelWait := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelWait()
	if _, err := WaitJob(waitCtx, jobRoute(ctx, root), second.ID); err == nil {
		t.Fatal("wait for a running job didn't time out")
	} else if jobs, _ := listTestJobs(t, root); jobs[0].State != JobRunning {
		t.Fatal("job affected by an abandoned wait:", jobs[0].State)
	}
}

func TestJobPruning(t *testing.T) {
	directory := useTestJobs(t)
	root := t.TempDir()
	ended := startTestJob(t, root, "", "true")
	running := startTestJob(t, root, "", "sleep", "60")
	waitTestJob(t, root, ended.ID)
	incomplete := filepath.Join(directory, "0badc0de")
	if err := os.Mkdir(incomplete, 0700); err != nil {
		t.Fatal(err)
	}

	// Age every job directory beyond the retention period.
	old := time.Now().Add(-jobRetention - time.Hour)
	for _, job := range []string{ended.ID, running.ID, "0badc0de"} {
		filepath.Walk(filepath.Join(directory, job), func(path string, _ os.FileInfo, _ error) error {
			return os.Chtimes(path, old, old)
		})
	}

	// Ended jobs and incomplete job directories are removed, while running
	// jobs are kept.
	jobs, _ := listTestJobs(t, root)
	if len(jobs) != 1 || jobs[0].ID != running.ID {
		t.Fatalf("incorrect jobs after pruning: %+v", jobs)
	} else if _, err := os.Stat(incomplete); !os.IsNotExist(err) {
		t.Fatal("incomplete job directory wasn't removed:", err)
	}
}

func TestJobScript(t *testing.T) {
	directory := useTestJobs(t)
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Scripts run as jobs are kept with the job.
	request := &Request{
		Script:    &Script{Name: "sweep.sh", Contents: []byte("for p in a b; do echo \"$p$1\"; done\n")},
		Arguments: []string{"!"},
		Job:       &JobRequest{Operation: JobStart},
	}
	job, _, err := StartJob(ctx, jobRoute(ctx, root), request)
	if err != nil {
		t.Fatal("unable to start job:", err)
	} else if job.Script != "sweep.sh" || strings.Join(job.Arguments, " ") != "!" {
		t.Fatalf("incorrect job: %+v", job)
	} else if ended := waitTestJob(t, root, job.ID); ended.State != JobExited || ended.ExitCode != 0 {
		t.Fatalf("incorrect ended job: %+v", ended)
	} else if log := readLog(t, job); log != "a!\nb!\n" {
		t.Fatalf("incorrect log: %q", log)
	} else if _, err := os.Stat(filepath.Join(directory, job.ID, jobScriptName)); err != nil {
		t.Fatal("script wasn't kept:", err)
	}
}

func TestJobRecordFormat(t *testing.T) {
	directory := useTestJobs(t)
	root := t.TempDir()

	// Job directories of unknown formats are skipped.
	job := startTestJob(t, root, "", "true")
	waitTestJob(t, root, job.ID)
	path := filepath.Join(directory, job.ID, jobRecordName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record["Format"] = jobFormat + 1
	if data, err = json.Marshal(record); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := listTestJobs(t, root); len(jobs) != 0 {
		t.Fatalf("job of an unknown format listed: %+v", jobs)
	}
}
