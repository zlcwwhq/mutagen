//go:build !windows

package remote

import (
	"os"
	"os/exec"
	"syscall"
)

// gracefulTermination indicates that interruptProcess asks commands to
// terminate, rather than terminating them immediately.
const gracefulTermination = true

// configureProcess isolates the command's process group so that termination
// reaches its children as well as the immediate command.
func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcess sends a signal to a started command's process group, returning
// os.ErrProcessDone if no process in the group remains.
func signalProcess(command *exec.Cmd, signal syscall.Signal) error {
	if err := syscall.Kill(-command.Process.Pid, signal); err == syscall.ESRCH {
		return os.ErrProcessDone
	} else {
		return err
	}
}

// interruptProcess asks a started command's process group to terminate by
// sending it SIGTERM, which processes can handle to exit cleanly, such as by
// removing partially written files.
func interruptProcess(command *exec.Cmd) error {
	return signalProcess(command, syscall.SIGTERM)
}

// killProcess kills a started command's process group with SIGKILL.
func killProcess(command *exec.Cmd) error {
	return signalProcess(command, syscall.SIGKILL)
}

// processRemains determines whether any process in a started command's process
// group remains, including the command itself until it's waited for.
func processRemains(command *exec.Cmd) bool {
	return signalProcess(command, 0) != os.ErrProcessDone
}

// processExitCode includes the conventional shell encoding for signal deaths.
func processExitCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}
