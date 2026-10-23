//go:build windows || aix

package remote

import (
	"os"
)

// lockScript locks the temporary file of a script run in the foreground. Script
// files can't be locked on this system, so nil is returned: scripts aren't
// supported on Windows, and AIX lacks locks that belong to open file
// descriptions, such as flock locks (see jobs_unsupported.go).
func lockScript(_ string) *os.File {
	return nil
}

// pruneScripts removes the files of scripts run in the foreground that are no
// longer in use. Script files can't be locked on this system (see lockScript),
// so whether a script still runs can't be determined, and no file is removed,
// not even those left behind by executors that were killed. Executors still
// remove the files of their scripts once the scripts exit.
func pruneScripts(_ string) {}
