package remote

import (
	"context"
)

// jobsSupported indicates whether detached jobs are supported on this system.
const jobsSupported = false

// jobLockHeld determines whether a job's lock is held. Detached jobs aren't
// supported on Windows.
func jobLockHeld(_ string) (bool, error) {
	return false, errJobsUnsupported
}

// startJob starts a requested command as a detached job. Detached jobs aren't
// supported on Windows.
func startJob(_, _ string, _ bool, _ *Request) result {
	return result{ExitCode: 126, Error: errJobsUnsupported.Error()}
}

// terminateJob terminates a running job. Detached jobs aren't supported on
// Windows.
func terminateJob(_ context.Context, _ string, _ int) (*Job, error) {
	return nil, errJobsUnsupported
}
