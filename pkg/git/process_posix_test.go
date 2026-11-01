//go:build !windows

package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runUntilCancelled runs a shell script in a new directory, with the process
// handling that Command provides, until it has created a file named ready, and
// then cancels it, waits for it, and returns the directory. Its output goes to a
// buffer rather than to a file, as it does for commands that Mutagen runs, which
// means that waiting for it also waits for the output to close.
func runUntilCancelled(t *testing.T, script string) string {
	t.Helper()
	directory := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := Command(ctx, directory, "version")
	command.Path, command.Args, command.Err = "/bin/sh", []string{"sh", "-c", script}, nil
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal("unable to start script:", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Lstat(filepath.Join(directory, "ready")); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("script didn't become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	start := time.Now()
	command.Wait()
	if elapsed := time.Since(start); elapsed > terminationGracePeriod+waitDelay {
		t.Error("waiting took too long:", elapsed)
	}
	return directory
}

// verifyCleanedUp verifies that a script run by runUntilCancelled has had the
// chance to create a file by the time that waiting for it returned.
func verifyCleanedUp(t *testing.T, directory, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(directory, name)); err != nil {
		t.Error("process wasn't allowed to clean up:", err)
	}
}

// TestCommandCancellationAllowsCleanup tests that cancellation gives a command
// the chance to clean up, which Git relies on to remove what it was working on.
func TestCommandCancellationAllowsCleanup(t *testing.T) {
	directory := runUntilCancelled(t, `
		trap 'echo cleaned > cleaned; exit 0' TERM
		touch ready
		while :; do sleep 0.05; done
	`)
	verifyCleanedUp(t, directory, "cleaned")
}

// TestCommandCancellationWaitsForHelper tests that cancellation reaches helper
// processes, and that waiting for a command covers the cleanup of a helper that
// outlives the command itself, because the caller may move or remove what it was
// working on as soon as waiting returns.
func TestCommandCancellationWaitsForHelper(t *testing.T) {
	directory := runUntilCancelled(t, `
		(
			trap 'sleep 0.3; echo cleaned > cleaned; exit 0' TERM
			touch ready
			while :; do sleep 0.05; done
		) &
		trap 'exit 0' TERM
		wait
	`)
	verifyCleanedUp(t, directory, "cleaned")
}

// TestCommandCancellationWaitsForHelperWithOwnOutput tests that waiting for a
// command covers the cleanup of a helper that outlives the command and doesn't
// have its output, so that the command's output closing says nothing about the
// helper. Git's clones are helpers of this sort when they're run for submodules,
// because Git captures their output. This also catches any change to
// exec.Cmd.Wait not returning before the command's Cancel function does, which
// configureProcess relies on.
func TestCommandCancellationWaitsForHelperWithOwnOutput(t *testing.T) {
	directory := runUntilCancelled(t, `
		(
			trap 'sleep 0.3; echo cleaned > cleaned; exit 0' TERM
			touch ready
			while :; do sleep 0.05; done
		) > /dev/null 2>&1 &
		trap 'exit 0' TERM
		wait
	`)
	verifyCleanedUp(t, directory, "cleaned")
}

// TestCommandCancellationContinuesStopped tests that cancellation allows a
// stopped process to clean up, which is what happens to a process that tries to
// prompt on a controlling terminal, since it isn't in the foreground.
func TestCommandCancellationContinuesStopped(t *testing.T) {
	directory := runUntilCancelled(t, `
		trap 'echo cleaned > cleaned; exit 0' TERM
		touch ready
		kill -STOP $$
		sleep 600
	`)
	verifyCleanedUp(t, directory, "cleaned")
}

// TestCommandCancellationKillsUnresponsive tests that a process group that
// doesn't respond to being asked to terminate is killed.
func TestCommandCancellationKillsUnresponsive(t *testing.T) {
	directory := runUntilCancelled(t, `
		trap '' TERM
		sleep 600 &
		echo $! > helper
		touch ready
		wait
	`)
	contents, err := os.ReadFile(filepath.Join(directory, "helper"))
	if err != nil {
		t.Fatal("unable to read process identifier:", err)
	}
	helper, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil {
		t.Fatal("invalid process identifier:", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(helper, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			syscall.Kill(helper, syscall.SIGKILL)
			t.Fatal("helper is still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
