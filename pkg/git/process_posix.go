//go:build !windows

package git

import (
	"os/exec"
	"syscall"
)

// configureProcess isolates a Git command in its own process group and makes
// cancellation kill the whole group. Git spawns helper processes for transports
// (such as ssh and git-remote-https) and for receiving objects (such as
// index-pack), which keep writing to its temporary directories and holding its
// output open for as long as they're running, and killing Git alone doesn't
// stop them. It also means that a process that tries to prompt on a controlling
// terminal can't read from it, and stops without affecting anything else, until
// the command is killed.
func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		// If the group can't be signaled, then it's probably because nothing
		// but zombies remain in it, which results in different errors on
		// different platforms. Killing the process itself reports that in a
		// uniform way.
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
			return command.Process.Kill()
		}
		return nil
	}
}
