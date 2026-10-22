//go:build !windows

package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
)

// jobsSupported indicates whether detached jobs are supported on this system.
const jobsSupported = true

// supervisorScript runs a detached job's command, specified by its arguments
// after the job directory, and records its exit status in the job directory
// once it exits. The supervisor holds the job's lock (inherited as descriptor
// 3), which it keeps from the command, until it exits, so that the lock is free
// exactly when the supervisor has recorded the status or never will.
//
// The supervisor leads the job's process group, so it receives the signals
// sent to the group to terminate the job. It catches them (rather than ignoring
// them, which commands would inherit), and since traps run only once the
// foreground command has exited, it then records the command's status. The
// command runs in a subshell, in which caught signals are reset to their
// defaults, and exec makes it an external command, as for commands run in the
// foreground, rather than a builtin or function. Only builtins are used, so
// that the supervisor doesn't depend on the PATH set for the command.
const supervisorScript = `mutagen_job=$1
shift
trap : HUP INT TERM
(exec "$@") 3>&-
mutagen_status=$?
printf '%s\n' "$mutagen_status" > "$mutagen_job/status"`

// flock applies or removes an advisory lock on a file, retrying if interrupted.
// Unlike the fcntl locks used by the locking package, flock locks belong to the
// open file description, so they're inherited by child processes along with
// the descriptor, and they conflict with locks on other descriptions of the
// same file, even within the same process.
func flock(file *os.File, operation int) error {
	for {
		if err := unix.Flock(int(file.Fd()), operation); err != unix.EINTR {
			return err
		}
	}
}

// jobLockHeld determines whether a job's lock is held, which indicates that the
// job's supervisor is running. A missing lock isn't held.
func jobLockHeld(path string) (bool, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer file.Close()
	if err := flock(file, unix.LOCK_SH|unix.LOCK_NB); err == unix.EWOULDBLOCK {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
}

// startJob starts a requested command (or script) as a detached job, which runs
// in a new session, with no standard input and with its output written to its
// log, and which outlives the executor. Failures are reported as results.
func startJob(root, prologue string, alpha bool, request *Request) result {
	directory, err := filesystem.Mutagen(true, jobsDirectoryName)
	if err != nil {
		return result{Error: fmt.Sprintf("unable to compute job directory: %v", err)}
	}
	pruneJobs(directory)

	// Create the job's directory, which is removed unless the job starts.
	var id, jobDirectory string
	for attempts := 0; ; attempts++ {
		if id, err = newJobID(); err != nil {
			return result{Error: err.Error()}
		}
		jobDirectory = filepath.Join(directory, id)
		if err = os.Mkdir(jobDirectory, 0700); err == nil {
			break
		} else if !os.IsExist(err) || attempts == 9 {
			return result{Error: fmt.Sprintf("unable to create job directory: %v", err)}
		}
	}
	started := false
	defer func() {
		if !started {
			os.RemoveAll(jobDirectory)
		}
	}()

	// Prepare the command, writing its script, if any, to the job directory,
	// where it's kept with the job.
	var scriptPath string
	if request.Script != nil {
		scriptPath = filepath.Join(jobDirectory, jobScriptName)
	}
	command, failure := prepare(root, prologue, alpha, request, scriptPath)
	if failure != nil {
		return *failure
	}

	// Open the log. A requested log is created like a file to which a shell
	// redirects output, while the default one is private.
	logPath, permissions := filepath.Join(jobDirectory, jobLogName), os.FileMode(0600)
	if request.Job.Log != "" {
		if logPath, err = resolveLog(command.directory, request.Job.Log); err != nil {
			return result{ExitCode: 126, Error: fmt.Sprintf("invalid log path: %v", err)}
		}
		permissions = 0666
	}
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND, permissions)
	if err != nil {
		return result{ExitCode: 126, Error: fmt.Sprintf("unable to open log: %v", err)}
	}
	defer log.Close()

	// Lock the job, passing the lock to the supervisor, which holds it once
	// this executor has closed its descriptor.
	lock, err := os.OpenFile(filepath.Join(jobDirectory, jobLockName), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result{Error: fmt.Sprintf("unable to create job lock: %v", err)}
	}
	defer lock.Close()
	if err := flock(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return result{Error: fmt.Sprintf("unable to lock job: %v", err)}
	}

	// Start the supervisor in a new session, so that it neither receives
	// signals meant for the executor's process group or terminal nor is
	// terminated along with the executor's commands.
	supervisor := exec.Command(prologueShell, slices.Concat([]string{"-c", supervisorScript, "remote-job", jobDirectory}, command.arguments)...)
	supervisor.Dir = command.directory
	supervisor.Env = append(supervisor.Environ(), slices.Concat(command.environment, []string{JobVariable + "=" + id})...)
	supervisor.Stdout, supervisor.Stderr = log, log
	supervisor.ExtraFiles = []*os.File{lock}
	supervisor.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	start := time.Now()
	if err := supervisor.Start(); err != nil {
		return result{ExitCode: 126, Error: fmt.Sprintf("unable to start job: %v", err)}
	}
	go supervisor.Wait()

	// Record the job. If that fails, then the job can't be found, so it's
	// killed.
	host, _ := os.Hostname()
	record := &jobRecord{
		Format:    jobFormat,
		Version:   mutagen.Version,
		ID:        id,
		Root:      root,
		Alpha:     alpha,
		Directory: command.directory,
		Arguments: request.Arguments,
		Log:       logPath,
		Host:      host,
		PID:       supervisor.Process.Pid,
		Started:   start,
	}
	if request.Script != nil {
		record.Script = request.Script.Name
	}
	data, err := json.Marshal(record)
	if err == nil {
		err = filesystem.WriteFileAtomic(filepath.Join(jobDirectory, jobRecordName), data, 0600)
	}
	if err != nil {
		syscall.Kill(-supervisor.Process.Pid, syscall.SIGKILL)
		return result{Error: fmt.Sprintf("unable to record job: %v", err)}
	}
	started = true
	job := &Job{
		ID:        id,
		Alpha:     alpha,
		Directory: record.Directory,
		Arguments: record.Arguments,
		Script:    record.Script,
		Log:       logPath,
		Started:   record.Started,
		State:     JobRunning,
	}
	if path, contained := relativePath(root, logPath); contained {
		job.LogPath = path
	}
	return result{Job: job}
}

// waitForJobLock waits until a job's lock is free or the specified duration
// elapses, returning whether the lock was freed.
func waitForJobLock(ctx context.Context, lock string, duration time.Duration) bool {
	deadline := time.Now().Add(duration)
	for {
		if running, err := jobLockHeld(lock); err == nil && !running {
			return true
		} else if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-time.After(terminationPollInterval):
		case <-ctx.Done():
			return false
		}
	}
}

// terminateJob terminates a running job, whose supervisor has the specified
// process identifier, by sending SIGTERM to its process group, and then
// SIGKILL if the job is still running once the termination grace period
// expires, and returns the job's state afterward. Since the job is running,
// its supervisor holds the lock, so its process group can't have been reused.
func terminateJob(ctx context.Context, directory string, pid int) (*Job, error) {
	if err := os.WriteFile(filepath.Join(directory, jobKilledName), nil, 0600); err != nil {
		return nil, fmt.Errorf("unable to mark job as killed: %w", err)
	}
	lock := filepath.Join(directory, jobLockName)
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return nil, err
	} else if !waitForJobLock(ctx, lock, terminationGracePeriod) {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return nil, err
		}
		waitForJobLock(ctx, lock, time.Second)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, job, err := loadJob(directory)
	return job, err
}
