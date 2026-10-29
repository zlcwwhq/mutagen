//go:build !windows && !aix

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestWaitJobTail(t *testing.T) {
	alpha, _ := setupPullSessions(t, map[string]string{"kept": "local"}, map[string]string{
		"results/kept": "remote", ".remote-prologue.sh": "exit 91\n",
	})
	beta := filepath.Join(filepath.Dir(alpha), "beta")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i, logPath := range []string{"", "results/train.log", "/dev/null"} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			router := &router{ctx: ctx, directory: alpha, selection: &remote.Selection{}, stderr: io.Discard}
			connection, _, err := router.dial(remote.OperationExecute)
			if err != nil {
				t.Fatal(err)
			}
			job, _, err := remote.StartJob(ctx, connection, &remote.Request{
				Arguments: []string{"sh", "-c", "printf 'first\\nlast\\n'; exit 7"},
				Job:       &remote.JobRequest{Operation: remote.JobStart, Log: logPath},
			})
			if err != nil {
				t.Fatal(err)
			}
			var output, errors bytes.Buffer
			code := Run([]string{"--node", "beta", "--wait", job.ID, "--tail", "1", "--no-flush"}, strings.NewReader(""), &output, &errors)
			if code != 7 || !strings.Contains(errors.String(), "exited with status 7") {
				t.Fatalf("job status wasn't preserved: %d, %q", code, errors.String())
			}
			if logPath == "/dev/null" {
				if output.Len() != 0 || !strings.Contains(errors.String(), "not a regular file") {
					t.Fatalf("device log wasn't reported: %q, %q", output.String(), errors.String())
				}
			} else if output.String() != "last\n" || strings.Contains(errors.String(), "warning") {
				t.Fatalf("incorrect tail: %q, %q", output.String(), errors.String())
			}
			// The test relay rejects synchronization operations. Any accidental
			// flush is therefore visible as a warning or a failure above.
			if _, err := os.Stat(filepath.Join(alpha, "results/train.log")); !os.IsNotExist(err) {
				t.Fatalf("ignored log appeared locally: %v", err)
			}
			if logPath == "results/train.log" {
				// A lost job reports remote failure status 125 when its log is available.
				status := filepath.Join(os.Getenv("MUTAGEN_DATA_DIRECTORY"), "remote-jobs", job.ID, "status")
				if err := os.Remove(status); err != nil {
					t.Fatal(err)
				}
				output.Reset()
				errors.Reset()
				code := Run([]string{"--node", "beta", "--wait", job.ID, "--tail", "1", "--no-flush"}, strings.NewReader(""), &output, &errors)
				if code != failureExitCode || output.String() != "last\n" || !strings.Contains(errors.String(), "without recording its exit status") {
					t.Fatalf("lost job result changed: %d, %q, %q", code, output.String(), errors.String())
				}
			}
		})
	}
	if contents, err := os.ReadFile(filepath.Join(beta, "results/train.log")); err != nil || string(contents) != "first\nlast\n" {
		t.Fatalf("custom remote log changed: %q, %v", contents, err)
	}
}

func TestWaitJobTailTimeout(t *testing.T) {
	alpha, _ := setupPullSessions(t, map[string]string{"kept": "local"}, map[string]string{"kept": "remote"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	router := &router{ctx: ctx, directory: alpha, selection: &remote.Selection{}, stderr: io.Discard}
	connection, _, err := router.dial(remote.OperationExecute)
	if err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(t.TempDir(), "release")
	job, _, err := remote.StartJob(ctx, connection, &remote.Request{
		Arguments: []string{"sh", "-c", `printf 'started\n'; while [ ! -e "$1" ]; do sleep 0.02; done; printf 'finished\n'`, "sh", release},
		Job:       &remote.JobRequest{Operation: remote.JobStart},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForContents(t, job.Log, "started\n")

	// If the timeout expires while the job is running, then the log's current
	// tail is printed before the usual report, and the job isn't affected.
	// To receive the tail in time, remote stops waiting shortly before the
	// timeout, which other waits, including those that follow the log, which
	// has been printed already, don't need to do. A zero tail prints nothing.
	report := fmt.Sprintf("remote: timed out after 1s while waiting for the job\nremote: job %[1]s is still running; remote --wait %[1]s resumes waiting, and remote --kill %[1]s terminates it\n", job.ID)
	for _, test := range []struct {
		options []string
		output  string
		early   bool
	}{
		{[]string{"--tail", "1"}, "started\n", true},
		{[]string{"--tail", "0"}, "", false},
		{[]string{"--follow"}, "started\n", false},
		{[]string{"--follow", "--tail", "0"}, "", false},
	} {
		var output, errors bytes.Buffer
		arguments := append([]string{"--node", "beta", "--wait", job.ID, "--no-flush", "--timeout", "1"}, test.options...)
		started := time.Now()
		if code := Run(arguments, strings.NewReader(""), &output, &errors); code != timeoutExitCode || output.String() != test.output || errors.String() != report {
			t.Fatalf("incorrect timed out wait with %v: %d, %q, %q", test.options, code, output.String(), errors.String())
		} else if elapsed := time.Since(started); test.early != (elapsed < time.Second) {
			t.Fatalf("timed out wait with %v took %v", test.options, elapsed)
		}
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var output, errors bytes.Buffer
	if code := Run([]string{"--node", "beta", "--wait", job.ID, "--tail", "1", "--no-flush"}, strings.NewReader(""), &output, &errors); code != 0 || output.String() != "finished\n" {
		t.Fatalf("wait couldn't be resumed: %d, %q, %q", code, output.String(), errors.String())
	}
}

// waitForContents waits until a file has the specified contents, failing the
// test if that takes longer than 10 seconds.
func waitForContents(t *testing.T, path, contents string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(path); err == nil && string(data) == contents {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("%s didn't have contents %q: %q, %v", path, contents, data, err)
		}
	}
}

// syncBuffer is a buffer that's safe for concurrent use, through which tests
// watch the output of commands that run concurrently.
type syncBuffer struct {
	// lock serializes access to data.
	lock sync.Mutex
	// data is the output written so far.
	data bytes.Buffer
}

// Write implements io.Writer.Write.
func (b *syncBuffer) Write(data []byte) (int, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.data.Write(data)
}

// String returns the output written so far.
func (b *syncBuffer) String() string {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.data.String()
}

// waitFor waits until the output contains the specified value, failing the
// test if that takes longer than 10 seconds.
func (b *syncBuffer) waitFor(t *testing.T, value string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(b.String(), value); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("output didn't contain %q: %q", value, b.String())
		}
	}
}

// runInBackground runs the command line concurrently, with output written to
// the specified buffers, and returns a channel that receives its exit code.
func runInBackground(arguments []string, output, errors *syncBuffer) <-chan int {
	code := make(chan int, 1)
	go func() {
		code <- Run(arguments, strings.NewReader(""), output, errors)
	}()
	return code
}

// receiveCode receives the exit code of a command line run by runInBackground,
// failing the test if it doesn't exit within 20 seconds.
func receiveCode(t *testing.T, code <-chan int) int {
	t.Helper()
	select {
	case result := <-code:
		return result
	case <-time.After(20 * time.Second):
		t.Fatal("remote didn't exit")
		return 0
	}
}

func TestWaitJobFollow(t *testing.T) {
	alpha, _ := setupPullSessions(t, map[string]string{"kept": "local"}, map[string]string{"results/kept": "remote"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i, logPath := range []string{"", "results/train.log", "/dev/null"} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			router := &router{ctx: ctx, directory: alpha, selection: &remote.Selection{}, stderr: io.Discard}
			connection, _, err := router.dial(remote.OperationExecute)
			if err != nil {
				t.Fatal(err)
			}
			release := filepath.Join(t.TempDir(), "release")
			job, _, err := remote.StartJob(ctx, connection, &remote.Request{
				Arguments: []string{"sh", "-c", `printf 'first\n'; while [ ! -e "$1" ]; do sleep 0.02; done; printf 'progress\rlast'; exit 7`, "sh", release},
				Job:       &remote.JobRequest{Operation: remote.JobStart, Log: logPath},
			})
			if err != nil {
				t.Fatal(err)
			}

			// The log is printed while the job runs, or a log that can't be
			// followed is reported once, and the job's status is preserved.
			var output, errors syncBuffer
			code := runInBackground([]string{"--node", "beta", "--wait", job.ID, "--follow", "--no-flush"}, &output, &errors)
			if logPath == "/dev/null" {
				errors.waitFor(t, "/dev/null is not a regular file")
			} else {
				output.waitFor(t, "first\n")
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if code := receiveCode(t, code); code != 7 {
				t.Fatalf("job status wasn't preserved: %d, %q", code, errors.String())
			}
			if logPath == "/dev/null" {
				if output.String() != "" || strings.Count(errors.String(), "warning") != 1 || !strings.HasPrefix(errors.String(), "remote: warning: unable to follow the log of job "+job.ID+": ") {
					t.Fatalf("incorrect report for a device log: %q, %q", output.String(), errors.String())
				}
				return
			}

			// The log's last line, which a carriage return redrew without
			// ending it, is printed unchanged, and the report starts a new
			// line of its own.
			if output.String() != "first\nprogress\rlast" || !strings.HasPrefix(errors.String(), "\nremote: job "+job.ID+" exited with status 7 after ") || strings.Contains(errors.String(), "warning") {
				t.Fatalf("incorrect followed output: %q, %q", output.String(), errors.String())
			}

			// Following an ended job prints its log from the requested tail,
			// and following without a tail prints the whole log.
			for _, test := range []struct {
				options []string
				output  string
			}{
				{[]string{"--tail", "1"}, "progress\rlast"},
				{[]string{"--tail", "0"}, ""},
				{nil, "first\nprogress\rlast"},
			} {
				var output, errors bytes.Buffer
				arguments := append([]string{"--node", "beta", "--wait", job.ID, "--follow", "--no-flush"}, test.options...)
				if code := Run(arguments, strings.NewReader(""), &output, &errors); code != 7 || output.String() != test.output {
					t.Fatalf("incorrect follow of an ended job with %v: %d, %q, %q", test.options, code, output.String(), errors.String())
				}
			}
		})
	}
}

// relayDroppableSession connects the endpoints of a session as relaySession
// does, without ignores, and returns a function that drops the routes that are
// open, as a session that reconnects would.
func relayDroppableSession(t *testing.T, session string, alpha, beta string) func() {
	t.Helper()
	submitting, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: session, Root: alpha, Alpha: true, Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	executing, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: session, Root: beta, Execute: true})
	if err != nil {
		submitting.Close()
		t.Fatal(err)
	}
	var lock sync.Mutex
	var open []io.Closer
	var routes sync.WaitGroup
	routes.Add(1)
	go func() {
		defer routes.Done()
		for {
			submission, err := submitting.Accept()
			if err != nil {
				return
			}
			routes.Add(1)
			go func() {
				defer routes.Done()
				if header, err := remote.ReceiveSubmission(context.Background(), submission); err != nil {
					remote.Reject(submission, err)
				} else if !replyTestRoute(submission, header, session) {
					return
				} else if header.Operation == remote.OperationNodes {
					replyTestNodes(submission, session, alpha, beta)
				} else if header.Operation != remote.OperationExecute {
					remote.Reject(submission, errors.New("unsupported operation"))
				} else if execution, err := executing.Open(context.Background()); err != nil {
					remote.Reject(submission, err)
				} else {
					lock.Lock()
					open = append(open, submission, execution)
					lock.Unlock()
					remote.Splice(submission, execution)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		submitting.Close()
		executing.Close()
		routes.Wait()
	})
	return func() {
		lock.Lock()
		defer lock.Unlock()
		for _, connection := range open {
			connection.Close()
		}
		open = nil
	}
}

func TestWaitJobFollowReconnect(t *testing.T) {
	// Keep the data directory short enough for Unix domain sockets on macOS.
	data, err := os.MkdirTemp("", "mr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	beta := t.TempDir()
	drop := relayDroppableSession(t, "sync_follow", t.TempDir(), beta)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connection, _, err := (&remote.Selection{Target: &remote.Target{SourceSession: "sync_follow", SourceAlpha: true}}).Dial(ctx, beta, remote.OperationExecute)
	if err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(t.TempDir(), "release")
	job, _, err := remote.StartJob(ctx, connection, &remote.Request{
		Arguments: []string{"sh", "-c", `printf 'one\n'; while [ ! -e "$1" ]; do sleep 0.02; done; printf 'two\n'; exit 7`, "sh", release},
		Job:       &remote.JobRequest{Operation: remote.JobStart},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Losing the connection to the executing endpoint while following makes
	// remote retry, and the retried wait resumes where the lost one stopped,
	// so that no output is repeated or skipped.
	var output, errors syncBuffer
	code := runInBackground([]string{"--node", "beta", "--wait", job.ID, "--follow", "--no-flush"}, &output, &errors)
	output.waitFor(t, "one\n")
	drop()
	errors.waitFor(t, "so remote retries for up to")
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if code := receiveCode(t, code); code != 7 || output.String() != "one\ntwo\n" || !strings.Contains(errors.String(), "exited with status 7") {
		t.Fatalf("incorrect resumed follow: %d, %q, %q", code, output.String(), errors.String())
	}
}

func TestDetachIgnoredLogWarning(t *testing.T) {
	alpha, _ := setupPullSessions(t, map[string]string{"kept": "local"}, map[string]string{"results/kept": "remote"})
	t.Chdir(alpha)
	for _, test := range []struct {
		log    string
		warned bool
	}{
		{"results/train.log", true},
		{"results/train.log", true},
		{"train.log", false},
	} {
		var output, errors bytes.Buffer
		arguments := []string{"--node", "beta", "--detach", "--log", test.log, "--no-flush", "sh", "-c", "printf 'done\\n'"}
		if code := Run(arguments, strings.NewReader(""), &output, &errors); code != 0 {
			t.Fatalf("unable to start job: %d, %q", code, errors.String())
		}
		id := strings.TrimSpace(output.String())

		// Every job with an ignored log is warned about, along with the wait
		// that prints the log's end.
		warning := "remote: warning: the session ignores results, so the log isn't synchronized; read it with remote cat or remote --wait " + id + " --tail <lines>, or copy it with remote --pull\n"
		if test.warned && !strings.Contains(errors.String(), warning) || !test.warned && strings.Contains(errors.String(), "warning") {
			t.Fatalf("incorrect warning for %s: %q", test.log, errors.String())
		}
		output.Reset()
		errors.Reset()
		if code := Run([]string{"--node", "beta", "--wait", id, "--tail", "1", "--no-flush"}, strings.NewReader(""), &output, &errors); code != 0 || output.String() != "done\n" {
			t.Fatalf("suggested wait didn't print the log: %d, %q, %q", code, output.String(), errors.String())
		}
	}
}
