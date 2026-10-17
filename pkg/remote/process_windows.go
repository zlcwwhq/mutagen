package remote

import (
	"os"
	"os/exec"
)

// configureProcess uses exec.CommandContext's default process cancellation.
func configureProcess(command *exec.Cmd) {}

// processExitCode returns the native command exit code.
func processExitCode(state *os.ProcessState) int {
	return state.ExitCode()
}
