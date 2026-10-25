package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
)

const (
	// jobsDirectoryName is the name of the directory, inside the Mutagen data
	// directory, that holds the state of detached jobs, with one directory per
	// job, named by its identifier.
	jobsDirectoryName = "remote-jobs"
	// jobRecordName is the name of the file describing a job.
	jobRecordName = "job.json"
	// jobLockName is the name of the file that a job's supervisor holds locked
	// while it runs.
	jobLockName = "lock"
	// jobStatusName is the name of the file to which a job's supervisor writes
	// the command's exit status before exiting.
	jobStatusName = "status"
	// jobKilledName is the name of the file that marks a job as terminated by
	// remote --kill.
	jobKilledName = "killed"
	// jobLogName is the name of the file to which a job's output is written
	// unless another log is requested.
	jobLogName = "log"
	// jobScriptName is the name of the file to which a job's script is written.
	jobScriptName = "script"
	// jobFormat is the format of job directories. Job directories outlive the
	// executors that created them, so executors of other versions of Mutagen,
	// such as after an upgrade, may find them, and they skip job directories of
	// formats that they don't know.
	jobFormat = 1
	// jobRetention is the time for which a job's directory is kept after the
	// job ends.
	jobRetention = 7 * 24 * time.Hour
	// incompleteJobRetention is the time after which a job directory without a
	// record, such as one whose executor failed while starting the job, is
	// removed, unless the job is running.
	incompleteJobRetention = 24 * time.Hour
	// jobPollInterval is the interval at which a job's lock is checked while
	// waiting for the job to end.
	jobPollInterval = 100 * time.Millisecond
	// JobVariable is the environment variable that identifies a detached job to
	// its command.
	JobVariable = "MUTAGEN_REMOTE_JOB"
)

// errJobsUnsupported indicates that detached jobs aren't supported on the
// executing system (see jobsSupported).
var errJobsUnsupported = errors.New("detached jobs aren't supported on Windows or AIX endpoints")

// JobError is an error that the executing endpoint reports for a job
// operation, such as for a job that doesn't exist, as opposed to a failure to
// reach the executing endpoint.
type JobError struct {
	// Message describes the error.
	Message string
}

// Error implements error.Error.
func (e *JobError) Error() string {
	return e.Message
}

// JobState describes the state of a detached job.
type JobState string

const (
	// JobRunning indicates that the job is running.
	JobRunning JobState = "running"
	// JobExited indicates that the job's command exited with Job.ExitCode.
	JobExited JobState = "exited"
	// JobKilled indicates that the job was terminated by a job request (see
	// JobKill), with Job.ExitCode, which is 137 if it had to be killed with
	// SIGKILL before it could record its status.
	JobKilled JobState = "killed"
	// JobLost indicates that the job's supervisor ended without recording the
	// command's exit status, such as because it was killed by SIGKILL or the
	// system restarted.
	JobLost JobState = "lost"
)

// Job describes a detached job.
type Job struct {
	// ID is the job's identifier.
	ID string
	// Alpha indicates whether the job runs on alpha.
	Alpha bool
	// Directory is the job's working directory on the executing system.
	Directory string
	// Arguments are the requested command and its arguments, or the script's
	// arguments if Script is non-empty.
	Arguments []string
	// Script is the name of the script that the job runs, if any (see
	// Script.Name).
	Script string `json:",omitempty"`
	// Log is the path of the job's log on the executing system.
	Log string
	// LogPath is the slash-separated path of the job's log relative to the
	// executing endpoint's root, if it's inside the root.
	LogPath string `json:",omitempty"`
	// Started is the time at which the job started.
	Started time.Time
	// State is the job's state.
	State JobState
	// ExitCode is the command's exit status, if the job exited or was killed.
	ExitCode int `json:",omitempty"`
	// Elapsed is the time for which the job has run, or ran, if known, as
	// measured on the executing system.
	Elapsed time.Duration `json:",omitempty"`
	// Group is the identifier of the job's process group, which its
	// supervisor led. It's zero if the executing endpoint predates its
	// reporting.
	Group int `json:",omitempty"`
	// Leftovers are processes that remain in the job's process group after
	// the job has ended, which its command left running, ordered by process
	// identifier, as found on systems where processes can be listed (see
	// findLeftovers). At most maximumLeftovers of them are listed.
	Leftovers []JobProcess `json:",omitempty"`
	// OmittedLeftovers is the number of leftover processes that weren't
	// listed in Leftovers because of its limit.
	OmittedLeftovers int `json:",omitempty"`
	// GroupMayRemain indicates that, on a system where processes can't be
	// listed, the job has ended but a process group with its group's
	// identifier still exists, which may be the job's group, with processes
	// that its command left running, or a new group to which the system
	// assigned the identifier once the job's group had ended.
	GroupMayRemain bool `json:",omitempty"`
}

// maximumLeftovers is the maximum number of leftover processes listed for a
// job (see Job.Leftovers), which bounds the size of job results.
const maximumLeftovers = 100

// JobProcess describes a process that a job's command left running.
type JobProcess struct {
	// PID is the process identifier.
	PID int
	// Command is the process's command line, with its arguments separated by
	// spaces, truncated if it's long.
	Command string
}

// jobRecord is the on-disk description of a detached job.
type jobRecord struct {
	// Format is the job directory's format (see jobFormat).
	Format int
	// Version is the Mutagen version of the executor that started the job.
	Version string
	// ID is the job's identifier.
	ID string
	// Root is the normalized synchronization root at which the job started.
	Root string
	// Alpha indicates whether the executing endpoint is alpha.
	Alpha bool
	// Directory is the job's working directory.
	Directory string
	// Arguments are the requested command and its arguments, or the script's
	// arguments.
	Arguments []string
	// Script is the name of the job's script, if any.
	Script string `json:",omitempty"`
	// Log is the path of the job's log.
	Log string
	// Host is the name of the system that started the job, so that a job
	// directory shared with other systems, such as through a network home
	// directory, can't be used to signal processes on the wrong system.
	Host string
	// PID is the process identifier of the job's supervisor, which leads the
	// job's process group.
	PID int
	// Started is the time at which the job started.
	Started time.Time
}

// newJobID generates a random job identifier (see validJobID).
func newJobID() (string, error) {
	var identifier [4]byte
	if _, err := rand.Read(identifier[:]); err != nil {
		return "", fmt.Errorf("unable to generate job identifier: %w", err)
	}
	return hex.EncodeToString(identifier[:]), nil
}

// loadJob reads the record of the job with the specified directory and
// determines its state. The lock is checked before the status, so that a job
// whose lock is free has either recorded its status or never will (see
// supervisorScript).
func loadJob(directory string) (*jobRecord, *Job, error) {
	data, err := os.ReadFile(filepath.Join(directory, jobRecordName))
	if err != nil {
		return nil, nil, err
	}
	var record jobRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, nil, fmt.Errorf("invalid job record: %w", err)
	} else if record.Format != jobFormat {
		return nil, nil, fmt.Errorf("job was started by Mutagen %s, which uses another job format", record.Version)
	}
	job := &Job{
		ID:        record.ID,
		Alpha:     record.Alpha,
		Directory: record.Directory,
		Arguments: record.Arguments,
		Script:    record.Script,
		Log:       record.Log,
		Started:   record.Started,
		Group:     record.PID,
	}
	if path, contained := relativePath(record.Root, record.Log); contained {
		job.LogPath = path
	}
	if running, err := jobLockHeld(filepath.Join(directory, jobLockName)); err != nil {
		return nil, nil, fmt.Errorf("unable to determine whether the job is running: %w", err)
	} else if running {
		job.State, job.Elapsed = JobRunning, time.Since(record.Started)
		return &record, job, nil
	}
	_, killedErr := os.Lstat(filepath.Join(directory, jobKilledName))
	killed := killedErr == nil
	if info, err := os.Stat(filepath.Join(directory, jobStatusName)); err == nil {
		data, err := os.ReadFile(filepath.Join(directory, jobStatusName))
		if err != nil {
			return nil, nil, fmt.Errorf("unable to read job status: %w", err)
		}
		code, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, nil, fmt.Errorf("invalid job status: %q", data)
		}
		job.State, job.ExitCode, job.Elapsed = JobExited, code, max(info.ModTime().Sub(record.Started), 0)
		if killed {
			job.State = JobKilled
		}
	} else if killed {
		job.State, job.ExitCode = JobKilled, 128+9
	} else {
		job.State = JobLost
	}
	return &record, job, nil
}

// jobEnded returns the time at which a job directory last changed, which, for
// a job that isn't running, approximates the time at which it ended.
func jobEnded(directory string) time.Time {
	var latest time.Time
	for _, name := range []string{"", jobRecordName, jobLockName, jobStatusName, jobKilledName} {
		if info, err := os.Lstat(filepath.Join(directory, name)); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}

// pruneJobs removes the directories of jobs that ended more than jobRetention
// ago, and incomplete job directories older than incompleteJobRetention. Job
// directories that are locked, or whose lock can't be checked, are kept. Logs
// written outside job directories are never removed.
func pruneJobs(directory string) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validJobID(entry.Name()) {
			continue
		}
		job := filepath.Join(directory, entry.Name())
		if running, err := jobLockHeld(filepath.Join(job, jobLockName)); err != nil || running {
			continue
		}
		retention := jobRetention
		if _, err := os.Lstat(filepath.Join(job, jobRecordName)); os.IsNotExist(err) {
			retention = incompleteJobRetention
		}
		if time.Since(jobEnded(job)) > retention {
			os.RemoveAll(job)
		}
	}
}

// listJobs lists the jobs started at the specified root, newest first, and
// counts the jobs started at other roots. Jobs whose state can't be determined
// are omitted.
func listJobs(directory, root string) ([]*Job, int) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, 0
	}
	var jobs []*Job
	var hidden int
	for _, entry := range entries {
		if !entry.IsDir() || !validJobID(entry.Name()) {
			continue
		}
		record, job, err := loadJob(filepath.Join(directory, entry.Name()))
		if err != nil {
			continue
		} else if record.Root != root {
			hidden++
			continue
		}
		jobs = append(jobs, job)
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		return jobs[i].Started.After(jobs[j].Started)
	})
	return jobs, hidden
}

// findJob loads the job with the specified identifier, which must have been
// started at the specified root.
func findJob(directory, root, id string) (*jobRecord, *Job, error) {
	record, job, err := loadJob(filepath.Join(directory, id))
	if os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("no job %s exists on this endpoint (jobs are removed %d days after they end)", id, int(jobRetention.Hours()/24))
	} else if err != nil {
		return nil, nil, fmt.Errorf("unable to load job %s: %w", id, err)
	} else if record.Root != root {
		return nil, nil, fmt.Errorf("job %s was started at another synchronization root (%s)", id, record.Root)
	}
	return record, job, nil
}

// serveJob performs a job operation other than starting a job for an executor
// with the specified normalized root, writing job listings to the specified
// output as one JSON object per line and requested log tails as raw bytes,
// so that neither is bounded by the size of protocol messages. Jobs that have
// ended are reported along with the
// processes that their commands left running, if any (see findLeftovers).
// Those processes aren't part of the jobs any more, so killing the jobs
// doesn't terminate them.
func serveJob(ctx context.Context, root string, request *JobRequest, output io.Writer) result {
	if !jobsSupported {
		return result{Error: errJobsUnsupported.Error()}
	}
	directory, err := filesystem.Mutagen(false, jobsDirectoryName)
	if err != nil {
		return result{Error: fmt.Sprintf("unable to compute job directory: %v", err)}
	}
	switch request.Operation {
	case JobList:
		pruneJobs(directory)
		jobs, hidden := listJobs(directory, root)
		findLeftovers(jobs)
		encoder := json.NewEncoder(output)
		for _, job := range jobs {
			if err := encoder.Encode(job); err != nil {
				return result{Error: fmt.Sprintf("unable to send job listing: %v", err)}
			}
		}
		return result{HiddenJobs: hidden}
	case JobWait:
		for {
			_, job, err := findJob(directory, root, request.ID)
			if err != nil {
				return result{Error: err.Error()}
			} else if job.State != JobRunning {
				findLeftovers([]*Job{job})
				response := result{Job: job}
				if request.Tail > 0 {
					response.JobLog = readJobLog(ctx, job.Log, request.Tail)
					if _, err := output.Write(response.JobLog.Data); err != nil {
						return result{Error: fmt.Sprintf("unable to send job log: %v", err)}
					}
				}
				return response
			}
			select {
			case <-time.After(jobPollInterval):
			case <-ctx.Done():
				return result{Error: ctx.Err().Error()}
			}
		}
	case JobKill:
		record, job, err := findJob(directory, root, request.ID)
		if err != nil {
			return result{Error: err.Error()}
		} else if job.State != JobRunning {
			findLeftovers([]*Job{job})
			return result{Job: job}
		} else if host, err := os.Hostname(); err != nil || host != record.Host {
			return result{Error: fmt.Sprintf("job %s was started on %s, not on this system", request.ID, record.Host)}
		}
		job, err = terminateJob(filepath.Join(directory, request.ID), record.PID)
		if err != nil {
			return result{Error: fmt.Sprintf("unable to terminate job %s: %v", request.ID, err)}
		}
		findLeftovers([]*Job{job})
		return result{Job: job}
	default:
		return result{Error: fmt.Sprintf("unknown job operation %q", request.Operation)}
	}
}

// resolveLog computes the path of a requested log on this system. Absolute
// and home-relative paths are normalized, while other paths are resolved
// against the specified working directory.
func resolveLog(directory, log string) (string, error) {
	if filepath.IsAbs(log) || strings.HasPrefix(log, "~") {
		return filesystem.Normalize(log)
	}
	return filepath.Join(directory, log), nil
}

// StartJob starts the command (or script) described by a request, whose Job
// must request JobStart, as a detached job over an established route (see
// Dial), and returns the job. It owns and closes the connection. Failures to
// start the job report exit codes as Execute does. Unlike commands run by
// Execute, the job keeps running when the connection closes.
func StartJob(ctx context.Context, connection io.ReadWriteCloser, request *Request) (*Job, int, error) {
	response, err := execute(ctx, connection, request, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		return nil, 0, err
	} else if response.Error != "" {
		return nil, response.ExitCode, errors.New(response.Error)
	} else if response.Job == nil {
		return nil, 0, errors.New("no job received")
	}
	return response.Job, 0, nil
}

// ListJobs lists the jobs started at the opposite endpoint's root over an
// established route (see Dial), newest first, and returns the number of jobs
// started at other roots on its system. It owns and closes the connection.
func ListJobs(ctx context.Context, connection io.ReadWriteCloser) ([]*Job, int, error) {
	var output bytes.Buffer
	response, err := execute(ctx, connection, &Request{Job: &JobRequest{Operation: JobList}}, strings.NewReader(""), &output, io.Discard)
	if err != nil {
		return nil, 0, err
	} else if response.Error != "" {
		return nil, 0, &JobError{response.Error}
	}
	var jobs []*Job
	decoder := json.NewDecoder(&output)
	for {
		var job Job
		if err := decoder.Decode(&job); err == io.EOF {
			break
		} else if err != nil {
			return nil, 0, fmt.Errorf("invalid job listing: %w", err)
		}
		jobs = append(jobs, &job)
	}
	return jobs, response.HiddenJobs, nil
}

// operateJob performs a job operation on the job with the specified identifier
// over an established route and returns the job. It owns and closes the
// connection. Errors reported by the executing endpoint are JobErrors.
func operateJob(ctx context.Context, connection io.ReadWriteCloser, operation JobOperation, id string) (*Job, error) {
	response, err := execute(ctx, connection, &Request{Job: &JobRequest{Operation: operation, ID: id}}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		return nil, err
	} else if response.Error != "" {
		return nil, &JobError{response.Error}
	} else if response.Job == nil {
		return nil, errors.New("no job received")
	}
	return response.Job, nil
}

// WaitJob waits for the job with the specified identifier, started at the
// opposite endpoint's root, to end over an established route (see Dial), and
// returns the job. It owns and closes the connection. Cancellation abandons
// the wait without affecting the job.
func WaitJob(ctx context.Context, connection io.ReadWriteCloser, id string) (*Job, error) {
	job, _, err := WaitJobWithTail(ctx, connection, id, 0)
	return job, err
}

// KillJob terminates the job with the specified identifier, started at the
// opposite endpoint's root, over an established route (see Dial), and returns
// the job. It owns and closes the connection. The job's process group receives
// SIGTERM, and then SIGKILL if any of its processes remain once
// TerminationGracePeriod expires. Cancellation abandons waiting for the result,
// but not the termination, which the executing endpoint completes. A job that
// is already being terminated doesn't receive SIGTERM again, and receives
// SIGKILL once the first termination's grace period expires. A job that has
// already ended is returned as it is: processes that its command left running
// aren't part of it any more and aren't terminated.
func KillJob(ctx context.Context, connection io.ReadWriteCloser, id string) (*Job, error) {
	return operateJob(ctx, connection, JobKill, id)
}
