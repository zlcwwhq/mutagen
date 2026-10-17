package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/platform"
)

// TestInstallPath tests that the installPath method functions correctly. This
// has on-disk side-effects (namely creating the agents directory and the
// install directory for this version of Mutagen), but they should be harmless.
func TestInstallPath(t *testing.T) {
	// Verify that installPath succeeds.
	if p, err := installPath(); err != nil {
		t.Fatal("unable to compute/create install path:", err)
	} else if p == "" {
		t.Error("empty install path returned")
	} else if !strings.Contains(p, mutagen.Version) {
		t.Error("install path does not contain Mutagen version")
	}
}

// TestIsRemoteExecutable tests that IsRemoteExecutable recognizes the remote
// executable name.
func TestIsRemoteExecutable(t *testing.T) {
	// Define test cases.
	testCases := []struct {
		path     string
		expected bool
	}{
		{"remote", true},
		{filepath.Join("agents", "remote"), true},
		{"mutagen-agent", false},
		{"remote-other", false},
		{"remote.exe", runtime.GOOS == "windows"},
		{"REMOTE.EXE", runtime.GOOS == "windows"},
	}

	// Process test cases.
	for _, testCase := range testCases {
		if actual := IsRemoteExecutable(testCase.path); actual != testCase.expected {
			t.Errorf("incorrect result for %s: %t != %t", testCase.path, actual, testCase.expected)
		}
	}
}

// TestInstallRemoteExecutable tests that installRemoteExecutable installs the
// remote executable as a link to the agent, that repeated installation
// succeeds, and that dangling links are replaced.
func TestInstallRemoteExecutable(t *testing.T) {
	// Create a placeholder agent.
	directory := t.TempDir()
	agentPath := filepath.Join(directory, platform.ExecutableName(BaseName, runtime.GOOS))
	if err := os.WriteFile(agentPath, []byte("agent"), 0700); err != nil {
		t.Fatal("unable to create placeholder agent:", err)
	}

	// Create a dangling link where the remote executable is installed, as left
	// by an installation that referred to a different agent location.
	if runtime.GOOS != "windows" {
		if err := os.Symlink("missing-agent", filepath.Join(directory, RemoteBaseName)); err != nil {
			t.Fatal("unable to create dangling link:", err)
		}
	}

	// Install the remote executable twice and verify that it refers to the
	// agent.
	for i := 0; i < 2; i++ {
		path, err := installRemoteExecutable(agentPath)
		if err != nil {
			t.Fatal("unable to install remote executable:", err)
		} else if filepath.Dir(path) != directory || !IsRemoteExecutable(path) {
			t.Fatal("incorrect remote executable path:", path)
		}
		agentInfo, err := os.Stat(agentPath)
		if err != nil {
			t.Fatal("unable to query agent:", err)
		}
		remoteInfo, err := os.Stat(path)
		if err != nil {
			t.Fatal("unable to query remote executable:", err)
		} else if !os.SameFile(agentInfo, remoteInfo) {
			t.Fatal("remote executable does not refer to agent")
		}
	}
}

// TestInstallRemoteExecutableMissingAgent tests that installRemoteExecutable
// fails, rather than creating a dangling link, if the agent doesn't exist.
func TestInstallRemoteExecutableMissingAgent(t *testing.T) {
	directory := t.TempDir()
	agentPath := filepath.Join(directory, platform.ExecutableName(BaseName, runtime.GOOS))
	if _, err := installRemoteExecutable(agentPath); err == nil {
		t.Fatal("installation succeeded without agent")
	} else if entries, err := os.ReadDir(directory); err != nil {
		t.Fatal("unable to read directory:", err)
	} else if len(entries) != 0 {
		t.Fatal("installation left files behind:", entries[0].Name())
	}
}
