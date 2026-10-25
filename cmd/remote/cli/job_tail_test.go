//go:build !windows && !aix

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
			code := Run([]string{"-s", "sync_outer", "--wait", job.ID, "--tail", "1", "--no-flush"}, strings.NewReader(""), &output, &errors)
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
				// A lost job still reports status 255 when its log is available.
				status := filepath.Join(os.Getenv("MUTAGEN_DATA_DIRECTORY"), "remote-jobs", job.ID, "status")
				if err := os.Remove(status); err != nil {
					t.Fatal(err)
				}
				output.Reset()
				errors.Reset()
				code := Run([]string{"-s", "sync_outer", "--wait", job.ID, "--tail", "1", "--no-flush"}, strings.NewReader(""), &output, &errors)
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
	job, _, err := remote.StartJob(ctx, connection, &remote.Request{
		Arguments: []string{"sh", "-c", "printf 'started\\n'; sleep 1; printf 'finished\\n'"},
		Job:       &remote.JobRequest{Operation: remote.JobStart},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output, errors bytes.Buffer
	arguments := []string{"-s", "sync_outer", "--wait", job.ID, "--tail", "1", "--no-flush"}
	if code := Run(append(arguments, "--timeout", "0.1"), strings.NewReader(""), &output, &errors); code != timeoutExitCode || output.Len() != 0 {
		t.Fatalf("incorrect timed out wait: %d, %q, %q", code, output.String(), errors.String())
	}
	output.Reset()
	errors.Reset()
	if code := Run(arguments, strings.NewReader(""), &output, &errors); code != 0 || output.String() != "finished\n" {
		t.Fatalf("wait couldn't be resumed: %d, %q, %q", code, output.String(), errors.String())
	}
}
