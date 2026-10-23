package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
)

func TestParseShebang(t *testing.T) {
	for _, test := range []struct {
		contents              string
		interpreter, argument string
		found, invalid        bool
	}{
		{"echo hi\n", "", "", false, false},
		{"", "", "", false, false},
		{" #!/bin/sh\n", "", "", false, false},
		{"#!/bin/sh\necho hi\n", "/bin/sh", "", true, false},
		{"#!/bin/sh", "/bin/sh", "", true, false},
		{"#! /usr/bin/env python3\n", "/usr/bin/env", "python3", true, false},
		// Like Linux, everything after the interpreter is a single argument.
		{"#!/usr/bin/env -S bash -e\n", "/usr/bin/env", "-S bash -e", true, false},
		{"#!/bin/bash\t-eu \t\n", "/bin/bash", "-eu", true, false},
		// Windows line endings don't become part of the interpreter's name.
		{"#!/bin/sh\r\necho hi\r\n", "/bin/sh", "", true, false},
		{"#!\n", "", "", true, true},
		{"#!  \n", "", "", true, true},
		{"#!/bin/\x00sh\n", "", "", true, true},
	} {
		interpreter, argument, found, err := parseShebang([]byte(test.contents))
		if (err != nil) != test.invalid || found != test.found || interpreter != test.interpreter || argument != test.argument {
			t.Errorf("incorrect parsing of %q: %q, %q, %t, %v", test.contents, interpreter, argument, found, err)
		}
	}
}

func TestScriptRequestValidation(t *testing.T) {
	script := &Script{Name: "-", Contents: []byte("echo hi\n")}
	if err := (&Request{Script: script}).EnsureValid(); err != nil {
		t.Fatal("script without arguments rejected:", err)
	} else if err := (&Request{Script: script, Arguments: []string{"a", ""}}).EnsureValid(); err != nil {
		t.Fatal("script with arguments rejected:", err)
	}
	for _, request := range []*Request{
		{Script: &Script{Name: "-", Contents: make([]byte, MaximumScriptSize+1)}},
		{Script: &Script{Name: "\xff", Contents: []byte("echo hi\n")}},
		{Script: script, File: "a"},
		{Script: script, Copy: "a"},
	} {
		if request.EnsureValid() == nil {
			t.Errorf("invalid script request accepted: %+v", request)
		}
	}

	// A script of the maximum size fits into a protocol message.
	request := &Request{Script: &Script{Name: "-", Contents: bytes.Repeat([]byte{0xff}, MaximumScriptSize)}, Arguments: []string{"a"}}
	if err := request.EnsureValid(); err != nil {
		t.Fatal("script of the maximum size rejected:", err)
	} else if err := writeMessage(io.Discard, request); err != nil {
		t.Fatal("script of the maximum size doesn't fit into a message:", err)
	}
}

// scriptFiles lists the files left in the directory of foreground scripts.
func scriptFiles(t *testing.T) []string {
	t.Helper()
	directory, err := filesystem.Mutagen(false, scriptsDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestExecuteScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scripts aren't supported on Windows")
	}
	useTestRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Scripts without a #! line run with bash, with the script's arguments
	// and the forwarded input.
	request := &Request{
		Script:    &Script{Name: "-", Contents: []byte("if [[ $1 == 'a b' ]]; then echo \"yes $2\"; fi\necho \"$MUTAGEN_REMOTE_ENDPOINT\"\ncat\n")},
		Arguments: []string{"a b", "$HOME"},
		Directory: t.TempDir(),
	}
	var output, errorOutput bytes.Buffer
	if code, err := executeTest(t, ctx, request, strings.NewReader("input\n"), &output, &errorOutput); err != nil || code != 0 {
		t.Fatalf("script failed: %d, %v, %q", code, err, errorOutput.String())
	} else if output.String() != "yes $HOME\nbeta\ninput\n" {
		t.Fatalf("incorrect script output: %q", output.String())
	}

	// A #! line selects the interpreter, which receives the path of the
	// script's file, which is removed once the script exits.
	request.Script.Contents = []byte("#!/bin/sh -e\necho \"$0\"\nfalse\necho unreachable\n")
	output.Reset()
	if code, err := executeTest(t, ctx, request, nil, &output, nil); err != nil || code != 1 {
		t.Fatalf("incorrect script result: %d, %v", code, err)
	}
	path := strings.TrimSpace(output.String())
	if filepath.Base(filepath.Dir(path)) != scriptsDirectoryName {
		t.Fatalf("script wasn't written to the script directory: %q", path)
	} else if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("script file wasn't removed:", err)
	} else if files := scriptFiles(t); len(files) != 0 {
		t.Fatal("script files left behind:", files)
	}
}

func TestExecuteScriptFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scripts aren't supported on Windows")
	}
	useTestRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A missing interpreter fails with 127 without leaving a file behind.
	for _, contents := range []string{"echo hi\n", "#!/nonexistent/interpreter\n"} {
		request := &Request{
			Script:      &Script{Name: "-", Contents: []byte(contents)},
			Environment: []string{"PATH=/nonexistent"},
			Directory:   t.TempDir(),
		}
		if code, err := executeTest(t, ctx, request, nil, nil, nil); code != 127 || err == nil || !strings.Contains(err.Error(), "interpreter") {
			t.Fatalf("incorrect missing interpreter result for %q: %d, %v", contents, code, err)
		}
	}

	// An invalid #! line fails with 126.
	request := &Request{Script: &Script{Name: "-", Contents: []byte("#!\necho hi\n")}, Directory: t.TempDir()}
	if code, err := executeTest(t, ctx, request, nil, nil, nil); code != 126 || err == nil {
		t.Fatalf("incorrect invalid #! line result: %d, %v", code, err)
	}

	// Cancellation terminates the script and removes its file. SIGTERM can
	// arrive after bash forks sleep but before sleep executes, when the child
	// doesn't act on it, in which case only SIGKILL ends sleep once the grace
	// period expires, so the grace period is shortened to keep that within the
	// time that executeTest waits for the server.
	useTerminationGracePeriod(t, 500*time.Millisecond)
	request = &Request{Script: &Script{Name: "-", Contents: []byte("printf ready\nsleep 60\n")}, Directory: t.TempDir()}
	cancelled, cancelScript := context.WithCancel(ctx)
	defer cancelScript()
	if _, err := executeTest(t, cancelled, request, nil, &readyWriter{ready: cancelScript}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("incorrect cancellation error: %v", err)
	}
	if files := scriptFiles(t); len(files) != 0 {
		t.Fatal("script files left behind after cancellation:", files)
	}
}

func TestExecuteScriptPrologue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scripts aren't supported on Windows")
	}
	useTestRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The prologue's PATH determines the interpreter of scripts without a #!
	// line.
	root := t.TempDir()
	interpreters := t.TempDir()
	fake := "#!/bin/sh\necho \"fake bash $(basename \"$1\" | cut -c1-7) $2\"\n"
	if err := os.WriteFile(filepath.Join(interpreters, "bash"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	prologue := filepath.Join(root, "prologue.sh")
	if err := os.WriteFile(prologue, []byte("PATH='"+interpreters+"':\"$PATH\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	request := &Request{Script: &Script{Name: "-", Contents: []byte("echo real\n")}, Arguments: []string{"argument"}, Prologue: true}
	var output bytes.Buffer
	if code, err := executeInRootTest(t, ctx, root, prologue, request, nil, &output, nil); err != nil || code != 0 {
		t.Fatalf("script failed: %d, %v", code, err)
	} else if output.String() != "fake bash script- argument\n" {
		t.Fatalf("incorrect interpreter: %q", output.String())
	}
}
