package remote

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRemoteHelper is executed as a child to exercise real process I/O.
func TestRemoteHelper(t *testing.T) {
	if os.Getenv("MUTAGEN_REMOTE_TEST_HELPER") != "1" {
		return
	}
	arguments := os.Args
	for len(arguments) > 0 && arguments[0] != "--" {
		arguments = arguments[1:]
	}
	arguments = arguments[1:]
	switch arguments[0] {
	case "inspect":
		directory, _ := os.Getwd()
		json.NewEncoder(os.Stdout).Encode([]any{directory, os.Getenv("REMOTE_VALUE"), arguments[1:]})
		fmt.Fprint(os.Stderr, "separate error output")
	case "copy":
		io.Copy(os.Stdout, os.Stdin)
	case "large":
		for i := 0; i < 64; i++ {
			os.Stdout.Write(bytes.Repeat([]byte{0, 255, 'x'}, 4096))
			os.Stderr.Write(bytes.Repeat([]byte{0, 254, 'y'}, 4096))
		}
	case "exit":
		os.Exit(37)
	case "wait":
		fmt.Fprint(os.Stdout, "ready")
		time.Sleep(time.Hour)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

// helperRequest creates a command invoking this test executable.
func helperRequest(t *testing.T, mode string, arguments ...string) *Request {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Request{
		Arguments:   append([]string{executable, "-test.run=^TestRemoteHelper$", "--", mode}, arguments...),
		Environment: []string{"MUTAGEN_REMOTE_TEST_HELPER=1"},
		Directory:   t.TempDir(),
	}
}

// executeTest uses a real duplex connection and waits for server cleanup.
// Requests specify absolute working directories, so no root is needed.
func executeTest(t *testing.T, ctx context.Context, request *Request, input io.Reader, output, errorOutput io.Writer) (int, error) {
	t.Helper()
	return executeInRootTest(t, ctx, "", request, input, output, errorOutput)
}

// executeInRootTest is executeTest with an explicit synchronization root.
func executeInRootTest(t *testing.T, ctx context.Context, root string, request *Request, input io.Reader, output, errorOutput io.Writer) (int, error) {
	t.Helper()
	client, server := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- Serve(context.Background(), server, root) }()
	if input == nil {
		input = strings.NewReader("")
	}
	if output == nil {
		output = io.Discard
	}
	if errorOutput == nil {
		errorOutput = io.Discard
	}
	code, err := Execute(ctx, client, request, input, output, errorOutput)
	select {
	case serverError := <-served:
		if ctx.Err() == nil && err == nil && serverError != nil {
			t.Fatalf("server failed: %v", serverError)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not terminate")
	}
	return code, err
}

// requireSameDirectory compares directory identity rather than spelling.
// os.Getwd may preserve PWD's symbolic links, such as /var on macOS.
func requireSameDirectory(t *testing.T, actual, expected string) {
	t.Helper()
	actualInfo, err := os.Stat(actual)
	if err != nil {
		t.Fatal("unable to inspect actual working directory:", err)
	}
	expectedInfo, err := os.Stat(expected)
	if err != nil {
		t.Fatal("unable to inspect expected working directory:", err)
	}
	if !actualInfo.IsDir() || !os.SameFile(actualInfo, expectedInfo) {
		t.Fatalf("incorrect working directory: %q, expected %q", actual, expected)
	}
}

func TestExecuteArgumentsEnvironmentAndDirectory(t *testing.T) {
	arguments := []string{"", "two words", "--help"}
	request := helperRequest(t, "inspect", arguments...)
	request.Environment = append(request.Environment, "REMOTE_VALUE=space,=\n$literal")
	var output, errorOutput bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, err := executeTest(t, ctx, request, nil, &output, &errorOutput)
	if err != nil || code != 0 {
		t.Fatalf("execution failed: %d, %v", code, err)
	}
	var actual []json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	var directory, value string
	var received []string
	json.Unmarshal(actual[0], &directory)
	json.Unmarshal(actual[1], &value)
	json.Unmarshal(actual[2], &received)
	requireSameDirectory(t, directory, request.Directory)
	if value != "space,=\n$literal" || !reflect.DeepEqual(received, arguments) {
		t.Fatalf("incorrect metadata: %s", output.String())
	}
	if errorOutput.String() != "separate error output" {
		t.Fatal("stderr was not preserved")
	}
}

func TestExecuteStreams(t *testing.T) {
	for _, mode := range []string{"copy", "large"} {
		t.Run(mode, func(t *testing.T) {
			request := helperRequest(t, mode)
			input := bytes.Repeat([]byte{0, 255, 'z'}, 100000)
			var output, errorOutput bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			code, err := executeTest(t, ctx, request, bytes.NewReader(input), &output, &errorOutput)
			if err != nil || code != 0 {
				t.Fatalf("execution failed: %d, %v", code, err)
			}
			if mode == "copy" && !bytes.Equal(output.Bytes(), input) {
				t.Fatal("stdin/stdout changed")
			}
			if mode == "large" && (!bytes.Equal(output.Bytes(), bytes.Repeat([]byte{0, 255, 'x'}, 4096*64)) || !bytes.Equal(errorOutput.Bytes(), bytes.Repeat([]byte{0, 254, 'y'}, 4096*64))) {
				t.Fatal("large output was truncated or changed")
			}
		})
	}
}

func TestExecuteDoesNotWaitForInput(t *testing.T) {
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := executeTest(t, ctx, helperRequest(t, "exit"), input, nil, nil)
	if err != nil || code != 37 {
		t.Fatalf("execution waited for input: %d, %v", code, err)
	}
}

// readyWriter triggers cancellation only after the remote command starts.
type readyWriter struct {
	once  sync.Once
	ready func()
}

func (w *readyWriter) Write(data []byte) (int, error) {
	w.once.Do(w.ready)
	return len(data), nil
}

func TestExecuteCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := executeTest(t, ctx, helperRequest(t, "wait"), nil, &readyWriter{ready: cancel}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("incorrect cancellation error: %v", err)
	}
}

func TestExecuteStartupFailures(t *testing.T) {
	// Failures are reported with conventional exit codes and identify their
	// cause, rather than (for example) reporting a missing working directory
	// as a missing executable.
	for _, test := range []struct {
		failure, message string
		code             int
	}{
		{"missing-command", "unable to find command", 127},
		{"missing-directory", "unable to access working directory", 126},
		{"file-directory", "working directory is not a directory", 126},
	} {
		t.Run(test.failure, func(t *testing.T) {
			request := helperRequest(t, "exit")
			switch test.failure {
			case "missing-command":
				request.Arguments[0] = "mutagen-no-such-command"
			case "missing-directory":
				request.Directory = filepath.Join(request.Directory, "missing")
			case "file-directory":
				request.Directory = filepath.Join(request.Directory, "file")
				if err := os.WriteFile(request.Directory, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if code, err := executeTest(t, ctx, request, nil, nil, nil); err == nil || code != test.code ||
				!strings.Contains(err.Error(), test.message) {
				t.Fatalf("incorrect startup failure: %d, %v", code, err)
			}
		})
	}
}

func TestExecutePathOverride(t *testing.T) {
	request := helperRequest(t, "exit")
	request.Environment = append(request.Environment, "PATH="+filepath.Dir(request.Arguments[0]))
	request.Arguments[0] = filepath.Base(request.Arguments[0])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if code, err := executeTest(t, ctx, request, nil, nil, nil); err != nil || code != 37 {
		t.Fatalf("PATH override failed: %d, %v", code, err)
	}
}

func TestInvalidRequests(t *testing.T) {
	for _, request := range []*Request{nil, {}, {Arguments: []string{""}}, {Arguments: []string{"echo", "\x00"}}, {Arguments: []string{"echo", "\xff"}}, {Arguments: []string{"echo"}, Environment: []string{"=value"}}, {Arguments: []string{"echo"}, Environment: []string{"name"}}} {
		if request.EnsureValid() == nil {
			t.Fatal("invalid request accepted")
		}
	}
	for _, path := range []string{"..", "../sibling", "/absolute", "a/../b", "a/", "./a", "a\\b"} {
		if (&Request{Arguments: []string{"echo"}, Path: path}).EnsureValid() == nil {
			t.Fatalf("escaping or non-canonical path %q accepted", path)
		}
	}
	for _, path := range []string{"", "a", "a/b", "..a/b"} {
		if err := (&Request{Arguments: []string{"echo"}, Path: path}).EnsureValid(); err != nil {
			t.Fatalf("valid path %q rejected: %v", path, err)
		}
	}
}

func TestWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(t.TempDir(), "elsewhere")
	for _, test := range []struct {
		path, directory, expected string
	}{
		{"", "", root},
		{"sub/dir", "", filepath.Join(root, "sub", "dir")},
		{"sub", "child", filepath.Join(root, "sub", "child")},
		{"sub", "..", root},
		{"sub", absolute, absolute},
		{"sub", "~", home},
	} {
		actual, err := workingDirectory(root, &Request{Path: test.path, Directory: test.directory})
		if err != nil || actual != test.expected {
			t.Errorf("incorrect directory for %q and %q: %q, %v", test.path, test.directory, actual, err)
		}
	}
}

func TestExecuteInMappedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub", "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	request := helperRequest(t, "inspect")
	request.Directory = ""
	request.Path = "sub/dir"
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if code, err := executeInRootTest(t, ctx, root, request, nil, &output, nil); err != nil || code != 0 {
		t.Fatalf("execution failed: %d, %v", code, err)
	}
	var actual []json.RawMessage
	var directory string
	if err := json.Unmarshal(output.Bytes(), &actual); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(actual[0], &directory); err != nil {
		t.Fatal(err)
	}
	requireSameDirectory(t, directory, filepath.Join(root, "sub", "dir"))
}

func TestRouteRejection(t *testing.T) {
	client, server := net.Pipe()
	go Reject(server, errors.New("beta is not connected"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Execute(ctx, client, helperRequest(t, "exit"), strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || err.Error() != "beta is not connected" {
		t.Fatalf("incorrect rejection error: %v", err)
	}
}

func TestMessageLimits(t *testing.T) {
	var buffer bytes.Buffer
	if err := writeMessage(&buffer, strings.Repeat("x", maximumMessageSize)); err == nil {
		t.Fatal("oversized message accepted")
	}
	binary.Write(&buffer, binary.BigEndian, uint32(maximumMessageSize+1))
	var request Request
	if err := readMessage(&buffer, &request); err == nil {
		t.Fatal("oversized incoming message accepted")
	}
}

// failingIO models a broken caller-owned standard stream.
type failingIO struct{}

func (failingIO) Read([]byte) (int, error)  { return 0, errors.New("input failure") }
func (failingIO) Write([]byte) (int, error) { return 0, errors.New("output failure") }

func TestStandardStreamFailures(t *testing.T) {
	for _, inputFailure := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var input io.Reader = strings.NewReader("output")
		var output io.Writer = failingIO{}
		if inputFailure {
			input, output = failingIO{}, io.Discard
		}
		_, err := executeTest(t, ctx, helperRequest(t, "copy"), input, output, nil)
		cancel()
		if err == nil {
			t.Fatal("standard stream failure was not reported")
		}
	}
}
