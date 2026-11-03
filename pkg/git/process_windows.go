package git

import (
	"os/exec"
)

// configureProcess performs platform-specific configuration of a Git
// command. There's nothing to configure on Windows, where cancellation kills
// the Git process itself. Its helper processes can outlive it, which is
// accounted for by the wait delay of the command and by retrying the removal of
// temporary clones. Git doesn't get to clean up, so a stalled clone of a nested
// submodule leaves a repository without commits that has to be removed by hand,
// and nothing here could do that reliably, since a helper may still be using it.
func configureProcess(_ *exec.Cmd) {}
