package remote

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// followChunkSize bounds the log contents that are read and sent at once while
// streaming a job's log, and thus the memory that streaming uses on both ends,
// however large the log is or however fast it grows.
const followChunkSize = 64 * 1024

// logFrame precedes each part of a job's log that JobWait streams as output
// (see JobRequest.Follow), which follows the frame as Size raw bytes. Frames
// are length-prefixed as other protocol messages are. Each wait's first frame
// reports where streaming starts and the log's identity, and frames' Offsets
// let clients resume streaming without repeating or skipping contents.
type logFrame struct {
	// Offset is the offset in the log of the contents that follow the frame.
	Offset int64
	// Size is the number of bytes of the log's contents that follow the frame,
	// at most followChunkSize.
	Size int `json:",omitempty"`
	// Identity identifies the log file (see logIdentity). It's reported in each
	// wait's first frame, if known.
	Identity string `json:",omitempty"`
	// Cut indicates that the final lines with which streaming starts (see
	// JobFollow.Tail) were limited to MaximumJobLogSize bytes, so that the
	// contents may start within a line.
	Cut bool `json:",omitempty"`
	// Shrunk, if positive, is the offset up to which the log had been streamed
	// when it was found to be shorter, such as because it was truncated, so
	// that streaming restarted at its beginning (with Offset zero).
	Shrunk int64 `json:",omitempty"`
	// Error, if non-empty, reports why the log can't be streamed (any more).
	// It's sent in the final frame, after which the wait continues without
	// streaming.
	Error string `json:",omitempty"`
}

// logStreamer streams a job's log as frames (see logFrame) while JobWait waits
// for the job. It keeps the log open, so that it keeps streaming the file that
// it opened even if the log's path is renamed, removed, or replaced, which
// doesn't affect the file that the job writes either.
type logStreamer struct {
	// output receives the frames.
	output io.Writer
	// file is the open log, or nil once streaming has stopped.
	file *os.File
	// stopClosing cancels the closing of the log upon cancellation.
	stopClosing func() bool
	// offset is the offset up to which the log has been sent.
	offset int64
	// chunk is the buffer into which the log is read.
	chunk []byte
	// message is the buffer in which frames and their contents are encoded.
	message []byte
}

// streamJobLog opens a job's log and starts streaming it from the position that
// the request specifies, sending a first frame that reports where streaming
// starts, along with the requested final lines, if any. Logs that can't be
// streamed, such as missing logs, logs that aren't regular files, or logs
// replaced since a previous wait streamed them, are reported in a final frame,
// which doesn't affect the wait. Only failures to send frames are returned.
// The streamer must be closed once the wait is done.
func streamJobLog(ctx context.Context, path string, request *JobFollow, output io.Writer) (*logStreamer, error) {
	s := &logStreamer{output: output}
	file, err := openJobLog(path)
	if err != nil {
		return s, s.stop(err.Error())
	}
	s.file, s.stopClosing = file, context.AfterFunc(ctx, func() { file.Close() })
	info, err := file.Stat()
	if err != nil {
		return s, s.stop(err.Error())
	}
	size := info.Size()

	// Determine where streaming starts. A requested offset beyond the log's
	// end means that the log became shorter since the previous wait, such as
	// because it was truncated, so streaming restarts at its beginning. A
	// file with another identity than the one that the previous wait streamed
	// has replaced the log at its path, while the job keeps writing to the
	// file that it opened, so it may be another job's log, or not one at all.
	first := logFrame{Identity: logIdentity(info)}
	var data []byte
	if request.Tail {
		s.offset = size
		if request.Lines > 0 {
			if data, first.Cut, err = readTail(file, size, request.Lines); err != nil {
				return s, s.stop(err.Error())
			}
			s.offset -= int64(len(data))
		}
	} else if request.Identity != "" && first.Identity != "" && request.Identity != first.Identity {
		return s, s.stop("the file at its path was replaced since remote last read it, so it may not be the job's log")
	} else if request.Offset > size {
		first.Shrunk = request.Offset
	} else {
		s.offset = request.Offset
	}
	return s, s.send(first, data)
}

// poll sends the log's contents that were appended since it was last polled, up
// to its current size, in parts of at most followChunkSize bytes. A log that's
// found to be shorter than the offset up to which it has been sent, such as
// because it was truncated, is streamed from its beginning again, which is
// reported. Read failures stop streaming, which is reported in a final frame.
// Only failures to send frames are returned.
func (s *logStreamer) poll() error {
	if s.file == nil {
		return nil
	}
	info, err := s.file.Stat()
	if err != nil {
		return s.stop(fmt.Sprintf("unable to check log size: %v", err))
	}
	size := info.Size()
	if size < s.offset {
		shrunk := s.offset
		s.offset = 0
		if err := s.send(logFrame{Shrunk: shrunk}, nil); err != nil {
			return err
		}
	}
	if s.chunk == nil {
		s.chunk = make([]byte, followChunkSize)
	}
	for s.offset < size {
		count, err := s.file.ReadAt(s.chunk[:min(size-s.offset, followChunkSize)], s.offset)
		if count > 0 {
			if err := s.send(logFrame{}, s.chunk[:count]); err != nil {
				return err
			}
		}
		// The log became shorter while it was being read, which the next
		// poll reports.
		if err == io.EOF {
			break
		} else if err != nil {
			return s.stop(fmt.Sprintf("unable to read log: %v", err))
		}
	}
	return nil
}

// send sends a frame with log contents that start at the offset up to which the
// log has been sent, splitting them into parts of at most followChunkSize
// bytes, with the frame's other fields sent with the first part, and advances
// the offset accordingly.
func (s *logStreamer) send(frame logFrame, data []byte) error {
	for first := true; first || len(data) > 0; first = false {
		frame.Offset, frame.Size = s.offset, min(len(data), followChunkSize)
		header, err := json.Marshal(&frame)
		if err != nil {
			return fmt.Errorf("unable to encode log frame: %w", err)
		}
		s.message = binary.BigEndian.AppendUint32(s.message[:0], uint32(len(header)))
		s.message = append(append(s.message, header...), data[:frame.Size]...)
		if _, err := s.output.Write(s.message); err != nil {
			return fmt.Errorf("unable to send job log: %w", err)
		}
		s.offset += int64(frame.Size)
		data, frame = data[frame.Size:], logFrame{}
	}
	return nil
}

// stop stops streaming, closing the log, and reports why in a final frame.
func (s *logStreamer) stop(reason string) error {
	s.close()
	return s.send(logFrame{Error: reason}, nil)
}

// close closes the log, if it's open.
func (s *logStreamer) close() {
	if s.file != nil {
		s.stopClosing()
		s.file.Close()
		s.file = nil
	}
}

// followState is the state of a LogFollower.
type followState uint8

const (
	// following indicates that the log is followed.
	following followState = iota
	// followStopped indicates that the log can't be followed, such as because
	// it's missing or isn't a regular file, which was reported.
	followStopped
	// followUnsupported indicates that the executing endpoint doesn't support
	// following logs, which was reported.
	followUnsupported
)

// LogFollower receives a job's log while WaitJobWithOptions waits for the job
// (see WaitOptions.Follow), and records how much of it has been received, so
// that waits retried with the same follower, such as after the connection to
// the executing endpoint was lost, resume where the previous one stopped,
// without repeating or skipping any of the log.
type LogFollower struct {
	// Output receives the log's contents as they arrive, unchanged. If writing
	// to it fails, the rest of the log is discarded, which is reported, without
	// affecting the wait.
	Output io.Writer
	// Lines, if non-negative, starts following with the log's final Lines
	// lines, which are limited to its final MaximumJobLogSize bytes as tails
	// are, or at its end if it's zero. If negative, following starts at the
	// log's beginning.
	Lines int64

	// state is the state of following.
	state followState
	// started indicates that a wait has reported where following starts,
	// after which offset and identity are known.
	started bool
	// offset is the offset in the log up to which it has been received.
	offset int64
	// identity is the identity of the followed log file, if known.
	identity string
	// partial indicates that the contents received so far end within a line.
	partial bool
	// discarding indicates that writing to Output failed, after which the
	// log's contents are discarded.
	discarding bool
}

// PartialLine indicates whether the log's contents written to the output so
// far end within a line, such as with a progress bar that's redrawn by
// carriage returns, so that output that follows them, such as on a terminal
// that shows both, would continue that line.
func (f *LogFollower) PartialLine() bool {
	return f.partial
}

// request returns the request that resumes following where it stopped, or
// starts it as requested if no wait has reported where it starts yet.
func (f *LogFollower) request() *JobFollow {
	if f.started {
		return &JobFollow{Offset: f.offset, Identity: f.identity}
	} else if f.Lines >= 0 {
		return &JobFollow{Tail: true, Lines: f.Lines}
	}
	return &JobFollow{}
}

// receive receives the frames of a streamed log (see logFrame) for the job
// with the specified identifier until the stream ends, writing their contents
// to the output and reporting problems through warn. Contents that were already
// received are skipped. A stream that ends within a frame, as it does when the
// connection is lost, ends receiving without a warning, since the wait reports
// that, while invalid frames stop following.
func (f *LogFollower) receive(reader io.Reader, id string, warn func(string)) {
	output := &followerOutput{follower: f, id: id, warn: warn}
	for {
		var frame logFrame
		err := readMessage(reader, &frame)
		if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
			return
		} else if err == nil && (frame.Offset < 0 || frame.Size < 0 || frame.Size > followChunkSize || frame.Shrunk < 0) {
			err = errors.New("invalid log position")
		}
		if err != nil {
			f.state = followStopped
			warn(fmt.Sprintf("unable to follow the log of job %s: invalid log stream: %v", id, err))
			return
		}

		// Handle the frame's reports.
		if frame.Error != "" {
			f.state = followStopped
			warn(fmt.Sprintf("unable to follow the log of job %s: %s", id, frame.Error))
			return
		} else if frame.Shrunk > 0 {
			f.offset = 0
			warn(fmt.Sprintf("the log of job %s became shorter than the %d bytes followed so far, such as because it was truncated; following it from its beginning", id, frame.Shrunk))
		}
		if !f.started {
			f.started, f.offset = true, frame.Offset
		}
		if frame.Identity != "" {
			f.identity = frame.Identity
		}
		if frame.Cut {
			warn("the requested log tail exceeds 1 MiB; following starts with only its final 1 MiB, which may start within a line")
		}

		// Write the frame's contents, skipping any that were already received.
		// Executors resume where requested, so contents are neither repeated
		// nor skipped unless the stream is invalid.
		if frame.Offset > f.offset {
			warn(fmt.Sprintf("%d bytes of the log of job %s were skipped", frame.Offset-f.offset, id))
			f.offset = frame.Offset
		}
		skip := min(f.offset-frame.Offset, int64(frame.Size))
		if _, err := io.CopyN(io.Discard, reader, skip); err != nil {
			return
		} else if _, err := io.CopyN(output, reader, int64(frame.Size)-skip); err != nil {
			return
		}
	}
}

// followerOutput writes the contents of a followed log to a follower's output,
// counting what it receives (see LogFollower.receive).
type followerOutput struct {
	// follower is the follower.
	follower *LogFollower
	// id is the followed job's identifier.
	id string
	// warn reports failures to write the output.
	warn func(string)
}

// Write implements io.Writer.Write. It never fails, so that contents that can't
// be written are still counted as received.
func (o *followerOutput) Write(data []byte) (int, error) {
	f := o.follower
	f.offset += int64(len(data))
	if len(data) > 0 {
		f.partial = data[len(data)-1] != '\n'
	}
	if !f.discarding {
		if _, err := f.Output.Write(data); err != nil {
			f.discarding = true
			o.warn(fmt.Sprintf("unable to print the log of job %s, so the rest of it is discarded: %v", o.id, err))
		}
	}
	return len(data), nil
}
