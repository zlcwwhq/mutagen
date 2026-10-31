package integration

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// newFakeSSHManager creates a session manager for a test with a fake remote
// system. Its log is recorded for failing tests (see logSessionOnFailure).
func newFakeSSHManager(t *testing.T) *synchronization.Manager {
	t.Helper()
	manager, err := synchronization.NewManager(logging.NewLogger(logging.LevelDebug, &synchronizationLog))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.Terminate(context.Background(), &selection.Selection{All: true}, "")
		manager.Shutdown()
	})
	return manager
}

// newFakeSSHTransport creates a transport for submodule scenarios that reaches
// beta using a fake remote system, so that Git runs under agents, in the
// environment that agents get. The agent is only installed once for all the
// sessions that use the transport, because they share a remote home directory.
func newFakeSSHTransport(t *testing.T, system *fakeSSHSystem, manager *synchronization.Manager) gitSubmoduleTransport {
	t.Helper()
	var sessions int
	return gitSubmoduleTransport{
		commands: true,
		configure: func(options *gitModeOptions) {
			options.manager = manager
			options.commands = synchronization.RemoteCommandMode_RemoteCommandModeBeta
			options.betaEndpoint = func() (*url.URL, string) {
				t.Helper()
				sessions++
				name := fmt.Sprintf("root%d", sessions)
				endpoint, err := url.Parse("server:"+name, url.Kind_Synchronization, false)
				if err != nil {
					t.Fatal("unable to parse URL:", err)
				}
				return endpoint, filepath.Join(system.home, name)
			}
		},
	}
}

// TestGitModeSubmodulesFakeSSH runs the scenarios for submodules with beta
// reached through the SSH transport, using fake ssh and scp commands, so that
// an agent is installed and that it's the one performing the operations for
// that endpoint. It requires the agent bundle of a local build (see
// BUILDING.md).
func TestGitModeSubmodulesFakeSSH(t *testing.T) {
	isolateGitMode(t)
	system := newFakeSSHSystem(t)
	manager := newFakeSSHManager(t)
	transport := newFakeSSHTransport(t, system, manager)
	for _, scenario := range gitSubmoduleScenarios {
		t.Run(scenario.name, func(t *testing.T) {
			scenario.run(t, transport)
		})
	}
}

// TestGitModeSubmoduleNoHotLoopFakeSSH tests that failures don't cause hot
// loops on an endpoint that's reached through the SSH transport.
func TestGitModeSubmoduleNoHotLoopFakeSSH(t *testing.T) {
	isolateGitMode(t)
	system := newFakeSSHSystem(t)
	manager := newFakeSSHManager(t)
	runGitSubmoduleNoHotLoop(t, newFakeSSHTransport(t, system, manager))
}

// TestGitModeSubmoduleStarFakeSSH tests submodules in a star whose nodes are
// reached through the SSH transport, sharing a fake remote system, which
// requires that their clones and temporary directories don't interfere with
// each other.
func TestGitModeSubmoduleStarFakeSSH(t *testing.T) {
	isolateGitMode(t)
	system := newFakeSSHSystem(t)
	manager := newFakeSSHManager(t)
	testGitSubmoduleStar(t, gitSubmoduleStarOptions{
		manager:   manager,
		base:      system.base,
		endpoints: map[string]string{"server": "server:server", "gpu": "gpu:gpu"},
		roots: map[string]string{
			"server": filepath.Join(system.home, "server"),
			"gpu":    filepath.Join(system.home, "gpu"),
		},
	})
}

// gitModeFilesContain indicates whether any file beneath a directory contains
// the specified text.
func gitModeFilesContain(t *testing.T, directory, text string) bool {
	t.Helper()
	var found bool
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || found || !entry.Type().IsRegular() {
			return err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found = bytes.Contains(contents, []byte(text))
		return nil
	})
	if err != nil {
		t.Fatal("unable to search directory:", err)
	}
	return found
}

// TestGitModeCredentialsFakeSSH tests that the credentials in the origin of a
// submodule stay in that repository's configuration: the other endpoint gets a
// URL without them, and neither what's reported, nor what's logged, nor what's
// stored by the daemon or the agent contains them, while a URL without
// credentials is what's found in all of those places.
func TestGitModeCredentialsFakeSSH(t *testing.T) {
	isolateGitMode(t)
	t.Setenv("GIT_ALLOW_PROTOCOL", "https:file")
	system := newFakeSSHSystem(t)
	manager := newFakeSSHManager(t)
	transport := newFakeSSHTransport(t, system, manager)
	const secret = "secret-token"
	const sanitized = "https://127.0.0.1:1/repository.git"

	// Create a session with two submodules whose origins have credentials and
	// can't be reached. One of them has to be cloned, which fails. The other is
	// already on the other endpoint, so that it goes in the archive.
	options := gitModeOptions{alpha: map[string]string{"cloned/file": "file", "shared/file": "file"}}
	options.prepare = func(session *gitModeSession) {
		for _, name := range []string{"cloned", "shared"} {
			directory := filepath.Join(session.alpha, name)
			gitModeRun(t, directory, "init", "--quiet")
			gitModeRun(t, directory, "add", "--", "file")
			gitModeRun(t, directory, "commit", "--quiet", "--no-gpg-sign", "-m", "commit")
		}
		gitModeRun(t, session.beta, "clone", "--quiet", filepath.Join(session.alpha, "shared"), "shared")
		gitModeRun(t, filepath.Join(session.beta, "shared"), "config", "remote.origin.url", "https://user:other-"+secret+"@127.0.0.1:1/repository.git")
		for _, name := range []string{"cloned", "shared"} {
			gitModeRun(t, filepath.Join(session.alpha, name), "config", "remote.origin.url", "https://user:"+secret+"@127.0.0.1:1/repository.git")
		}
	}
	transport.configure(&options)
	session := newGitModeSession(t, options)
	state := session.flushAllowingProblems()

	// Verify what's reported, which comes from the other system.
	problem := gitModeProblem(t, state.BetaState.TransitionProblems, "cloned", sanitized)
	if strings.Contains(problem.Error, secret) || strings.Contains(problem.Error, "user") {
		t.Errorf("problem contains credentials: %s", problem.Error)
	}

	// Nothing is left of the attempt, and the original is unchanged.
	session.expectMissing(session.beta, "cloned")
	session.expect(session.beta, map[string]string{"shared/file": "file"})
	session.expect(session.alpha, map[string]string{"cloned/file": "file"})
	origin := gitModeRun(t, filepath.Join(session.alpha, "cloned"), "config", "--get", "remote.origin.url")
	if !strings.Contains(origin, secret) {
		t.Errorf("origin was changed: %s", origin)
	}

	// The search below has to be able to find what it's looking for, or else
	// the result doesn't mean anything: the sanitized URL is in the archive.
	data := os.Getenv("MUTAGEN_DATA_DIRECTORY")
	if !gitModeFilesContain(t, data, sanitized) {
		t.Error("archive doesn't contain sanitized URL")
	}

	// Verify that the credentials didn't reach any of the daemon's or the
	// agent's storage or the logs. The beta repository's configuration is where
	// they are meant to be, so it has its own value, which it gets to keep.
	if gitModeFilesContain(t, data, secret) {
		t.Error("daemon's data directory contains credentials")
	}
	if gitModeFilesContain(t, filepath.Join(system.home, ".mutagen-dev"), secret) {
		t.Error("agent's data directory contains credentials")
	}
	if strings.Contains(synchronizationLog.String(), secret) {
		t.Error("log contains credentials")
	}
}
