package remote

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
)

// prologueShell is the shell that sources prologues.
const prologueShell = "/bin/sh"

// prologueScript sources the prologue specified by its first argument and then
// replaces itself with the command specified by its remaining arguments, so the
// command's arguments still aren't subject to shell interpretation and its exit
// status is reported directly. The shell locates the command only after the
// prologue has run, so commands may be found using a PATH that it modifies.
//
// The prologue is sourced inside a function so that changes to positional
// parameters don't affect the command. Any failure to source it, including a
// non-zero status, a syntax error, or the prologue exiting the shell, triggers
// the exit trap, which reports the failure with exit status 126 (indicating
// that the command couldn't be started). The prologue's status is deliberately
// not tested in a conditional context, since that would disable errexit within
// the prologue.
const prologueScript = `mutagen_prologue=$1
shift
trap 'mutagen_status=$?; trap - EXIT; echo "remote: prologue $mutagen_prologue failed with exit status $mutagen_status" >&2; exit 126' EXIT
mutagen_source() { . "$mutagen_prologue"; }
mutagen_source
mutagen_status=$?
[ "$mutagen_status" -eq 0 ] || exit "$mutagen_status"
trap - EXIT
unset -f mutagen_source
unset mutagen_prologue mutagen_status
exec "$@"`

// resolvePrologue computes the path of an endpoint's prologue on this system.
// Absolute and home-relative paths are normalized, while other paths are
// resolved against the normalized synchronization root. An empty path
// indicates that the endpoint has no prologue.
func resolvePrologue(root, prologue string) (string, error) {
	if prologue == "" {
		return "", nil
	} else if filepath.IsAbs(prologue) || strings.HasPrefix(prologue, "~") {
		return filesystem.Normalize(prologue)
	}
	return filepath.Join(root, prologue), nil
}

// prologueArguments wraps command arguments so that the specified resolved
// prologue is sourced before the command runs. The prologue's existence is
// checked here so that a missing prologue is reported clearly.
func prologueArguments(prologue string, arguments []string) ([]string, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("remote prologues are not supported on Windows")
	} else if info, err := os.Stat(prologue); err != nil {
		return nil, fmt.Errorf("unable to access prologue: %w", err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("prologue is a directory: %s", prologue)
	}
	return append([]string{prologueShell, "-c", prologueScript, "remote", prologue}, arguments...), nil
}
