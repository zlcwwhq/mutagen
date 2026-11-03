package agent

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/logging"
)

// failingTransport is a transport whose agent writes an error message and exits
// without performing a handshake.
type failingTransport struct{}

// Copy implements Transport.Copy.
func (failingTransport) Copy(_, _ string) error {
	return errors.New("copying not supported")
}

// Command implements Transport.Command.
func (failingTransport) Command(_ string) (*exec.Cmd, error) {
	return exec.Command("sh", "-c", "printf 'agent failure\\n' >&2; exit 1"), nil
}

// ClassifyError implements Transport.ClassifyError.
func (failingTransport) ClassifyError(_ *os.ProcessState, _ string) (bool, bool, error) {
	return false, false, nil
}

// slowWriter is a writer that discards what's written to it after a delay.
type slowWriter struct{}

// Write implements io.Writer.Write.
func (slowWriter) Write(data []byte) (int, error) {
	time.Sleep(100 * time.Millisecond)
	return len(data), nil
}

// TestConnectReportsErrorOutput tests that a failed handshake reports the
// agent's error output, which, as the race detector checks, is only read once
// it's no longer being written. The error output is also logged, through a
// slow writer, so that it's still being forwarded when the agent has exited.
func TestConnectReportsErrorOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	logger := logging.NewLogger(logging.LevelError, slowWriter{})
	_, tryInstall, cmdExe, err := connect(logger, failingTransport{}, CommandSynchronizer, "", false)
	if err == nil {
		t.Fatal("connection succeeded unexpectedly")
	} else if !strings.Contains(err.Error(), "(error output: agent failure)") {
		t.Error("error output not reported:", err)
	}
	if tryInstall || cmdExe {
		t.Errorf("incorrect classification: install %t, cmd.exe %t", tryInstall, cmdExe)
	}
}
