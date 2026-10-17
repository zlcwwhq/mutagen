package integration

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
		connection, target, err := remote.Dial(ctx, directory, "")
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
	// executor) and remote commands enabled in both directions.
	ctx := context.Background()
	sessionID, err := synchronizationManager.Create(
		ctx,
		&url.URL{Path: alphaRoot},
		&url.URL{Protocol: netpipe.Protocol_Netpipe, Path: betaRoot},
		&synchronization.Configuration{
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
	if err := waitForSuccessfulSynchronizationCycle(ctx, sessionID, false, false, false); err != nil {
		t.Fatal("unable to wait for synchronization:", err)
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

	// Terminating the session removes its registrations.
	if err := synchronizationManager.Terminate(ctx, &selection.Selection{Specifications: []string{sessionID}}, ""); err != nil {
		t.Fatal("unable to terminate session:", err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := remote.Dial(dialCtx, alphaRoot, ""); err == nil {
		t.Fatal("registration remained after termination")
	}
}
