package cli

import (
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestKillNoFlush(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "aix" {
		t.Skip("detached jobs aren't supported")
	}

	// Keep the data directory short enough for Unix domain sockets on macOS.
	data, err := os.MkdirTemp("", "mr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	root := t.TempDir()
	relaySession(t, "sync_kill", t.TempDir(), root, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := func() io.ReadWriteCloser {
		t.Helper()
		connection, _, err := (&remote.Selection{Target: &remote.Target{SourceSession: "sync_kill", SourceAlpha: true}}).Dial(ctx, root, remote.OperationExecute)
		if err != nil {
			t.Fatal(err)
		}
		return connection
	}

	// Use an ended job so that repeated kill requests return the same result.
	job, code, err := remote.StartJob(ctx, dial(), &remote.Request{
		Arguments: []string{"sh", "-c", "exit 0"},
		Job:       &remote.JobRequest{Operation: remote.JobStart},
	})
	if err != nil || code != 0 {
		t.Fatalf("unable to start job: %d, %v", code, err)
	} else if job, err = remote.WaitJob(ctx, dial(), job.ID); err != nil || job.State != remote.JobExited {
		t.Fatalf("unable to wait for job: %+v, %v", job, err)
	}

	// The relay refuses flush requests, so synchronizing before or after the
	// kill would cause an error or warning instead of this successful report.
	// Explicitly disabling --no-flush must also leave the behavior unchanged.
	expected := "remote: job " + job.ID + " had already exited with status 0\n"
	for _, option := range []string{"", "--no-flush", "--no-flush=false"} {
		arguments := []string{"--node", "beta", "--timeout", "10", "--kill", job.ID}
		if option != "" {
			arguments = append(arguments, option)
		}
		var stdout, stderr strings.Builder
		if code := Run(arguments, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("kill with %q failed: %d, %s", option, code, stderr.String())
		} else if stdout.Len() != 0 || stderr.String() != expected {
			t.Fatalf("unexpected kill output with %q: stdout %q, stderr %q", option, stdout.String(), stderr.String())
		}
	}
}

func TestShellQuote(t *testing.T) {
	for argument, expected := range map[string]string{
		"train.py":        "train.py",
		"--lr=3e-4":       "--lr=3e-4",
		"":                "''",
		"two words":       "'two words'",
		"it's":            `'it'\''s'`,
		"$HOME":           "'$HOME'",
		"a/b_c,d:e+f@g%h": "a/b_c,d:e+f@g%h",
	} {
		if quoted := shellQuote(argument); quoted != expected {
			t.Errorf("incorrect quoting of %q: %s", argument, quoted)
		}
	}
}

func TestFormatElapsed(t *testing.T) {
	for elapsed, expected := range map[time.Duration]string{
		0:                                "0s",
		1499 * time.Millisecond:          "1s",
		59 * time.Second:                 "59s",
		12*time.Minute + 5*time.Second:   "12m05s",
		3*time.Hour + 2*time.Minute + 40: "3h02m",
		50 * time.Hour:                   "50h00m",
	} {
		if formatted := formatElapsed(elapsed); formatted != expected {
			t.Errorf("incorrect formatting of %v: %s", elapsed, formatted)
		}
	}
}

func TestPrintJobs(t *testing.T) {
	started := time.Date(2026, 10, 22, 14, 3, 11, 0, time.Local)
	locator := &locator{root: "/project", base: "sub", contained: true}
	jobs := []*remote.Job{
		{ID: "3f9a1c07", State: remote.JobRunning, Elapsed: 12*time.Minute + 5*time.Second, Started: started,
			Log: "/server/project/sub/train.log", LogPath: "sub/train.log", Arguments: []string{"python", "train.py", "--name", "a b"}},
		{ID: "b2c4d6e8", State: remote.JobExited, ExitCode: 1, Elapsed: 3 * time.Second, Started: started,
			Log: "/home/u/.mutagen/remote-jobs/b2c4d6e8/log", Script: "-", Arguments: []string{"x\ny"}},
		{ID: "91aa0b3c", State: remote.JobLost, Started: started, Log: "/l", Arguments: []string{"sleep", "60"}},
	}
	var output strings.Builder
	if err := printJobs(&output, locator, "beta", jobs, 2); err != nil {
		t.Fatal(err)
	}
	expected := "" +
		"ID        STATE     ELAPSED  STARTED              LOG                                        COMMAND\n" +
		"3f9a1c07  running   12m05s   2026-10-22 14:03:11  train.log                                  python train.py --name 'a b'\n" +
		"b2c4d6e8  exited 1  3s       2026-10-22 14:03:11  /home/u/.mutagen/remote-jobs/b2c4d6e8/log  --script - 'x\\ny'\n" +
		"91aa0b3c  lost      -        2026-10-22 14:03:11  /l                                         sleep 60\n" +
		"(2 jobs started at other roots on beta aren't shown.)\n"
	if output.String() != expected {
		t.Fatalf("incorrect listing:\n%s\nexpected:\n%s", output.String(), expected)
	}

	// Jobs that left processes running are followed by a line that lists them,
	// or, where they can't be listed, notes that their groups may still have
	// processes, and the listing ends with how to deal with them. The table
	// stays aligned.
	jobs[1].Group, jobs[1].Leftovers = 4242, []remote.JobProcess{{PID: 4250, Command: "python worker.py"}, {PID: 4251, Command: "bad\nname"}}
	jobs[2].Group, jobs[2].GroupMayRemain = 5151, true
	output.Reset()
	if err := printJobs(&output, locator, "beta", jobs, 0); err != nil {
		t.Fatal(err)
	}
	expected = "" +
		"ID        STATE     ELAPSED  STARTED              LOG                                        COMMAND\n" +
		"3f9a1c07  running   12m05s   2026-10-22 14:03:11  train.log                                  python train.py --name 'a b'\n" +
		"b2c4d6e8  exited 1  3s       2026-10-22 14:03:11  /home/u/.mutagen/remote-jobs/b2c4d6e8/log  --script - 'x\\ny'\n" +
		"  left running: 4250 python worker.py; 4251 bad\\nname\n" +
		"91aa0b3c  lost      -        2026-10-22 14:03:11  /l                                         sleep 60\n" +
		"  process group 5151 may still have processes\n" +
		"(Processes that jobs left running aren't part of them, so remote --kill doesn't terminate them; remote -- kill PID... does.)\n" +
		"(remote -- ps -A -o pid,pgid,args lists processes with their groups.)\n"
	if output.String() != expected {
		t.Fatalf("incorrect listing with leftovers:\n%s\nexpected:\n%s", output.String(), expected)
	}

	output.Reset()
	if err := printJobs(&output, locator, "alpha", nil, 1); err != nil {
		t.Fatal(err)
	} else if output.String() != "No jobs were started at this node's root on alpha.\n(1 job started at another root on alpha isn't shown.)\n" {
		t.Fatalf("incorrect empty listing: %q", output.String())
	}
}

func TestLeftoverReports(t *testing.T) {
	// Up to five leftover processes are shown, followed by how many more there
	// are, including those that the executing endpoint didn't list, while the
	// suggested command terminates every listed one.
	job := &remote.Job{ID: "b2c4d6e8", State: remote.JobExited, Group: 4242, OmittedLeftovers: 2}
	for pid := 4250; pid < 4257; pid++ {
		job.Leftovers = append(job.Leftovers, remote.JobProcess{PID: pid, Command: "worker"})
	}
	expected := "4250 worker; 4251 worker; 4252 worker; 4253 worker; 4254 worker; and 4 more"
	if described := describeLeftovers(job); described != expected {
		t.Errorf("incorrect leftovers: %q", described)
	}
	expected = "remote: warning: job b2c4d6e8 left processes running: " + expected + "; remote -- kill 4250 4251 4252 4253 4254 4255 4256 terminates them\n"
	if report := describeLeftoverReport(job.ID, job, false); report != expected {
		t.Errorf("incorrect leftover warning: %q", report)
	}

	// Killing a job that has ended reports its leftovers, which remote --kill
	// doesn't terminate.
	job.Leftovers, job.OmittedLeftovers = job.Leftovers[:1], 0
	expected = "remote: job b2c4d6e8 had already exited with status 0\n" +
		"remote: job b2c4d6e8 left processes running, which remote --kill doesn't terminate: 4250 worker; remote -- kill 4250 terminates them\n"
	if report, err := describeKill(job.ID, job); err != nil || report != expected {
		t.Errorf("incorrect kill report: %q, %v", report, err)
	}

	// Where processes can't be listed, the job's process group is reported if
	// it may still have processes.
	job.Leftovers, job.GroupMayRemain = nil, true
	expected = "remote: warning: process group 4242 of job b2c4d6e8 may still have processes that the job left running; remote -- ps -A -o pid,pgid,args lists processes with their groups\n"
	if report := describeLeftoverReport(job.ID, job, false); report != expected {
		t.Errorf("incorrect group warning: %q", report)
	}
	expected = "remote: job b2c4d6e8 had already exited with status 0\n" +
		"remote: process group 4242 of job b2c4d6e8 may still have processes that the job left running, which remote --kill doesn't terminate; remote -- ps -A -o pid,pgid,args lists processes with their groups\n"
	if report, err := describeKill(job.ID, job); err != nil || report != expected {
		t.Errorf("incorrect kill report for a group: %q, %v", report, err)
	}

	// Nothing is reported for jobs without leftovers, but executing endpoints
	// that predate their reporting, which don't report groups, can't tell.
	job.GroupMayRemain = false
	if report := describeLeftoverReport(job.ID, job, false); report != "" {
		t.Errorf("incorrect report without leftovers: %q", report)
	} else if report, err := describeKill(job.ID, job); err != nil || report != "remote: job b2c4d6e8 had already exited with status 0\n" {
		t.Errorf("incorrect kill report without leftovers: %q, %v", report, err)
	}
	job.Group = 0
	if report, err := describeKill(job.ID, job); err != nil || report != "remote: job b2c4d6e8 had already exited with status 0; processes that it left running aren't terminated\n" {
		t.Errorf("incorrect kill report from an older endpoint: %q, %v", report, err)
	}
	job.State = remote.JobRunning
	if _, err := describeKill(job.ID, job); err == nil {
		t.Error("running job reported as killed")
	}
}
