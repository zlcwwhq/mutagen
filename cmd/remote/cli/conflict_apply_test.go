package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestApplySuperseded(t *testing.T) {
	// A replacement refused because a later apply of the conflict reached the
	// copy first is reported as such, rather than as a copy that changed since
	// checkout. A result recorded for the attempt is forgotten if the remote
	// copy's replacement was refused, since neither copy was written, but kept
	// if only the local copy's was.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	roots := [2]string{t.TempDir(), t.TempDir()}
	read := func(side int) string {
		data, _ := os.ReadFile(filepath.Join(roots[side], "file"))
		return string(data)
	}
	for side, contents := range []string{"local\n", "remote\n"} {
		if err := os.WriteFile(filepath.Join(roots[side], "file"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w := newTestWorkspace(t, "local\n", "remote\n")
	a := &application{w: w, result: w.Local, target: &remote.Target{Root: roots[0]}}
	write := func(sequence uint64) error {
		client, server := net.Pipe()
		go remote.Serve(ctx, server, roots[1], "", false)
		return a.write(ctx, client, w.file(localName), sequence)
	}
	applied := func() int {
		saved, err := readWorkspace(w.directory)
		if err != nil {
			t.Fatal(err)
		} else if len(saved.Applied) != len(w.Applied) {
			t.Fatal("applied results not saved")
		}
		return len(w.Applied)
	}

	// A later apply's replacement that finds a copy already as it is records
	// its sequence number without changing the copy.
	admit := func(side int, sequence uint64) {
		t.Helper()
		contents := read(side)
		entry := &remote.Entry{Kind: remote.EntryFile, Size: int64(len(contents)), Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(contents)))}
		replacement := &remote.Replacement{Path: "file", Expected: []*remote.Entry{entry}, Result: entry, Sequence: sequence}
		if err := remote.ReplaceFile(ctx, roots[side], replacement, strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
	}
	superseded := func(err error, prefix string) bool {
		return errors.Is(err, remote.ErrSuperseded) && !errors.Is(err, remote.ErrStale) &&
			strings.HasPrefix(err.Error(), prefix) && strings.Contains(err.Error(), "list conflicts") &&
			!strings.Contains(err.Error(), "check out again")
	}
	admit(1, 10)
	if err := write(5); !superseded(err, "unable to update the remote copy") {
		t.Fatal("refused replacement of the remote copy not reported:", err)
	} else if applied() != 0 {
		t.Fatal("result of an attempt that wrote neither copy not forgotten")
	} else if read(0) != "local\n" || read(1) != "remote\n" {
		t.Fatal("copies changed by refused replacements")
	}
	admit(0, 20)
	if err := write(15); !superseded(err, "updated the remote copy, but unable to update the local copy") {
		t.Fatal("refused replacement of the local copy not reported:", err)
	} else if applied() != 1 {
		t.Fatal("result written to the remote copy forgotten")
	} else if read(1) != "local\n" {
		t.Fatal("result not written to the remote copy")
	}
}
