package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

// progressTestPeer simulates the daemon's side of a flush or conflicts
// submission by sending the specified messages, then waiting for the client
// to close the connection if hold is true. It returns the client's side.
func progressTestPeer(t *testing.T, messages []any, hold bool) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		for _, message := range messages {
			if err := writeMessage(server, message); err != nil {
				return
			}
		}
		if hold {
			var buffer [1]byte
			server.Read(buffer[:])
		}
	}()
	t.Cleanup(func() {
		client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("progress test peer didn't stop")
		}
	})
	return client
}

func TestFlushWithProgressPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	scanning := &Progress{Status: "scanning"}
	staging := &Progress{Status: "staging-alpha", Staging: &StagingProgress{
		ExpectedFiles: 1, ReceivedSize: 1024, Path: "out.bin", FileReceivedSize: 1024, FileExpectedSize: 4096,
	}}
	report := &Report{Problems: []Problem{{Alpha: true, Path: "secret", Error: "permission denied"}}}
	for _, test := range []struct {
		name     string
		messages []any
		progress []*Progress
		error    string
	}{
		// Daemons that report progress send it before the outcome.
		{"progress", []any{&progressMessage{scanning}, &progressMessage{staging}, &flushResponse{Report: report}}, []*Progress{scanning, staging}, ""},
		// Daemons that predate progress messages, or ignore the request,
		// send only the outcome.
		{"earlier-daemon", []any{&flushResponse{Report: report}}, nil, ""},
		// Failures end the wait, whether reported by the daemon or by a hop
		// that couldn't reach it, which sends a status.
		{"failure", []any{&progressMessage{scanning}, &flushResponse{Error: "synchronization stopped"}}, []*Progress{scanning}, "synchronization stopped"},
		{"hop-failure", []any{&status{Error: "unable to reach the Mutagen daemon"}}, nil, "unable to reach the Mutagen daemon"},
		// A response without a report or progress is invalid.
		{"invalid", []any{&progressMessage{scanning}, &flushResponse{}}, []*Progress{scanning}, "no synchronization report received"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var received []*Progress
			result, err := FlushWithProgress(ctx, progressTestPeer(t, test.messages, false), func(progress *Progress) {
				received = append(received, progress)
			})
			if test.error != "" {
				if err == nil || err.Error() != test.error {
					t.Fatalf("incorrect error: %+v, %v", result, err)
				}
			} else if err != nil || !reflect.DeepEqual(result, report) {
				t.Fatalf("incorrect report: %+v, %v", result, err)
			}
			if !reflect.DeepEqual(received, test.progress) {
				t.Fatalf("incorrect progress: %+v", received)
			}
		})
	}

	// Flush ignores progress, and cancellation abandons the wait while
	// progress arrives.
	if result, err := Flush(ctx, progressTestPeer(t, []any{&progressMessage{scanning}, &flushResponse{Report: report}}, false)); err != nil || !reflect.DeepEqual(result, report) {
		t.Fatalf("progress not ignored: %+v, %v", result, err)
	}
	cancelled, cancelWait := context.WithCancel(ctx)
	received := make(chan struct{})
	var once sync.Once
	go func() {
		<-received
		cancelWait()
	}()
	_, err := FlushWithProgress(cancelled, progressTestPeer(t, []any{&progressMessage{scanning}}, true), func(*Progress) {
		once.Do(func() { close(received) })
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not reported:", err)
	}
}

func TestProgressMessagesDistinct(t *testing.T) {
	// Progress messages decode as neither an outcome nor a listing message
	// for the clients that request them, while statuses and outcomes never
	// decode as progress.
	decode := func(message, value any) {
		t.Helper()
		if data, err := json.Marshal(message); err != nil {
			t.Fatal(err)
		} else if err := json.Unmarshal(data, value); err != nil {
			t.Fatal(err)
		}
	}
	progress := &progressMessage{&Progress{Status: "scanning"}}
	var flush flushResponse
	decode(progress, &flush)
	if flush.Error != "" || flush.Report != nil || flush.Progress == nil {
		t.Fatalf("progress decoded incorrectly: %+v", flush)
	}
	var listing conflictsMessage
	decode(progress, &listing)
	if !reflect.DeepEqual(listing, conflictsMessage{Progress: progress.Progress}) {
		t.Fatalf("progress decoded incorrectly: %+v", listing)
	}
	for _, message := range []any{&status{Error: "failed"}, &flushResponse{Report: &Report{}}, &conflictsMessage{Done: true}} {
		flush, listing = flushResponse{}, conflictsMessage{}
		decode(message, &flush)
		decode(message, &listing)
		if flush.Progress != nil || listing.Progress != nil {
			t.Fatalf("%+v decoded as progress", message)
		}
	}
}

func TestListConflictsWithProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	scanning := &Progress{Status: "scanning"}
	conflict := ConflictRoot{Path: "a", Files: true}
	for _, test := range []struct {
		name     string
		messages []any
		progress []*Progress
	}{
		{"progress", []any{&progressMessage{scanning}, &conflictsMessage{Conflict: &conflict}, &conflictsMessage{Done: true}}, []*Progress{scanning}},
		{"earlier-daemon", []any{&conflictsMessage{Conflict: &conflict}, &conflictsMessage{Done: true}}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var received []*Progress
			listing, err := ListConflictsWithProgress(ctx, progressTestPeer(t, test.messages, false), func(progress *Progress) {
				received = append(received, progress)
			})
			if err != nil || !reflect.DeepEqual(listing, &Listing{Conflicts: []ConflictRoot{conflict}}) {
				t.Fatalf("incorrect listing: %+v, %v", listing, err)
			} else if !reflect.DeepEqual(received, test.progress) {
				t.Fatalf("incorrect progress: %+v", received)
			}
		})
	}
}

func TestReportProgress(t *testing.T) {
	// Samples are taken from a sequence whose last sample repeats.
	samples := []*Progress{
		{Status: "scanning"},
		{Status: "scanning"},
		nil,
		{Status: "staging-beta", Staging: &StagingProgress{ExpectedFiles: 2, ReceivedSize: 1}},
		{Status: "staging-beta", Staging: &StagingProgress{ExpectedFiles: 2, ReceivedSize: 1}},
		{Status: "staging-beta", Staging: &StagingProgress{ExpectedFiles: 2, ReceivedSize: 9}},
		{Status: "saving"},
	}
	var lock sync.Mutex
	taken := 0
	sample := func() *Progress {
		lock.Lock()
		defer lock.Unlock()
		taken++
		return samples[min(taken, len(samples))-1]
	}

	// Only samples that differ from the last one sent are sent, and nil
	// samples aren't, until the heartbeat interval elapses.
	client, server := net.Pipe()
	defer client.Close()
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ReportProgress(server, sample, time.Millisecond, time.Hour, stop)
	}()
	receive := func() *Progress {
		t.Helper()
		var message flushResponse
		client.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := readMessage(client, &message); err != nil || message.Progress == nil {
			t.Fatalf("progress not received: %+v, %v", message, err)
		}
		return message.Progress
	}
	for _, expected := range []*Progress{samples[0], samples[3], samples[5], samples[6]} {
		if received := receive(); !reflect.DeepEqual(received, expected) {
			t.Fatalf("incorrect progress: %+v instead of %+v", received, expected)
		}
	}

	// Once the heartbeat interval elapses, an unchanged sample is sent again.
	// The interval is long here, so the repeated sample isn't sent before stop
	// is closed, after which nothing is sent.
	close(stop)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("reporting didn't stop")
	}
	server.Close()
	var message flushResponse
	if err := readMessage(client, &message); err == nil {
		t.Fatalf("progress sent after stopping: %+v", message)
	}

	// Heartbeats repeat unchanged samples.
	client, server = net.Pipe()
	defer client.Close()
	stop, stopped = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ReportProgress(server, func() *Progress { return &Progress{Status: "scanning"} }, time.Millisecond, 5*time.Millisecond, stop)
	}()
	for range 3 {
		if received := receive(); received.Status != "scanning" {
			t.Fatalf("incorrect heartbeat: %+v", received)
		}
	}
	close(stop)
	<-stopped

	// The first sample is sent even if stop is already closed, and reporting
	// stops at the first failure to send.
	client, server = net.Pipe()
	defer client.Close()
	stop = make(chan struct{})
	close(stop)
	go ReportProgress(server, func() *Progress { return &Progress{Status: "watching"} }, time.Hour, time.Hour, stop)
	if received := receive(); received.Status != "watching" {
		t.Fatalf("incorrect first sample: %+v", received)
	}
	client.Close()
	stopped = make(chan struct{})
	go func() {
		defer close(stopped)
		ReportProgress(server, func() *Progress { return &Progress{Status: "scanning"} }, time.Millisecond, time.Hour, make(chan struct{}))
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("reporting didn't stop after a failure")
	}
}
