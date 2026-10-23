package remote

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeProcess describes a process in a fake proc file system.
type fakeProcess struct {
	// pid is the process identifier.
	pid int
	// name is the process's name.
	name string
	// state is the process's state.
	state string
	// group is the process's process group.
	group int
	// environment is the process's environment, without its terminating null
	// byte, or "-" if its environ file is missing, as if it couldn't be read.
	environment string
	// command is the process's command line, with arguments separated by null
	// bytes and without the terminating null byte.
	command string
}

// useFakeProc creates a fake proc file system with the specified processes and
// substitutes it for the real one for the duration of the test.
func useFakeProc(t *testing.T, processes []fakeProcess) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "self"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		directory := filepath.Join(root, strconv.Itoa(process.pid))
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (%s) %s 1 %d %d 0 -1 4194560 100 0 0 0\n", process.pid, process.name, process.state, process.group, process.group)
		files := map[string]string{"stat": stat}
		if process.command != "" {
			files["cmdline"] = process.command + "\x00"
		} else {
			files["cmdline"] = ""
		}
		if process.environment != "-" {
			files["environ"] = process.environment
			if process.environment != "" {
				files["environ"] += "\x00"
			}
		}
		for name, contents := range files {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	previous := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = previous })
}

func TestFindLeftoversInFakeProc(t *testing.T) {
	// Two jobs share a process group, as they would if the system reused a
	// supervisor's process identifier, and a third leaves more processes than
	// are listed.
	first := &Job{ID: "0badc0de", State: JobExited, Group: 4242}
	second := &Job{ID: "12345678", State: JobKilled, Group: 4242}
	many := &Job{ID: "87654321", State: JobLost, Group: 5000}
	running := &Job{ID: "aaaaaaaa", State: JobRunning, Group: 6000}
	unknown := &Job{ID: "bbbbbbbb", State: JobExited}
	firstVariable, manyVariable := JobVariable+"=0badc0de", JobVariable+"=87654321"
	processes := []fakeProcess{
		// A leftover process.
		{300, "sleep", "S", 4242, "PATH=/bin\x00" + firstVariable, "sleep\x0060"},
		// A leftover process whose name contains spaces and parentheses, and
		// that has no command line, which is listed after the previous one,
		// since processes are ordered numerically.
		{1000, "a (b) c", "R", 4242, firstVariable, ""},
		// A zombie, a process in another group, processes whose environments
		// don't contain the job's identifier exactly, and a process whose
		// environment can't be read aren't listed.
		{301, "sleep", "Z", 4242, "", ""},
		{302, "sleep", "S", 999, firstVariable, "sleep\x0060"},
		{303, "sleep", "S", 4242, firstVariable + "e\x00X" + firstVariable, "sleep\x0060"},
		{304, "sleep", "S", 4242, "-", "sleep\x0060"},
		// A leftover process of the job that shares the group.
		{305, "sleep", "S", 4242, JobVariable + "=12345678", "sleep\x001"},
		// A leftover process with a long command line, which is truncated
		// without splitting a character.
		{306, "python", "S", 5000, manyVariable, "python\x00" + strings.Repeat("é", 150)},
		// Processes in a running job's group aren't listed.
		{6001, "sleep", "S", 6000, JobVariable + "=aaaaaaaa", "sleep\x0060"},
	}
	for pid := 5001; pid <= 5104; pid++ {
		processes = append(processes, fakeProcess{pid, "worker", "S", 5000, manyVariable, "worker"})
	}
	useFakeProc(t, processes)
	findLeftovers([]*Job{first, second, many, running, unknown})

	if expected := []JobProcess{{300, "sleep 60"}, {1000, "[a (b) c]"}}; !reflect.DeepEqual(first.Leftovers, expected) {
		t.Errorf("incorrect leftovers: %+v", first.Leftovers)
	}
	if expected := []JobProcess{{305, "sleep 1"}}; !reflect.DeepEqual(second.Leftovers, expected) {
		t.Errorf("incorrect leftovers of a job sharing a group: %+v", second.Leftovers)
	}
	if len(many.Leftovers) != maximumLeftovers || many.OmittedLeftovers != 5 {
		t.Errorf("incorrect number of leftovers: %d listed, %d omitted", len(many.Leftovers), many.OmittedLeftovers)
	} else if expected := "python " + strings.Repeat("é", 96) + "..."; many.Leftovers[0] != (JobProcess{306, expected}) {
		t.Errorf("incorrect truncated leftover: %+v", many.Leftovers[0])
	} else if many.Leftovers[maximumLeftovers-1].PID != 5099 {
		t.Errorf("incorrect last listed leftover: %+v", many.Leftovers[maximumLeftovers-1])
	}
	for _, job := range []*Job{first, second, many, running, unknown} {
		if job.GroupMayRemain {
			t.Errorf("job %s marked as possibly having leftovers despite a readable proc file system", job.ID)
		}
	}
	if running.Leftovers != nil || unknown.Leftovers != nil {
		t.Errorf("leftovers listed for a running job or one of an unknown group: %+v, %+v", running.Leftovers, unknown.Leftovers)
	}

	// If the proc file system can't be read, then jobs whose groups exist
	// are marked as possibly having leftovers. This process's own group exists.
	procRoot = filepath.Join(t.TempDir(), "missing")
	ended := &Job{ID: "0badc0de", State: JobExited, Group: syscall.Getpgrp()}
	findLeftovers([]*Job{ended})
	if !ended.GroupMayRemain || ended.Leftovers != nil {
		t.Errorf("incorrect fallback for an unreadable proc file system: %+v", ended)
	}
}

// readTestPID reads a process identifier written to a file by a test job,
// waiting for it to be written.
func readTestPID(t *testing.T, path string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(waitForLines(t, path, 1)))
	if err != nil {
		t.Fatal("invalid process identifier:", err)
	}
	return pid
}

func TestJobLeftovers(t *testing.T) {
	directory := useTestJobs(t)
	root := t.TempDir()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid isn't available:", err)
	}

	// Start a job whose command leaves a process running in the job's process
	// group and another in a new session, which carries the job's identifier
	// in its environment as well, records their process identifiers, and
	// exits. The processes are killed by their identifiers when the test ends,
	// unless the test has killed them already.
	pids := filepath.Join(t.TempDir(), "pid")
	script := `sleep 60 & echo $! > "$1.group"; setsid sh -c 'echo $$ > "$1"; exec sleep 60' sh "$1.session" & while [ ! -s "$1.session" ]; do sleep 0.01; done`
	job := startTestJob(t, root, "", "sh", "-c", script, "sh", pids)
	group, session := readTestPID(t, pids+".group"), readTestPID(t, pids+".session")
	alive := map[int]bool{group: true, session: true}
	t.Cleanup(func() {
		for pid := range alive {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	// Waiting for the job, listing it, and killing it once it has ended all
	// report the process left in its group, and only that one.
	record, _, err := loadJob(filepath.Join(directory, job.ID))
	if err != nil {
		t.Fatal(err)
	}
	check := func(operation string, ended *Job) {
		t.Helper()
		if ended.State != JobExited || ended.ExitCode != 0 || ended.Group != record.PID {
			t.Fatalf("incorrect job reported by %s: %+v", operation, ended)
		} else if len(ended.Leftovers) != 1 || ended.Leftovers[0].PID != group || ended.OmittedLeftovers != 0 || ended.GroupMayRemain {
			t.Fatalf("incorrect leftovers reported by %s (expected only %d): %+v", operation, group, ended)
		} else if !strings.Contains(ended.Leftovers[0].Command, "sleep") {
			t.Fatalf("incorrect leftover command reported by %s: %q", operation, ended.Leftovers[0].Command)
		}
	}
	check("waiting", waitTestJob(t, root, job.ID))
	jobs, _ := listTestJobs(t, root)
	if len(jobs) != 1 {
		t.Fatalf("incorrect listing: %+v", jobs)
	}
	check("listing", jobs[0])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	killed, err := KillJob(ctx, jobRoute(ctx, root), job.ID)
	if err != nil {
		t.Fatal("unable to kill job:", err)
	}
	check("killing", killed)
	if err := syscall.Kill(group, 0); err != nil {
		t.Fatal("killing an ended job terminated a process that it left running:", err)
	}

	// Once the leftover process has been killed, it's no longer reported. It
	// isn't reported as a zombie if it hasn't been reaped yet.
	if err := syscall.Kill(group, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	delete(alive, group)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if jobs, _ := listTestJobs(t, root); len(jobs) == 1 && len(jobs[0].Leftovers) == 0 && !jobs[0].GroupMayRemain {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("killed leftover still reported: %+v", jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
