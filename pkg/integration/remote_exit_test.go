package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

func TestRemoteExitInputStatus(t *testing.T) {
	t.Setenv(remote.SessionVariable, "")
	const unset = "MUTAGEN_REMOTE_EXIT_TEST_UNSET"
	t.Setenv(unset, "")
	if err := os.Unsetenv(unset); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing")
	for _, test := range []struct {
		name      string
		arguments []string
		code      int
		message   string
	}{
		{"unknown-option", []string{"--unknown-remote-option"}, 125, "unknown"},
		{"invalid-combination", []string{"--flush", "--no-flush"}, 125, "mutually exclusive"},
		{"unset-environment", []string{"-e", unset, "true"}, 125, "not set"},
		{"missing-environment-file", []string{"--env-file", missing, "true"}, 125, "environment file"},
		{"missing-script", []string{"--script", missing}, 125, "script"},
		{"invalid-selection", []string{"--no-flush", "-s", "mutagen-no-such-exit-test-session", "true"}, 125, "deprecated"},
		{"unreachable-session", []string{"--no-flush", "true"}, 255, "no connected node"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, output, errorOutput := runRemote(t, directory, test.arguments...)
			if code != test.code || output != "" || !strings.Contains(errorOutput, test.message) {
				t.Fatalf("incorrect input failure: code %d (want %d), stdout %q, stderr %q", code, test.code, output, errorOutput)
			}
		})
	}
}

func TestRemoteExitOperationStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX commands")
	}
	t.Setenv(remote.SessionVariable, "")
	for _, alphaExecutes := range []bool{false, true} {
		t.Run(fmt.Sprintf("alpha-executes-%t", alphaExecutes), func(t *testing.T) {
			alpha, beta := t.TempDir(), t.TempDir()
			createRemoteSession(t, "exit-status", alpha, beta, alphaExecutes, "missing", "absent")
			submitting, executing := alpha, beta
			if alphaExecutes {
				submitting, executing = beta, alpha
			}
			missingDirectory := filepath.Join(executing, "missing-directory")
			for _, test := range []struct {
				name      string
				arguments []string
				code      int
				message   string
			}{
				{"missing-pull", []string{"--pull", "missing"}, 125, "doesn't exist on the opposite endpoint"},
				{"multiple-missing-pulls", []string{"--pull", "missing", "absent"}, 125, "unable to pull absent"},
				{"no-conflict", []string{"--conflict-checkout", "missing-conflict"}, 125, "conflict"},
				{"missing-cwd", []string{"--no-flush", "-C", missingDirectory, "true"}, 126, missingDirectory},
				{"invalid-cwd", []string{"--no-flush", "-C", "~mutagen-exit-test-no-such-user-0714", "true"}, 126, "invalid working directory"},
				{"missing-command", []string{"--no-flush", "mutagen-exit-test-no-such-command-0714"}, 127, "unable to find command"},
			} {
				t.Run(test.name, func(t *testing.T) {
					code, _, errorOutput := runRemote(t, submitting, test.arguments...)
					if code != test.code || !strings.Contains(errorOutput, test.message) {
						t.Fatalf("incorrect operation failure: code %d (want %d), stderr %q", code, test.code, errorOutput)
					}
				})
			}
		})
	}
}

// exitStatusRoots creates a session with both command directions available.
// Preparing both routes once keeps individual status assertions independent of
// the asynchronous connection of command channels.
func exitStatusRoots(t *testing.T) [2]string {
	t.Helper()
	roots := createConflictSessionWithCommands(t, synchronization.RemoteCommandMode_RemoteCommandModeBoth,
		core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	if _, err := flushRemote(roots[0]); err != nil {
		t.Fatal("unable to synchronize exit status session:", err)
	}
	for _, root := range roots {
		if _, code, err := runRemoteCommand(root, "true"); err != nil || code != 0 {
			t.Fatalf("unable to prepare command route: %d, %v", code, err)
		}
	}
	return roots
}

func TestRemoteExitCommandStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Setenv(remote.SessionVariable, "")
	roots := exitStatusRoots(t)
	for index, root := range roots {
		t.Run(fmt.Sprintf("submit-%d", index), func(t *testing.T) {
			// These values overlap remote's own failures. A completed command
			// always owns its exit status, including the common test-tool codes.
			for _, expected := range []int{1, 2, 124, 125, 126, 127, 255} {
				t.Run(strconv.Itoa(expected), func(t *testing.T) {
					code, _, errorOutput := runRemote(t, root, "--no-flush", "sh", "-c", "exit "+strconv.Itoa(expected))
					if code != expected || errorOutput != "" {
						t.Fatalf("command status changed: %d (want %d), stderr %q", code, expected, errorOutput)
					}
				})
			}
		})
	}
}

func TestRemoteExitJobStatus(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "aix" {
		t.Skip("detached jobs require POSIX shells and flock")
	}
	t.Setenv(remote.SessionVariable, "")
	roots := exitStatusRoots(t)
	for index, root := range roots {
		t.Run(fmt.Sprintf("submit-%d", index), func(t *testing.T) {
			for _, expected := range []int{1, 2, 124, 125, 126, 127, 255} {
				t.Run(strconv.Itoa(expected), func(t *testing.T) {
					code, output, errorOutput := runRemote(t, root, "--no-flush", "--detach", "sh", "-c", "exit "+strconv.Itoa(expected))
					id := strings.TrimSpace(output)
					if code != 0 || len(id) != 8 {
						t.Fatalf("unable to start fast job: %d, %q, %q", code, output, errorOutput)
					}
					code, _, errorOutput = runRemote(t, root, "--no-flush", "--timeout", "5s", "--wait", id)
					if code != expected || !strings.Contains(errorOutput, "exited with status "+strconv.Itoa(expected)) {
						t.Fatalf("job status changed: %d (want %d), stderr %q", code, expected, errorOutput)
					}

					if expected != 255 {
						return
					}
					// A completed job remains authoritative when its log can no
					// longer be read. Losing the recorded exit status instead
					// makes the job lost and reports remote's own failure code.
					directory, err := filesystem.Mutagen(false, "remote-jobs", id)
					if err != nil {
						t.Fatal(err)
					} else if err := os.Remove(filepath.Join(directory, "log")); err != nil {
						t.Fatal(err)
					}
					code, _, errorOutput = runRemote(t, root, "--no-flush", "--timeout", "5s", "--wait", id, "--tail", "1")
					if code != 255 || !strings.Contains(errorOutput, "warning") || !strings.Contains(errorOutput, "exited with status 255") {
						t.Fatalf("log warning replaced the job status: %d, %q", code, errorOutput)
					} else if err := os.Remove(filepath.Join(directory, "status")); err != nil {
						t.Fatal(err)
					}
					code, _, errorOutput = runRemote(t, root, "--no-flush", "--timeout", "5s", "--wait", id)
					if code != 125 || !strings.Contains(errorOutput, "without recording its exit status") {
						t.Fatalf("lost job did not report an operation failure: %d, %q", code, errorOutput)
					}
				})
			}
		})
	}
}
