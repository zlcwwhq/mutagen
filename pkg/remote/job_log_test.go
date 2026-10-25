package remote

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/multiplexing"
)

// jobLogTestRoute simulates a peer sending the specified log output and result.
// A nil result drops the connection after sending output.
func jobLogTestRoute(t *testing.T, ctx context.Context, output []byte, response *result) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		if err := writeMessage(server, &status{}); err != nil {
			return
		}
		mux := multiplexing.Multiplex(multiplexing.NewCarrierFromStream(server), false, commandMultiplexerConfiguration())
		defer mux.Close()
		var streams [4]*multiplexing.Stream
		for i := range streams {
			var err error
			if streams[i], err = mux.AcceptStream(ctx); err != nil {
				return
			}
		}
		var request Request
		if err := readMessage(streams[0], &request); err != nil {
			return
		}
		if _, err := streams[2].Write(output); err != nil {
			return
		}
		streams[2].CloseWrite()
		streams[3].CloseWrite()
		if response != nil {
			if err := writeMessage(streams[0], response); err != nil {
				return
			}
			var acknowledgement [1]byte
			io.ReadFull(streams[0], acknowledgement[:])
		}
	}()
	t.Cleanup(func() {
		client.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("log test peer didn't stop")
		}
	})
	return client
}

func TestWaitJobTailPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job := &Job{ID: "0123abcd", State: JobExited, ExitCode: 7}
	for _, test := range []struct {
		name, output, error string
		log                 *JobLog
	}{
		{"supported", "last\n", "", &JobLog{}},
		{"old-peer", "", "doesn't support log tails", nil},
		{"read-failure", "", "read failed", &JobLog{Error: "read failed"}},
		{"excess-output", strings.Repeat("x", MaximumJobLogSize+1), "larger than 1 MiB", &JobLog{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := jobLogTestRoute(t, ctx, []byte(test.output), &result{Job: job, JobLog: test.log})
			ended, log, err := WaitJobWithTail(ctx, route, job.ID, 1<<32)
			if err != nil || ended.ExitCode != 7 || log == nil {
				t.Fatalf("incorrect wait result: %+v, %+v, %v", ended, log, err)
			} else if test.error != "" {
				if !strings.Contains(log.Error, test.error) || len(log.Data) != 0 {
					t.Fatalf("incorrect log failure: %+v", log)
				}
			} else if log.Error != "" || string(log.Data) != test.output {
				t.Fatalf("incorrect log: %+v", log)
			}
		})
	}

	// Receiving some or all of the streamed tail without the final result
	// must not return any output. A retry can then print the successful tail
	// once, without duplicating the earlier attempt's partial output.
	route := jobLogTestRoute(t, ctx, bytes.Repeat([]byte("x"), MaximumJobLogSize), nil)
	if ended, log, err := WaitJobWithTail(ctx, route, job.ID, 1); err == nil || ended != nil || log != nil {
		t.Fatalf("interrupted transfer returned partial results: %+v, %+v, %v", ended, log, err)
	}
	route = jobLogTestRoute(t, ctx, []byte("complete\n"), &result{Job: job, JobLog: &JobLog{}})
	if ended, log, err := WaitJobWithTail(ctx, route, job.ID, 1); err != nil || ended.ExitCode != 7 || string(log.Data) != "complete\n" {
		t.Fatalf("retry didn't return its complete log: %+v, %+v, %v", ended, log, err)
	}
}
