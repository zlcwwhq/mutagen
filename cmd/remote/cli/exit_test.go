package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// failedCommandIO models a caller-owned stream that cannot read or write.
type failedCommandIO struct{}

func (failedCommandIO) Read([]byte) (int, error)  { return 0, errors.New("local input unavailable") }
func (failedCommandIO) Write([]byte) (int, error) { return 0, errors.New("local output unavailable") }

func TestCommandLocalIOExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	alpha, _ := setupPullSessions(t, map[string]string{"file": "x"}, map[string]string{"file": "x"})
	t.Chdir(alpha)
	for _, stream := range []string{"stdin", "stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			var input io.Reader = strings.NewReader("input")
			var output, diagnostic io.Writer = io.Discard, io.Discard
			command := "cat"
			switch stream {
			case "stdin":
				input = failedCommandIO{}
			case "stdout":
				output = failedCommandIO{}
			case "stderr":
				command, diagnostic = "cat >&2", failedCommandIO{}
			}
			if code := Run([]string{"--no-flush", "sh", "-c", command}, input, output, diagnostic); code != failureExitCode {
				t.Fatalf("local %s failure returned %d, expected 125", stream, code)
			}
		})
	}
	for _, arguments := range [][]string{{"--ignores", "file"}, {"--sessions"}, {"--version"}, {"--help"}} {
		if code := Run(arguments, strings.NewReader(""), failedCommandIO{}, io.Discard); code != failureExitCode {
			t.Fatalf("%v output failure returned %d, expected 125", arguments, code)
		}
	}
}

func TestPullBatchFailurePriority(t *testing.T) {
	data, err := os.MkdirTemp("", "me-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	channel, err := remote.NewLocalChannel(nil, &remote.Endpoint{
		Session: "sync_mixed", Root: root, Alpha: true, Submit: true,
		Ignores: &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"results"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Each remote request encounters an internal connection deadline. It is
	// still a connection failure, while a caller deadline must take priority.
	var handlers sync.WaitGroup
	var interruptNext atomic.Bool
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		for {
			connection, err := channel.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				remote.ReceiveSubmission(context.Background(), connection)
				if interruptNext.Swap(false) {
					// Receiving a submission proves Run installed its signal
					// handler. Keep the route pending until cancellation closes
					// it, rather than racing a response against the signal.
					process, _ := os.FindProcess(os.Getpid())
					process.Signal(os.Interrupt)
					io.Copy(io.Discard, connection)
					connection.Close()
					return
				}
				remote.Reject(connection, remote.ConnectionError(context.DeadlineExceeded))
			}()
		}
	}()
	t.Cleanup(func() { channel.Close(); handlers.Wait() })
	for _, paths := range [][]string{{"tracked", "results/file"}, {"results/file", "tracked"}} {
		var diagnostic strings.Builder
		if code := Run(append([]string{"--pull"}, paths...), strings.NewReader(""), io.Discard, &diagnostic); code != connectionExitCode {
			t.Fatalf("mixed failures %v returned %d: %s", paths, code, diagnostic.String())
		} else if strings.Count(diagnostic.String(), "remote: unable to pull") != 2 {
			t.Fatalf("batch diagnostics missing or duplicated: %s", diagnostic.String())
		}
	}
	if code := Run([]string{"--timeout", "1ns", "--pull", "tracked", "results/file"}, strings.NewReader(""), io.Discard, io.Discard); code != timeoutExitCode {
		t.Fatalf("caller timeout returned %d, expected 124", code)
	}
	if runtime.GOOS != "windows" {
		interruptNext.Store(true)
		if code := Run([]string{"--pull", "tracked", "results/file"}, strings.NewReader(""), io.Discard, io.Discard); code != 130 {
			t.Fatalf("signal did not take precedence over batch failure: %d", code)
		}
	}

	// Already printed conflict errors retain the same priority regardless of
	// path ordering, and keep the marker that prevents duplicate diagnostics.
	for _, causes := range [][]error{
		{errors.New("refused"), remote.ConnectionError(errors.New("lost"))},
		{remote.ConnectionError(errors.New("lost")), errors.New("refused")},
	} {
		p := &printer{output: io.Discard, errors: io.Discard}
		for _, cause := range causes {
			p.print("file", nil, cause)
		}
		err := errors.Join(append([]error{errReported}, p.failures...)...)
		if !errors.Is(err, errReported) || errorExitCode(err) != connectionExitCode {
			t.Fatalf("conflict failure priority lost: %v", err)
		} else if errorExitCode(errors.Join(err, context.DeadlineExceeded)) != timeoutExitCode {
			t.Fatal("deadline did not take precedence over batch failures")
		}
	}
}
