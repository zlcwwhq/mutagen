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

// jobLogTestRoute simulates an executing endpoint sending the specified log
// output and result. A nil result drops the connection after sending output.
func jobLogTestRoute(t *testing.T, ctx context.Context, output []byte, response *result) net.Conn {
	t.Helper()
	connection, _ := jobTestPeer(t, ctx, &testPeer{output: output, response: response})
	return connection
}

// testPeer describes how a simulated executing endpoint responds to a request
// (see jobTestPeer).
type testPeer struct {
	// output is the output that the peer sends.
	output []byte
	// response is the result that the peer sends after its output. If it's
	// nil, then the peer drops the connection after sending output, unless
	// hold is set.
	response *result
	// hold indicates that the peer keeps the connection open without sending
	// a result until the client closes it, as an executor that waits does.
	hold bool
}

// jobTestPeer simulates an executing endpoint that responds to one request as
// specified, and returns the connection to it and a channel that receives the
// request.
func jobTestPeer(t *testing.T, ctx context.Context, peer *testPeer) (net.Conn, <-chan *Request) {
	t.Helper()
	client, server := net.Pipe()
	requests := make(chan *Request, 1)
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
		requests <- &request
		if _, err := streams[2].Write(peer.output); err != nil {
			return
		} else if peer.hold {
			<-mux.Closed()
			return
		}
		streams[2].CloseWrite()
		streams[3].CloseWrite()
		if peer.response != nil {
			if err := writeMessage(streams[0], peer.response); err != nil {
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
	return client, requests
}

// encodeFrames encodes log frames, each followed by its contents, as an
// executor streams them, with the frames' sizes set to those of the contents.
func encodeFrames(t *testing.T, frames ...any) []byte {
	t.Helper()
	var output bytes.Buffer
	for f := 0; f < len(frames); f += 2 {
		frame, data := frames[f].(logFrame), frames[f+1].(string)
		frame.Size = len(data)
		if err := writeMessage(&output, &frame); err != nil {
			t.Fatal(err)
		}
		output.WriteString(data)
	}
	return output.Bytes()
}

func TestWaitJobFollowPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job := &Job{ID: "0123abcd", State: JobExited, ExitCode: 7}
	var warnings []string
	warn := func(warning string) { warnings = append(warnings, warning) }

	// Waits that follow logs send requests that start following as requested,
	// or resume where the previous wait stopped, without deadlines, which waits
	// that follow logs don't need, and contents that were already received
	// aren't printed again.
	var output bytes.Buffer
	follower := &LogFollower{Output: &output, Lines: 2}
	frames := encodeFrames(t, logFrame{Offset: 10, Identity: "1:2"}, "abc\n", logFrame{Offset: 12}, "c\nde")
	deadline := time.Now().Add(5 * time.Second)
	route, requests := jobTestPeer(t, ctx, &testPeer{output: frames, response: &result{Job: job}})
	ended, log, err := WaitJobWithOptions(ctx, route, job.ID, &WaitOptions{Tail: 1, Deadline: deadline, Follow: follower, Warn: warn})
	request := (<-requests).Job
	if err != nil || ended.ExitCode != 7 || log != nil || output.String() != "abc\nde" || len(warnings) != 0 {
		t.Fatalf("incorrect followed result: %+v, %+v, %v, %q, %q", ended, log, err, output.String(), warnings)
	} else if request.Tail != 0 || request.Timeout != 0 || request.Follow == nil || *request.Follow != (JobFollow{Tail: true, Lines: 2}) {
		t.Fatalf("incorrect request to follow: %+v", request)
	} else if !follower.PartialLine() {
		t.Fatal("partial line not reported")
	}
	route, requests = jobTestPeer(t, ctx, &testPeer{response: &result{Job: job}})
	if ended, _, err := WaitJobWithOptions(ctx, route, job.ID, &WaitOptions{Follow: follower, Warn: warn}); err != nil || ended.ExitCode != 7 {
		t.Fatalf("incorrect resumed result: %+v, %v", ended, err)
	} else if request := (<-requests).Job; request.Follow == nil || *request.Follow != (JobFollow{Offset: 16, Identity: "1:2"}) || request.Timeout != 0 {
		t.Fatalf("incorrect resumed request: %+v", request)
	}

	// Waits that read tails send deadlines that leave time for their results,
	// and accept running jobs, which other waits don't.
	running := &Job{ID: job.ID, State: JobRunning}
	route, requests = jobTestPeer(t, ctx, &testPeer{output: []byte("b\n"), response: &result{Job: running, JobLog: &JobLog{}}})
	if ended, log, err := WaitJobWithOptions(ctx, route, job.ID, &WaitOptions{Tail: 1, Deadline: deadline}); err != nil || ended.State != JobRunning || log == nil || string(log.Data) != "b\n" {
		t.Fatalf("incorrect result before a deadline: %+v, %+v, %v", ended, log, err)
	} else if request := (<-requests).Job; request.Timeout < 3*time.Second || request.Timeout > 5*time.Second-waitMargin {
		t.Fatalf("incorrect timeout for a deadline 5 seconds away: %v", request.Timeout)
	}
	for _, options := range []*WaitOptions{{}, {Deadline: deadline}, {Deadline: deadline, Follow: &LogFollower{Output: &output}}} {
		route, requests = jobTestPeer(t, ctx, &testPeer{response: &result{Job: running}})
		if _, _, err := WaitJobWithOptions(ctx, route, job.ID, options); err == nil {
			t.Fatalf("running job accepted with options %+v", options)
		} else if request := (<-requests).Job; request.Timeout != 0 {
			t.Fatalf("deadline sent with options %+v: %+v", options, request)
		}
	}

	// Streams that report failures, or that are invalid, stop following with a
	// warning, without affecting the job's status, and later waits don't
	// follow.
	for _, test := range []struct {
		output, warning string
	}{
		{string(encodeFrames(t, logFrame{Error: "read failed"}, "")), "unable to follow the log of job 0123abcd: read failed"},
		{string(encodeFrames(t, logFrame{Offset: -1}, "")), "invalid log stream"},
		{"\x00\x00\x00\x01x", "invalid log stream"},
	} {
		warnings = nil
		output.Reset()
		follower := &LogFollower{Output: &output, Lines: -1}
		route, requests := jobTestPeer(t, ctx, &testPeer{output: []byte(test.output), response: &result{Job: job}})
		if ended, _, err := WaitJobWithOptions(ctx, route, job.ID, &WaitOptions{Follow: follower, Warn: warn}); err != nil || ended.ExitCode != 7 || output.Len() != 0 {
			t.Fatalf("stream failure changed the result: %+v, %v, %q", ended, err, output.String())
		} else if request := (<-requests).Job; request.Follow == nil || *request.Follow != (JobFollow{}) {
			t.Fatalf("incorrect request to follow from the beginning: %+v", request)
		} else if len(warnings) != 1 || !strings.Contains(warnings[0], test.warning) {
			t.Fatalf("incorrect warnings for a stream failure: %q", warnings)
		}
		route, requests = jobTestPeer(t, ctx, &testPeer{response: &result{Job: job}})
		if ended, _, err := WaitJobWithOptions(ctx, route, job.ID, &WaitOptions{Follow: follower, Warn: warn}); err != nil || ended.ExitCode != 7 {
			t.Fatalf("incorrect retried result: %+v, %v", ended, err)
		} else if request := (<-requests).Job; request.Follow != nil || len(warnings) != 1 {
			t.Fatalf("retried wait followed or warned again: %+v, %q", request, warnings)
		}
	}
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

	// A result without the requested tail is invalid.
	route := jobLogTestRoute(t, ctx, nil, &result{Job: job})
	if ended, log, err := WaitJobWithTail(ctx, route, job.ID, 1); err == nil || ClassifyError(err) != ErrorKindConnection || ended != nil || log != nil {
		t.Fatalf("result without the requested tail accepted: %+v, %+v, %v", ended, log, err)
	}

	// Receiving some or all of the streamed tail without the final result
	// must not return any output. A retry can then print the successful tail
	// once, without duplicating the earlier attempt's partial output.
	route = jobLogTestRoute(t, ctx, bytes.Repeat([]byte("x"), MaximumJobLogSize), nil)
	if ended, log, err := WaitJobWithTail(ctx, route, job.ID, 1); err == nil || ended != nil || log != nil {
		t.Fatalf("interrupted transfer returned partial results: %+v, %+v, %v", ended, log, err)
	}
	route = jobLogTestRoute(t, ctx, []byte("complete\n"), &result{Job: job, JobLog: &JobLog{}})
	if ended, log, err := WaitJobWithTail(ctx, route, job.ID, 1); err != nil || ended.ExitCode != 7 || string(log.Data) != "complete\n" {
		t.Fatalf("retry didn't return its complete log: %+v, %+v, %v", ended, log, err)
	}
}
