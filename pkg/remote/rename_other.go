//go:build !linux && !darwin

package remote

import (
	"errors"
	"os"
)

// renameNoReplace isn't supported on this platform.
func renameNoReplace(_ *os.Root, _, _ string) error {
	return errors.ErrUnsupported
}
