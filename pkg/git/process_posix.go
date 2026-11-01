//go:build !windows

package git

import (
	"os/exec"
	"syscall"
	"time"
)

const (
	// terminationGracePeriod is the amount of time that a Git command and its
	// helper processes have to exit after being asked to, before they're
	// killed. It's shorter than waitDelay, so that when waiting on a command
	// returns, none of them are still running. Git only has to remove a few
	// temporary files, so this only matters on a filesystem that's stopped
	// responding, or if a helper process doesn't respond to the request.
	terminationGracePeriod = 3 * time.Second
	// terminationPollInterval is the interval at which a process group is
	// checked during terminationGracePeriod.
	terminationPollInterval = 50 * time.Millisecond
)

// configureProcess isolates a Git command in its own process group and makes
// cancellation terminate the whole group. Git spawns helper processes for
// transports (such as ssh and git-remote-https) and for receiving objects (such
// as index-pack), which keep writing to its temporary directories and holding
// its output open for as long as they're running, and stopping Git alone doesn't
// stop them. It also means that a process that tries to prompt on a controlling
// terminal can't read from it, and stops without affecting anything else, until
// the command is terminated.
//
// The group is asked to terminate instead of being killed, because Git cleans up
// when it's asked to, and what it leaves behind if it's killed can be worse than
// the failure that led to it. In particular, a submodule's clone leaves a
// repository without any commits, which Git then won't clone again, and a .git
// file that can stop working, since it has an absolute path to that repository
// until the clone completes. Interrupted commands can also leave lock files in
// the user's repositories. A group that's still around after a grace period gets
// killed, which is still bounded by the command's wait delay.
func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		// If the group can't be signaled, then it's probably because nothing
		// but zombies remain in it, which results in different errors on
		// different platforms. Killing the process itself reports that in a
		// uniform way.
		group := -command.Process.Pid
		if err := syscall.Kill(group, syscall.SIGTERM); err != nil {
			return command.Process.Kill()
		}

		// A stopped process (see above) won't act on the signal until it's
		// been continued.
		syscall.Kill(group, syscall.SIGCONT)

		// Make sure that the group goes away.
		go killAfterGracePeriod(group)
		return nil
	}
}

// killAfterGracePeriod kills a process group unless it goes away during
// terminationGracePeriod. A group's identifier can't be reused while the group
// has members, and we stop looking at it as soon as it has none, so there's no
// meaningful risk of signaling an unrelated group.
func killAfterGracePeriod(group int) {
	for deadline := time.Now().Add(terminationGracePeriod); time.Now().Before(deadline); {
		time.Sleep(terminationPollInterval)
		if syscall.Kill(group, 0) != nil {
			return
		}
	}
	syscall.Kill(group, syscall.SIGKILL)
}
