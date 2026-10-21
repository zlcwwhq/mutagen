package remote

import (
	"os"
	"os/exec"
)

// gracefulTermination indicates that interruptProcess asks commands to
// terminate, rather than terminating them immediately.
const gracefulTermination = false

// configureProcess uses the default process configuration.
func configureProcess(command *exec.Cmd) {}

// interruptProcess terminates a started command immediately, since Windows
// offers no way to ask a process without a console to terminate. Its child
// processes aren't terminated.
func interruptProcess(command *exec.Cmd) error {
	return command.Process.Kill()
}

// killProcess does nothing, since interruptProcess has already terminated the
// command.
func killProcess(command *exec.Cmd) error {
	return nil
}

// processRemains reports that no process remains, since child processes aren't
// tracked and interruptProcess has already terminated the command.
func processRemains(command *exec.Cmd) bool {
	return false
}

// processExitCode returns the native command exit code.
func processExitCode(state *os.ProcessState) int {
	return state.ExitCode()
}
