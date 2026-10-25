//go:build !windows && !aix

package remote

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadJobLog(t *testing.T) {
	for _, test := range []struct {
		name, contents, expected string
		lines                    int64
		truncated                bool
	}{
		{"empty", "", "", 10, false},
		{"zero", "one\ntwo\n", "", 0, false},
		{"terminated", "one\ntwo\nthree\n", "two\nthree\n", 2, false},
		{"unterminated", "one\ntwo\nthree", "two\nthree", 2, false},
		{"one", "one\ntwo\n", "two\n", 1, false},
		{"blank", "one\n\n\n", "\n\n", 2, false},
		{"only-newline", "\n", "\n", 1, false},
		{"fewer", "one\ntwo", "one\ntwo", 30, false},
		{"maximum-count", "one\ntwo\n", "one\ntwo\n", int64(^uint64(0) >> 1), false},
		{"binary", "one\n\xff\x00\n", "\xff\x00\n", 1, false},
		{"large-prefix", strings.Repeat("x", MaximumJobLogSize) + "\nlast\n", "last\n", 1, false},
		{"long-line", strings.Repeat("x", MaximumJobLogSize+1), strings.Repeat("x", MaximumJobLogSize), 1, true},
		{"exact-limit", strings.Repeat("x", MaximumJobLogSize), strings.Repeat("x", MaximumJobLogSize), 1, false},
		{"boundary", "prefix\n" + strings.Repeat("x", MaximumJobLogSize-1) + "\n", strings.Repeat("x", MaximumJobLogSize-1) + "\n", 1, false},
		{"too-many", strings.Repeat("x\n", MaximumJobLogSize), strings.Repeat("x\n", MaximumJobLogSize/2), MaximumJobLogSize, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log")
			if err := os.WriteFile(path, []byte(test.contents), 0600); err != nil {
				t.Fatal(err)
			}
			log := readJobLog(context.Background(), path, test.lines)
			if log.Error != "" || log.Truncated != test.truncated || string(log.Data) != test.expected {
				t.Fatalf("incorrect log: %d bytes, truncated %t, error %q (expected %d bytes, truncated %t)", len(log.Data), log.Truncated, log.Error, len(test.expected), test.truncated)
			}
		})
	}
}

func TestReadJobLogFailures(t *testing.T) {
	base := t.TempDir()
	fifo := filepath.Join(base, "fifo")
	if err := mkfifo(fifo); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{base, fifo, link, "/dev/null", filepath.Join(base, "missing")} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			// Bound the test if opening a FIFO regresses to a blocking open.
			// The open itself must be nonblocking; context cannot interrupt it.
			result := make(chan *JobLog, 1)
			go func() { result <- readJobLog(context.Background(), path, 1) }()
			select {
			case log := <-result:
				if log.Error == "" || len(log.Data) != 0 {
					t.Fatalf("unreadable log returned data: %+v", log)
				}
			case <-time.After(time.Second):
				t.Fatal("reading a non-regular log blocked")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if log := readJobLog(ctx, filepath.Join(base, "missing"), 1); log.Error != context.Canceled.Error() {
		t.Fatalf("cancelled read not reported: %+v", log)
	}
}

func TestJobLogBuffer(t *testing.T) {
	var buffer jobLogBuffer
	for i := 0; i < 5; i++ {
		data := bytes.Repeat([]byte("x"), MaximumJobLogSize/2)
		if n, err := buffer.Write(data); n != len(data) || err != nil {
			t.Fatalf("unexpected buffer write result: %d, %v", n, err)
		}
	}
	if len(buffer.data) != MaximumJobLogSize || !buffer.overflow {
		t.Fatalf("buffer didn't bound log data: %d bytes, overflow %t", len(buffer.data), buffer.overflow)
	}
}

func TestWaitJobWithTail(t *testing.T) {
	useTestJobs(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "results"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, logPath := range []string{"", "results/train.log", filepath.Join(t.TempDir(), "external.log")} {
		job := startTestJob(t, root, logPath, "sh", "-c", "printf 'one\\ntwo\\nthree'; exit 7")
		ended, log, err := WaitJobWithTail(ctx, jobRoute(ctx, root), job.ID, 2)
		if err != nil {
			t.Fatal(err)
		} else if ended.State != JobExited || ended.ExitCode != 7 || log == nil || log.Error != "" || log.Truncated || string(log.Data) != "two\nthree" {
			t.Fatalf("incorrect wait result: %+v, %+v", ended, log)
		}

		// Missing logs don't turn a known job status into an RPC error, and
		// --tail 0 doesn't attempt to read them at all.
		if err := os.Remove(job.Log); err != nil {
			t.Fatal(err)
		}
		ended, log, err = WaitJobWithTail(ctx, jobRoute(ctx, root), job.ID, 2)
		if err != nil || ended.ExitCode != 7 || log == nil || log.Error == "" {
			t.Fatalf("missing log changed wait result: %+v, %+v, %v", ended, log, err)
		}
		ended, log, err = WaitJobWithTail(ctx, jobRoute(ctx, root), job.ID, 0)
		if err != nil || ended.ExitCode != 7 || log != nil {
			t.Fatalf("zero count attempted log reading: %+v, %+v, %v", ended, log, err)
		}
	}

	// An abandoned wait doesn't return partial log output or affect the job.
	job := startTestJob(t, root, "", "sh", "-c", "printf 'ready\\n'; sleep 1; printf 'done\\n'")
	waiting, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if ended, log, err := WaitJobWithTail(waiting, jobRoute(ctx, root), job.ID, 1); err == nil || ended != nil || log != nil {
		t.Fatalf("abandoned wait returned output: %+v, %+v, %v", ended, log, err)
	}
	if ended, log, err := WaitJobWithTail(ctx, jobRoute(ctx, root), job.ID, 1); err != nil || ended.ExitCode != 0 || string(log.Data) != "done\n" {
		t.Fatalf("retry didn't return final log: %+v, %+v, %v", ended, log, err)
	}
}
