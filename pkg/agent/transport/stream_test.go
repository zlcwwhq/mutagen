package transport

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowFirstWriter is a writer that delays its first write, as a busy log might,
// and records what's written to it.
type slowFirstWriter struct {
	// lock serializes access to the other fields.
	lock sync.Mutex
	// delayed indicates whether the first write has started.
	delayed bool
	// buffer records what's been written.
	buffer bytes.Buffer
}

// Write implements io.Writer.Write.
func (w *slowFirstWriter) Write(data []byte) (int, error) {
	w.lock.Lock()
	delay := !w.delayed
	w.delayed = true
	w.lock.Unlock()
	if delay {
		time.Sleep(300 * time.Millisecond)
	}
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.buffer.Write(data)
}

// String returns what's been written.
func (w *slowFirstWriter) String() string {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.buffer.String()
}

// TestStreamForwardsErrorOutputBeforeExit tests that error output that a
// process writes before exiting has been forwarded by the time Close returns,
// even if forwarding it is still in progress when the process exits.
func TestStreamForwardsErrorOutputBeforeExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}

	// Start a process that writes error output in two parts and exits. The
	// receiver delays the first part, so that the second is still waiting to
	// be forwarded when the process exits.
	receiver := &slowFirstWriter{}
	process := exec.Command("sh", "-c", "printf 'first\\n' >&2; sleep 0.1; printf 'second\\n' >&2; exit 3")
	stream, err := NewStream(process, receiver)
	if err != nil {
		t.Fatal("unable to create stream:", err)
	} else if err := process.Start(); err != nil {
		t.Fatal("unable to start process:", err)
	}

	// Wait for the process to close its output, and thus to exit, then close
	// the stream, which should report the process' exit status.
	if _, err := bufio.NewReader(stream).ReadString('\n'); err == nil {
		t.Error("process wrote unexpected output")
	}
	var exitErr *exec.ExitError
	if err := stream.Close(); err == nil {
		t.Error("closure didn't report process failure")
	} else if !(errors.As(err, &exitErr) && exitErr.ExitCode() == 3) {
		t.Error("closure reported unexpected error:", err)
	}

	// Check that all of the error output was forwarded.
	if output := receiver.String(); output != "first\nsecond\n" {
		t.Errorf("incorrect error output forwarded: %q", output)
	}
}

// TestStreamBoundsErrorForwarding tests that Close returns, without an error
// for a process that exits successfully, if a subprocess keeps the process'
// error output stream open after the process exits.
func TestStreamBoundsErrorForwarding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}

	// Start a process that leaves a subprocess holding its error output stream
	// and prints that subprocess' process identifier. Terminate the subprocess
	// once we're done.
	receiver := &slowFirstWriter{}
	process := exec.Command("sh", "-c", "sleep 60 </dev/null >/dev/null & echo $!; printf 'error\\n' >&2")
	stream, err := NewStream(process, receiver)
	if err != nil {
		t.Fatal("unable to create stream:", err)
	} else if err := process.Start(); err != nil {
		t.Fatal("unable to start process:", err)
	}
	line, err := bufio.NewReader(stream).ReadString('\n')
	if err != nil {
		stream.Close()
		t.Fatal("unable to read subprocess identifier:", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		stream.Close()
		t.Fatal("invalid subprocess identifier:", err)
	}
	defer func() {
		if subprocess, err := os.FindProcess(pid); err == nil {
			subprocess.Kill()
		}
	}()

	// Close the stream and check that it returns without waiting for the
	// subprocess and without an error, having forwarded the error output.
	start := time.Now()
	if err := stream.Close(); err != nil {
		t.Error("closure reported unexpected error:", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Error("closure waited for subprocess:", elapsed)
	}
	if output := receiver.String(); output != "error\n" {
		t.Errorf("incorrect error output forwarded: %q", output)
	}
}
