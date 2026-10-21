//go:build !windows

package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSignalExitCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := &Request{Arguments: []string{"sh", "-c", "kill -TERM $$"}, Directory: t.TempDir()}
	if code, err := executeTest(t, ctx, request, nil, nil, nil); err != nil || code != 143 {
		t.Fatalf("incorrect signal exit: %d, %v", code, err)
	}
}

func TestCancellationTerminatesChildren(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survivor")
	request := &Request{Arguments: []string{"sh", "-c", `sh -c 'sleep 1; echo survived > "$1"' sh "$1" & printf ready; wait`, "sh", marker}, Directory: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := executeTest(t, ctx, request, nil, &readyWriter{ready: cancel}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("incorrect cancellation error: %v", err)
	}
	// A surviving child would create the marker even after its parent exits.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("child survived cancellation")
	}
}

// useTerminationGracePeriod sets the termination grace period for a test.
func useTerminationGracePeriod(t *testing.T, period time.Duration) {
	t.Helper()
	previous := terminationGracePeriod
	terminationGracePeriod = period
	t.Cleanup(func() { terminationGracePeriod = previous })
}

func TestCancellationInterruptsGracefully(t *testing.T) {
	// Commands are asked to terminate before being killed, so they can clean
	// up, and output that they write meanwhile, which can no longer be
	// delivered, doesn't interrupt them.
	marker := filepath.Join(t.TempDir(), "cleaned")
	script := `trap 'echo terminating; echo terminating >&2; echo cleaned > "$1"; exit 3' TERM; printf ready; while :; do sleep 1; done`
	request := &Request{Arguments: []string{"sh", "-c", script, "sh", marker}, Directory: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := executeTest(t, ctx, request, nil, &readyWriter{ready: cancel}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("incorrect cancellation error: %v", err)
	} else if data, err := os.ReadFile(marker); err != nil || string(data) != "cleaned\n" {
		t.Fatalf("command didn't terminate gracefully: %q, %v", data, err)
	}
}

func TestCancellationKillsAfterGracePeriod(t *testing.T) {
	useTerminationGracePeriod(t, 200*time.Millisecond)

	// The shell handles SIGTERM without exiting, and then waits for a child
	// that ignores it, so only SIGKILL, once the grace period expires,
	// terminates them. The server, which executeTest waits for, only returns
	// once the command is terminated. The child reports readiness once it
	// ignores SIGTERM.
	received := filepath.Join(t.TempDir(), "received")
	script := `trap 'echo received > "$1"' TERM; (trap '' TERM; printf ready; exec sleep 1000) & wait; wait`
	request := &Request{Arguments: []string{"sh", "-c", script, "sh", received}, Directory: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cancelled time.Time
	ready := func() {
		cancelled = time.Now()
		cancel()
	}
	if _, err := executeTest(t, ctx, request, nil, &readyWriter{ready: ready}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("incorrect cancellation error: %v", err)
	} else if elapsed := time.Since(cancelled); elapsed < terminationGracePeriod {
		t.Fatalf("command killed before the grace period expired: %v", elapsed)
	} else if _, err := os.Stat(received); err != nil {
		t.Fatal("command wasn't asked to terminate before being killed:", err)
	}
}

func TestCancellationKillsRemainingProcesses(t *testing.T) {
	useTerminationGracePeriod(t, 200*time.Millisecond)

	// A child that ignores SIGTERM and then stops holding the command's output
	// remains after the command exits, and is killed once the grace period
	// expires. The child reports readiness once it ignores SIGTERM.
	marker := filepath.Join(t.TempDir(), "survivor")
	script := `(trap '' TERM; printf ready; exec > /dev/null; sleep 1; echo survived > "$1") 2> /dev/null < /dev/null & wait`
	request := &Request{Arguments: []string{"sh", "-c", script, "sh", marker}, Directory: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cancelled time.Time
	ready := func() {
		cancelled = time.Now()
		cancel()
	}
	if _, err := executeTest(t, ctx, request, nil, &readyWriter{ready: ready}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("incorrect cancellation error: %v", err)
	} else if elapsed := time.Since(cancelled); elapsed < terminationGracePeriod {
		t.Fatalf("termination didn't wait for the remaining process: %v", elapsed)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("remaining process survived termination")
	}
}

func TestCancellationAfterParentExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	request := &Request{Arguments: []string{"sh", "-c", "sleep 30 &"}, Directory: t.TempDir()}
	_, err := executeTest(t, ctx, request, nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("incorrect cancellation error: %v", err)
	}
}
