//go:build windows || aix

package remote

import (
	"context"
)

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

// terminateJob terminates a running job. Detached jobs aren't supported on
// this system.
func terminateJob(_ context.Context, _ string, _ int) (*Job, error) {
	return nil, errJobsUnsupported
}
