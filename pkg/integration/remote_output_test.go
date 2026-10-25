package integration

import (
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/cmd/remote/cli"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// outputChunks forwards each write without retaining the caller's buffer.
type outputChunks chan string

func (w outputChunks) Write(data []byte) (int, error) {
	w <- string(data)
	return len(data), nil
}

func TestRemoteOutputBeforeExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	if _, err := flushRemote(roots[0]); err != nil {
		t.Fatal(err)
	}
	t.Chdir(roots[0])
	output := make(outputChunks, 16)
	completed := make(chan int, 1)
	go func() {
		completed <- cli.Run([]string{"--timeout", "10s", "sh", "-c", "echo a; sleep 2; echo b"}, strings.NewReader(""), output, io.Discard)
	}()

	// The first line must arrive while the command is still running, rather
	// than being buffered until its output streams close.
	select {
	case first := <-output:
		if first != "a\n" {
			t.Errorf("unexpected first output: %q", first)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no first output received")
	}
	select {
	case code := <-completed:
		t.Fatalf("command already ended when first output arrived: %d", code)
	case <-time.After(100 * time.Millisecond):
	}
	if code := <-completed; code != 0 {
		t.Fatal("command failed:", code)
	}
	if second := <-output; second != "b\n" {
		t.Fatal("unexpected second output:", second)
	}

	// Check the documented Python spelling when that interpreter is available.
	if _, err := exec.LookPath("python"); err == nil {
		code, text, diagnostic := runRemote(t, roots[0], "python", "-u", "-c", "print('unbuffered')")
		if code != 0 || text != "unbuffered\n" {
			t.Fatalf("python -u failed: %d %q %s", code, text, diagnostic)
		}
	}
}
