package integration

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/cmd/remote/cli"
	"github.com/mutagen-io/mutagen/pkg/integration/protocols/netpipe"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// runRemoteCommand submits a command from the specified directory, retrying
// while command channels are still connecting. As with the remote executable,
// the executing endpoint's prologue is requested when the directory lies inside
// the submitting root.
func runRemoteCommand(directory string, arguments ...string) (string, int, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		connection, target, err := remote.Dial(ctx, directory, "", remote.OperationExecute)
		var output bytes.Buffer
		var code int
		if err == nil {
			request := &remote.Request{Arguments: arguments, Path: target.Path, Prologue: target.Contained}
			code, err = remote.Execute(ctx, connection, request, strings.NewReader(""), &output, io.Discard)
		}
		cancel()
		if err == nil || time.Now().After(deadline) {
			return strings.TrimSpace(output.String()), code, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// flushRemote forces a synchronization cycle through the session containing
// the specified directory, as the remote executable does around commands,
// retrying while command channels are still connecting. It returns the
// synchronization report.
func flushRemote(directory string) (*remote.Report, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		connection, _, err := remote.Dial(ctx, directory, "", remote.OperationFlush)
		var report *remote.Report
		if err == nil {
			report, err = remote.Flush(ctx, connection)
		}
		cancel()
		if err == nil || time.Now().After(deadline) {
			return report, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runRemote runs the remote command line in the specified directory.
func runRemote(t *testing.T, directory string, arguments ...string) (int, string, string) {
	t.Helper()
	t.Chdir(directory)
	var output, errorOutput strings.Builder
	code := cli.Run(arguments, strings.NewReader(""), &output, &errorOutput)
	return code, output.String(), errorOutput.String()
}

func TestRemoteCommandsInMemory(t *testing.T) {
	// This test relies on a POSIX shell.
	if runtime.GOOS == "windows" {
		t.Skip()
	}

	// Create an alpha root with content to synchronize. Resolve symbolic links
	// so that working directories can be compared with physical paths.
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alphaRoot := filepath.Join(directory, "alpha")
	betaRoot := filepath.Join(directory, "beta")
	if err := os.MkdirAll(filepath.Join(alphaRoot, "sub"), 0700); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(alphaRoot, "sub", "file"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}

	// Create endpoint-specific prologues: beta's is synchronized from alpha and
	// specified relative to its root, while alpha's lies outside its root.
	if err := os.WriteFile(filepath.Join(alphaRoot, "prologue.sh"), []byte("export REMOTE_PROLOGUE=beta\n"), 0600); err != nil {
		t.Fatal(err)
	}
	alphaPrologue := filepath.Join(directory, "alpha-prologue.sh")
	if err := os.WriteFile(alphaPrologue, []byte("export REMOTE_PROLOGUE=alpha\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Create a session with an in-memory beta (which uses an agent-style
	// executor) and remote commands enabled in both directions. Watching is
	// disabled, so that only flushes trigger synchronization.
	ctx := context.Background()
	sessionID, err := synchronizationManager.Create(
		ctx,
		&url.URL{Path: alphaRoot},
		&url.URL{Protocol: netpipe.Protocol_Netpipe, Path: betaRoot},
		&synchronization.Configuration{
			WatchMode:         synchronization.WatchMode_WatchModeNoWatch,
			RemoteCommandMode: synchronization.RemoteCommandMode_RemoteCommandModeBoth,
		},
		&synchronization.Configuration{RemotePrologue: alphaPrologue},
		&synchronization.Configuration{RemotePrologue: "prologue.sh"},
		"remoteCommands",
		nil,
		false,
		"",
	)
	if err != nil {
		t.Fatal("unable to create session:", err)
	}
	defer synchronizationManager.Terminate(ctx, &selection.Selection{Specifications: []string{sessionID}}, "")
	if _, err := flushRemote(alphaRoot); err != nil {
		t.Fatal("unable to perform initial synchronization:", err)
	}

	// Commands submitted on either endpoint run in the corresponding directory
	// of the opposite endpoint, after sourcing that endpoint's prologue.
	for _, test := range []struct{ from, to, prologue string }{
		{filepath.Join(alphaRoot, "sub"), filepath.Join(betaRoot, "sub"), "beta"},
		{filepath.Join(betaRoot, "sub"), filepath.Join(alphaRoot, "sub"), "alpha"},
	} {
		output, code, err := runRemoteCommand(test.from, "sh", "-c", `pwd -P; printf %s "$REMOTE_PROLOGUE"`)
		if err != nil || code != 0 {
			t.Fatalf("command from %s failed: %d, %v", test.from, code, err)
		} else if expected := test.to + "\n" + test.prologue; output != expected {
			t.Fatalf("command from %s produced %q instead of %q", test.from, output, expected)
		}
	}

	// Exit codes are propagated.
	if _, code, err := runRemoteCommand(alphaRoot, "sh", "-c", "exit 7"); err != nil || code != 7 {
		t.Fatalf("incorrect exit status: %d, %v", code, err)
	}

	// Flushes submitted from either endpoint synchronize changes in both
	// directions before returning.
	if err := os.WriteFile(filepath.Join(alphaRoot, "sub", "edited"), []byte("edited"), 0600); err != nil {
		t.Fatal(err)
	} else if _, err := flushRemote(alphaRoot); err != nil {
		t.Fatal("unable to flush from alpha:", err)
	} else if output, code, err := runRemoteCommand(filepath.Join(alphaRoot, "sub"), "cat", "edited"); err != nil || code != 0 || output != "edited" {
		t.Fatalf("flushed change not visible on beta: %d, %q, %v", code, output, err)
	} else if _, code, err := runRemoteCommand(filepath.Join(alphaRoot, "sub"), "sh", "-c", "printf produced > produced"); err != nil || code != 0 {
		t.Fatalf("unable to produce file on beta: %d, %v", code, err)
	} else if _, err := flushRemote(filepath.Join(betaRoot, "sub")); err != nil {
		t.Fatal("unable to flush from beta:", err)
	} else if data, err := os.ReadFile(filepath.Join(alphaRoot, "sub", "produced")); err != nil || string(data) != "produced" {
		t.Fatalf("flushed change not present on alpha: %q, %v", data, err)
	}

	// Modifying a synchronized file on both endpoints creates a conflict, which
	// flushes report.
	alphaShared, betaShared := filepath.Join(alphaRoot, "sub", "shared"), filepath.Join(betaRoot, "sub", "shared")
	if err := os.WriteFile(alphaShared, []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	} else if report, err := flushRemote(alphaRoot); err != nil || !report.Empty() {
		t.Fatalf("unable to synchronize shared file: %+v, %v", report, err)
	} else if err := os.WriteFile(alphaShared, []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(betaShared, []byte("remote\n"), 0600); err != nil {
		t.Fatal(err)
	}
	expectedConflict := remote.Conflict{
		Root:  "sub/shared",
		Alpha: []remote.Change{{Path: "sub/shared", Old: "file", New: "file"}},
		Beta:  []remote.Change{{Path: "sub/shared", Old: "file", New: "file"}},
	}
	if report, err := flushRemote(alphaRoot); err != nil || len(report.Conflicts) != 1 || !reflect.DeepEqual(report.Conflicts[0], expectedConflict) {
		t.Fatalf("conflict not reported: %+v, %v", report, err)
	}

	// The remote executable warns about the conflict once per invocation and
	// shows it as a diff from the local copy to the remote one.
	warning := "remote:   shared: modified file locally, modified file remotely\n"
	if code, _, errorOutput := runRemote(t, filepath.Join(alphaRoot, "sub"), "true"); code != 0 || strings.Count(errorOutput, warning) != 1 {
		t.Fatalf("conflict not reported by command: %d, %q", code, errorOutput)
	} else if code, output, errorOutput := runRemote(t, filepath.Join(alphaRoot, "sub"), "--diff"); code != 1 ||
		output != "--- shared (local)\n+++ shared (remote)\n@@ -1 +1 @@\n-local\n+remote\n" || !strings.Contains(errorOutput, warning) {
		t.Fatalf("conflict not shown as diff: %d, %q, %q", code, output, errorOutput)
	} else if code, output, errorOutput := runRemote(t, filepath.Join(betaRoot, "sub"), "--diff", "shared", "file"); code != 1 ||
		output != "--- shared (local)\n+++ shared (remote)\n@@ -1 +1 @@\n-remote\n+local\n" {
		t.Fatalf("paths not compared from beta: %d, %q, %q", code, output, errorOutput)
	} else if code, _, errorOutput := runRemote(t, filepath.Join(alphaRoot, "sub"), "--diff", "missing"); code != 255 ||
		!strings.Contains(errorOutput, "missing doesn't exist locally or remotely") {
		t.Fatalf("nonexistent path not reported: %d, %q", code, errorOutput)
	}

	// Deleting one copy resolves the conflict in favor of the other.
	if err := os.Remove(betaShared); err != nil {
		t.Fatal(err)
	} else if code, output, errorOutput := runRemote(t, filepath.Join(alphaRoot, "sub"), "--diff"); code != 0 || output != "" ||
		errorOutput != "remote: there are no conflicts to compare\n" {
		t.Fatalf("conflict not resolved: %d, %q, %q", code, output, errorOutput)
	} else if data, err := os.ReadFile(betaShared); err != nil || string(data) != "local\n" {
		t.Fatalf("remaining copy not synchronized: %q, %v", data, err)
	}

	// Terminating the session removes its registrations.
	if err := synchronizationManager.Terminate(ctx, &selection.Selection{Specifications: []string{sessionID}}, ""); err != nil {
		t.Fatal("unable to terminate session:", err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := remote.Dial(dialCtx, alphaRoot, "", remote.OperationExecute); err == nil {
		t.Fatal("registration remained after termination")
	}
}

// createRemoteDiffSession creates a session with only alpha submitting, so it
// can also be selected from outside the roots when synchronizing single files.
func createRemoteDiffSession(t *testing.T, alphaRoot, betaRoot string) string {
	t.Helper()
	ctx := context.Background()
	sessionID, err := synchronizationManager.Create(ctx,
		&url.URL{Path: alphaRoot},
		&url.URL{Protocol: netpipe.Protocol_Netpipe, Path: betaRoot},
		&synchronization.Configuration{
			WatchMode:         synchronization.WatchMode_WatchModeNoWatch,
			RemoteCommandMode: synchronization.RemoteCommandMode_RemoteCommandModeBeta,
		},
		&synchronization.Configuration{}, &synchronization.Configuration{},
		"remoteDiff", nil, false, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := synchronizationManager.Terminate(ctx, &selection.Selection{Specifications: []string{sessionID}}, ""); err != nil {
			t.Error("unable to terminate session:", err)
		}
	})
	if _, err := flushRemote(alphaRoot); err != nil {
		t.Fatal("unable to synchronize:", err)
	}

	// The executor channel connects independently of synchronization. Wait
	// until it can serve entries before exercising the command line.
	readyContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		connection, _, err := remote.Dial(readyContext, alphaRoot, sessionID, remote.OperationExecute)
		if err == nil {
			_, err = remote.Fetch(readyContext, connection, "")
		}
		if err == nil {
			break
		} else if readyContext.Err() != nil {
			t.Fatal("executor didn't connect:", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return sessionID
}

func TestRemoteDiffFileRoot(t *testing.T) {
	directory := t.TempDir()
	alphaRoot, betaRoot := filepath.Join(directory, "alpha"), filepath.Join(directory, "beta")
	if err := os.WriteFile(alphaRoot, []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sessionID := createRemoteDiffSession(t, alphaRoot, betaRoot)
	if err := os.WriteFile(alphaRoot, []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(betaRoot, []byte("remote\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Both automatic conflict comparison and an explicit root path must
	// compare the file, even though submission occurs outside the root.
	for _, paths := range [][]string{nil, {alphaRoot}} {
		arguments := append([]string{"--timeout", "5s", "--session", sessionID, "--diff"}, paths...)
		code, output, errorOutput := runRemote(t, directory, arguments...)
		if code != 1 || !strings.Contains(output, "-local\n+remote\n") {
			t.Fatalf("root file wasn't compared: %d, %q, %q", code, output, errorOutput)
		}
	}
}

func TestRemoteDiffTypeConflict(t *testing.T) {
	for _, replaceAlpha := range []bool{true, false} {
		name := "remote file"
		if replaceAlpha {
			name = "local file"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			alphaRoot, betaRoot := filepath.Join(directory, "alpha"), filepath.Join(directory, "beta")
			if err := os.MkdirAll(filepath.Join(alphaRoot, "d"), 0700); err != nil {
				t.Fatal(err)
			} else if err := os.WriteFile(filepath.Join(alphaRoot, "d", "f"), []byte("base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			createRemoteDiffSession(t, alphaRoot, betaRoot)
			replaced, modified := alphaRoot, betaRoot
			if !replaceAlpha {
				replaced, modified = betaRoot, alphaRoot
			}
			if err := os.RemoveAll(filepath.Join(replaced, "d")); err != nil {
				t.Fatal(err)
			} else if err := os.WriteFile(filepath.Join(replaced, "d"), []byte("replacement\n"), 0600); err != nil {
				t.Fatal(err)
			} else if err := os.WriteFile(filepath.Join(modified, "d", "f"), []byte("edited\n"), 0600); err != nil {
				t.Fatal(err)
			}
			code, output, errorOutput := runRemote(t, alphaRoot, "--timeout", "5s", "--diff")
			change := "-edited\n"
			if replaceAlpha {
				change = "+edited\n"
			}
			if code != 1 || !strings.Contains(output, "d: ") || !strings.Contains(output, change) {
				t.Fatalf("type conflict wasn't compared: %d, %q, %q", code, output, errorOutput)
			}
		})
	}
}
