package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestChannelEndpointEnvironment(t *testing.T) {
	t.Setenv("MUTAGEN_REMOTE_ENDPOINT", "inherited")
	for _, name := range []string{"alpha", "beta"} {
		t.Run(name, func(t *testing.T) {
			endpoint := &Endpoint{Root: t.TempDir(), Alpha: name == "alpha", Execute: true}
			local, err := NewLocalChannel(nil, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { local.Close() })
			for kind, channel := range map[string]Channel{"local": local, "agent": testChannel(t, endpoint, nil)} {
				for _, requested := range []bool{false, true} {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					route, err := channel.Open(ctx)
					if err != nil {
						cancel()
						t.Fatal(err)
					}
					request := helperRequest(t, "endpoint")
					request.Prologue = requested
					request.Environment = append(request.Environment, "MUTAGEN_REMOTE_ENDPOINT=submitted")
					if runtime.GOOS == "windows" {
						request.Environment = append(request.Environment, "mutagen_remote_endpoint=case-insensitive")
					}
					var output bytes.Buffer
					code, err := Execute(ctx, route, request, strings.NewReader(""), &output, io.Discard)
					cancel()
					if err != nil || code != 0 || output.String() != name {
						t.Fatalf("incorrect %s endpoint with prologue %t: %d, %q, %v", kind, requested, code, output.String(), err)
					}
				}
			}
		})
	}
}

func TestChannelWorkspacePrologue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("prologues require a POSIX shell")
	}
	for _, name := range []string{"alpha", "beta"} {
		for _, configured := range []string{"", "configured.sh"} {
			t.Run(name+"/"+configured, func(t *testing.T) {
				root := t.TempDir()
				fallback := "submitted"
				if configured != "" {
					if err := os.WriteFile(filepath.Join(root, configured), []byte("export REMOTE_VALUE=\"configured:$MUTAGEN_REMOTE_ENDPOINT\"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					fallback = "configured:" + name
				}
				endpoint := &Endpoint{Root: root, Alpha: name == "alpha", Execute: true, Prologue: configured}
				local, err := NewLocalChannel(nil, endpoint)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { local.Close() })
				for kind, channel := range map[string]Channel{"local": local, "agent": testChannel(t, endpoint, nil)} {
					// Reuse each channel across creation, editing, emptying, and
					// removal to ensure selection isn't cached at initialization.
					for _, step := range []struct{ name, script, expected string }{
						{"absent", "", fallback},
						{"create", "case \"$MUTAGEN_REMOTE_ENDPOINT\" in\nalpha) export REMOTE_VALUE=alpha-workspace;;\nbeta) export REMOTE_VALUE=beta-workspace;;\nesac\n", name + "-workspace"},
						{"edit", "export REMOTE_VALUE=edited\n", "edited"},
						{"empty", "", "submitted"},
						{"remove", "", fallback},
					} {
						override := filepath.Join(root, workspacePrologueName)
						if step.name == "remove" {
							if err := os.Remove(override); err != nil {
								t.Fatal(err)
							}
						} else if step.name != "absent" {
							if err := os.WriteFile(override, []byte(step.script), 0600); err != nil {
								t.Fatal(err)
							}
						}
						for _, requested := range []bool{true, false} {
							ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							route, err := channel.Open(ctx)
							if err != nil {
								cancel()
								t.Fatal(err)
							}
							request := &Request{
								Arguments:   []string{"sh", "-c", `printf '%s:%s' "$MUTAGEN_REMOTE_ENDPOINT" "$REMOTE_VALUE"`},
								Environment: []string{"REMOTE_VALUE=submitted", "MUTAGEN_REMOTE_ENDPOINT=incorrect"},
								Prologue:    requested,
							}
							var output bytes.Buffer
							code, err := Execute(ctx, route, request, strings.NewReader(""), &output, io.Discard)
							cancel()
							expected := "submitted"
							if requested {
								expected = step.expected
							}
							if err != nil || code != 0 || output.String() != name+":"+expected {
								t.Fatalf("incorrect %s output after %s with prologue %t: %d, %q, %v", kind, step.name, requested, code, output.String(), err)
							}
						}
					}
				}
			})
		}
	}
}

func TestWorkspacePrologueExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("prologues require a POSIX shell")
	}
	root, outside := t.TempDir(), t.TempDir()
	sub := filepath.Join(root, "sub")
	mkdirs(t, sub)
	// Conflicting scripts in command directories must not affect selection.
	for _, directory := range []string{sub, outside} {
		if err := os.WriteFile(filepath.Join(directory, workspacePrologueName), []byte("exit 1\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, workspacePrologueName), []byte("export REMOTE_VALUE=\"override:$REMOTE_VALUE\"\nset -- clobbered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"", outside} {
		arguments := []string{"", "two words", "$HOME", "*"}
		request := helperRequest(t, "inspect", arguments...)
		request.Environment = append(request.Environment, "REMOTE_VALUE=submitted")
		request.Path, request.Directory, request.Prologue = "sub", directory, true
		var output bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		code, err := executeInRootTest(t, ctx, root, filepath.Join(root, "missing-default.sh"), request, nil, &output, nil)
		cancel()
		if err != nil || code != 0 {
			t.Fatalf("workspace execution failed: %d, %v", code, err)
		}
		var actual []json.RawMessage
		if err := json.Unmarshal(output.Bytes(), &actual); err != nil {
			t.Fatal(err)
		}
		var cwd, value string
		var received []string
		json.Unmarshal(actual[0], &cwd)
		json.Unmarshal(actual[1], &value)
		json.Unmarshal(actual[2], &received)
		expected := sub
		if directory != "" {
			expected = directory
		}
		requireSameDirectory(t, cwd, expected)
		if value != "override:submitted" || !reflect.DeepEqual(received, arguments) {
			t.Fatalf("incorrect command metadata: %s", output.String())
		}
	}
}

func TestWorkspacePrologueFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("prologues require a POSIX shell")
	}
	for _, kind := range []string{"directory", "dangling", "loop", "unreadable", "status", "syntax", "exit"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			override := filepath.Join(root, workspacePrologueName)
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(override, 0700)
			case "dangling":
				err = os.Symlink("missing.sh", override)
			case "loop":
				err = os.Symlink(workspacePrologueName, override)
			case "unreadable":
				err = os.WriteFile(override, nil, 0000)
				if file, openErr := os.Open(override); openErr == nil {
					file.Close()
					t.Skip("process can read files without read permissions")
				}
			default:
				script := map[string]string{"status": "false\n", "syntax": "if then\n", "exit": "exit 0\n"}[kind]
				err = os.WriteFile(override, []byte(script), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			fallback := filepath.Join(root, "default.sh")
			if err := os.WriteFile(fallback, []byte("printf fallback\n"), 0600); err != nil {
				t.Fatal(err)
			}
			request := helperRequest(t, "endpoint")
			request.Prologue = true
			var output, errorOutput bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			code, err := executeInRootTest(t, ctx, root, fallback, request, nil, &output, &errorOutput)
			message := errorOutput.String()
			if err != nil {
				message += err.Error()
			}
			if code != 126 || output.Len() != 0 || !strings.Contains(message, override) {
				t.Fatalf("invalid override not reported: %d, %q, %s", code, output.String(), message)
			}

			// Disabling prologues must bypass both the invalid override and
			// the configured script, leaving commands available for repair.
			request.Prologue = false
			if code, err := executeInRootTest(t, ctx, root, fallback, request, nil, &output, nil); err != nil || code != 0 || output.String() != "beta" {
				t.Fatalf("disabled override affected execution: %d, %q, %v", code, output.String(), err)
			}
		})
	}
}

func TestWorkspacePrologueSymbolicLinkAndPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("prologues require a POSIX shell")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	mkdirs(t, bin)
	if err := os.WriteFile(filepath.Join(root, "setup.sh"), []byte("export PATH=\"$PWD/bin:$PATH\"\n"), 0600); err != nil {
		t.Fatal(err)
	} else if err := os.Symlink("setup.sh", filepath.Join(root, workspacePrologueName)); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(bin, "workspace-command"), []byte("#!/bin/sh\nprintf '%s' \"$MUTAGEN_REMOTE_ENDPOINT\"\nexit 37\n"), 0700); err != nil {
		t.Fatal(err)
	}
	request := &Request{Arguments: []string{"workspace-command"}, Prologue: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var output bytes.Buffer
	if code, err := executeInRootTest(t, ctx, root, "", request, nil, &output, nil); code != 37 || err != nil || output.String() != "beta" {
		t.Fatalf("workspace PATH or exit status lost: %d, %q, %v", code, output.String(), err)
	}
}

func TestWorkspacePrologueWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific behavior")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, workspacePrologueName), nil, 0600); err != nil {
		t.Fatal(err)
	}
	request := helperRequest(t, "endpoint")
	request.Prologue = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var output bytes.Buffer
	if code, err := executeInRootTest(t, ctx, root, "", request, nil, &output, nil); code != 126 || err == nil ||
		!strings.Contains(err.Error(), "not supported on Windows") || output.Len() != 0 {
		t.Fatalf("Windows override not rejected: %d, %q, %v", code, output.String(), err)
	}
	request.Prologue = false
	if code, err := executeInRootTest(t, ctx, root, "", request, nil, &output, nil); code != 0 || err != nil || output.String() != "beta" {
		t.Fatalf("disabled Windows override affected execution: %d, %q, %v", code, output.String(), err)
	}
}
