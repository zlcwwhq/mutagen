package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
)

// lockedBuilder is a strings.Builder that's safe for concurrent use.
type lockedBuilder struct {
	lock    sync.Mutex
	builder strings.Builder
}

// Write implements io.Writer.Write.
func (b *lockedBuilder) Write(data []byte) (int, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.builder.Write(data)
}

// String returns the data written.
func (b *lockedBuilder) String() string {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.builder.String()
}

// setProgressTiming sets the progress delay and intervals for a test.
func setProgressTiming(t *testing.T, delay, line, refresh time.Duration) {
	t.Helper()
	previousDelay, previousLine, previousRefresh := ProgressDelay, progressLineInterval, progressRefreshInterval
	ProgressDelay, progressLineInterval, progressRefreshInterval = delay, line, refresh
	t.Cleanup(func() {
		ProgressDelay, progressLineInterval, progressRefreshInterval = previousDelay, previousLine, previousRefresh
	})
}

// testClock is a manually advanced clock.
type testClock struct {
	lock sync.Mutex
	now  time.Time
}

// read returns the current time.
func (c *testClock) read() time.Time {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.now
}

// advance advances the clock.
func (c *testClock) advance(duration time.Duration) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.now = c.now.Add(duration)
}

// testProgressDisplay creates a display whose clock the test advances, from
// the perspective of alpha, whose root contains the working directory.
func testProgressDisplay(t *testing.T, output *progressOutput) (*progressDisplay, *testClock, func(*remote.Progress)) {
	t.Helper()
	clock := &testClock{now: time.Unix(1000, 0)}
	display := newProgressDisplay(output, &remote.Target{Alpha: true, Root: "/root", Path: "sub", Contained: true})
	display.now = clock.read
	return display, clock, display.begin()
}

func TestNewProgressOutput(t *testing.T) {
	// Progress is shown on output other than terminals only if enabled, and
	// never if disabled.
	enabled, disabled := true, false
	file, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var builder strings.Builder
	for _, writer := range []io.Writer{&builder, file} {
		if progress := newProgressOutput(writer, nil); progress.writer != nil {
			t.Errorf("progress shown by default on %T", writer)
		} else if progress := newProgressOutput(writer, &enabled); progress.writer == nil || progress.terminal {
			t.Errorf("progress not shown as lines on %T when enabled: %+v", writer, progress)
		} else if progress := newProgressOutput(writer, &disabled); progress.writer != nil {
			t.Errorf("progress shown on %T when disabled", writer)
		}
	}

	// No display is created if progress isn't shown, and a missing display
	// shows nothing.
	var display *progressDisplay = newProgressDisplay(&progressOutput{}, &remote.Target{})
	if display != nil || display.begin() != nil {
		t.Fatal("display created without output")
	}
	display.finish(context.Background(), nil)
}

func TestProgressLines(t *testing.T) {
	// Real timers never fire during this test, whose clock is advanced
	// manually.
	setProgressTiming(t, 3*time.Second, 10*time.Second, time.Hour)
	var output strings.Builder
	display, clock, update := testProgressDisplay(t, &progressOutput{writer: &output})

	// Nothing is shown before the delay elapses. The session is between
	// cycles only until it starts the requested one, which the next sample
	// shows, so the time until then belongs to the next phase.
	clock.advance(100 * time.Millisecond)
	update(&remote.Progress{Status: "watching"})
	clock.advance(900 * time.Millisecond)
	update(&remote.Progress{Status: "scanning"})
	if output.Len() != 0 {
		t.Fatalf("progress shown before the delay: %q", output.String())
	}

	// Lines are printed once the delay elapses and when the phase changes, but
	// not for other changes, such as in staging progress.
	clock.advance(2000 * time.Millisecond)
	update(&remote.Progress{Status: "scanning"})
	clock.advance(28 * time.Second)
	update(&remote.Progress{Status: "staging-alpha"})
	clock.advance(time.Second)
	update(&remote.Progress{Status: "staging-alpha", Staging: &remote.StagingProgress{
		ExpectedFiles: 1, ReceivedSize: 1 << 20, Path: "sub/out.bin", FileReceivedSize: 1 << 20, FileExpectedSize: 5 << 20,
	}})
	clock.advance(11 * time.Second)
	update(&remote.Progress{Status: "staging-alpha", Staging: &remote.StagingProgress{
		ExpectedFiles: 1, ReceivedSize: 4 << 20, Path: "sub/out.bin", FileReceivedSize: 4 << 20, FileExpectedSize: 5 << 20,
	}})
	clock.advance(200 * time.Millisecond)
	update(&remote.Progress{Status: "transitioning"})
	clock.advance(200 * time.Millisecond)
	update(&remote.Progress{Status: "saving"})
	clock.advance(100 * time.Millisecond)
	update(&remote.Progress{Status: "watching"})

	// Statuses that remote doesn't know are shown as they are, but neutralized
	// for terminals and kept on a single line.
	clock.advance(200 * time.Millisecond)
	update(&remote.Progress{Status: "future\x1b[2J\nstatus"})
	clock.advance(1700 * time.Millisecond)

	// The summary describes the phases that took at least a second (once
	// rounded) or staged files. Staging completed, so the rest of the last
	// file, which was being received when last sampled, is included.
	display.finish(context.Background(), nil)
	expected := "remote: synchronizing for 3s: scanning\n" +
		"remote: synchronizing for 31s: staging on alpha (local)\n" +
		"remote: synchronizing for 43s: applying changes\n" +
		"remote: synchronizing for 43s: saving\n" +
		"remote: synchronizing for 44s: future^[[2J?status\n" +
		"remote: synchronized in 45s: scanning 31s; staging on alpha (local) 12s, 1 file, 5.0 MiB; future^[[2J?status 2s\n"
	if output.String() != expected {
		t.Fatalf("incorrect progress output:\n%s\ninstead of:\n%s", output.String(), expected)
	}
}

func TestProgressTerminal(t *testing.T) {
	setProgressTiming(t, 3*time.Second, time.Hour, time.Hour)
	var output strings.Builder
	display, clock, update := testProgressDisplay(t, &progressOutput{writer: &output, terminal: true, width: 80})

	// The line is redrawn in place for every update, padded to cover the
	// previous line, and cleared before the summary, which is printed on its
	// own line. Staged files' paths are shown relative to the working
	// directory, and staging operations that follow each other in one phase
	// are counted separately.
	clock.advance(3 * time.Second)
	update(&remote.Progress{Status: "staging-beta", Staging: &remote.StagingProgress{
		ExpectedFiles: 2, ReceivedFiles: 1, ReceivedSize: 1024, Path: "sub/a", FileReceivedSize: 24, FileExpectedSize: 48,
	}})
	clock.advance(2 * time.Second)
	update(&remote.Progress{Status: "staging-beta", Staging: &remote.StagingProgress{ExpectedFiles: 1, ReceivedSize: 1024}})
	update(&remote.Progress{Status: "saving"})
	display.finish(context.Background(), nil)
	first := "remote: synchronizing for 3s: staging on beta (remote), 1 of 2 files, 1.0 KiB; a: 24 B of 48 B"
	second := "remote: synchronizing for 5s: staging on beta (remote), 0 of 1 files, 1.0 KiB"
	third := "remote: synchronizing for 5s: saving"
	expected := "\r" + first[:79] +
		"\r" + second + strings.Repeat(" ", 79-len(second)) +
		"\r" + third + strings.Repeat(" ", len(second)-len(third)) +
		"\r" + strings.Repeat(" ", len(third)) + "\r" +
		"remote: synchronized in 5s: staging on beta (remote) 2s, 3 files, 2.0 KiB\n"
	if output.String() != expected {
		t.Fatalf("incorrect terminal output:\n%q\ninstead of:\n%q", output.String(), expected)
	}
}

func TestProgressOutcomes(t *testing.T) {
	// Synchronizations that end before the delay show nothing.
	setProgressTiming(t, 3*time.Second, time.Hour, time.Hour)
	var output strings.Builder
	display, clock, update := testProgressDisplay(t, &progressOutput{writer: &output})
	update(&remote.Progress{Status: "scanning"})
	clock.advance(2 * time.Second)
	display.finish(context.Background(), errors.New("failed"))
	if output.Len() != 0 {
		t.Fatalf("progress shown for a fast synchronization: %q", output.String())
	}

	// Slower ones, even if the delay elapses only as they end, are summarized
	// according to how the wait ended. Without progress messages, such as
	// from daemons that predate them, only the elapsed time is shown.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		ctx      context.Context
		err      error
		expected string
	}{
		{context.Background(), nil, "remote: synchronized in 4s\n"},
		{context.Background(), errors.New("failed"), "remote: synchronization failed after 4s\n"},
		{cancelled, context.Canceled, "remote: stopped waiting for synchronization after 4s\n"},
	} {
		output.Reset()
		display, clock, _ = testProgressDisplay(t, &progressOutput{writer: &output})
		clock.advance(4 * time.Second)
		display.finish(test.ctx, test.err)
		if output.String() != test.expected {
			t.Errorf("incorrect summary: %q instead of %q", output.String(), test.expected)
		}
	}
}

func TestProgressRefresh(t *testing.T) {
	// Without progress messages, lines with the elapsed time are printed once
	// the delay elapses and then at the line interval, on the real clock.
	setProgressTiming(t, 0, 5*time.Millisecond, time.Millisecond)
	var output lockedBuilder
	display := newProgressDisplay(&progressOutput{writer: &output}, &remote.Target{})
	display.begin()
	for deadline := time.Now().Add(10 * time.Second); strings.Count(output.String(), "\n") < 3; {
		if time.Now().After(deadline) {
			t.Fatalf("progress not refreshed: %q", output.String())
		}
		time.Sleep(time.Millisecond)
	}
	display.finish(context.Background(), nil)
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	for _, line := range lines[:len(lines)-1] {
		if line != "remote: synchronizing for 0s" {
			t.Fatalf("incorrect progress line: %q", line)
		}
	}
	if summary := lines[len(lines)-1]; summary != "remote: synchronized in 0s" {
		t.Fatalf("incorrect summary: %q", summary)
	}
}

func TestProgressStatusLabels(t *testing.T) {
	// Every status that the daemon reports has a label.
	for value := range synchronization.Status_name {
		text, err := synchronization.Status(value).MarshalText()
		if err != nil {
			t.Fatal(err)
		} else if _, ok := statusLabels[string(text)]; !ok {
			t.Errorf("no label for status %s", text)
		}
	}
}

func TestProgressFlush(t *testing.T) {
	// Keep the data directory short enough for Unix domain sockets on macOS.
	data, err := os.MkdirTemp("", "mr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	setProgressTiming(t, 0, time.Hour, time.Hour)

	// Respond to flushes as a daemon that reports progress if requested would,
	// or as one that predates progress messages, recording whether progress
	// was requested.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	channel, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: "sync_progress", Root: root, Alpha: true, Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { channel.Close() })
	var reporting atomic.Bool
	requested := make(chan bool, 1)
	go func() {
		for {
			submission, err := channel.Accept()
			if err != nil {
				return
			}
			header, err := remote.ReceiveSubmission(context.Background(), submission)
			if err != nil {
				submission.Close()
				continue
			}
			if !replyTestRoute(submission, header, "sync_progress") {
				continue
			}
			if header.Operation == remote.OperationNodes {
				replyTestNodes(submission, "sync_progress", root, root)
				continue
			}
			requested <- header.Progress
			if reporting.Load() && header.Progress {
				stop := make(chan struct{})
				close(stop)
				remote.ReportProgress(submission, func() *remote.Progress { return &remote.Progress{Status: "scanning"} }, time.Hour, time.Hour, stop)
			}
			remote.RespondToFlush(submission, &remote.Report{}, nil)
		}
	}()
	t.Chdir(root)
	for _, test := range []struct {
		reporting bool
		options   []string
		requested bool
		expected  string
	}{
		// By default, progress isn't requested or shown on output other than
		// terminals, which leaves the output unchanged.
		{true, nil, false, ""},
		{true, []string{"--progress=false"}, false, ""},
		{true, []string{"--progress"}, true, "remote: synchronizing for 0s: scanning\nremote: synchronized in 0s\n"},
		// Daemons that predate progress messages show only the summary,
		// without warnings or errors.
		{false, []string{"--progress"}, true, "remote: synchronized in 0s\n"},
	} {
		reporting.Store(test.reporting)
		var stdout, stderr strings.Builder
		code := Run(append(test.options, "--flush"), strings.NewReader(""), &stdout, &stderr)
		if code != 0 || stdout.Len() != 0 || !strings.HasSuffix(stderr.String(), test.expected) ||
			test.expected == "" && stderr.Len() != 0 || strings.Contains(stderr.String(), "warning") {
			t.Fatalf("incorrect flush with %v: %d, %q, %q", test.options, code, stdout.String(), stderr.String())
		} else if received := <-requested; received != test.requested {
			t.Fatalf("progress requested incorrectly with %v: %t", test.options, received)
		}
	}
}
