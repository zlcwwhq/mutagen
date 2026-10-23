package agent

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/mutagen"
)

// TestVerifyBundleVersion tests that verifyBundleVersion only accepts archives
// whose first entry is a version entry with the current Mutagen version.
func TestVerifyBundleVersion(t *testing.T) {
	// Define test cases.
	type entry struct {
		name     string
		contents string
	}
	testCases := []struct {
		description string
		entries     []entry
		expected    string
	}{
		{"current version", []entry{{BundleVersionEntryName, mutagen.Version}, {"linux_amd64", "agent"}}, ""},
		{"other version", []entry{{BundleVersionEntryName, "0.19.0-other"}}, "is from Mutagen 0.19.0-other"},
		{"version with newline", []entry{{BundleVersionEntryName, mutagen.Version + "\n"}}, "is from Mutagen"},
		{"no version entry", []entry{{"linux_amd64", "agent"}}, "doesn't record its version"},
		{"version entry not first", []entry{{"linux_amd64", "agent"}, {BundleVersionEntryName, mutagen.Version}}, "doesn't record its version"},
		{"oversized version entry", []entry{{BundleVersionEntryName, strings.Repeat("0", maximumBundleVersionEntrySize+1)}}, "too large"},
		{"empty archive", nil, "is empty"},
	}

	// Process test cases.
	for _, testCase := range testCases {
		// Create the archive.
		buffer := &bytes.Buffer{}
		archiver := tar.NewWriter(buffer)
		for _, e := range testCase.entries {
			header := &tar.Header{Name: e.name, Mode: 0644, Size: int64(len(e.contents))}
			if err := archiver.WriteHeader(header); err != nil {
				t.Fatalf("%s: unable to write archive header: %v", testCase.description, err)
			} else if _, err := archiver.Write([]byte(e.contents)); err != nil {
				t.Fatalf("%s: unable to write archive entry: %v", testCase.description, err)
			}
		}
		if err := archiver.Close(); err != nil {
			t.Fatalf("%s: unable to close archive: %v", testCase.description, err)
		}

		// Perform verification and check the result.
		err := verifyBundleVersion(tar.NewReader(buffer))
		if testCase.expected == "" {
			if err != nil {
				t.Errorf("%s: verification failed: %v", testCase.description, err)
			}
		} else if err == nil {
			t.Errorf("%s: verification succeeded unexpectedly", testCase.description)
		} else if !strings.Contains(err.Error(), testCase.expected) {
			t.Errorf("%s: error doesn't contain %q: %v", testCase.description, testCase.expected, err)
		}
	}
}

// TestExecutableForInvalidOS tests that ExecutableForPlatform fails for an
// invalid OS specification.
func TestExecutableForInvalidOS(t *testing.T) {
	if _, err := ExecutableForPlatform("fakeos", runtime.GOARCH, ""); err == nil {
		t.Fatal("extracting agent executable succeeded for invalid OS")
	}
}

// TestExecutableForInvalidArchitecture tests that ExecutableForPlatform fails
// for an invalid architecture specification.
func TestExecutableForInvalidArchitecture(t *testing.T) {
	if _, err := ExecutableForPlatform(runtime.GOOS, "fakearch", ""); err == nil {
		t.Fatal("extracting agent executable succeeded for invalid architecture")
	}
}

// TestExecutableForInvalidPair tests that ExecutableForPlatform fails for an
// invalid OS/architecture specification.
func TestExecutableForInvalidPair(t *testing.T) {
	if _, err := ExecutableForPlatform("fakeos", "fakearch", ""); err == nil {
		t.Fatal("extracting agent executable succeeded for invalid architecture")
	}
}

// TestExecutableForPlatform tests that ExecutableForPlatform succeeds for the
// current OS/architecture.
func TestExecutableForPlatform(t *testing.T) {
	if executable, err := ExecutableForPlatform(runtime.GOOS, runtime.GOARCH, ""); err != nil {
		t.Fatal("unable to extract agent bundle for current platform:", err)
	} else if err = os.Remove(executable); err != nil {
		t.Error("unable to remove agent executable:", err)
	}
}

// TestExecutableForPlatformWithOutputPath tests that ExecutableForPlatform
// functions correctly when an output path is specified.
func TestExecutableForPlatformWithOutputPath(t *testing.T) {
	// Compute the output path.
	outputPath := filepath.Join(t.TempDir(), "agent_output")

	// Perform executable extraction.
	executable, err := ExecutableForPlatform(runtime.GOOS, runtime.GOARCH, outputPath)
	if err != nil {
		t.Fatal("unable to extract agent bundle for current platform:", err)
	}

	// Verify the output path.
	if executable != outputPath {
		t.Error("executable output path does not match expected:", executable, "!=", outputPath)
	}

	// Remove the file.
	if err = os.Remove(executable); err != nil {
		t.Error("unable to remove agent executable:", err)
	}
}
