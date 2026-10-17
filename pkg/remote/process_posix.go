//go:build !windows

package remote

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcess isolates the command's process group so that disconnects
// cancel children as well as the immediate command.
func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err == syscall.ESRCH {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
}

// processExitCode includes the conventional shell encoding for signal deaths.
func processExitCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}
