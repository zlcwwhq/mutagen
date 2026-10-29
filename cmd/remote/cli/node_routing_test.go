package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// Ordinary operations resolve the selected destination on their existing
// connection, without asking for a node graph first.
func TestCommandRoutingDoesNotDiscoverNodes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("command requires a POSIX shell")
	}
	data, err := os.MkdirTemp("", "mr-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	alpha, beta := t.TempDir(), t.TempDir()
	source, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: "sync_route", Root: alpha, Alpha: true, Submit: true, Node: "alpha", PeerNode: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: "sync_route", Root: beta, Execute: true, Node: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	var handlers sync.WaitGroup
	var operations []remote.Operation
	var lock sync.Mutex
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		for {
			connection, err := source.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				header, err := remote.ReceiveSubmission(context.Background(), connection)
				if err != nil {
					connection.Close()
					return
				}
				lock.Lock()
				operations = append(operations, header.Operation)
				lock.Unlock()
				if header.Operation == remote.OperationNodes {
					remote.Reject(connection, io.ErrUnexpectedEOF)
					return
				}
				if !replyTestRoute(connection, header, "sync_route") {
					return
				}
				if header.Operation == remote.OperationFlush {
					remote.RespondToFlush(connection, &remote.Report{}, nil)
					return
				}
				execution, err := executor.Open(context.Background())
				if err != nil {
					remote.Reject(connection, err)
					return
				}
				remote.Splice(connection, execution)
			}()
		}
	}()
	t.Cleanup(func() { source.Close(); executor.Close(); handlers.Wait() })
	t.Chdir(alpha)
	var output, diagnostic bytes.Buffer
	if code := Run([]string{"--node", "beta", "printf", "executed"}, strings.NewReader(""), &output, &diagnostic); code != 0 || output.String() != "executed" {
		t.Fatalf("command failed: %d %q %s", code, output.String(), diagnostic.String())
	}
	lock.Lock()
	defer lock.Unlock()
	expected := []remote.Operation{remote.OperationFlush, remote.OperationExecute, remote.OperationFlush}
	if !reflect.DeepEqual(operations, expected) {
		t.Fatalf("unexpected extra routing operations: %v", operations)
	}
}

// Star commands refuse an ignored submission directory before submitting
// anything, with the same guidance as pair commands.
func TestStarCommandRefusesIgnoredDirectory(t *testing.T) {
	data, err := os.MkdirTemp("", "mr-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	root := t.TempDir()
	directory := filepath.Join(root, "ignored", "sub")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := remote.NewLocalChannel(nil, &remote.Endpoint{
		Session: "sync_star", Star: "project", Root: root, Alpha: true, Submit: true, Node: "center", PeerNode: "gpu",
		Ignores: &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"ignored"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var handlers sync.WaitGroup
	var submitted []remote.Operation
	var lock sync.Mutex
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		for {
			connection, err := source.Accept()
			if err != nil {
				return
			}
			if header, err := remote.ReceiveSubmission(context.Background(), connection); err == nil {
				lock.Lock()
				submitted = append(submitted, header.Operation)
				lock.Unlock()
				remote.Reject(connection, io.ErrUnexpectedEOF)
			} else {
				connection.Close()
			}
		}
	}()
	t.Cleanup(func() { source.Close(); handlers.Wait() })
	t.Chdir(directory)
	var output, diagnostic bytes.Buffer
	if code := Run([]string{"--node", "gpu", "true"}, strings.NewReader(""), &output, &diagnostic); code != failureExitCode ||
		!strings.Contains(diagnostic.String(), "ignored/sub is inside ignored, which session sync_star ignores") ||
		!strings.Contains(diagnostic.String(), "--cwd . runs in the other side's own copy") {
		t.Fatalf("ignored directory not refused locally: %d %s", code, diagnostic.String())
	}
	lock.Lock()
	defer lock.Unlock()
	if len(submitted) != 0 {
		t.Fatalf("refused command was submitted: %v", submitted)
	}
}
