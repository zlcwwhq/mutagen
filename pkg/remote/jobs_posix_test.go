//go:build !windows

package remote

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	// input, and are identified to their commands.
	job := startTestJob(t, root, "", "sh", "-c", `echo out; echo err >&2; cat; printf '%s\n' "$MUTAGEN_REMOTE_JOB" "$MUTAGEN_REMOTE_ENDPOINT"; exit 37`)
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
	if _, code, err := StartJob(ctx, jobRoute(ctx, root), request); code != 126 || err == nil || !strings.Contains(err.Error(), "log") {
		t.Fatalf("incorrect result for an inaccessible log: %d, %v", code, err)
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
