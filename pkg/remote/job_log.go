package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// MaximumJobLogSize bounds the contents read and returned by a job log tail.
// One additional byte is read to recognize a line boundary at this limit.
const MaximumJobLogSize = 1024 * 1024

// JobLog describes the requested tail of a detached job's log.
type JobLog struct {
	// Data contains the final requested lines, limited to MaximumJobLogSize
	// bytes. It's transmitted separately from the metadata.
	Data []byte `json:"-"`
	// Truncated indicates that the requested lines exceed MaximumJobLogSize.
	// In that case, Data may start within a line.
	Truncated bool `json:",omitempty"`
	// Error reports why the log couldn't be read. It doesn't affect the job's
	// exit status, and Data is empty if it's non-empty.
	Error string `json:",omitempty"`
	// ErrorKind classifies this non-fatal log diagnostic.
	ErrorKind ErrorKind `json:",omitempty"`
}

// readJobLog reads a bounded snapshot of the final lines of a job's log. It
// follows symbolic links, as starting the job does, but only reads regular
// files. Appends after the size is determined aren't included.
func readJobLog(ctx context.Context, path string, lines int64) *JobLog {
	if lines == 0 {
		return &JobLog{}
	} else if err := ctx.Err(); err != nil {
		return &JobLog{Error: err.Error(), ErrorKind: ClassifyError(err)}
	}
	file, err := openJobLog(path)
	if err != nil {
		return &JobLog{Error: err.Error(), ErrorKind: ClassifyError(err)}
	}
	defer file.Close()
	stop := context.AfterFunc(ctx, func() { file.Close() })
	defer stop()
	info, err := file.Stat()
	if err != nil {
		return &JobLog{Error: err.Error(), ErrorKind: ClassifyError(err)}
	}
	data, truncated, err := readTail(file, info.Size(), lines)
	if err != nil {
		return &JobLog{Error: err.Error(), ErrorKind: ClassifyError(err)}
	} else if err := ctx.Err(); err != nil {
		return &JobLog{Error: err.Error(), ErrorKind: ClassifyError(err)}
	}
	return &JobLog{Data: data, Truncated: truncated}
}

// readTail reads the final lines, which must be at least one, of an open log
// whose size is known, limited to its final MaximumJobLogSize bytes, and
// reports whether that limit cut them.
func readTail(file *os.File, size int64, lines int64) ([]byte, bool, error) {
	offset := max(int64(0), size-MaximumJobLogSize-1)
	data := make([]byte, int(size-offset))
	if _, err := file.ReadAt(data, offset); err != nil {
		return nil, false, fmt.Errorf("unable to read log snapshot: %w", err)
	}

	// An ending newline terminates the last line rather than starting an
	// empty one. An unterminated final line counts as a line as well. Count
	// down, so even the largest requested count can't overflow.
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	truncated := offset > 0
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			lines--
			if lines == 0 {
				data, truncated = data[i+1:], false
				break
			}
		}
	}
	if len(data) > MaximumJobLogSize {
		data, truncated = data[len(data)-MaximumJobLogSize:], true
	}
	return data, truncated, nil
}

// jobLogBuffer bounds received log data independently of the executor. Excess
// output is discarded so that the job's exit status can still be received.
type jobLogBuffer struct {
	data     []byte
	overflow bool
}

func (b *jobLogBuffer) Write(data []byte) (int, error) {
	count := min(len(data), MaximumJobLogSize-len(b.data))
	b.data = append(b.data, data[:count]...)
	b.overflow = b.overflow || count < len(data)
	return len(data), nil
}

// waitMargin is the longest time by which a wait with a deadline asks the
// executing endpoint to return before the deadline, so that the job and its
// log's tail, which the endpoint reads once it stops waiting, can arrive in
// time over slow connections. Waits with less time left than twice the margin
// use half of the time left instead.
const waitMargin = time.Second

// WaitOptions describe how WaitJobWithOptions waits for a job.
type WaitOptions struct {
	// Tail is the number of final log lines to read once the job ends, or once
	// Deadline passes while it's running. Zero disables log reading. It's
	// ignored if Follow is non-nil.
	Tail int64
	// Deadline, if non-zero, is the time at which a wait that reads a tail
	// returns the job even if it's still running, along with the tail. It
	// should be the context's deadline: the executing endpoint is asked to
	// return somewhat earlier (see waitMargin), so that its result arrives
	// before the context ends the wait. Other waits, including those that
	// follow logs, whose contents have arrived already, end with the context
	// instead, so that they don't stop waiting early.
	Deadline time.Time
	// Follow, if non-nil, streams the job's log to its output while waiting.
	Follow *LogFollower
	// Warn, if non-nil, receives warnings about the job's log, none of which
	// affect the wait, such as logs that can't be followed. It's never called
	// concurrently, or after WaitJobWithOptions returns.
	Warn func(string)
}

// WaitJobWithOptions waits for the job with the specified identifier, started
// at the opposite endpoint's root, to end over an established route (see Dial),
// and returns the job. It owns and closes the connection. Cancellation abandons
// the wait without affecting the job. It also reads the final lines of the
// job's log, which it returns along with the job if the job is still running
// once a deadline approaches, or streams the log while waiting, as the options
// specify. Errors reading the log are reported in JobLog.Error, or
// through WaitOptions.Warn while following, without affecting the job's status.
// A tail is buffered until the request succeeds, so callers can retry
// interrupted requests without duplicating partial output, while followed
// output, which isn't buffered, is resumed by retrying with the same follower.
// A nil log is returned if no tail was read.
func WaitJobWithOptions(ctx context.Context, connection io.ReadWriteCloser, id string, options *WaitOptions) (*Job, *JobLog, error) {
	warn := options.Warn
	if warn == nil {
		warn = func(string) {}
	}

	// Build the request. Followers whose logs can't be followed any more wait
	// without following them.
	request := &JobRequest{Operation: JobWait, ID: id, Tail: options.Tail}
	if follow := options.Follow; follow != nil {
		request.Tail = 0
		if follow.state == following {
			request.Follow = follow.request()
		}
	}
	if !options.Deadline.IsZero() && request.Tail > 0 {
		remaining := time.Until(options.Deadline)
		request.Timeout = max(remaining-min(waitMargin, remaining/2), time.Millisecond)
	}

	// Send the request, receiving a followed log as it arrives, or else
	// buffering the tail, if any.
	var output io.Writer
	var buffer jobLogBuffer
	var stream *io.PipeWriter
	var received chan struct{}
	if request.Follow != nil {
		var reader *io.PipeReader
		reader, stream = io.Pipe()
		received = make(chan struct{})
		go func() {
			defer close(received)
			options.Follow.receive(reader, id, warn)
			io.Copy(io.Discard, reader)
		}()
		output = stream
	} else {
		output = &buffer
	}
	response, err := execute(ctx, connection, &Request{Job: request}, strings.NewReader(""), output, io.Discard)
	if stream != nil {
		stream.Close()
		<-received
	}
	if err != nil {
		return nil, nil, err
	} else if response.Error != "" {
		return nil, nil, receivedError(response.ErrorKind, &JobError{response.Error})
	} else if response.Job == nil {
		return nil, nil, ConnectionError(errors.New("no job received"))
	} else if response.Job.State == JobRunning && request.Timeout == 0 {
		return nil, nil, ConnectionError(errors.New("the executing endpoint returned a running job"))
	} else if request.Tail == 0 {
		return response.Job, nil, nil
	}
	log := response.JobLog
	if log == nil {
		return nil, nil, ConnectionError(errors.New("no job log received"))
	} else if buffer.overflow {
		log = &JobLog{Error: "the executing endpoint returned a log tail larger than 1 MiB", ErrorKind: ErrorKindConnection}
	} else if log.Error == "" {
		log.Data = buffer.data
	}
	return response.Job, log, nil
}
