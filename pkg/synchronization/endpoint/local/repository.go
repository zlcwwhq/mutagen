package local

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/git"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

const (
	// repositoryChunkSize is the maximum size of the parts of a pack that
	// SupplyRepository passes to a receiver.
	repositoryChunkSize = 256 * 1024
)

// repositoryReceiver implements synchronization.RepositoryReceiver by passing
// a pack to git index-pack, which adds its objects to the repository at the
// root, or to the one that's being prepared for attachment. Like a fetch, this
// only adds objects. The command runs independently of the context of the call
// to StageRepository that started it, and it's stopped when the receiver is
// finished, when StageRepository or TransitionRepository is called again
// without finishing it, or when the endpoint is shut down.
type repositoryReceiver struct {
	// endpoint is the endpoint.
	endpoint *endpoint
	// temporary is the directory of the repository that's being prepared for
	// attachment, or empty if objects are added to the root's repository.
	temporary string
	// command is the index-pack command.
	command *exec.Cmd
	// input is the command's standard input.
	input io.WriteCloser
	// output records the command's standard error.
	output *git.OutputRecorder
	// cancel stops the command.
	cancel context.CancelFunc
	// writeFailed indicates that writing to the command has failed, which
	// happens if it has exited, in which case the rest of the pack is
	// discarded, and waiting on it reports why.
	writeFailed bool
	// finished indicates that the receiver has been finished.
	finished bool
}

// Receive implements synchronization.RepositoryReceiver.Receive.
func (r *repositoryReceiver) Receive(data []byte) error {
	if r.finished {
		return errors.New("receiver finished")
	} else if !r.writeFailed {
		if _, err := r.input.Write(data); err != nil {
			r.writeFailed = true
		}
	}
	return nil
}

// Finish implements synchronization.RepositoryReceiver.Finish.
func (r *repositoryReceiver) Finish(problem string) error {
	// Ensure that the receiver isn't finished twice.
	if r.finished {
		return errors.New("receiver already finished")
	}
	r.finished = true
	if r.endpoint.repositoryReceiver == r {
		r.endpoint.repositoryReceiver = nil
	}

	// If the supplier has failed, then stop the command, so that whatever it
	// has received is discarded, even if it forms a complete pack. Otherwise,
	// signal the end of the pack and wait for the command to store it.
	if problem != "" {
		r.cancel()
	}
	r.input.Close()
	err := r.command.Wait()
	r.cancel()
	if err != nil && errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}

	// Record any failure for TransitionRepository, and discard the prepared
	// repository, which is useless without the objects. The supplier's
	// problem says what happened by itself (see SupplyRepository).
	if problem != "" {
		r.endpoint.repositoryProblem = gitRootText(problem)
	} else if err != nil {
		r.endpoint.repositoryProblem = "the commits that arrived couldn't be stored: " + (&git.CommandError{Err: err, Output: r.output.Message()}).Error()
	}
	if r.endpoint.repositoryProblem != "" && r.temporary != "" {
		os.RemoveAll(r.temporary)
	}
	return nil
}

// abandonRepositoryReceiver finishes the outstanding receiver, if any, as if
// the supplier had failed.
func (e *endpoint) abandonRepositoryReceiver() {
	if e.repositoryReceiver != nil {
		e.repositoryReceiver.Finish("the transfer of the commits was abandoned")
	}
}

// startRepositoryReceiver starts a receiver that adds objects to the
// repository in a directory, which is the root's, or that of the repository
// that's being prepared for attachment if temporary is non-empty. If the
// receiver can't be started, then it records why for TransitionRepository and
// returns nil.
func (e *endpoint) startRepositoryReceiver(directory, temporary string) *repositoryReceiver {
	// Create the command. Thin packs refer to objects that aren't in the pack,
	// which the repository has, and which have to be added to the pack to
	// make it usable on its own.
	ctx, cancel := context.WithCancel(context.Background())
	command := git.Command(ctx, directory, "index-pack", "--stdin", "--fix-thin")
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		e.repositoryProblem = fmt.Sprintf("the commits couldn't be received: %v", err)
		return nil
	}
	output := git.NewOutputRecorder()
	command.Stderr = output

	// Start the command.
	if err := command.Start(); err != nil {
		cancel()
		e.repositoryProblem = "the commits couldn't be received: " + (&git.CommandError{Err: err}).Error()
		return nil
	}

	// Create and record the receiver.
	receiver := &repositoryReceiver{
		endpoint:  e,
		temporary: temporary,
		command:   command,
		input:     input,
		output:    output,
		cancel:    cancel,
	}
	e.repositoryReceiver = receiver
	return receiver
}

// checkRepositoryAction verifies that the endpoint can apply repository
// actions and that an action is valid. Failures indicate that the controller is
// malfunctioning.
func (e *endpoint) checkRepositoryAction(action *core.RepositoryAction) error {
	if !e.gitMode {
		return errors.New("repository actions require Git mode")
	} else if e.readOnly {
		return errors.New("endpoint is in read-only mode")
	} else if err := action.EnsureValid(); err != nil {
		return fmt.Errorf("invalid repository action: %w", err)
	}
	return nil
}

// StageRepository implements synchronization.Endpoint.StageRepository.
func (e *endpoint) StageRepository(ctx context.Context, action *core.RepositoryAction) ([][]byte, synchronization.RepositoryReceiver, error) {
	// Verify that the action can be applied.
	if err := e.checkRepositoryAction(action); err != nil {
		return nil, nil, err
	}

	// Forget about any previous staging.
	e.abandonRepositoryReceiver()
	e.repositoryProblem = ""
	commit := hex.EncodeToString(action.Target.Commit)

	// If HEAD is to follow the other endpoint, then see if the commit is
	// already here, such as if it's been fetched or made here as well.
	// Otherwise, the commits that HEAD and the target branch identify here can
	// be left out of the pack, along with their history. The target branch
	// isn't validated here, since any commit that it might identify would do.
	if action.Old != nil {
		if gitHasHistory(ctx, e.root, commit) {
			return nil, nil, nil
		}
		var haves [][]byte
		if len(action.Old.Commit) > 0 {
			haves = append(haves, action.Old.Commit)
		}
		if action.Target.Branch != "" {
			output, err := git.Run(ctx, e.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+action.Target.Branch+"^{commit}")
			if err == nil {
				if tip, err := git.CommitFromOutput(output); err == nil && !bytes.Equal(tip, action.Old.Commit) {
					haves = append(haves, tip)
				}
			}
		}
		if receiver := e.startRepositoryReceiver(e.root, ""); receiver != nil {
			return haves, receiver, nil
		}
		return nil, nil, nil
	}

	// Otherwise, a repository is to be attached, which is prepared in a
	// temporary directory, so remove what any earlier attempt left behind.
	temporary := e.temporaryRepositoryPath()
	if err := os.RemoveAll(temporary); err != nil {
		e.repositoryProblem = fmt.Sprintf("the temporary directory of a previous attempt couldn't be removed: %v", err)
		return nil, nil, nil
	}

	// If the root doesn't exist (yet), or if it has a .git entry by now, then
	// there's nowhere to prepare the repository, or nothing to attach it to,
	// and TransitionRepository says so.
	if metadata, err := os.Stat(e.root); err != nil || !metadata.IsDir() {
		return nil, nil, nil
	} else if _, err := os.Lstat(filepath.Join(e.root, ".git")); !os.IsNotExist(err) {
		return nil, nil, nil
	}

	// Create the repository, with the commit's object format, and without
	// anything from a template (such as sample hooks).
	objectFormat := "sha1"
	if len(action.Target.Commit) == 32 {
		objectFormat = "sha256"
	}
	if err := os.Mkdir(temporary, 0700); err != nil {
		e.repositoryProblem = fmt.Sprintf("a temporary directory couldn't be created for the commits: %v", err)
		return nil, nil, nil
	} else if _, err := git.Run(ctx, temporary, "init", "--quiet", "--template=", "--object-format="+objectFormat); err != nil {
		os.RemoveAll(temporary)
		e.repositoryProblem = fmt.Sprintf("a repository couldn't be created for the commits: %v", err)
		return nil, nil, nil
	}

	// Receive all objects.
	if receiver := e.startRepositoryReceiver(temporary, temporary); receiver != nil {
		return nil, receiver, nil
	}
	os.RemoveAll(temporary)
	return nil, nil, nil
}

// SupplyRepository implements synchronization.Endpoint.SupplyRepository.
func (e *endpoint) SupplyRepository(ctx context.Context, commit []byte, haves [][]byte, receiver synchronization.RepositoryReceiver) (*core.Problem, error) {
	// Verify that the request is valid.
	if !e.gitMode {
		return nil, errors.New("repository actions require Git mode")
	} else if receiver == nil {
		return nil, errors.New("nil receiver")
	} else if len(commit) != 20 && len(commit) != 32 {
		return nil, errors.New("invalid commit")
	}

	// Supply the pack, and finish the receiver with any problem.
	message, err := e.supplyRepository(ctx, commit, haves, receiver)
	if err != nil {
		return nil, err
	} else if err := receiver.Finish(message); err != nil {
		return nil, err
	} else if message != "" {
		return &core.Problem{Path: gitRootProblemPath, Error: message}, nil
	}
	return nil, nil
}

// supplyRepository implements SupplyRepository, except for finishing the
// receiver. It returns a problem if the pack can't be produced, or an error if
// the receiver fails, in which case the receiver isn't finished.
func (e *endpoint) supplyRepository(ctx context.Context, commit []byte, haves [][]byte, receiver synchronization.RepositoryReceiver) (string, error) {
	// Create a function to describe failures. The description is included in
	// the receiving endpoint's problems as well, so it refers to this
	// endpoint's repository as the one that has the commit.
	failure := func(format string, arguments ...any) string {
		return gitRootText(fmt.Sprintf(
			"commit %s couldn't be supplied, because %s",
			gitShortCommit(hex.EncodeToString(commit)), fmt.Sprintf(format, arguments...),
		))
	}

	// A shallow repository doesn't have the full history of its commits, so
	// it can't produce a pack that's complete, even for a receiver that has
	// some of them, since the receiver could need what's missing.
	if output, err := git.Run(ctx, e.root, "rev-parse", "--is-shallow-repository"); err != nil {
		return failure("Git couldn't examine the repository that has it: %v", err), nil
	} else if output == "true" {
		return failure("the repository that has it is shallow; run git fetch --unshallow in that repository"), nil
	}

	// Determine which of the receiver's commits we have, since Git fails if
	// it's told to leave out the history of a commit that it doesn't have. The
	// output has a line for each one, saying what kind of object it is, or that
	// it's missing.
	var revisions strings.Builder
	revisions.WriteString(hex.EncodeToString(commit) + "\n")
	thin := false
	if len(haves) > 0 {
		var input strings.Builder
		for _, have := range haves {
			if len(have) == len(commit) {
				input.WriteString(hex.EncodeToString(have) + "\n")
			}
		}
		command := git.Command(ctx, e.root, "cat-file", "--batch-check=%(objectname) %(objecttype)")
		command.Stdin = strings.NewReader(input.String())
		output := git.NewOutputRecorder()
		command.Stderr = output
		result, err := command.Output()
		if err != nil {
			return failure("Git couldn't examine the repository that has it: %v", &git.CommandError{Err: err, Output: output.Message()}), nil
		}
		for _, line := range strings.Split(string(result), "\n") {
			if have, ok := strings.CutSuffix(line, " commit"); ok {
				revisions.WriteString("^" + have + "\n")
				thin = true
			}
		}
	}

	// Start producing the pack. The receiver's commits are left out, along
	// with everything that they need, and the pack is thin, i.e. it can
	// include changes relative to their objects. Replacements are ignored,
	// since they're local to this repository.
	arguments := []string{"--no-replace-objects", "pack-objects", "--revs", "--stdout", "--delta-base-offset", "-q"}
	if thin {
		arguments = append(arguments, "--thin")
	}
	packCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := git.Command(packCtx, e.root, arguments...)
	command.Stdin = strings.NewReader(revisions.String())
	output := git.NewOutputRecorder()
	command.Stderr = output
	pack, err := command.StdoutPipe()
	if err != nil {
		return failure("%v", err), nil
	} else if err := command.Start(); err != nil {
		return failure("%v", &git.CommandError{Err: err}), nil
	}

	// Pass the pack to the receiver in parts. If the receiver fails, then stop
	// the command.
	buffer := make([]byte, repositoryChunkSize)
	for {
		count, readErr := io.ReadFull(pack, buffer)
		if count > 0 {
			if err := receiver.Receive(buffer[:count]); err != nil {
				cancel()
				command.Wait()
				return "", err
			}
		}
		if readErr != nil {
			break
		}
	}

	// Wait for the command. A failure here can mean that the repository is a
	// partial clone that lacks some of the objects, which Git doesn't fetch
	// (see git.Command).
	if err := command.Wait(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return failure("Git couldn't pack it from the repository that has it: %v", &git.CommandError{Err: err, Output: output.Message()}), nil
	}
	return "", nil
}

// TransitionRepository implements
// synchronization.Endpoint.TransitionRepository.
func (e *endpoint) TransitionRepository(ctx context.Context, action *core.RepositoryAction) (*core.RepositoryResult, error) {
	// Verify that the action can be applied.
	if err := e.checkRepositoryAction(action); err != nil {
		return nil, err
	}

	// Stop any receiver that wasn't finished.
	e.abandonRepositoryReceiver()

	// Apply the action.
	var result *core.RepositoryResult
	if action.Old == nil {
		result = e.attachRootRepository(ctx, action.Target)
	} else {
		result = e.followRootRepository(ctx, action)
	}

	// Remove any repository that was prepared for attachment, whether or not
	// it's been used, and forget about the staging.
	os.RemoveAll(e.temporaryRepositoryPath())
	e.repositoryProblem = ""

	// Done.
	return result, nil
}
