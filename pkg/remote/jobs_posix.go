//go:build !windows && !aix

package remote

import (
	"context"
	"encoding/json"
	"errors"
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

// jobsSupported indicates whether detached jobs are supported on this system
// (see jobs_unsupported.go for the systems on which they aren't).
const jobsSupported = true

// jobStartTimeout is the time within which a job's supervisor must report that
// the job's command is starting (see supervisorScript), after which the job is
// killed and fails to start. Supervisors only run a few shell builtins before
// reporting, so it only bounds the wait for one that hangs.
const jobStartTimeout = 30 * time.Second

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
//
// Before executing the command, the subshell writes a byte to the pipe
// inherited as descriptor 4, which it also keeps from the command. startJob
// waits for the byte before recording the job and reporting its identifier,
// without which the job can't be killed, so that by the time it can be,
// signals sent to the group reach the subshell, or the command that replaces
// it, with their default actions. Otherwise, a signal that arrived before the
// supervisor caught signals would kill the supervisor, and one that arrived
// before the subshell existed would be caught without reaching the command,
// which would then run until killed with SIGKILL, so that either way the job
// would end without the chance to exit cleanly, and with the status of
// SIGKILL. The supervisor keeps its copy of the pipe until it exits, so the
// pipe only closes without the byte once the supervisor has exited, and the
// subshell doesn't execute the command if it can't write the byte, which only
// happens once startJob has stopped waiting for it.
const supervisorScript = `mutagen_job=$1
shift
trap : HUP INT TERM
(printf . >&4 && exec "$@" 4>&-) 3>&-
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

	// Create the pipe through which the supervisor reports that the command is
	// starting (see supervisorScript).
	ready, readyWriter, err := os.Pipe()
	if err != nil {
		return result{Error: fmt.Sprintf("unable to create job pipe: %v", err)}
	}
	defer ready.Close()

	// Start the supervisor in a new session, so that it neither receives
	// signals meant for the executor's process group or terminal nor is
	// terminated along with the executor's commands. This executor closes its
	// copy of the pipe's write end once the supervisor has started, so that
	// only the supervisor and its subshell hold it. Other processes that this
	// process starts don't inherit it, since descriptors are opened
	// close-on-exec.
	supervisor := exec.Command(prologueShell, slices.Concat([]string{"-c", supervisorScript, "remote-job", jobDirectory}, command.arguments)...)
	supervisor.Dir = command.directory
	supervisor.Env = append(supervisor.Environ(), slices.Concat(command.environment, []string{JobVariable + "=" + id})...)
	supervisor.Stdout, supervisor.Stderr = log, log
	supervisor.ExtraFiles = []*os.File{lock, readyWriter}
	supervisor.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	start := time.Now()
	err = supervisor.Start()
	readyWriter.Close()
	if err != nil {
		return result{ExitCode: 126, Error: fmt.Sprintf("unable to start job: %v", err)}
	}
	go supervisor.Wait()

	// Wait until the command is starting, so that the job can't be killed
	// before signals reach the command (see supervisorScript). The pipe also
	// closes if the supervisor exits first, in which case the job is recorded
	// with whatever state the supervisor left. A supervisor that doesn't
	// report in time is killed rather than recorded, since its subshell can't
	// write to the pipe once this executor has closed it, and then doesn't
	// execute the command.
	if !waitForCommandStart(ready) {
		syscall.Kill(-supervisor.Process.Pid, syscall.SIGKILL)
		return result{ExitCode: 126, Error: fmt.Sprintf("unable to start job: the job's command didn't start within %v", jobStartTimeout)}
	}

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

// waitForCommandStart waits until a job's supervisor reports, through the
// specified pipe, that the job's command is starting, or until the pipe closes
// because the supervisor exited without doing so, and returns false if neither
// happens within jobStartTimeout. The timeout relies on the pipe supporting
// deadlines, as pipes do on the systems that support jobs.
func waitForCommandStart(ready *os.File) bool {
	ready.SetReadDeadline(time.Now().Add(jobStartTimeout))
	_, err := ready.Read(make([]byte, 1))
	return !errors.Is(err, os.ErrDeadlineExceeded)
}

// jobGroupRemains determines whether any process remains in the process group
// led by the job supervisor with the specified process identifier. Zombies
// remain until they're reaped, and processes that this process isn't allowed
// to signal remain as well.
func jobGroupRemains(pid int) bool {
	return syscall.Kill(-pid, 0) != syscall.ESRCH
}

// waitForJobExit waits until a job's lock is free and no process remains in its
// process group, which its supervisor, with the specified process identifier,
// leads, or until the specified duration elapses or the context is cancelled,
// and returns whether the job exited.
func waitForJobExit(ctx context.Context, lock string, pid int, duration time.Duration) bool {
	deadline := time.Now().Add(duration)
	for {
		if running, err := jobLockHeld(lock); err == nil && !running && !jobGroupRemains(pid) {
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
// process identifier and leads the job's process group, and returns the job's
// state afterward. It sends SIGTERM to the group, waits for the supervisor to
// release the lock and for every process in the group to exit, and sends
// SIGKILL to the group if any process remains once the termination grace
// period expires. The supervisor releases the lock once the command exits, so
// waiting for the group as well ensures that processes that ignore SIGTERM,
// such as a command's workers, don't outlive a job reported as killed.
// Processes that leave the group, such as with setsid, aren't terminated.
// Zombies remain in the group until they're reaped, so where orphans aren't
// reaped, such as in a container whose PID 1 doesn't reap them, terminating a
// job can take the full grace period (and the wait after SIGKILL) even if its
// processes exit at once. The wait is bounded, so that only delays the result.
//
// The group is signaled by its identifier, which the system can reuse for a
// new process group once the job's group has ended. The system doesn't reuse a
// process group's identifier while the group has members, zombies included,
// and the supervisor is one while it holds the lock, so a check that finds the
// job running (before SIGTERM) or its group remaining (before SIGKILL) ensures
// that the identifier is still the job's. It could only belong to another
// group by the time of the signal if, between the check and the signal, the
// group's last member exited and was reaped and the system assigned the
// identifier to a new group, which would require it to reuse process
// identifiers within that interval.
func terminateJob(ctx context.Context, directory string, pid int) (*Job, error) {
	if err := os.WriteFile(filepath.Join(directory, jobKilledName), nil, 0600); err != nil {
		return nil, fmt.Errorf("unable to mark job as killed: %w", err)
	}
	lock := filepath.Join(directory, jobLockName)
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return nil, err
	} else if !waitForJobExit(ctx, lock, pid, terminationGracePeriod) {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return nil, err
		}
		waitForJobExit(ctx, lock, pid, time.Second)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, job, err := loadJob(directory)
	return job, err
}
