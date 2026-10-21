package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cancellingEntryReader cancels a comparison after a specified amount of data
// has been read, allowing cancellation during hashing to be tested without
// depending on filesystem speed or allocating large files.
type cancellingEntryReader struct {
	reader    io.Reader
	remaining int
	cancel    context.CancelFunc
}

func (r *cancellingEntryReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, errors.New("continued reading after cancellation")
	}
	n, err := r.reader.Read(buffer[:min(len(buffer), r.remaining)])
	r.remaining -= n
	if r.remaining == 0 {
		r.cancel()
	}
	return n, err
}

func TestReadFileEntryCancellation(t *testing.T) {
	data := bytes.Repeat([]byte("x"), MaximumComparedFileSize+4096)
	for _, test := range []struct {
		name      string
		size      int64
		readUntil int
	}{
		{"small", 4096, 1024},
		{"large", int64(len(data)), 1024},
		{"growing", 4096, MaximumComparedFileSize + 2048},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := &cancellingEntryReader{bytes.NewReader(data), test.readUntil, cancel}
			if entry, err := readFileEntry(ctx, reader, test.size, false); !errors.Is(err, context.Canceled) || entry != nil {
				t.Fatalf("cancelled comparison returned an entry: %+v, %v", entry, err)
			}
		})
	}
}

func TestReadEntryCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadEntry(ctx, filepath.Join(t.TempDir(), "missing")); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled comparison accessed the filesystem:", err)
	}
}

func TestSymbolicLinkTargetsSlashSeparated(t *testing.T) {
	// Windows reads symbolic link targets with backslashes, which are
	// represented with slashes, as synchronization represents them, so that
	// links taken from a Windows endpoint work on POSIX endpoints. Links are
	// created from that representation, which Go converts back.
	if runtime.GOOS != "windows" {
		t.Skip()
	}
	ctx := context.Background()
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink("dir/target", link); err != nil {
		t.Skip("symbolic links unavailable:", err)
	} else if target, err := os.Readlink(link); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("target read as %q", target)
	}
	if entry, err := ReadEntry(ctx, link); err != nil || entry.Kind != EntrySymbolicLink || entry.Target != "dir/target" {
		t.Fatalf("link read as %+v: %v", entry, err)
	} else if entry, err := Snapshot(ctx, root, "link"); err != nil || entry.Kind != EntrySymbolicLink || entry.Target != "dir/target" {
		t.Fatalf("link snapshotted as %+v: %v", entry, err)
	}
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	replacement := &Replacement{Path: "replaced", Expected: []*Entry{{Kind: EntryMissing}}, Result: &Entry{Kind: EntrySymbolicLink, Target: "dir/target"}, Sequence: 1}
	if err := ReplaceFile(ctx, root, replacement, strings.NewReader("")); err != nil {
		t.Fatal(err)
	} else if entry, err := Snapshot(ctx, root, "replaced"); err != nil || !SameContents(entry, replacement.Result) {
		t.Fatalf("link written as %+v: %v", entry, err)
	}
}

func TestSymbolicLinkTargetsRawOnPOSIX(t *testing.T) {
	// Backslashes are filename characters on POSIX systems, so targets read
	// there are left as they are (and synchronization reports them as
	// problems).
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	root := t.TempDir()
	if err := os.Symlink(`dir\target`, filepath.Join(root, "link")); err != nil {
		t.Skip("symbolic links unavailable:", err)
	} else if entry, err := Snapshot(context.Background(), root, "link"); err != nil || entry.Target != `dir\target` {
		t.Fatalf("link snapshotted as %+v: %v", entry, err)
	}
}
