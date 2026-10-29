package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
	case "endpoint":
		fmt.Fprint(os.Stdout, os.Getenv("MUTAGEN_REMOTE_ENDPOINT"))
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
	case "start-job":
		// Start a detached job in the working directory, which serves as the
		// root, and exit at once, as an executor may.
		root, _ := os.Getwd()
		response := startJob(root, "", false, &Request{Arguments: arguments[1:], Job: &JobRequest{Operation: JobStart}})
		if response.Job == nil {
			fmt.Fprint(os.Stderr, response.Error)
			os.Exit(1)
		}
		fmt.Fprint(os.Stdout, response.Job.ID)
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

// useTerminationGracePeriod sets the termination grace period for a test.
func useTerminationGracePeriod(t *testing.T, period time.Duration) {
	t.Helper()
	previous := terminationGracePeriod
	terminationGracePeriod = period
	t.Cleanup(func() { terminationGracePeriod = previous })
}

// executeTest uses a real duplex connection and waits for server cleanup,
// which takes up to the termination grace period for cancelled commands, so
// tests that cancel commands should shorten it (see useTerminationGracePeriod).
// Requests specify absolute working directories, so no root is needed.
func executeTest(t *testing.T, ctx context.Context, request *Request, input io.Reader, output, errorOutput io.Writer) (int, error) {
	t.Helper()
	return executeInRootTest(t, ctx, "", "", request, input, output, errorOutput)
}

// executeInRootTest is executeTest with an explicit synchronization root and
// resolved prologue.
func executeInRootTest(t *testing.T, ctx context.Context, root, prologue string, request *Request, input io.Reader, output, errorOutput io.Writer) (int, error) {
	t.Helper()
	client, server := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- Serve(context.Background(), server, root, prologue, false, "") }()
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

func TestExecuteConnectionLoss(t *testing.T) {
	// The executor's context is cancelled when its channel closes, such as
	// when the session stops synchronizing, which closes the connection while
	// the command runs and terminates the command. The client distinguishes
	// this from other failures.
	client, server := net.Pipe()
	executorCtx, closeExecutor := context.WithCancel(context.Background())
	defer closeExecutor()
	served := make(chan error, 1)
	go func() { served <- Serve(executorCtx, server, "", "", false, "") }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Execute(ctx, client, helperRequest(t, "wait"), strings.NewReader(""), &readyWriter{ready: closeExecutor}, io.Discard)
	if !errors.Is(err, ErrConnectionLost) {
		t.Fatal("connection loss not reported:", err)
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not terminate")
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

	// File requests must name a path inside the root, without a command.
	for _, path := range []string{"..", "../sibling", "/absolute", "a/../b", "a/", "a\\b", "a\x00"} {
		if (&Request{File: path}).EnsureValid() == nil {
			t.Fatalf("invalid file path %q accepted", path)
		}
	}
	if (&Request{File: "a", Arguments: []string{"echo"}}).EnsureValid() == nil {
		t.Fatal("file request with a command accepted")
	} else if err := (&Request{File: "a/b"}).EnsureValid(); err != nil {
		t.Fatal("valid file request rejected:", err)
	}
}

func TestFetch(t *testing.T) {
	root := t.TempDir()
	large := bytes.Repeat([]byte("x"), MaximumComparedFileSize+1)
	for name, contents := range map[string][]byte{"file": []byte("a\nb"), "empty": nil, "large": large} {
		if err := os.WriteFile(filepath.Join(root, name), contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	mkdirs(t, filepath.Join(root, "directory"))
	digest := func(data []byte) string {
		return fmt.Sprintf("%x", sha256.Sum256(data))
	}
	expected := map[string]*Entry{
		"":           {Kind: EntryDirectory},
		".":          {Kind: EntryDirectory},
		"file":       {Kind: EntryFile, Size: 3, Digest: digest([]byte("a\nb")), Data: []byte("a\nb")},
		"empty":      {Kind: EntryFile, Digest: digest(nil)},
		"large":      {Kind: EntryFile, Size: int64(len(large)), Digest: digest(large)},
		"directory":  {Kind: EntryDirectory},
		"missing":    {Kind: EntryMissing},
		"file/child": {Kind: EntryMissing},
	}
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(filepath.Join(root, "executable"), []byte("#!/bin/sh\n"), 0700); err != nil {
			t.Fatal(err)
		} else if err := os.Symlink("file", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		expected["executable"] = &Entry{Kind: EntryFile, Size: 10, Executable: true, Digest: digest([]byte("#!/bin/sh\n")), Data: []byte("#!/bin/sh\n")}
		expected["link"] = &Entry{Kind: EntrySymbolicLink, Target: "file"}
	}

	// Entries are read identically on both endpoints, with file contents
	// included only up to the comparison limit.
	for name, entry := range expected {
		client, server := net.Pipe()
		go Serve(context.Background(), server, root, "", false, "")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		fetched, err := Fetch(ctx, client, name)
		cancel()
		local, localErr := ReadEntry(context.Background(), filepath.Join(root, name))
		if err != nil || localErr != nil {
			t.Fatalf("unable to read %s: %v, %v", name, err, localErr)
		}
		metadata := *entry
		metadata.Data = nil
		for _, actual := range []*Entry{fetched, local} {
			actualMetadata := *actual
			actualMetadata.Data = nil
			if !bytes.Equal(actual.Data, entry.Data) || !reflect.DeepEqual(actualMetadata, metadata) {
				t.Errorf("%s read incorrectly: %+v", name, actual)
			}
		}
	}
}

func TestFetchFileRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file")
	contents := []byte("root file\n")
	if err := os.WriteFile(root, contents, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "."} {
		client, server := net.Pipe()
		go Serve(context.Background(), server, root, "", false, "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		entry, err := Fetch(ctx, client, path)
		cancel()
		if err != nil {
			t.Fatal("unable to fetch root:", err)
		} else if entry.Kind != EntryFile || !bytes.Equal(entry.Data, contents) {
			t.Fatalf("incorrect root contents: %+v", entry)
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
	if code, err := executeInRootTest(t, ctx, root, "", request, nil, &output, nil); err != nil || code != 0 {
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

func TestResolvePrologue(t *testing.T) {
	root := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(t.TempDir(), "prologue.sh")
	for _, test := range []struct {
		prologue, expected string
	}{
		{"", ""},
		{"prologue.sh", filepath.Join(root, "prologue.sh")},
		{"tools/../env.sh", filepath.Join(root, "env.sh")},
		{absolute, absolute},
		{"~/prologue.sh", filepath.Join(home, "prologue.sh")},
	} {
		if actual, err := resolvePrologue(root, test.prologue); err != nil || actual != test.expected {
			t.Errorf("incorrect resolution of %q: %q, %v", test.prologue, actual, err)
		}
	}
}

func TestExecutePrologue(t *testing.T) {
	// Prologues are sourced by a POSIX shell.
	if runtime.GOOS == "windows" {
		t.Skip()
	}

	// Create a command that's only found using the PATH set by the prologue,
	// as is the case for commands provided by conda environments.
	root := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bin")
	mkdirs(t, filepath.Join(root, "sub"), bin)
	command := "#!/bin/sh\nprintf 'found:%s' \"$REMOTE_VALUE\"\n"
	if err := os.WriteFile(filepath.Join(bin, "mutagen-prologue-command"), []byte(command), 0700); err != nil {
		t.Fatal(err)
	}
	writePrologue := func(name, contents string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// The prologue sees submitted variables, and changing its positional
	// parameters doesn't affect the command.
	prologue := writePrologue("prologue.sh", fmt.Sprintf(
		"PATH='%s':\"$PATH\"\nexport REMOTE_VALUE=\"prologue:$REMOTE_VALUE\"\nset -- clobbered\n", bin,
	))
	execute := func(prologue string, request *Request) (int, string, string, error) {
		t.Helper()
		var output, errorOutput bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		code, err := executeInRootTest(t, ctx, root, prologue, request, nil, &output, &errorOutput)
		return code, output.String(), errorOutput.String(), err
	}
	environment := []string{"REMOTE_VALUE=submitted"}

	// Requested prologues run before the command, which is located afterward.
	request := &Request{Arguments: []string{"mutagen-prologue-command"}, Environment: environment, Path: "sub", Prologue: true}
	if code, output, _, err := execute(prologue, request); err != nil || code != 0 || output != "found:prologue:submitted" {
		t.Fatalf("prologue not applied: %d, %q, %v", code, output, err)
	}

	// Prologues only run when requested.
	request.Prologue = false
	if code, _, _, err := execute(prologue, request); err == nil || code != 127 {
		t.Fatalf("unrequested prologue applied: %d, %v", code, err)
	}

	// Arguments, working directories, and exit codes are preserved.
	arguments := []string{"", "two words", "--help", "$HOME", "*"}
	request = helperRequest(t, "inspect", arguments...)
	request.Environment = append(request.Environment, environment...)
	request.Directory, request.Path, request.Prologue = "", "sub", true
	code, output, _, err := execute(prologue, request)
	if err != nil || code != 0 {
		t.Fatalf("execution failed: %d, %v", code, err)
	}
	var actual []json.RawMessage
	var directory, value string
	var received []string
	if err := json.Unmarshal([]byte(output), &actual); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(actual[0], &directory)
	json.Unmarshal(actual[1], &value)
	json.Unmarshal(actual[2], &received)
	requireSameDirectory(t, directory, filepath.Join(root, "sub"))
	if value != "prologue:submitted" || !reflect.DeepEqual(received, arguments) {
		t.Fatalf("incorrect metadata: %s", output)
	}
	request = helperRequest(t, "exit")
	request.Directory, request.Prologue = "", true
	if code, _, _, err := execute(prologue, request); err != nil || code != 37 {
		t.Fatalf("incorrect exit status: %d, %v", code, err)
	}

	// Commands that can't be found after the prologue are reported as such.
	request = &Request{Arguments: []string{"mutagen-no-such-command"}, Prologue: true}
	if code, _, _, err := execute(prologue, request); err != nil || code != 127 {
		t.Fatalf("missing command not reported: %d, %v", code, err)
	}

	// Prologue failures prevent the command from running, regardless of how
	// the prologue fails.
	request = &Request{Arguments: []string{"mutagen-prologue-command"}, Prologue: true}
	for name, contents := range map[string]string{
		"status":  "false\n",
		"exit":    "exit 0\n",
		"errexit": "set -e\nfalse\ntrue\n",
		"syntax":  "if then\n",
	} {
		failing := writePrologue(name+".sh", fmt.Sprintf("PATH='%s':\"$PATH\"\n%s", bin, contents))
		if code, output, errorOutput, err := execute(failing, request); err != nil || code != 126 || output != "" ||
			!strings.Contains(errorOutput, "prologue "+failing+" failed with exit status") {
			t.Errorf("%s prologue failure not reported: %d, %q, %q, %v", name, code, output, errorOutput, err)
		}
	}

	// Missing prologues are reported without running the command.
	if code, _, _, err := execute(filepath.Join(root, "missing.sh"), request); err == nil || code != 126 ||
		!strings.Contains(err.Error(), "unable to access prologue") {
		t.Fatalf("missing prologue not reported: %d, %v", code, err)
	}
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

func TestReceiveSubmission(t *testing.T) {
	receive := func(ctx context.Context, header any) (*Submission, error) {
		client, server := net.Pipe()
		defer client.Close()
		if header != nil {
			go writeMessage(client, header)
		}
		return ReceiveSubmission(ctx, server)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, operation := range []Operation{OperationExecute, OperationFlush, OperationConflicts, OperationClaim, OperationRelease, OperationTreeExecute, OperationNodes, OperationNodeConflicts, OperationResolve} {
		for _, progress := range []bool{false, true} {
			sent := &Submission{Operation: operation, Progress: progress}
			if received, err := receive(ctx, sent); err != nil || *received != *sent {
				t.Fatalf("header %+v received incorrectly: %+v, %v", sent, received, err)
			}
		}
	}
	if _, err := receive(ctx, &Submission{Operation: OperationResolve + 1}); err == nil {
		t.Fatal("unsupported operation accepted")
	}

	// Headers of earlier versions, which only specify the operation, don't
	// request progress, and progress isn't encoded unless it's requested, so
	// that headers of operations without it are unchanged.
	if received, err := receive(ctx, &struct{ Operation Operation }{OperationFlush}); err != nil || received.Operation != OperationFlush || received.Progress {
		t.Fatalf("earlier header received incorrectly: %+v, %v", received, err)
	} else if data, err := json.Marshal(&Submission{Operation: OperationFlush}); err != nil || string(data) != `{"Operation":1}` {
		t.Fatalf("header without progress encoded incorrectly: %s, %v", data, err)
	}

	// Cancellation interrupts the wait for a header.
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if _, err := receive(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not reported:", err)
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
		} else if errors.Is(err, ErrConnectionLost) {
			t.Fatal("standard stream failure reported as connection loss:", err)
		} else if ClassifyError(err) != ErrorKindOperation {
			t.Fatal("local standard stream failure misclassified:", err)
		}
	}
}
