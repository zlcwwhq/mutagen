package synchronization

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/state"
	"github.com/mutagen-io/mutagen/pkg/synchronization/rsync"
)

// writeRelayFrame writes a length-prefixed JSON message in the background, as
// submitters of every version frame their headers.
func writeRelayFrame(t *testing.T, connection net.Conn, message any) {
	t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		binary.Write(connection, binary.BigEndian, uint32(len(data)))
		connection.Write(data)
	}()
}

// readRelayFrames reads length-prefixed JSON messages until the connection
// closes, as submitters of every version frame them.
func readRelayFrames(t *testing.T, connection net.Conn) []map[string]json.RawMessage {
	t.Helper()
	var messages []map[string]json.RawMessage
	connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var size uint32
		if err := binary.Read(connection, binary.BigEndian, &size); err == io.EOF {
			return messages
		} else if err != nil {
			t.Fatal("unable to read message:", err)
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(connection, data); err != nil {
			t.Fatal("unable to read message:", err)
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal("invalid message:", err)
		}
		messages = append(messages, message)
	}
}

func TestCommandRelayFlushProgress(t *testing.T) {
	// Sample progress often, so that changes would be reported while flushes
	// wait.
	interval := progressSampleInterval
	progressSampleInterval = time.Millisecond
	t.Cleanup(func() { progressSampleInterval = interval })

	// The session's status changes while the synchronization loop holds flush
	// requests, which it completes once released.
	var lock sync.Mutex
	status := "scanning"
	setStatus := func(value string) {
		lock.Lock()
		status = value
		lock.Unlock()
	}
	relay := &commandRelay{
		flushRequests: make(chan chan error, 1),
		report:        func() *remote.Report { return &remote.Report{} },
		progress: func() *remote.Progress {
			lock.Lock()
			defer lock.Unlock()
			return &remote.Progress{Status: status}
		},
		conflicts: func() (*remote.Listing, error) {
			return &remote.Listing{Conflicts: []remote.ConflictRoot{{Path: "a"}}}, nil
		},
	}
	release := make(chan struct{})
	go func() {
		for request := range relay.flushRequests {
			<-release
			request <- nil
		}
	}()
	t.Cleanup(func() { close(relay.flushRequests) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	submit := func(header any) net.Conn {
		t.Helper()
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close() })
		go relay.route(ctx, server, 1)
		writeRelayFrame(t, client, header)
		return client
	}

	// Submitters of earlier versions, which don't request progress, receive
	// only the outcome that they expect, however long the flush takes and
	// however its progress changes meanwhile.
	for _, operation := range []remote.Operation{remote.OperationFlush, remote.OperationConflicts} {
		setStatus("scanning")
		client := submit(&struct{ Operation remote.Operation }{operation})
		time.Sleep(20 * time.Millisecond)
		setStatus("staging-alpha")
		time.Sleep(20 * time.Millisecond)
		release <- struct{}{}
		messages := readRelayFrames(t, client)
		if operation == remote.OperationFlush && (len(messages) != 1 || len(messages[0]) != 2 ||
			string(messages[0]["Error"]) != `""` || messages[0]["Report"] == nil) {
			t.Fatalf("incorrect flush messages for an earlier submitter: %s", messages)
		} else if operation == remote.OperationConflicts && (len(messages) != 2 || len(messages[0]) != 1 ||
			messages[0]["Conflict"] == nil || len(messages[1]) != 1 || messages[1]["Done"] == nil) {
			t.Fatalf("incorrect conflict messages for an earlier submitter: %s", messages)
		}
	}

	// Submitters that request progress receive it until the outcome.
	statuses := make(chan string, 1000)
	record := func(progress *remote.Progress) { statuses <- progress.Status }
	await := func(expected string) {
		t.Helper()
		for {
			select {
			case received := <-statuses:
				if received == expected {
					return
				}
			case <-ctx.Done():
				t.Fatal("progress not reported:", expected)
			}
		}
	}
	for _, operation := range []remote.Operation{remote.OperationFlush, remote.OperationConflicts} {
		setStatus("scanning")
		client := submit(&remote.Submission{Operation: operation, Progress: true})
		type outcome struct {
			report  *remote.Report
			listing *remote.Listing
			err     error
		}
		outcomes := make(chan outcome, 1)
		go func() {
			var result outcome
			if operation == remote.OperationFlush {
				result.report, result.err = remote.FlushWithProgress(ctx, client, record)
			} else {
				result.listing, result.err = remote.ListConflictsWithProgress(ctx, client, record)
			}
			outcomes <- result
		}()
		await("scanning")
		setStatus("staging-alpha")
		await("staging-alpha")
		release <- struct{}{}
		result := <-outcomes
		if result.err != nil || (operation == remote.OperationFlush && result.report == nil) ||
			(operation == remote.OperationConflicts && (result.listing == nil || len(result.listing.Conflicts) != 1)) {
			t.Fatalf("incorrect outcome with progress: %+v", result)
		}
	}
}

func TestRemoteProgress(t *testing.T) {
	staging := &rsync.ReceiverState{
		Path:              "out.bin",
		ReceivedSize:      1024,
		ExpectedSize:      4096,
		ReceivedFiles:     1,
		ExpectedFiles:     2,
		TotalReceivedSize: 2048,
	}
	expectedStaging := &remote.StagingProgress{
		ReceivedFiles:    1,
		ExpectedFiles:    2,
		ReceivedSize:     2048,
		Path:             "out.bin",
		FileReceivedSize: 1024,
		FileExpectedSize: 4096,
	}
	for _, test := range []struct {
		status      Status
		alpha, beta *rsync.ReceiverState
		expected    *remote.Progress
	}{
		{Status_Scanning, nil, nil, &remote.Progress{Status: "scanning"}},
		{Status_StagingAlpha, staging, nil, &remote.Progress{Status: "staging-alpha", Staging: expectedStaging}},
		{Status_StagingBeta, nil, staging, &remote.Progress{Status: "staging-beta", Staging: expectedStaging}},
		// Staging progress is only reported for the endpoint that stages,
		// once it starts receiving files.
		{Status_StagingBeta, staging, nil, &remote.Progress{Status: "staging-beta"}},
		{Status_Transitioning, staging, staging, &remote.Progress{Status: "transitioning"}},
	} {
		c := &controller{
			stateLock: state.NewTrackingLock(state.NewTracker()),
			state: &State{
				Status:     test.status,
				AlphaState: &EndpointState{StagingProgress: test.alpha},
				BetaState:  &EndpointState{StagingProgress: test.beta},
			},
		}
		if progress := c.remoteProgress(); !reflect.DeepEqual(progress, test.expected) {
			t.Errorf("incorrect progress for %v: %+v", test.status, progress)
		}
	}
}
