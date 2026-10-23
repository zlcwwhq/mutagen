//go:build !windows && !aix

package remote

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ageScriptFile sets a file's modification time to before the retention of
// script files, without waiting for it to pass.
func ageScriptFile(t *testing.T, path string) {
	t.Helper()
	aged := time.Now().Add(-scriptRetention - time.Hour)
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatal(err)
	}
}

// writeAgedScriptFile writes a file in the directory of foreground scripts
// that's older than their retention, such as one left behind by an executor
// that was killed while its script ran.
func writeAgedScriptFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("echo orphan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ageScriptFile(t, path)
}

// requireScriptFiles checks whether each of the specified paths exists.
func requireScriptFiles(t *testing.T, exist bool, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); exist && err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(path), err)
		} else if !exist && !os.IsNotExist(err) {
			t.Errorf("%s wasn't removed: %v", filepath.Base(path), err)
		}
	}
}

func TestPruneScriptsKeepsHeldScripts(t *testing.T) {
	useTestRegistry(t)

	// Prepare a script, whose file is held until the command is cleaned up,
	// and make the file older than the retention of script files, as if the
	// script had been running for longer.
	request := &Request{Script: &Script{Name: "-", Contents: []byte("echo hi\n")}, Directory: t.TempDir()}
	command, failure := prepare("", "", false, request, "")
	if failure != nil {
		t.Fatal("unable to prepare script:", failure.Error)
	}
	defer command.cleanup()
	ageScriptFile(t, command.script)

	// Add files that are left behind, recent, or not script files, along with
	// a directory, all but the recent one old.
	directory := filepath.Dir(command.script)
	orphan := filepath.Join(directory, "script-orphan")
	writeAgedScriptFile(t, orphan)
	recent := filepath.Join(directory, "script-recent")
	if err := os.WriteFile(recent, nil, 0600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(directory, "other")
	writeAgedScriptFile(t, other)
	subdirectory := filepath.Join(directory, "script-directory")
	if err := os.Mkdir(subdirectory, 0700); err != nil {
		t.Fatal(err)
	}
	ageScriptFile(t, subdirectory)

	// Only the old script file that isn't held is removed.
	pruneScripts(directory)
	requireScriptFiles(t, true, command.script, recent, other, subdirectory)
	requireScriptFiles(t, false, orphan)

	// Once its lock is released without the file being removed, as when the
	// executor is killed, the script's file is removed as well.
	if command.scriptLock == nil {
		t.Fatal("script file isn't locked")
	}
	command.scriptLock.Close()
	command.scriptLock = nil
	pruneScripts(directory)
	requireScriptFiles(t, true, recent, other, subdirectory)
	requireScriptFiles(t, false, command.script)
}

// firstLineWriter records output and reports its first line once written.
type firstLineWriter struct {
	lock     sync.Mutex
	buffer   bytes.Buffer
	reported bool
	line     chan string
}

func (w *firstLineWriter) Write(data []byte) (int, error) {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.buffer.Write(data)
	if line, _, found := strings.Cut(w.buffer.String(), "\n"); found && !w.reported {
		w.reported = true
		w.line <- line
	}
	return len(data), nil
}

func (w *firstLineWriter) String() string {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.buffer.String()
}

func TestRunningScriptSurvivesPruning(t *testing.T) {
	useTestRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start a script that reports its path, waits for its input to close, and
	// then reads itself by its path, as processes such as Python's
	// multiprocessing workers and tracebacks do.
	contents := "echo \"$0\"\nread -r _\ncat \"$0\"\n"
	request := &Request{Script: &Script{Name: "-", Contents: []byte(contents)}, Directory: t.TempDir()}
	input, release := io.Pipe()
	defer release.Close()
	output := &firstLineWriter{line: make(chan string, 1)}
	client, server := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- Serve(context.Background(), server, "", "", false) }()
	type outcome struct {
		code int
		err  error
	}
	executed := make(chan outcome, 1)
	go func() {
		code, err := Execute(ctx, client, request, input, output, io.Discard)
		executed <- outcome{code, err}
	}()
	var path string
	select {
	case path = <-output.line:
	case <-ctx.Done():
		t.Fatal("script didn't report its path")
	}

	// Make the script's file older than the retention of script files, as if
	// the script had been running for longer, and leave another one behind,
	// so that its removal shows that pruning happened.
	ageScriptFile(t, path)
	orphan := filepath.Join(filepath.Dir(path), "script-orphan")
	writeAgedScriptFile(t, orphan)

	// Run another script, which prunes old script files that aren't in use.
	other := &Request{Script: &Script{Name: "-", Contents: []byte("exit 0\n")}, Directory: t.TempDir()}
	if code, err := executeTest(t, ctx, other, nil, nil, nil); err != nil || code != 0 {
		t.Fatalf("other script failed: %d, %v", code, err)
	}
	requireScriptFiles(t, false, orphan)
	requireScriptFiles(t, true, path)

	// The running script can still read itself, and its file is removed once
	// it exits.
	release.Close()
	var result outcome
	select {
	case result = <-executed:
	case <-ctx.Done():
		t.Fatal("script didn't exit")
	}
	if result.err != nil || result.code != 0 {
		t.Errorf("script failed: %d, %v", result.code, result.err)
	}
	if output.String() != path+"\n"+contents {
		t.Errorf("incorrect script output: %q", output.String())
	}
	select {
	case err := <-served:
		if err != nil {
			t.Error("server failed:", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not terminate")
	}
	if files := scriptFiles(t); len(files) != 0 {
		t.Error("script files left behind:", files)
	}
}
