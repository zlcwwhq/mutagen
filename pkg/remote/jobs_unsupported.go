//go:build windows || aix

package remote

import "os"

// openJobLog opens a job log for reading. Detached jobs aren't supported on
// this system.
func openJobLog(_ string) (*os.File, error) {
	return nil, errJobsUnsupported
}

// logIdentity identifies a log file. Detached jobs aren't supported on this
// system, so it's unknown.
func logIdentity(_ os.FileInfo) string {
	return ""
}

// jobsSupported indicates whether detached jobs are supported on this system.
// They aren't supported on Windows, which lacks the POSIX shell, sessions, and
// process groups that their supervisors rely on (see jobs_posix.go), or on AIX,
// which lacks locks that belong to open file descriptions, such as flock locks,
// through which supervisors indicate that they're running: its fcntl locks
// belong to processes, aren't inherited by child processes, and are released
// when the process closes any descriptor of the file.
const jobsSupported = false

// jobLockHeld determines whether a job's lock is held. Detached jobs aren't
// supported on this system.
func jobLockHeld(_ string) (bool, error) {
	return false, errJobsUnsupported
}

// startJob starts a requested command as a detached job. Detached jobs aren't
// supported on this system.
func startJob(_, _ string, _ bool, _ *Request) result {
	return result{ExitCode: 126, Error: errJobsUnsupported.Error()}
}

// findLeftovers looks for processes that ended jobs left running. Detached jobs
// aren't supported on this system, so it does nothing.
func findLeftovers(_ []*Job) {}

// terminateJob terminates a running job. Detached jobs aren't supported on
// this system.
func terminateJob(_ string, _ int) (*Job, error) {
	return nil, errJobsUnsupported
}
