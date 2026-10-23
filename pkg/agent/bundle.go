package agent

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/klauspost/compress/gzip"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/platform"
)

const (
	// BundleName is the base name of the agent bundle.
	BundleName = "mutagen-agents.tar.gz"
	// BundleVersionEntryName is the name of the agent bundle's first entry,
	// which contains the Mutagen version of the bundle's agents.
	BundleVersionEntryName = "version"
	// maximumBundleVersionEntrySize is the maximum size of the agent bundle's
	// version entry.
	maximumBundleVersionEntrySize = 256
)

// BundleLocation encodes an expected location for the agent bundle.
type BundleLocation uint8

const (
	// BundleLocationDefault indicates that the ExecutableForPlatform function
	// should expect to find the agent bundle in the same directory as the
	// current executable or (if the current executable resides in the "bin"
	// directory of a Filesystem Hierarchy Standard layout) in the libexec
	// directory.
	BundleLocationDefault BundleLocation = iota
	// BundleLocationBuildDirectory indicates that the ExecutableForPlatform
	// function should expect to find the agent bundle in the Mutagen build
	// directory. This mode is only used during integration testing. It is
	// required because test executables will be built in temporary directories.
	BundleLocationBuildDirectory
)

// ExpectedBundleLocation specifies the expected agent bundle location. It is
// set by the time that init functions have completed. After that, it should
// only be set at the process entry point, before any code calls into the agent
// package.
var ExpectedBundleLocation BundleLocation

// verifyBundleVersion reads the first entry of an agent bundle archive, which
// must be the bundle's version entry, and verifies that the version that it
// contains is the current Mutagen version. Bundles of earlier releases don't
// have a version entry.
func verifyBundleVersion(bundleArchive *tar.Reader) error {
	// Read the first header and ensure that it's for the version entry.
	header, err := bundleArchive.Next()
	if err == io.EOF {
		return errors.New("bundle is empty")
	} else if err != nil {
		return fmt.Errorf("unable to read archive header: %w", err)
	} else if header.Name != BundleVersionEntryName {
		return fmt.Errorf("bundle doesn't record its version and thus isn't from Mutagen %s", mutagen.Version)
	} else if header.Size > maximumBundleVersionEntrySize {
		return errors.New("version entry too large")
	}

	// Read the version and compare it with ours.
	version, err := io.ReadAll(bundleArchive)
	if err != nil {
		return fmt.Errorf("unable to read version entry: %w", err)
	} else if string(version) != mutagen.Version {
		return fmt.Errorf("bundle is from Mutagen %s, not %s", version, mutagen.Version)
	}

	// Success.
	return nil
}

// ExecutableForPlatform attempts to locate the agent bundle, verify that it's
// from the current Mutagen version, and extract an agent executable for the
// specified target platform. If no output path is specified, then the extracted
// file will be in a temporary location accessible to only the user, will have
// an appropriate extension for the target platform, and will have the
// executability bit set if it makes sense. The path to the extracted file will
// be returned, and the caller is responsible for cleaning up the file if this
// function returns a nil error.
func ExecutableForPlatform(goos, goarch, outputPath string) (string, error) {
	// Compute the path to the location in which we expect to find the agent
	// bundle.
	var bundleSearchPaths []string
	if ExpectedBundleLocation == BundleLocationDefault {
		// Add the executable directory as a search path.
		if executablePath, err := os.Executable(); err != nil {
			return "", fmt.Errorf("unable to determine executable path: %w", err)
		} else {
			bundleSearchPaths = append(bundleSearchPaths, filepath.Dir(executablePath))
		}

		// If the executable is in what appears to be a Filesystem Hierarchy
		// Standard layout, then add the libexec directory as a search path.
		if libexecPath, err := filesystem.LibexecPath(); err == nil {
			bundleSearchPaths = append(bundleSearchPaths, libexecPath)
		}
	} else if ExpectedBundleLocation == BundleLocationBuildDirectory {
		if sourceTreePath, err := mutagen.SourceTreePath(); err != nil {
			return "", fmt.Errorf("unable to determine Mutagen source tree path: %w", err)
		} else {
			bundleSearchPaths = append(bundleSearchPaths, filepath.Join(sourceTreePath, mutagen.BuildDirectoryName))
		}
	} else {
		panic("invalid bundle location specification")
	}

	// Loop until we find a bundle file. If we fail to locate a bundle, then
	// abort. If we succeed, then defer its closure.
	var bundle *os.File
	var bundlePath string
	for _, path := range bundleSearchPaths {
		candidatePath := filepath.Join(path, BundleName)
		if file, err := os.Open(candidatePath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("unable to open agent bundle (%s): %w", candidatePath, err)
		} else if metadata, err := file.Stat(); err != nil {
			file.Close()
			return "", fmt.Errorf("unable to access agent bundle (%s) file metadata: %w", candidatePath, err)
		} else if metadata.Mode()&os.ModeType != 0 {
			file.Close()
			return "", fmt.Errorf("agent bundle (%s) is not a file", candidatePath)
		} else {
			bundle = file
			bundlePath = candidatePath
			defer bundle.Close()
		}
	}
	if bundle == nil {
		return "", fmt.Errorf("unable to locate agent bundle (search paths: %v)", bundleSearchPaths)
	}

	// Create a decompressor and defer its closure.
	bundleDecompressor, err := gzip.NewReader(bundle)
	if err != nil {
		return "", fmt.Errorf("unable to decompress agent bundle: %w", err)
	}
	defer bundleDecompressor.Close()

	// Create an archive reader.
	bundleArchive := tar.NewReader(bundleDecompressor)

	// Verify that the bundle's agents are of this version. The version
	// handshake only covers the major, minor, and patch components, which
	// releases of this fork share, and endpoints keep an installed agent under
	// this version's path, so an agent from another release's bundle would be
	// used without any error. Since the agent bundle is released separately
	// from the CLI, a bundle from an earlier installation is easily left in
	// place when upgrading.
	if err := verifyBundleVersion(bundleArchive); err != nil {
		return "", fmt.Errorf("unusable agent bundle (%s): %w", bundlePath, err)
	}

	// Scan until we find a matching header.
	var header *tar.Header
	for {
		if h, err := bundleArchive.Next(); err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("unable to read archive header: %w", err)
		} else if h.Name == fmt.Sprintf("%s_%s", goos, goarch) {
			header = h
			break
		}
	}

	// Check if we have a valid header. If not, there was no match.
	if header == nil {
		return "", errors.New("unsupported platform")
	}

	// If an output path has been specified, then open the path for writing,
	// otherwise create a temporary file.
	var file *os.File
	if outputPath != "" {
		file, err = os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	} else {
		file, err = os.CreateTemp("", platform.ExecutableName(BaseName+".*", goos))
	}
	if err != nil {
		return "", fmt.Errorf("unable to create output file: %w", err)
	}

	// Copy data into the file.
	if _, err := io.CopyN(file, bundleArchive, header.Size); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", fmt.Errorf("unable to copy agent data: %w", err)
	}

	// If we're not on Windows and our target system is not Windows, mark the
	// file as executable. This will save us an additional "chmod +x" command
	// during agent installation. Note that the mechanism we use here
	// (os.File.Chmod) does not work on Windows (only the path-based os.Chmod is
	// supported there), but this is fine because this code wouldn't make sense
	// to use on Windows in any scenario (where executability bits don't exist).
	if runtime.GOOS != "windows" && goos != "windows" {
		if err := file.Chmod(0700); err != nil {
			file.Close()
			os.Remove(file.Name())
			return "", fmt.Errorf("unable to make agent executable: %w", err)
		}
	}

	// Close the file.
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", fmt.Errorf("unable to close temporary file: %w", err)
	}

	// Success.
	return file.Name(), nil
}
