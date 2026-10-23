//go:build !linux && !windows && !aix

package remote

// findLeftovers looks for processes that the commands of those of the specified
// jobs that have ended left running. Unlike on Linux (see leftovers_linux.go),
// processes and their environments can't be listed portably on this system,
// so jobs are only marked as possibly having leftover processes if a process
// group with their group's identifier exists (see Job.GroupMayRemain).
func findLeftovers(jobs []*Job) {
	probeJobGroups(endedJobs(jobs))
}
