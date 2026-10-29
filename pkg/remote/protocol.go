package remote

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// maximumMessageSize bounds command metadata independently of streamed I/O.
const maximumMessageSize = 1024 * 1024

// MaximumScriptSize bounds the size of scripts (see Request.Script), so that
// requests carrying them, whose metadata encodes them in base64, fit into
// protocol messages alongside their other metadata.
const MaximumScriptSize = 512 * 1024

// Script is a script that the executing endpoint runs (see Request.Script).
type Script struct {
	// Name describes the script's source in diagnostics, such as the path from
	// which the submitter read it, or - for its standard input.
	Name string
	// Contents are the script's contents, which needn't be valid UTF-8.
	Contents []byte
}

// JobOperation identifies an operation on detached jobs (see JobRequest).
type JobOperation string

const (
	// JobStart starts the requested command as a detached job.
	JobStart JobOperation = "start"
	// JobList lists the detached jobs started at the executing root.
	JobList JobOperation = "list"
	// JobWait waits for a detached job to finish.
	JobWait JobOperation = "wait"
	// JobKill terminates a detached job.
	JobKill JobOperation = "kill"
)

// JobRequest requests an operation on detached jobs (see Request.Job).
type JobRequest struct {
	// Operation is the requested operation.
	Operation JobOperation
	// ID identifies the job for JobWait and JobKill.
	ID string `json:",omitempty"`
	// Log optionally specifies the file to which a job started by JobStart
	// writes its output, which is otherwise kept with the job's state.
	// Absolute and home-relative paths are interpreted on the executing
	// system, while other paths are resolved against the command's working
	// directory.
	Log string `json:",omitempty"`
	// Tail is the number of final log lines requested with JobWait. Zero
	// disables log reading, and negative counts are invalid. It must be zero
	// if Follow is non-nil.
	Tail int64 `json:",omitempty"`
	// Follow, if non-nil, requests that JobWait stream the job's log as output
	// while waiting, as frames (see logFrame), instead of sending its tail.
	// Once the job ends, the log's contents up to then are sent before the
	// result. Executors that predate following, which don't report
	// featureFollow, ignore it.
	Follow *JobFollow `json:",omitempty"`
	// Timeout, if positive, is the time after which JobWait returns the job
	// even if it's still running, along with the tail requested by Tail, so
	// that the result can arrive before the client stops waiting. Executors
	// that predate it, which don't report featureWaitTimeout, ignore it.
	Timeout time.Duration `json:",omitempty"`
}

// JobFollow describes where JobWait starts streaming a job's log (see
// JobRequest.Follow).
type JobFollow struct {
	// Tail indicates that streaming starts with the log's final Lines lines,
	// limited to its final MaximumJobLogSize bytes as tails are, or at its end
	// if Lines is zero. Otherwise, streaming starts at Offset.
	Tail bool `json:",omitempty"`
	// Lines is the number of final lines with which streaming starts if Tail
	// is set. Negative counts are invalid.
	Lines int64 `json:",omitempty"`
	// Offset is the log offset at which streaming starts unless Tail is set:
	// zero for the log's beginning, or the offset up to which a previous wait
	// received the log, to resume streaming where it stopped.
	Offset int64 `json:",omitempty"`
	// Identity identifies the log file that a previous wait streamed, as its
	// frames reported it, so that streaming doesn't resume in another file that
	// has replaced it at the log's path. It's empty if unknown.
	Identity string `json:",omitempty"`
}

// ensureValid checks the options of a job request, if any, that only apply to
// waiting for a job.
func (r *JobRequest) ensureValid() error {
	if r == nil {
		return nil
	} else if r.Tail < 0 || r.Tail != 0 && r.Operation != JobWait {
		return errors.New("a non-negative log tail count can only be specified when waiting for a job")
	} else if r.Timeout < 0 || r.Timeout != 0 && r.Operation != JobWait {
		return errors.New("a non-negative timeout can only be specified when waiting for a job")
	} else if r.Follow == nil {
		return nil
	} else if r.Operation != JobWait || r.Tail != 0 {
		return errors.New("logs can only be followed when waiting for a job, without a log tail")
	} else if r.Follow.Lines < 0 || r.Follow.Offset < 0 {
		return errors.New("log positions must not be negative")
	} else if r.Follow.Tail && (r.Follow.Offset != 0 || r.Follow.Identity != "") || !r.Follow.Tail && r.Follow.Lines != 0 {
		return errors.New("logs are followed either from their tails or from offsets")
	} else if !utf8.ValidString(r.Follow.Identity) || strings.IndexByte(r.Follow.Identity, 0) >= 0 {
		return errors.New("log identity contains invalid UTF-8 or a null byte")
	}
	return nil
}

// Request describes a single command (or script), which may be run as a
// detached job, or a request for a file or for another job operation.
// Arguments are passed directly to the executable, without shell
// interpretation. Environment entries override the executing process's
// inherited environment, with later entries taking precedence. The executor
// sets MUTAGEN_REMOTE_ENDPOINT after these entries to identify itself as alpha
// or beta.
type Request struct {
	// Arguments contains the executable name followed by its arguments, or the
	// script's arguments if Script is non-nil.
	Arguments []string
	// Environment contains explicit overrides of the remote environment.
	Environment []string
	// Path is the slash-separated working directory relative to the target
	// synchronization root. An empty path refers to the root itself.
	Path string
	// Directory optionally overrides the working directory. Absolute and
	// home-relative values are interpreted on the target system, while other
	// values are resolved against the directory specified by Path.
	Directory string
	// Prologue indicates whether the executing endpoint's workspace override or
	// configured prologue, if any, should be sourced. Submitters request it when
	// the submission directory lies inside the submitting root (and thus Path
	// identifies the corresponding directory), regardless of Directory.
	Prologue bool
	// File is the slash-separated path, relative to the target root, of an
	// entry to read (see ReadEntry) instead of running a command. Requests for
	// files specify no arguments. A dot refers to the root itself.
	File string
	// Copy is the slash-separated path, relative to the target root, of a
	// regular file whose complete contents are sent as output (see CopyFile)
	// instead of running a command. A dot refers to the root itself.
	Copy string
	// Archive is the slash-separated path, relative to the target root, of an
	// entry whose archive is sent as output (see ArchiveEntry) instead of
	// running a command. It must not refer to the root itself.
	Archive string `json:",omitempty"`
	// Replace requests replacement or deletion of an entry, with contents read
	// from input (see ReplaceFile), instead of running a command.
	Replace *Replacement `json:",omitempty"`
	// Script, if non-nil, is run instead of an executable: the executing
	// endpoint writes it to a file and runs the interpreter named by its #!
	// line (or bash, if it has none) with that file and Arguments, which may
	// be empty, as arguments.
	Script *Script `json:",omitempty"`
	// Job, if non-nil, requests an operation on detached jobs. JobStart runs the
	// command (or script) as a detached job, while other operations specify no
	// command.
	Job *JobRequest `json:",omitempty"`
}

// validJobID determines whether a value is a valid job identifier, which
// consists of 8 lowercase hexadecimal digits, so that it also names the job's
// directory safely.
func validJobID(value string) bool {
	if len(value) != 8 {
		return false
	}
	for _, c := range value {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// validRelativePath determines whether a slash-separated path lies inside the
// root that it's relative to.
func validRelativePath(value string) bool {
	return !path.IsAbs(value) && path.Clean(value) == value && value != ".." &&
		!strings.HasPrefix(value, "../") && !strings.Contains(value, "\\")
}

// EnsureValid checks that command metadata can be represented losslessly by
// the protocol and passed to the operating system.
func (r *Request) EnsureValid() error {
	if r == nil {
		return errors.New("no command specified")
	} else if err := r.Job.ensureValid(); err != nil {
		return err
	} else if (r.File != "" || r.Copy != "" || r.Archive != "" || r.Replace != nil) && (r.Script != nil || r.Job != nil) {
		return errors.New("file operations must not specify a script or a job operation")
	} else if r.Copy != "" || r.Archive != "" || r.Replace != nil {
		operations, path := 0, ""
		if r.Copy != "" {
			operations, path = operations+1, r.Copy
		}
		if r.Archive != "" {
			operations, path = operations+1, r.Archive
		}
		if r.Replace != nil {
			operations, path = operations+1, r.Replace.Path
		}
		if len(r.Arguments) > 0 || r.File != "" || operations > 1 {
			return errors.New("file operations must not specify a command or another operation")
		} else if path == "" || !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 || !validRelativePath(path) {
			return errors.New("file operation path must be relative to the synchronization root")
		} else if path == "." && r.Archive != "" {
			return errors.New("the synchronization root can't be archived")
		}
		return nil
	} else if r.File != "" {
		if len(r.Arguments) > 0 {
			return errors.New("file requests must not specify a command")
		} else if !utf8.ValidString(r.File) || strings.IndexByte(r.File, 0) >= 0 || !validRelativePath(r.File) {
			return errors.New("requested file path must be relative to the synchronization root")
		}
		return nil
	} else if r.Job != nil && r.Job.Operation != JobStart {
		if len(r.Arguments) > 0 || r.Script != nil || r.Job.Log != "" {
			return errors.New("job operations other than starting a job must not specify a command")
		}
		switch r.Job.Operation {
		case JobList:
			if r.Job.ID != "" {
				return errors.New("listing jobs doesn't take a job identifier")
			}
		case JobWait, JobKill:
			if !validJobID(r.Job.ID) {
				return fmt.Errorf("invalid job identifier %q (job identifiers consist of 8 lowercase hexadecimal digits)", r.Job.ID)
			}
		default:
			return fmt.Errorf("unknown job operation %q", r.Job.Operation)
		}
		return nil
	} else if r.Script == nil && (len(r.Arguments) == 0 || r.Arguments[0] == "") {
		return errors.New("no command specified")
	} else if r.Script != nil && len(r.Script.Contents) > MaximumScriptSize {
		return fmt.Errorf("script exceeds the size limit of %d bytes", MaximumScriptSize)
	} else if r.Job != nil && r.Job.ID != "" {
		return errors.New("job identifiers are assigned when jobs start")
	}
	values := append([]string{r.Path, r.Directory}, r.Arguments...)
	if r.Script != nil {
		values = append(values, r.Script.Name)
	}
	if r.Job != nil {
		values = append(values, r.Job.Log)
	}
	for _, value := range append(values, r.Environment...) {
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return errors.New("command metadata contains invalid UTF-8 or a null byte")
		}
	}
	for _, entry := range r.Environment {
		if key, _, ok := strings.Cut(entry, "="); !ok || key == "" {
			return errors.New("environment entries must have the form NAME=VALUE")
		}
	}
	// The relative path is computed by the submitting endpoint and must stay
	// inside the target root. Explicit directory overrides are not restricted.
	if r.Path != "" && !validRelativePath(r.Path) {
		return errors.New("working directory path must be relative to the synchronization root")
	}
	return nil
}

// Operation identifies what a submission requests from the daemon.
type Operation uint8

const (
	// OperationExecute requests a route to the opposite endpoint's executor.
	OperationExecute Operation = iota
	// OperationFlush requests a synchronization cycle, whose completion the
	// daemon reports with a status message.
	OperationFlush
	// OperationConflicts requests a synchronization cycle followed by a
	// complete listing of the session's conflicts (see ListConflicts).
	OperationConflicts
	// OperationClaim requests that the daemon record a checkout of a conflict
	// by the submitting endpoint (see ClaimCheckout).
	OperationClaim
	// OperationRelease requests that the daemon remove a checkout held by the
	// submitting endpoint (see ReleaseCheckout).
	OperationRelease
	// OperationNodes queries the submitting workspace's node graph without
	// synchronizing files or executing a command.
	OperationNodes
	// OperationResolve selects a destination (see Submission.Resolve) without
	// other side effects.
	OperationResolve
)

// Submission is the header of each submission, written by the submitting
// client and consumed by the daemon before it handles the submission (see
// ReceiveSubmission). Hops between them forward it unchanged, along with the
// rest of the route's data.
type Submission struct {
	// Operation is the requested operation.
	Operation Operation
	// Resolve requests a routing acknowledgement before the operation response.
	// The acknowledgement shares this connection; no discovery round trip is needed.
	Resolve bool `json:",omitempty"`
	// Node selects the destination for every operation. The daemon identifies
	// the source from the submitting endpoint, never from client input.
	Node string `json:",omitempty"`
	// Revision pins the edge selected by an operation's first submission (see
	// ResolvedRoute) for its later submissions, which fail if that edge was
	// replaced or removed in the meantime. Other edges don't affect it.
	Revision string `json:",omitempty"`
	// Progress requests progress messages (see ReportProgress) while the
	// daemon waits for the synchronization cycle of an OperationFlush or
	// OperationConflicts submission. Daemons that predate it ignore it and
	// send only their final response, which submissions must accept.
	Progress bool `json:",omitempty"`
}

// Features of the protocol that executing endpoints report supporting (see
// status.Features), so that submitters can adapt their requests to executors
// of other versions, which ignore request fields that they don't know.
const (
	// featureFollow indicates that the executor streams job logs while waiting
	// for jobs (see JobRequest.Follow).
	featureFollow = "follow"
	// featureWaitTimeout indicates that the executor returns running jobs once
	// a wait's timeout expires (see JobRequest.Timeout).
	featureWaitTimeout = "wait-timeout"
)

// executorFeatures are the features that this executor reports supporting.
var executorFeatures = []string{featureFollow, featureWaitTimeout}

// status reports whether a command route reached an executing endpoint. It is
// written by the executing endpoint on success or by any intermediate hop that
// is unable to establish the route.
type status struct {
	// Error is the routing error, if any.
	Error string
	// ErrorKind classifies Error independently of its diagnostic text.
	ErrorKind ErrorKind `json:",omitempty"`
	// Features are the optional protocol features that the executing endpoint
	// supports. Executors that predate feature reporting report none, and hops
	// that report routing errors don't report any.
	Features []string `json:",omitempty"`
}

// supports indicates whether the executing endpoint reported supporting a
// feature.
func (s *status) supports(feature string) bool {
	return slices.Contains(s.Features, feature)
}

// flushResponse reports the outcome of a flush, or its progress. Hops that are
// unable to reach the daemon send a status instead, which decodes compatibly
// as an outcome.
type flushResponse struct {
	// Error is the flush or routing error, if any.
	Error string
	// ErrorKind classifies Error independently of its diagnostic text.
	ErrorKind ErrorKind `json:",omitempty"`
	// Report describes content that the session couldn't synchronize. It's
	// non-nil if and only if the flush succeeded.
	Report *Report `json:",omitempty"`
	// Progress is set, without Error and Report, by the progress messages that
	// precede the outcome if the submission requested them (see
	// progressMessage).
	Progress *Progress `json:",omitempty"`
}

// progressMessage is a progress message sent to a flush or conflicts
// submission that requested progress (see Submission.Progress), before the
// final response. It decodes as a flushResponse or conflictsMessage with only
// Progress set, which no final response or status is.
type progressMessage struct {
	// Progress is the session's progress.
	Progress *Progress
}

// result distinguishes command exit status from execution infrastructure errors.
type result struct {
	// ExitCode is the command exit code.
	ExitCode int
	// Error is the execution infrastructure error, if any.
	Error string
	// ErrorKind classifies Error independently of its diagnostic text.
	ErrorKind ErrorKind `json:",omitempty"`
	// DirectoryError classifies a working directory failure without requiring
	// clients to inspect Error. Directory identifies the failed execution path,
	// or the explicit path that could not be expanded.
	DirectoryError DirectoryErrorKind `json:",omitempty"`
	Directory      string             `json:",omitempty"`
	// Entry is the requested entry for file requests, excluding its data,
	// which is sent as output.
	Entry *Entry `json:",omitempty"`
	// Stale indicates that a replacement failed because the entry changed
	// since it was checked out (see ErrStale).
	Stale bool `json:",omitempty"`
	// Superseded indicates that a replacement failed because a replacement
	// with a later sequence number was admitted first (see ErrSuperseded).
	Superseded bool `json:",omitempty"`
	// Job describes the job started, waited for, or terminated by a job
	// request.
	Job *Job `json:",omitempty"`
	// JobLog describes the requested tail of a waited-for job's log. Its
	// contents are sent as output. A nil value means no tail was requested,
	// or the executing endpoint predates log tails.
	JobLog *JobLog `json:",omitempty"`
	// HiddenJobs is the number of jobs omitted from a job listing because they
	// were started at other roots.
	HiddenJobs int `json:",omitempty"`
}

// writeMessage writes bounded, length-prefixed command metadata.
func writeMessage(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	} else if len(data) > maximumMessageSize {
		return errors.New("command metadata exceeds size limit")
	}
	// Encoding and size errors are local input failures. Only failures in
	// writing the encoded message belong to the connection.
	transport := connectionWriter{writer}
	if err := binary.Write(transport, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err = transport.Write(data)
	return err
}

// readMessage reads bounded, length-prefixed command metadata.
func readMessage(reader io.Reader, value any) error {
	var size uint32
	if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
		return err
	} else if size > maximumMessageSize {
		return errors.New("command metadata exceeds size limit")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return err
	} else if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("invalid command metadata: %w", err)
	}
	return nil
}
