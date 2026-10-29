package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/integration/protocols/netpipe"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

func TestRemotePullOtherSession(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alphaRoot, betaRoot, copyRoot := filepath.Join(directory, "alpha"), filepath.Join(directory, "beta"), filepath.Join(directory, "copy")
	results := filepath.Join(alphaRoot, "results")
	if err := os.MkdirAll(results, 0700); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(results, "data"), []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// The session through which content is pulled ignores results, so its
	// copy of results/data exists only on beta.
	createRemoteSession(t, "pull-outer", alphaRoot, betaRoot, false, "results")
	if err := os.MkdirAll(filepath.Join(betaRoot, "results"), 0700); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(betaRoot, "results", "data"), []byte("remote\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Another session synchronizes results without running remote commands,
	// as sessions do by default.
	ctx := context.Background()
	inner, err := synchronizationManager.Create(ctx,
		&url.URL{Path: results},
		&url.URL{Protocol: netpipe.Protocol_Netpipe, Path: copyRoot},
		&synchronization.Configuration{WatchMode: synchronization.WatchMode_WatchModeNoWatch},
		&synchronization.Configuration{}, &synchronization.Configuration{},
		"pull-inner", nil, nil, false, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := synchronizationManager.Terminate(ctx, &selection.Selection{Specifications: []string{inner}}, ""); err != nil {
			t.Error("unable to terminate session:", err)
		}
	})
	logSessionOnFailure(t, inner)

	// Pulling content that the other session synchronizes is refused, which
	// leaves the local content unchanged. The session is labeled by
	// identifier, since its endpoints don't know its name.
	if code, _, errorOutput := runRemote(t, alphaRoot, "--pull", "results/data"); code != 125 ||
		errorOutput != "remote: unable to pull results/data: it's synchronized by "+inner+", so remote doesn't write it\n" {
		t.Errorf("content synchronized by another session pulled: %d, %q", code, errorOutput)
	}
	if contents, err := os.ReadFile(filepath.Join(results, "data")); err != nil || string(contents) != "local\n" {
		t.Errorf("local content changed: %q, %v", contents, err)
	}
}
