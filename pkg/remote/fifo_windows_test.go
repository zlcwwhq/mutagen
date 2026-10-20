package remote

import (
	"errors"
)

// mkfifo isn't supported on Windows.
func mkfifo(_ string) error {
	return errors.ErrUnsupported
}
