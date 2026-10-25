//go:build !windows && !aix

package remote

import (
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

// checkLogType returns an error, naming the file's type, unless a file with the
// specified mode (as reported by stat) can serve as a job's log (see openLog).
func checkLogType(path string, mode uint32) error {
	var kind string
	switch mode & unix.S_IFMT {
	case unix.S_IFREG, unix.S_IFCHR:
		return nil
	case unix.S_IFIFO:
		kind = "a FIFO"
	case unix.S_IFSOCK:
		kind = "a socket"
	case unix.S_IFDIR:
		kind = "a directory"
	case unix.S_IFBLK:
		kind = "a block device"
	default:
		kind = "a special file"
	}
	return fmt.Errorf("%s is %s, not a regular file or character device", path, kind)
}

// openLog opens a job's log, creating it with the specified permissions
// (subject to the umask) if it doesn't exist, and truncating it if it's a
// regular file, as a shell does with the files to which it redirects output.
// Logs must be regular files or character devices, such as /dev/null, and
// files of other types are refused: opening a FIFO that no process reads
// would block until one does, holding up the executor, and with it the
// closing of its channel, indefinitely, while output written to a FIFO or
// socket with a reader goes to whichever process reads it. On an agent, for
// example, /dev/stdout can name the agent's connection to the daemon.
//
// The log is therefore opened without blocking, which makes opening a FIFO
// without a reader fail at once (with ENXIO), and without O_TRUNC: it's only
// truncated once the open descriptor has been found to refer to a regular
// file, so that the check applies to the file that was opened, even if the
// path is replaced meanwhile. A log that can't be opened, such as a FIFO
// without a reader or a socket, is reported by its type if that's one that
// isn't supported. The descriptor is returned in blocking mode, which the
// job's processes, which share its open file description, expect, and is
// close-on-exec, as descriptors opened by Go are, so that only the supervisor,
// to which it's passed explicitly, inherits it.
func openLog(path string, permissions os.FileMode) (*os.File, error) {
	// Open the log without blocking, retrying if interrupted.
	var descriptor int
	var err error
	for {
		descriptor, err = unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(permissions.Perm()))
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		var metadata unix.Stat_t
		if unix.Stat(path, &metadata) == nil {
			if typeErr := checkLogType(path, uint32(metadata.Mode)); typeErr != nil {
				return nil, typeErr
			}
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	opened := false
	defer func() {
		if !opened {
			unix.Close(descriptor)
		}
	}()

	// Check the type of the opened file, and truncate it if it's a regular
	// file. Character devices can't be truncated, and are used as they are.
	var metadata unix.Stat_t
	if err := unix.Fstat(descriptor, &metadata); err != nil {
		return nil, &os.PathError{Op: "fstat", Path: path, Err: err}
	} else if err := checkLogType(path, uint32(metadata.Mode)); err != nil {
		return nil, err
	} else if metadata.Mode&unix.S_IFMT == unix.S_IFREG {
		if err := unix.Ftruncate(descriptor, 0); err != nil {
			return nil, &os.PathError{Op: "truncate", Path: path, Err: err}
		}
	}

	// Restore blocking mode. It's restored before the descriptor is wrapped,
	// so that the file isn't registered with Go's poller.
	if err := unix.SetNonblock(descriptor, false); err != nil {
		return nil, &os.PathError{Op: "fcntl", Path: path, Err: err}
	}
	opened = true
	return os.NewFile(uintptr(descriptor), path), nil
}

// startJob starts a requested command (or script) as a detached job, which runs
// in a new session, with no standard input and with its output written to its
// log, and which outlives the executor. Failures are reported as results. It
// doesn't block on the log, which must be a regular file or character device
// (see openLog), so that it only waits for the job's supervisor to report that
// the command is starting, for at most jobStartTimeout.
func startJob(root, prologue string, alpha bool, request *Request) result {
	directory, err := filesystem.Mutagen(true, jobsDirectoryName)
	if err != nil {
		return failedResult(fmt.Errorf("unable to compute job directory: %w", err))
	}
	pruneJobs(directory)

	// Create the job's directory, which is removed unless the job starts.
	var id, jobDirectory string
	for attempts := 0; ; attempts++ {
		if id, err = newJobID(); err != nil {
			return failedResult(err)
		}
		jobDirectory = filepath.Join(directory, id)
		if err = os.Mkdir(jobDirectory, 0700); err == nil {
			break
		} else if !os.IsExist(err) || attempts == 9 {
			return failedResult(fmt.Errorf("unable to create job directory: %w", err))
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
	// redirects output, while the default one is private. Logs that aren't
	// regular files or character devices are refused (see openLog).
	logPath, permissions := filepath.Join(jobDirectory, jobLogName), os.FileMode(0600)
	if request.Job.Log != "" {
		if logPath, err = resolveLog(command.directory, request.Job.Log); err != nil {
			return failedResult(fmt.Errorf("invalid log path: %w", err))
		}
		permissions = 0666
	}
	log, err := openLog(logPath, permissions)
	if err != nil {
		return failedResult(fmt.Errorf("unable to open log: %w", err))
	}
	defer log.Close()

	// Lock the job, passing the lock to the supervisor, which holds it once
	// this executor has closed its descriptor.
	lock, err := os.OpenFile(filepath.Join(jobDirectory, jobLockName), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return failedResult(fmt.Errorf("unable to create job lock: %w", err))
	}
	defer lock.Close()
	if err := flock(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return failedResult(fmt.Errorf("unable to lock job: %w", err))
	}

	// Create the pipe through which the supervisor reports that the command is
	// starting (see supervisorScript).
	ready, readyWriter, err := os.Pipe()
	if err != nil {
		return failedResult(fmt.Errorf("unable to create job pipe: %w", err))
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
		return result{ExitCode: 126, ErrorKind: ErrorKindOperation, Error: fmt.Sprintf("unable to start job: %v", err)}
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
		return result{ExitCode: 126, ErrorKind: ErrorKindOperation, Error: fmt.Sprintf("unable to start job: the job's command didn't start within %v", jobStartTimeout)}
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
		return failedResult(fmt.Errorf("unable to record job: %w", err))
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

// signalJobGroup sends a signal to the process group led by the job supervisor
// with the specified process identifier. It's a variable so that tests can
// simulate how other systems report signaling failures (see terminateJob).
var signalJobGroup = func(pid int, signal syscall.Signal) error {
	return syscall.Kill(-pid, signal)
}

// jobGroupRemains determines whether any process remains in the process group
// led by the job supervisor with the specified process identifier. Zombies
// remain until they're reaped, and processes that this process isn't allowed
// to signal remain as well.
func jobGroupRemains(pid int) bool {
	return syscall.Kill(-pid, 0) != syscall.ESRCH
}

// endedJobs returns the specified jobs that have ended and whose process groups
// are known, which are those for which leftover processes are looked for (see
// findLeftovers).
func endedJobs(jobs []*Job) []*Job {
	var ended []*Job
	for _, job := range jobs {
		if job.State != JobRunning && job.Group > 0 {
			ended = append(ended, job)
		}
	}
	return ended
}

// probeJobGroups marks the specified jobs, which must have ended, as possibly
// having leftover processes if a process group with their group's identifier
// exists (see Job.GroupMayRemain), for systems on which processes can't be
// listed. A group that this process isn't allowed to signal counts as
// existing.
func probeJobGroups(jobs []*Job) {
	for _, job := range jobs {
		job.GroupMayRemain = jobGroupRemains(job.Group)
	}
}

// waitForJobExit waits until a job's lock is free and no process remains in its
// process group, which its supervisor, with the specified process identifier,
// leads, or until the specified deadline passes, and returns whether the job
// exited. It doesn't take a context, since termination isn't abandoned once it
// has started (see terminateJob), so it only returns early if the job exits.
func waitForJobExit(lock string, pid int, deadline time.Time) bool {
	for {
		if running, err := jobLockHeld(lock); err == nil && !running && !jobGroupRemains(pid) {
			return true
		} else if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(terminationPollInterval)
	}
}

// terminateJob terminates a running job, whose supervisor has the specified
// process identifier and leads the job's process group, and returns the job's
// state afterward. It marks the job as killed, sends SIGTERM to the group,
// waits for the supervisor to release the lock and for every process in the
// group to exit, and sends SIGKILL to the group if any process remains once the
// termination grace period expires. The supervisor releases the lock once the
// command exits, so waiting for the group as well ensures that processes that
// ignore SIGTERM, such as a command's workers, don't outlive a job reported as
// killed. Processes that leave the group, such as with setsid, aren't
// terminated, and neither are processes that the command left running once
// the job has ended, since a job that has ended isn't terminated (serveJob
// reports them instead). Zombies remain in the group until they're reaped,
// so where orphans aren't reaped, such as in a container whose PID 1 doesn't
// reap them, terminating a job can take the full grace period (and the wait
// after SIGKILL) even if its processes exit at once. The wait is bounded, so
// that only delays the result.
//
// Termination runs to completion once it has started, even if the request for
// it is cancelled, such as because remote was interrupted or timed out, or the
// executor is closing: abandoning it during the grace period would leave a job
// that ignores SIGTERM running, and sending SIGKILL at once instead would cut
// short the cleanup of a job that handles SIGTERM. It takes at most the grace
// period and the wait after SIGKILL, for which the executor's channel waits
// when it closes, as it does for the termination of foreground commands (see
// runForeground).
//
// The file that marks the job as killed is created exclusively, so that only
// the first termination of a job sends SIGTERM. A later termination that finds
// the job already marked, such as one requested again or concurrently, doesn't
// send SIGTERM again, which would interrupt the cleanup of a job that handles
// it. It instead waits for the grace period that began when the job was
// marked, as recorded by the marker's modification time, and sends SIGKILL if
// any process remains once that expires, at once if it already has, such as
// because the executor of the first termination exited during the grace
// period. A later termination thus neither extends the first's grace period
// nor cuts it short, except by the precision of file timestamps, since the
// first termination creates the marker just before it sends SIGTERM. Its
// deadline is never more than the grace period away, so that a timestamp in
// the future, such as one set by the server of a network file system whose
// clock is ahead, can't delay SIGKILL further.
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
//
// Signaling the group fails with EPERM if none of its remaining processes can
// be signaled. That includes processes that this process isn't allowed to
// signal, but on Darwin, it also includes zombies: a group whose members are
// all zombies, such as one that a concurrent termination's SIGKILL has just
// ended, can't be signaled. The supervisor can be signaled while it holds the
// lock, so EPERM implies that the job has ended or is ending. Termination thus
// continues as if the signal had been sent and only reports EPERM if the job
// is still running afterward.
func terminateJob(directory string, pid int) (*Job, error) {
	// Mark the job as killed and, if this termination marked it, send SIGTERM.
	// Either way, determine when the grace period expires.
	lock, killed := filepath.Join(directory, jobLockName), filepath.Join(directory, jobKilledName)
	var deadline time.Time
	var unsignaled error
	if marker, err := os.OpenFile(killed, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600); err == nil {
		marker.Close()
		if err := signalJobGroup(pid, syscall.SIGTERM); err == syscall.EPERM {
			unsignaled = err
		} else if err != nil && err != syscall.ESRCH {
			return nil, err
		}
		deadline = time.Now().Add(terminationGracePeriod)
	} else if !os.IsExist(err) {
		return nil, fmt.Errorf("unable to mark job as killed: %w", err)
	} else if info, err := os.Lstat(killed); err != nil {
		return nil, fmt.Errorf("unable to determine when the job was marked as killed: %w", err)
	} else {
		deadline = info.ModTime().Add(terminationGracePeriod)
		if latest := time.Now().Add(terminationGracePeriod); deadline.After(latest) {
			deadline = latest
		}
	}

	// Wait for the job to exit, and kill whatever remains of it once the grace
	// period expires.
	if !waitForJobExit(lock, pid, deadline) {
		if err := signalJobGroup(pid, syscall.SIGKILL); err == syscall.EPERM {
			unsignaled = err
		} else if err != nil && err != syscall.ESRCH {
			return nil, err
		}
		waitForJobExit(lock, pid, time.Now().Add(time.Second))
	}
	_, job, err := loadJob(directory)
	if err == nil && job.State == JobRunning && unsignaled != nil {
		return nil, unsignaled
	}
	return job, err
}
