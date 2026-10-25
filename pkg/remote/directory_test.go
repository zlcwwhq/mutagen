package remote

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExecuteDirectoryErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, directory, path, failed string
		kind                          DirectoryErrorKind
	}{
		{"missing-explicit", filepath.Join(root, "missing"), "", filepath.Join(root, "missing"), DirectoryErrorMissing},
		{"missing-mapped", "", "missing", filepath.Join(root, "missing"), DirectoryErrorMissing},
		{"not-directory", filepath.Join(root, "file"), "", filepath.Join(root, "file"), DirectoryErrorNotDirectory},
		{"invalid", "~mutagen-test-no-such-user-0f4b3", "", "~mutagen-test-no-such-user-0f4b3", DirectoryErrorInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request := helperRequest(t, "exit")
			request.Directory, request.Path = test.directory, test.path
			code, err := executeInRootTest(t, ctx, root, "", request, nil, nil, nil)
			var failure *DirectoryError
			if code != 126 || !errors.As(err, &failure) || failure.Kind != test.kind || failure.Path != test.failed ||
				!strings.Contains(err.Error(), test.failed) {
				t.Fatalf("incorrect structured directory failure: %d, %#v", code, err)
			}
		})
	}
}

func TestExecuteDirectoryAccessError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symbolic links")
	}
	root := t.TempDir()
	loop := filepath.Join(root, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	request := helperRequest(t, "exit")
	request.Directory = filepath.Join(loop, "child")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := executeInRootTest(t, ctx, root, "", request, nil, nil, nil)
	var failure *DirectoryError
	if code != 126 || !errors.As(err, &failure) || failure.Kind != DirectoryErrorInaccessible || failure.Path != request.Directory {
		t.Fatalf("access error classified incorrectly: %d, %#v", code, err)
	}
}

func TestStartJobDirectoryError(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "aix" {
		t.Skip("detached jobs unsupported")
	}
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	root := t.TempDir()
	request := helperRequest(t, "exit")
	request.Directory = filepath.Join(root, "missing")
	request.Job = &JobRequest{Operation: JobStart}
	client, server := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- Serve(context.Background(), server, root, "", false) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, code, err := StartJob(ctx, client, request)
	var failure *DirectoryError
	if code != 126 || !errors.As(err, &failure) || failure.Kind != DirectoryErrorMissing || failure.Path != request.Directory {
		t.Fatalf("job directory error lost in protocol: %d, %#v", code, err)
	}
	if err := <-served; err != nil && !errors.Is(err, io.EOF) {
		t.Fatal("server failed:", err)
	}
}
