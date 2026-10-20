//go:build !windows

package remote

import (
	"errors"
	"os"
	"syscall"
)

// chownFile changes the owner and group of an open file. Tests replace it to
// exercise failures that require other users.
var chownFile = (*os.File).Chown

// preserveOwnership gives a replacement's staged file the owner and group of
// the entry that it replaces, as described by information read at its path, so
// that a resolved copy isn't handed to the user that the agent runs as (such as
// root in a container). Only privileged agents can give files to other users,
// and others can only use groups that they belong to, so ownership is
// reproduced as far as permitted: the owner and group, otherwise the group
// alone, and otherwise neither, which leaves the file owned as synchronization
// leaves files that it creates without a default owner. Only lacking
// permission, or ownership that the filesystem or user namespace can't
// represent, is tolerated; other failures fail the replacement.
func preserveOwnership(file *os.File, info os.FileInfo) error {
	replaced, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	staged, err := file.Stat()
	if err != nil {
		return err
	}
	current, ok := staged.Sys().(*syscall.Stat_t)
	if !ok || (current.Uid == replaced.Uid && current.Gid == replaced.Gid) {
		return nil
	}
	err = chownFile(file, int(replaced.Uid), int(replaced.Gid))
	if err == nil || !unreproducibleOwnership(err) || current.Gid == replaced.Gid {
		return tolerateOwnership(err)
	}
	return tolerateOwnership(chownFile(file, -1, int(replaced.Gid)))
}

// tolerateOwnership discards errors that only indicate that ownership can't be
// reproduced (see preserveOwnership).
func tolerateOwnership(err error) error {
	if err != nil && unreproducibleOwnership(err) {
		return nil
	}
	return err
}

// unreproducibleOwnership determines whether an error changing ownership only
// indicates that it can't be reproduced: a lack of permission, an owner or
// group that isn't mapped into the user namespace, or a filesystem that
// doesn't support ownership.
func unreproducibleOwnership(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}
