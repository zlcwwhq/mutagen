package remote

import (
	"os"
)

// preserveOwnership does nothing on Windows, where a file's access is governed
// by the access control list that it inherits from its directory, as it is for
// files that synchronization creates.
func preserveOwnership(_ *os.File, _ os.FileInfo) error {
	return nil
}
