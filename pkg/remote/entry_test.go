package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
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
