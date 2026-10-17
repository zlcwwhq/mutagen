package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/platform"
)

const (
	// BaseName is the base name for agent executables (sans any
	// platform-specific suffix like ".exe").
	BaseName = "mutagen-agent"
	// RemoteBaseName is the base name for remote command executables (sans
	// any platform-specific suffix like ".exe").
	RemoteBaseName = "remote"
)

// installPath computes and creates the parent directories of the path where the
// current executable should be installed if it is an agent binary with the
// current Mutagen version.
func installPath() (string, error) {
	// Compute (and create) the path to the agent parent directory.
	parent, err := filesystem.Mutagen(true, filesystem.MutagenAgentsDirectoryName, mutagen.Version)
	if err != nil {
		return "", fmt.Errorf("unable to compute parent directory: %w", err)
	}

	// Compute the target executable name.
	executableName := platform.ExecutableName(BaseName, runtime.GOOS)

	// Compute the installation path.
	return filepath.Join(parent, executableName), nil
}

// IsRemoteExecutable determines whether or not an executable path (typically
// the first element of the process' argument vector) names the remote command
// executable, which agents act as when invoked by that name.
func IsRemoteExecutable(path string) bool {
	name := filepath.Base(path)
	if runtime.GOOS == "windows" {
		if extension := filepath.Ext(name); strings.EqualFold(extension, ".exe") {
			name = strings.TrimSuffix(name, extension)
		}
		return strings.EqualFold(name, RemoteBaseName)
	}
	return name == RemoteBaseName
}

// InstallRemoteExecutable installs the remote command executable alongside the
// running agent executable, unless it's already present, and returns its path.
// The remote executable is a link to the agent executable rather than a
// separate executable, so that agent bundles don't need to carry both. On POSIX
// systems, it's a relative symbolic link. On Windows, where symbolic links
// typically require elevated privileges, it's a hard link, falling back to a
// copy if the filesystem doesn't support hard links.
func InstallRemoteExecutable() (string, error) {
	// Locate the running agent executable. Its location (rather than the
	// location computed from the Mutagen data directory, which may have changed
	// since installation) determines where the link is installed and what it
	// refers to.
	agentPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("unable to determine agent executable path: %w", err)
	} else if agentPath, err = filepath.EvalSymlinks(agentPath); err != nil {
		return "", fmt.Errorf("unable to resolve agent executable path: %w", err)
	}
	return installRemoteExecutable(agentPath)
}

// installRemoteExecutable implements InstallRemoteExecutable for the agent
// executable at the specified path.
func installRemoteExecutable(agentPath string) (string, error) {
	// Compute the remote executable path.
	path := filepath.Join(filepath.Dir(agentPath), platform.ExecutableName(RemoteBaseName, runtime.GOOS))

	// If a usable remote executable already exists, then there's nothing to do.
	// If it's a dangling symbolic link (e.g. one created by an earlier version),
	// then remove it so that it can be replaced.
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("unable to probe remote executable: %w", err)
	} else if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("unable to remove dangling remote executable: %w", err)
		}
	}

	// Create the link. A concurrent executor may install the remote executable
	// first (and be running it), in which case its installation is used. In
	// either case, verify that the result refers to an existing executable.
	var err error
	if runtime.GOOS != "windows" {
		err = os.Symlink(filepath.Base(agentPath), path)
	} else if err = os.Link(agentPath, path); err != nil && !os.IsExist(err) {
		err = copyExecutable(agentPath, path)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if err == nil {
			os.Remove(path)
			err = statErr
		}
		return "", fmt.Errorf("unable to install remote executable: %w", err)
	}
	return path, nil
}

// copyExecutable copies an executable, staging the copy beside its destination
// so that it can be renamed into place atomically.
func copyExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.CreateTemp(filepath.Dir(destination), ".remote-*")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	} else if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(output.Name(), destination)
}
