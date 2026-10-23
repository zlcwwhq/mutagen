package local

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
)

// TODO: Implement tests for the remaining endpoint behavior.

// scopeRecords lists the scope registration records (see remote.RegisterScope)
// in the specified Mutagen data directory.
func scopeRecords(t *testing.T, dataDirectory string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDirectory, "remote-scopes"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var result []string
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".") {
			result = append(result, name)
		}
	}
	return result
}

func TestEndpointScopeRegistration(t *testing.T) {
	// Use a short data directory, since registrations create sockets in it,
	// whose paths have tight length limits.
	dataDirectory, err := os.MkdirTemp("", "mtg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dataDirectory) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", dataDirectory)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "results"), 0700); err != nil {
		t.Fatal(err)
	}
	synchronizing := func(path string) []string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sessions, err := remote.Synchronizing(ctx, filepath.Join(root, filepath.FromSlash(path)), false)
		if err != nil {
			t.Fatal("unable to determine synchronizing sessions:", err)
		}
		return sessions
	}

	// Endpoints register their roots and effective ignores, so that remote
	// --pull knows the content that they synchronize, even though the session
	// doesn't run remote commands.
	configuration := &synchronization.Configuration{
		WatchMode: synchronization.WatchMode_WatchModeNoWatch,
		Ignores:   []string{"results"},
	}
	endpoint, err := NewEndpoint(nil, root, "sync_scope", synchronization.Version_Version1, configuration, true)
	if err != nil {
		t.Fatal("unable to create endpoint:", err)
	}
	if records := scopeRecords(t, dataDirectory); len(records) != 1 {
		t.Errorf("incorrect scope records after creation: %v", records)
	} else if sessions := synchronizing("data"); !slices.Equal(sessions, []string{"sync_scope"}) {
		t.Errorf("incorrect sessions synchronizing content in the root: %v", sessions)
	} else if sessions := synchronizing("results/data"); len(sessions) != 0 {
		t.Errorf("ignored content counted as synchronized: %v", sessions)
	}

	// Shutdown removes the registration.
	if err := endpoint.Shutdown(); err != nil {
		t.Fatal("unable to shut down endpoint:", err)
	} else if records := scopeRecords(t, dataDirectory); len(records) != 0 {
		t.Errorf("scope records remain after shutdown: %v", records)
	}
}
