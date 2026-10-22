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
	logSessionOnFailure(t, sessionID)
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

	// A workspace override synchronizes before execution, selects the target
	// endpoint's branch, and replaces both endpoint-specific defaults.
	override := filepath.Join(alphaRoot, ".remote-prologue.sh")
	script := "case \"$MUTAGEN_REMOTE_ENDPOINT\" in\nalpha) export REMOTE_PROLOGUE=workspace-alpha;;\nbeta) export REMOTE_PROLOGUE=workspace-beta;;\nesac\n"
	if err := os.WriteFile(override, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	environmentFile := filepath.Join(directory, "environment")
	if err := os.WriteFile(environmentFile, []byte("MUTAGEN_REMOTE_ENDPOINT=from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		from, expected string
		options        []string
	}{
		{filepath.Join(alphaRoot, "sub"), "beta:workspace-beta", nil},
		{betaRoot, "alpha:workspace-alpha", nil},
		{alphaRoot, "beta:workspace-beta", []string{"--cwd", directory}},
		{alphaRoot, "beta:skipped", []string{"--no-prologue", "-e", "REMOTE_PROLOGUE=skipped"}},
		{alphaRoot, "beta:workspace-beta", []string{"--env-file", environmentFile, "-e", "MUTAGEN_REMOTE_ENDPOINT=from-flag"}},
	} {
		arguments := append(test.options, "sh", "-c", `printf '%s:%s' "$MUTAGEN_REMOTE_ENDPOINT" "$REMOTE_PROLOGUE"`)
		if code, output, errorOutput := runRemote(t, test.from, arguments...); code != 0 || output != test.expected {
			t.Fatalf("workspace prologue from %s with %v failed: %d, %q, %q", test.from, test.options, code, output, errorOutput)
		}
	}
	// Removing the override restores the configured default after the next
	// automatic flush, without restarting the session or its command channels.
	if err := os.Remove(override); err != nil {
		t.Fatal(err)
	} else if code, output, errorOutput := runRemote(t, alphaRoot, "sh", "-c", `printf %s "$REMOTE_PROLOGUE"`); code != 0 || output != "beta" {
		t.Fatalf("configured prologue not restored: %d, %q, %q", code, output, errorOutput)
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
	return createRemoteSession(t, "remoteDiff", alphaRoot, betaRoot, false)
}

// createRemoteSession creates a session with the specified name from a local
// alpha to an in-memory beta, with watching disabled, so that only flushes
// synchronize. Only one endpoint submits commands, so that the session can
// also be selected from outside the roots: alpha, for beta's agent-style
// executor, unless alphaExecutes is true, in which case beta submits commands
// that alpha executes in the daemon process. It performs the initial
// synchronization through the session itself, from the submitting endpoint,
// even if another session's root contains its root, and waits until the
// session can execute commands. The session ignores the specified patterns, if
// any.
func createRemoteSession(t *testing.T, name, alphaRoot, betaRoot string, alphaExecutes bool, ignores ...string) string {
	t.Helper()
	mode, submitting := synchronization.RemoteCommandMode_RemoteCommandModeBeta, alphaRoot
	if alphaExecutes {
		mode, submitting = synchronization.RemoteCommandMode_RemoteCommandModeAlpha, betaRoot
	}
	ctx := context.Background()
	sessionID, err := synchronizationManager.Create(ctx,
		&url.URL{Path: alphaRoot},
		&url.URL{Protocol: netpipe.Protocol_Netpipe, Path: betaRoot},
		&synchronization.Configuration{
			WatchMode:         synchronization.WatchMode_WatchModeNoWatch,
			RemoteCommandMode: mode,
			Ignores:           ignores,
		},
		&synchronization.Configuration{}, &synchronization.Configuration{},
		name, nil, false, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := synchronizationManager.Terminate(ctx, &selection.Selection{Specifications: []string{sessionID}}, ""); err != nil {
			t.Error("unable to terminate session:", err)
		}
	})
	logSessionOnFailure(t, sessionID)
	readyContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		connection, _, err := remote.Dial(readyContext, submitting, sessionID, remote.OperationFlush)
		if err == nil {
			_, err = remote.Flush(readyContext, connection)
		}
		if err == nil {
			break
		} else if readyContext.Err() != nil {
			t.Fatal("unable to synchronize:", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The executor channel connects independently of synchronization. Wait
	// until it can serve entries before exercising the command line.
	for {
		connection, _, err := remote.Dial(readyContext, submitting, sessionID, remote.OperationExecute)
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

func TestRemoteWorkspacePrologueOutsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("prologues require a POSIX shell")
	}
	directory := t.TempDir()
	alphaRoot, betaRoot := filepath.Join(directory, "alpha"), filepath.Join(directory, "beta")
	if err := os.Mkdir(alphaRoot, 0700); err != nil {
		t.Fatal(err)
	}
	// Only beta executes, so --session identifies a unique route even when
	// invoked outside both roots. An invalid override must then be ignored,
	// regardless of whether --cwd points back into the executing root.
	sessionID := createRemoteDiffSession(t, alphaRoot, betaRoot)
	if err := os.WriteFile(filepath.Join(alphaRoot, ".remote-prologue.sh"), []byte("exit 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{directory, betaRoot} {
		code, output, errorOutput := runRemote(t, directory, "--session", sessionID, "--cwd", cwd,
			"sh", "-c", `printf %s "$MUTAGEN_REMOTE_ENDPOINT"`)
		if code != 0 || output != "beta" {
			t.Fatalf("outside submission sourced override: %d, %q, %q", code, output, errorOutput)
		}
	}
	if code, _, errorOutput := runRemote(t, alphaRoot, "true"); code != 126 {
		t.Fatalf("inside submission didn't source override: %d, %q", code, errorOutput)
	}
}

func TestRemoteSessionSelection(t *testing.T) {
	// Commands rely on a POSIX shell.
	if runtime.GOOS == "windows" {
		t.Skip()
	}

	// Create two sessions synchronizing distinct roots, each with output that
	// commands submitted for the other session must not delete.
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstAlpha, firstBeta := filepath.Join(directory, "first-alpha"), filepath.Join(directory, "first-beta")
	secondAlpha, secondBeta := filepath.Join(directory, "second-alpha"), filepath.Join(directory, "second-beta")
	for _, root := range []string{firstAlpha, secondAlpha} {
		if err := os.MkdirAll(filepath.Join(root, "sub", "build"), 0700); err != nil {
			t.Fatal(err)
		} else if err := os.WriteFile(filepath.Join(root, "sub", "build", "output"), []byte("output"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	createRemoteSession(t, "first", firstAlpha, firstBeta, false)
	createRemoteSession(t, "second", secondAlpha, secondBeta, false)
	submission := filepath.Join(secondAlpha, "sub")
	pwd := []string{"sh", "-c", "pwd -P"}

	// MUTAGEN_REMOTE_SESSION doesn't redirect commands submitted from inside
	// another session's root, but it selects a session outside every root,
	// where commands run at the root of the session's opposite endpoint.
	t.Setenv("MUTAGEN_REMOTE_SESSION", "first")
	for _, test := range []struct{ from, expected string }{
		{submission, filepath.Join(secondBeta, "sub")},
		{directory, firstBeta},
	} {
		if code, output, errorOutput := runRemote(t, test.from, pwd...); code != 0 || output != test.expected+"\n" {
			t.Fatalf("incorrect directory for submission from %s: %d, %q, %q", test.from, code, output, errorOutput)
		}
	}

	// Explicitly selecting a session whose roots don't contain the directory
	// fails while another session's root does, without running the command or
	// synchronizing, unless the directory is specified explicitly.
	if code, _, errorOutput := runRemote(t, submission, "-s", "first", "rm", "-rf", "sub/build"); code != 255 ||
		!strings.Contains(errorOutput, "is synchronized by session second") {
		t.Fatalf("explicit session redirected a command: %d, %q", code, errorOutput)
	}
	for _, root := range []string{firstAlpha, firstBeta} {
		if _, err := os.Stat(filepath.Join(root, "sub", "build", "output")); err != nil {
			t.Fatal("redirected command ran:", err)
		}
	}
	explicit := append([]string{"-s", "first", "--cwd", "sub"}, pwd...)
	if code, output, errorOutput := runRemote(t, submission, explicit...); code != 0 || output != filepath.Join(firstBeta, "sub")+"\n" {
		t.Fatalf("explicit directory not used: %d, %q, %q", code, output, errorOutput)
	}

	// Explicit selections still apply to operations that don't run in a
	// corresponding directory, and to commands outside every root.
	if code, _, errorOutput := runRemote(t, submission, "-s", "first", "--flush"); code != 0 {
		t.Fatalf("explicitly selected session not synchronized: %d, %q", code, errorOutput)
	}
	t.Setenv("MUTAGEN_REMOTE_SESSION", "")
	if code, output, errorOutput := runRemote(t, directory, append([]string{"-s", "second"}, pwd...)...); code != 0 || output != secondBeta+"\n" {
		t.Fatalf("explicit session not used outside every root: %d, %q, %q", code, output, errorOutput)
	}
}

func TestRemoteNestedSessionSelection(t *testing.T) {
	// Commands rely on a POSIX shell.
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outer, inner := filepath.Join(directory, "outer"), filepath.Join(directory, "outer", "inner")
	if err := os.MkdirAll(filepath.Join(inner, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	outerBeta, innerBeta := filepath.Join(directory, "outer-beta"), filepath.Join(directory, "inner-beta")
	createRemoteSession(t, "outer", outer, outerBeta, false)
	innerID := createRemoteSession(t, "inner", inner, innerBeta, false)

	// Commands submitted inside the nested session's root run through it, with
	// a warning, since the enclosing session also synchronizes the directory,
	// unless the session is selected explicitly.
	submission := filepath.Join(inner, "sub")
	if code, output, errorOutput := runRemote(t, submission, "sh", "-c", "pwd -P"); code != 0 || output != filepath.Join(innerBeta, "sub")+"\n" ||
		errorOutput != "remote: warning: sessions inner and outer both synchronize the current directory; using inner (select one with -s)\n" {
		t.Fatalf("nested session not used with a warning: %d, %q, %q", code, output, errorOutput)
	} else if code, _, errorOutput := runRemote(t, submission, "-s", "inner", "true"); code != 0 || errorOutput != "" {
		t.Fatalf("explicitly selected session warned: %d, %q", code, errorOutput)
	}

	// Both sessions are listed, with the one that's used marked.
	if code, output, errorOutput := runRemote(t, submission, "--sessions"); code != 0 ||
		output != "Sessions whose roots contain the current directory:\n  inner: alpha root "+inner+" (used by default)\n  outer: alpha root "+outer+"\n" {
		t.Fatalf("incorrect session listing: %d, %q, %q", code, output, errorOutput)
	}

	// While the nested session is paused, selecting it explicitly fails,
	// rather than running the command through the enclosing session.
	if err := synchronizationManager.Pause(context.Background(), &selection.Selection{Specifications: []string{innerID}}, ""); err != nil {
		t.Fatal("unable to pause session:", err)
	} else if code, output, errorOutput := runRemote(t, submission, "-s", "inner", "sh", "-c", "pwd -P"); code != 255 || output != "" ||
		!strings.Contains(errorOutput, `no connected session matching "inner"`) {
		t.Fatalf("paused session not reported: %d, %q, %q", code, output, errorOutput)
	}
}

func TestRemoteSharedRootSelection(t *testing.T) {
	// Commands rely on a POSIX shell.
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(directory, "project")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	firstBeta, secondBeta := filepath.Join(directory, "gpu1"), filepath.Join(directory, "gpu2")
	createRemoteSession(t, "gpu1", root, firstBeta, false)
	createRemoteSession(t, "gpu2", root, secondBeta, false)
	submission := filepath.Join(root, "sub")
	pwd := []string{"sh", "-c", "pwd -P"}

	// The session created first is used, with a warning, unless a session is
	// selected.
	if code, output, errorOutput := runRemote(t, submission, pwd...); code != 0 || output != filepath.Join(firstBeta, "sub")+"\n" ||
		errorOutput != "remote: warning: sessions gpu1 and gpu2 both synchronize the current directory; using gpu1 (select one with -s)\n" {
		t.Fatalf("first session not used with a warning: %d, %q, %q", code, output, errorOutput)
	} else if code, output, errorOutput := runRemote(t, submission, append([]string{"-s", "gpu2"}, pwd...)...); code != 0 ||
		output != filepath.Join(secondBeta, "sub")+"\n" || errorOutput != "" {
		t.Fatalf("selected session not used: %d, %q, %q", code, output, errorOutput)
	}
	t.Setenv("MUTAGEN_REMOTE_SESSION", "gpu2")
	if code, output, errorOutput := runRemote(t, submission, pwd...); code != 0 || output != filepath.Join(secondBeta, "sub")+"\n" || errorOutput != "" {
		t.Fatalf("default session not used: %d, %q, %q", code, output, errorOutput)
	} else if code, output, errorOutput := runRemote(t, submission, "--sessions"); code != 0 ||
		output != "Sessions whose roots contain the current directory:\n  gpu1: alpha root "+root+"\n  gpu2: alpha root "+root+" (used by default)\n" {
		t.Fatalf("incorrect session listing: %d, %q, %q", code, output, errorOutput)
	}
}

func TestRemoteNestedSessionExcluded(t *testing.T) {
	// Commands rely on a POSIX shell.
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outer, inner := filepath.Join(directory, "outer"), filepath.Join(directory, "outer", "inner")
	if err := os.MkdirAll(filepath.Join(inner, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	outerBeta, innerBeta := filepath.Join(directory, "outer-beta"), filepath.Join(directory, "inner-beta")
	createRemoteSession(t, "outer", outer, outerBeta, false, "inner")
	innerID := createRemoteSession(t, "inner", inner, innerBeta, false)
	submission := filepath.Join(inner, "sub")
	pwd := []string{"sh", "-c", "pwd -P"}
	if code, output, errorOutput := runRemote(t, submission, pwd...); code != 0 || output != filepath.Join(innerBeta, "sub")+"\n" || errorOutput != "" {
		t.Fatalf("nested session not used without warnings: %d, %q, %q", code, output, errorOutput)
	}

	// While the nested session is paused, commands submitted inside its root
	// aren't run through the enclosing session, which ignores it, unless the
	// directory is specified explicitly.
	if err := synchronizationManager.Pause(context.Background(), &selection.Selection{Specifications: []string{innerID}}, ""); err != nil {
		t.Fatal("unable to pause session:", err)
	} else if code, output, errorOutput := runRemote(t, submission, pwd...); code != 255 || output != "" ||
		!strings.Contains(errorOutput, "is inside "+inner+", which session outer") || !strings.Contains(errorOutput, "--cwd .") {
		t.Fatalf("command in an excluded directory not refused: %d, %q, %q", code, output, errorOutput)
	}
	if err := os.MkdirAll(filepath.Join(outerBeta, "inner", "sub"), 0700); err != nil {
		t.Fatal(err)
	} else if code, output, errorOutput := runRemote(t, submission, append([]string{"--cwd", "."}, pwd...)...); code != 0 ||
		output != filepath.Join(outerBeta, "inner", "sub")+"\n" {
		t.Fatalf("explicit directory not used: %d, %q, %q", code, output, errorOutput)
	}
}

func TestRemoteCommandTerminatedByPause(t *testing.T) {
	// Commands rely on a POSIX shell.
	if runtime.GOOS == "windows" {
		t.Skip()
	}

	// Commands are executed by an agent-style executor on beta, and by the
	// daemon itself on alpha.
	for _, alphaExecutes := range []bool{false, true} {
		name := "agent executor"
		if alphaExecutes {
			name = "daemon executor"
		}
		t.Run(name, func(t *testing.T) {
			directory, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			alphaRoot, betaRoot := filepath.Join(directory, "alpha"), filepath.Join(directory, "beta")
			if err := os.Mkdir(alphaRoot, 0700); err != nil {
				t.Fatal(err)
			}
			sessionID := createRemoteSession(t, "paused", alphaRoot, betaRoot, alphaExecutes)
			submitting := alphaRoot
			if alphaExecutes {
				submitting = betaRoot
			}

			// Run a command that cleans up when asked to terminate, keeping
			// its markers outside the roots, and pause the session once it's
			// running.
			started, terminated := filepath.Join(directory, "started"), filepath.Join(directory, "terminated")
			script := `trap 'echo terminated > "$2"; exit 1' TERM; touch "$1"; while :; do sleep 1; done`
			t.Chdir(submitting)
			type outcome struct {
				code        int
				errorOutput string
			}
			outcomes := make(chan outcome, 1)
			go func() {
				var output, errorOutput strings.Builder
				code := cli.Run([]string{"sh", "-c", script, "sh", started, terminated}, strings.NewReader(""), &output, &errorOutput)
				outcomes <- outcome{code, errorOutput.String()}
			}()
			for deadline := time.Now().Add(30 * time.Second); ; {
				if _, err := os.Stat(started); err == nil {
					break
				} else if time.Now().After(deadline) {
					t.Fatal("command didn't start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			pausing := time.Now()
			if err := synchronizationManager.Pause(context.Background(), &selection.Selection{Specifications: []string{sessionID}}, ""); err != nil {
				t.Fatal("unable to pause session:", err)
			}
			paused := time.Since(pausing)

			// The command is asked to terminate, which doesn't delay pausing
			// beyond the time that the command takes to exit, and remote
			// reports that it was terminated and that the session wasn't
			// synchronized afterwards.
			select {
			case result := <-outcomes:
				expected := "remote: the connection to the executing endpoint was lost (the session may have stopped synchronizing), " +
					"so the command was terminated and the session wasn't synchronized afterwards\n"
				if result.code != 255 || result.errorOutput != expected {
					t.Fatalf("termination reported incorrectly: %d, %q", result.code, result.errorOutput)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("remote didn't exit")
			}
			if data, err := os.ReadFile(terminated); err != nil || string(data) != "terminated\n" {
				t.Fatalf("command wasn't asked to terminate: %q, %v", data, err)
			} else if paused >= remote.TerminationGracePeriod {
				t.Fatalf("pausing waited for the grace period: %v", paused)
			}
		})
	}
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

// runRemoteWithInput runs the remote command line in the specified directory
// with the specified input.
func runRemoteWithInput(t *testing.T, directory, input string, arguments ...string) (int, string, string) {
	t.Helper()
	t.Chdir(directory)
	var output, errorOutput strings.Builder
	code := cli.Run(arguments, strings.NewReader(input), &output, &errorOutput)
	return code, output.String(), errorOutput.String()
}

// waitForFile waits for a file to exist.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, err := os.Stat(path); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatal("file didn't appear:", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRemoteDetachedJobs(t *testing.T) {
	// Jobs rely on a POSIX shell.
	if runtime.GOOS == "windows" {
		t.Skip()
	}

	// Jobs are executed by an agent-style executor on beta, and by the daemon
	// itself on alpha.
	for _, alphaExecutes := range []bool{false, true} {
		name := "agent executor"
		if alphaExecutes {
			name = "daemon executor"
		}
		t.Run(name, func(t *testing.T) {
			directory, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			alphaRoot, betaRoot := filepath.Join(directory, "alpha"), filepath.Join(directory, "beta")
			if err := os.Mkdir(alphaRoot, 0700); err != nil {
				t.Fatal(err)
			}
			sessionID := createRemoteSession(t, "jobs", alphaRoot, betaRoot, alphaExecutes)
			submitting := alphaRoot
			if alphaExecutes {
				submitting = betaRoot
			}

			// Start a job that waits for a release marker, keeping its markers
			// outside the roots. Only its identifier is printed.
			started, release := filepath.Join(directory, "started"), filepath.Join(directory, "release")
			script := `echo > "$1"; while [ ! -e "$2" ]; do sleep 0.1; done; echo done; exit 3`
			code, output, errorOutput := runRemote(t, submitting, "--detach", "--log", "out.log", "sh", "-c", script, "sh", started, release)
			id := strings.TrimSpace(output)
			if code != 0 || len(id) != 8 || !strings.Contains(errorOutput, "remote --wait "+id) {
				t.Fatalf("unable to start job: %d, %q, %q", code, output, errorOutput)
			}
			waitForFile(t, started)
			if code, output, _ := runRemote(t, submitting, "--jobs"); code != 0 || !strings.Contains(output, id+"  running") || !strings.Contains(output, "out.log") {
				t.Fatalf("incorrect job listing: %d, %q", code, output)
			}

			// The job keeps running while the session is paused, unlike
			// commands.
			specification := &selection.Selection{Specifications: []string{sessionID}}
			if err := synchronizationManager.Pause(context.Background(), specification, ""); err != nil {
				t.Fatal("unable to pause session:", err)
			} else if err := synchronizationManager.Resume(context.Background(), specification, ""); err != nil {
				t.Fatal("unable to resume session:", err)
			} else if _, err := flushRemote(submitting); err != nil {
				t.Fatal("unable to synchronize after resuming:", err)
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}

			// Waiting returns the job's status, once the session has been
			// synchronized, so the log is present locally.
			if code, _, errorOutput := runRemote(t, submitting, "--wait", id); code != 3 || !strings.Contains(errorOutput, "exited with status 3") {
				t.Fatalf("incorrect wait result: %d, %q", code, errorOutput)
			} else if data, err := os.ReadFile(filepath.Join(submitting, "out.log")); err != nil || string(data) != "done\n" {
				t.Fatalf("log not synchronized: %q, %v", data, err)
			}

			// Killing a job terminates it, and waiting for it reports the
			// status with which it was terminated.
			code, output, errorOutput = runRemote(t, submitting, "--detach", "sleep", "60")
			if code != 0 {
				t.Fatalf("unable to start job: %d, %q", code, errorOutput)
			}
			id = strings.TrimSpace(output)
			if code, _, errorOutput := runRemote(t, submitting, "--kill", id); code != 0 || !strings.Contains(errorOutput, "terminated job "+id+" (status 143)") {
				t.Fatalf("incorrect kill result: %d, %q", code, errorOutput)
			} else if code, _, errorOutput := runRemote(t, submitting, "--wait", id, "--no-flush"); code != 143 || !strings.Contains(errorOutput, "terminated by remote --kill") {
				t.Fatalf("incorrect wait result for a killed job: %d, %q", code, errorOutput)
			} else if code, _, errorOutput := runRemote(t, submitting, "--wait", "00000000"); code != 255 || !strings.Contains(errorOutput, "no job 00000000") {
				t.Fatalf("incorrect wait result for a nonexistent job: %d, %q", code, errorOutput)
			}
		})
	}
}

func TestRemoteScript(t *testing.T) {
	// Scripts rely on POSIX shells.
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alphaRoot, betaRoot := filepath.Join(directory, "alpha"), filepath.Join(directory, "beta")
	if err := os.MkdirAll(filepath.Join(alphaRoot, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	createRemoteSession(t, "scripts", alphaRoot, betaRoot, false)
	submitting := filepath.Join(alphaRoot, "sub")

	// Scripts read from standard input run with bash by default, in the
	// corresponding directory, receive the arguments that follow them, even
	// ones that look like options, and nothing is expanded locally.
	script := `if [[ $1 == --name ]]; then echo "name $2"; fi
echo "$PWD" "$MUTAGEN_REMOTE_ENDPOINT"
cat
`
	code, output, errorOutput := runRemoteWithInput(t, submitting, script, "--script", "-", "--name", "a b")
	if code != 0 || output != "name a b\n"+filepath.Join(betaRoot, "sub")+" beta\n" {
		t.Fatalf("incorrect script result: %d, %q, %q", code, output, errorOutput)
	}

	// A #! line selects the interpreter, and the exit status is the script's.
	if code, output, _ := runRemoteWithInput(t, submitting, "#!/bin/sh\necho sh\nexit 4\n", "--script", "-"); code != 4 || output != "sh\n" {
		t.Fatalf("incorrect script result: %d, %q", code, output)
	}

	// Scripts can run as detached jobs.
	code, output, errorOutput = runRemoteWithInput(t, submitting, "for p in a b; do echo \"$p$1\"; done\n", "--detach", "--log", "sweep.log", "--script", "-", "!")
	if code != 0 {
		t.Fatalf("unable to start script job: %d, %q", code, errorOutput)
	}
	id := strings.TrimSpace(output)
	if code, _, errorOutput := runRemote(t, submitting, "--wait", id); code != 0 {
		t.Fatalf("incorrect wait result: %d, %q", code, errorOutput)
	} else if data, err := os.ReadFile(filepath.Join(submitting, "sweep.log")); err != nil || string(data) != "a!\nb!\n" {
		t.Fatalf("incorrect script job log: %q, %v", data, err)
	} else if code, output, _ := runRemote(t, submitting, "--jobs"); code != 0 || !strings.Contains(output, "--script - '!'") {
		t.Fatalf("incorrect listing of a script job: %q", output)
	}
}

func TestRemotePullIgnored(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alphaRoot, betaRoot := filepath.Join(directory, "alpha"), filepath.Join(directory, "beta")
	if err := os.Mkdir(alphaRoot, 0700); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(alphaRoot, "train.py"), []byte("train\n"), 0600); err != nil {
		t.Fatal(err)
	}
	createRemoteSession(t, "pull", alphaRoot, betaRoot, false, "results/", "*.log")

	// Content that the session ignores exists only on beta.
	write := func(path, data string) {
		t.Helper()
		path = filepath.Join(betaRoot, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		} else if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("results/run1/metrics.json", "1\n")
	write("train.log", "log\n")

	// The ignores show which paths aren't synchronized.
	code, output, _ := runRemote(t, alphaRoot, "--ignores", "results/run1", "train.log", "train.py")
	expected := "results/run1: ignored (inside results, which the session ignores)\ntrain.log: ignored\ntrain.py: synchronized\n"
	if code != 1 || output != expected {
		t.Fatalf("incorrect ignores: %d, %q", code, output)
	} else if code, output, _ := runRemote(t, alphaRoot, "--ignores"); code != 0 || !strings.Contains(output, "results/") {
		t.Fatalf("incorrect ignore listing: %d, %q", code, output)
	}

	// Ignored paths are pulled.
	if code, _, errorOutput := runRemote(t, alphaRoot, "--pull", "results", "train.log"); code != 0 ||
		errorOutput != "remote: pulled results (1 added, 0 replaced, 0 unchanged)\nremote: pulled train.log (1 added, 0 replaced, 0 unchanged)\n" {
		t.Fatalf("incorrect pull result: %d, %q", code, errorOutput)
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(alphaRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if read("results/run1/metrics.json") != "1\n" || read("train.log") != "log\n" {
		t.Fatal("incorrect pulled contents")
	}

	// Pulling again replaces changed files and keeps local ones.
	write("results/run1/metrics.json", "2\n")
	write("results/run2/metrics.json", "3\n")
	if err := os.WriteFile(filepath.Join(alphaRoot, "results", "local"), []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, errorOutput := runRemote(t, alphaRoot, "--pull", "results"); code != 0 || errorOutput != "remote: pulled results (1 added, 1 replaced, 0 unchanged)\n" {
		t.Fatalf("incorrect pull result: %d, %q", code, errorOutput)
	} else if read("results/run1/metrics.json") != "2\n" || read("results/run2/metrics.json") != "3\n" || read("results/local") != "local\n" {
		t.Fatal("incorrect merged contents")
	}

	// Synchronized and missing paths aren't pulled.
	if code, _, errorOutput := runRemote(t, alphaRoot, "--pull", "train.py"); code != 255 || !strings.Contains(errorOutput, "the session synchronizes it") {
		t.Fatalf("synchronized path pulled: %d, %q", code, errorOutput)
	} else if code, _, errorOutput := runRemote(t, alphaRoot, "--pull", "missing.log"); code != 255 || !strings.Contains(errorOutput, "doesn't exist on the opposite endpoint") {
		t.Fatalf("missing path pulled: %d, %q", code, errorOutput)
	}
}
