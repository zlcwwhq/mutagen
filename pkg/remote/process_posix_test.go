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

func TestCancellationAfterParentExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	request := &Request{Arguments: []string{"sh", "-c", "sleep 30 &"}, Directory: t.TempDir()}
	_, err := executeTest(t, ctx, request, nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("incorrect cancellation error: %v", err)
	}
}
