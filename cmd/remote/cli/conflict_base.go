package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/hashing"
)

// baseCandidate is a candidate for the common ancestor of two conflicting
// copies.
type baseCandidate struct {
	// source describes where the candidate comes from.
	source string
	// write writes the candidate's contents to output.
	write func(output io.Writer) error
}

// gitCandidate returns a candidate read from Git by running git cat-file in a
// file's directory, which outputs a blob as stored, without running filters,
// hooks, or the file system monitor.
func gitCandidate(ctx context.Context, git, local, revision, source string) baseCandidate {
	return baseCandidate{source: source, write: func(output io.Writer) error {
		command := exec.CommandContext(ctx, git, "-c", "core.fsmonitor=false", "--no-optional-locks",
			"cat-file", "blob", revision+":./"+filepath.ToSlash(filepath.Base(local)))
		command.Dir = filepath.Dir(local)
		command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		command.Stdout = output
		return command.Run()
	}}
}

// listedAlgorithm returns the session's hashing algorithm, which computed the
// digests listed for a conflict between two files. It fails if this build
// doesn't support the algorithm, or if the listing doesn't include the copies'
// digests, since downloaded copies can then not be verified to be the listed
// ones, and the listed common ancestor might not be theirs.
func listedAlgorithm(conflict *remote.ConflictRoot) (hashing.Algorithm, error) {
	var algorithm hashing.Algorithm
	if conflict.AlphaDigest == "" || conflict.BetaDigest == "" {
		return algorithm, errors.New("the conflict listing doesn't identify the copies")
	} else if err := algorithm.UnmarshalText([]byte(conflict.Algorithm)); err != nil || algorithm.IsDefault() ||
		algorithm.SupportStatus() != hashing.AlgorithmSupportStatusSupported {
		return algorithm, fmt.Errorf("the session's hashing algorithm (%s) isn't supported", conflict.Algorithm)
	}
	return algorithm, nil
}

// findBase looks for the common ancestor of two conflicting files: the file
// that both endpoints had at the conflict root when they last agreed, which the
// conflict identifies by its digest. Mutagen doesn't retain its contents, so
// candidates are verified against the digest: an explicitly specified file, or
// otherwise the versions of the local file in Git's HEAD commit and index. It
// writes a verified candidate to a path and returns a description of its
// source, or an empty string if none was found. The copies must have been
// verified to be the listed ones (see listedAlgorithm), whose ancestor the
// digest identifies. Only an explicitly specified candidate that can't be
// verified is an error.
func findBase(ctx context.Context, conflict *remote.ConflictRoot, local, explicit, path string) (string, error) {
	if conflict.BaseDigest == "" {
		if explicit != "" {
			return "", errors.New("the copies have no common ancestor (the file didn't exist when the endpoints last agreed), so --base can't be used")
		}
		return "", nil
	}
	algorithm, err := listedAlgorithm(conflict)
	if err != nil {
		if explicit != "" {
			return "", fmt.Errorf("%w, so --base can't be verified", err)
		}
		return "", nil
	}
	var candidates []baseCandidate
	if explicit != "" {
		candidates = append(candidates, baseCandidate{source: "the specified file", write: func(output io.Writer) error {
			input, err := os.Open(explicit)
			if err != nil {
				return err
			}
			defer input.Close()
			_, err = io.Copy(output, input)
			return err
		}})
	} else if git, err := exec.LookPath("git"); err == nil {
		candidates = append(candidates,
			gitCandidate(ctx, git, local, "HEAD", "Git's HEAD commit"),
			gitCandidate(ctx, git, local, "", "Git's index"),
		)
	}
	for _, candidate := range candidates {
		hasher := algorithm.Factory()()
		err := writeLocalFile(path, func(file io.Writer) error {
			return candidate.write(io.MultiWriter(file, hasher))
		})
		if err == nil && fmt.Sprintf("%x", hasher.Sum(nil)) == conflict.BaseDigest {
			return candidate.source + ", verified as the copies' common ancestor", nil
		}
		os.Remove(path)
		if ctx.Err() != nil {
			return "", ctx.Err()
		} else if explicit != "" && err != nil {
			return "", fmt.Errorf("unable to read %s: %w", explicit, err)
		} else if explicit != "" {
			return "", fmt.Errorf("%s isn't the copies' common ancestor: its contents differ from the file that both endpoints had when they last agreed", explicit)
		}
	}
	return "", nil
}
