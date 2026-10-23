package remote

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// procRoot is the mount point of the proc file system, through which leftover
// processes are found (see findLeftovers). It's a variable so that tests can
// substitute another directory.
var procRoot = "/proc"

// maximumCommandLength is the length in bytes beyond which the command lines of
// leftover processes are truncated.
const maximumCommandLength = 200

// findLeftovers looks for processes that the commands of those of the specified
// jobs that have ended left running, and records them in the jobs (see
// Job.Leftovers). It scans the proc file system once for all of the jobs, so
// that listing many jobs doesn't scan it repeatedly.
//
// A process counts as a job's leftover if it's in the job's process group and
// its environment contains the job's identifier (see JobVariable), which the
// job's supervisor sets and the command's processes inherit. Checking the
// environment as well as the group ensures that processes in a new group, to
// which the system assigned the identifier of the job's group once the job's
// group had ended, aren't listed. Zombies aren't listed, and neither are
// processes whose files this process can't read, such as those of other users
// and setuid processes, processes that left the group, such as with setsid,
// and processes started with environments that lack the job's identifier.
// If the proc file system can't be read, then jobs are only marked as possibly
// having leftover processes, as on other systems.
func findLeftovers(jobs []*Job) {
	if jobs = endedJobs(jobs); len(jobs) == 0 {
		return
	} else if err := scanLeftovers(procRoot, jobs); err != nil {
		probeJobGroups(jobs)
	}
}

// scanLeftovers finds the leftover processes of the specified jobs, which must
// have ended, by scanning the proc file system mounted at the specified root
// (see findLeftovers). It fails only if the root can't be read.
func scanLeftovers(root string, jobs []*Job) error {
	// Index the jobs by process group. Jobs can share groups if the system
	// reused their supervisors' process identifiers.
	groups := make(map[int][]*Job, len(jobs))
	for _, job := range jobs {
		groups[job.Group] = append(groups[job.Group], job)
	}

	// Scan the processes, reading the environments of only those in the jobs'
	// groups. Processes that exit during the scan are skipped as their files
	// become unreadable.
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	leftovers := make(map[*Job][]JobProcess)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		directory := filepath.Join(root, entry.Name())
		stat, err := os.ReadFile(filepath.Join(directory, "stat"))
		if err != nil {
			continue
		}
		name, state, group, ok := parseProcessStat(stat)
		if !ok || state == 'Z' || state == 'X' || len(groups[group]) == 0 {
			continue
		}
		environment, err := os.ReadFile(filepath.Join(directory, "environ"))
		if err != nil {
			continue
		}
		if job := matchJobEnvironment(environment, groups[group]); job != nil {
			leftovers[job] = append(leftovers[job], JobProcess{PID: pid, Command: readCommand(directory, name)})
		}
	}

	// Record the leftovers, ordered by process identifier, up to the limit.
	for job, processes := range leftovers {
		sort.Slice(processes, func(i, j int) bool {
			return processes[i].PID < processes[j].PID
		})
		if len(processes) > maximumLeftovers {
			job.OmittedLeftovers = len(processes) - maximumLeftovers
			processes = processes[:maximumLeftovers]
		}
		job.Leftovers = processes
	}
	return nil
}

// parseProcessStat parses a process's name, state, and process group from the
// contents of its stat file in the proc file system. The name is enclosed in
// parentheses and can itself contain spaces and parentheses, so the fields
// that follow it are located from the last closing parenthesis.
func parseProcessStat(data []byte) (name string, state byte, group int, ok bool) {
	start, end := bytes.IndexByte(data, '('), bytes.LastIndexByte(data, ')')
	if start < 0 || end < start {
		return "", 0, 0, false
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 3 || len(fields[0]) != 1 {
		return "", 0, 0, false
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, 0, false
	}
	return string(data[start+1 : end]), fields[0][0], group, true
}

// matchJobEnvironment returns the job among the specified ones whose identifier
// a process's environment, as read from its environ file in the proc file
// system, assigns to JobVariable, if any.
func matchJobEnvironment(environment []byte, jobs []*Job) *Job {
	prefix := []byte(JobVariable + "=")
	for _, variable := range bytes.Split(environment, []byte{0}) {
		if value, ok := bytes.CutPrefix(variable, prefix); ok {
			for _, job := range jobs {
				if string(value) == job.ID {
					return job
				}
			}
		}
	}
	return nil
}

// readCommand reads the command line of the process with the specified
// directory in the proc file system, with its arguments separated by spaces,
// and truncates it to maximumCommandLength. Processes without command lines
// are described by their names in brackets, as ps describes them.
func readCommand(directory, name string) string {
	var data []byte
	if file, err := os.Open(filepath.Join(directory, "cmdline")); err == nil {
		data, _ = io.ReadAll(io.LimitReader(file, maximumCommandLength+1))
		file.Close()
	}
	truncated := len(data) > maximumCommandLength
	if truncated {
		// Remove the end of a character that the limit splits.
		data = data[:maximumCommandLength]
		for i := 1; i < utf8.UTFMax && len(data) > 0; i++ {
			if r, size := utf8.DecodeLastRune(data); r != utf8.RuneError || size != 1 {
				break
			}
			data = data[:len(data)-1]
		}
	}
	command := strings.TrimRight(string(bytes.ReplaceAll(data, []byte{0}, []byte{' '})), " ")
	if command == "" {
		return "[" + name + "]"
	} else if truncated {
		return command + "..."
	}
	return command
}
