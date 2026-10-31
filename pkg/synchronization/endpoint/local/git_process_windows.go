package local

import (
	"os/exec"
)

// configureGitProcess performs platform-specific configuration of a Git
// command. There's nothing to configure on Windows, where cancellation kills
// the Git process itself. Its helper processes can outlive it, which is
// accounted for by the wait delay of the command and by retrying the removal of
// temporary clones.
func configureGitProcess(_ *exec.Cmd) {}
