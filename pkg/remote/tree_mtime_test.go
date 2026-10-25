package remote

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveModificationTimes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"files/early": "early", "files/sub/late": "late"})
	original, err := Snapshot(ctx, root, "files")
	if err != nil {
		t.Fatal(err)
	}
	// Read back the times set on the source so expectations account for the
	// precision that its filesystem actually supports.
	expected := make(map[string]time.Time)
	for path, mtime := range map[string]time.Time{
		"early":    time.Unix(1700000000, 123456789),
		"sub/late": time.Unix(1700000000, 876543210),
	} {
		source := filepath.Join(root, "files", filepath.FromSlash(path))
		if err := os.Chtimes(source, mtime, mtime); err != nil {
			t.Fatal(err)
		} else if info, err := os.Stat(source); err != nil {
			t.Fatal(err)
		} else {
			expected[path] = info.ModTime().Truncate(time.Second)
		}
	}
	if changed, err := Snapshot(ctx, root, "files"); err != nil || !SameContents(original, changed) {
		t.Fatalf("modification time changed a snapshot: %v", err)
	}

	// Both conflict workspace and pull extraction preserve times, for a
	// single file and for regular files at every depth of a directory.
	for name, extract := range map[string]func(context.Context, io.Reader, string) (*Entry, error){
		"private": ExtractArchive, "shared": ExtractSharedArchive,
	} {
		for _, path := range []string{"files/early", "files"} {
			t.Run(name+"/"+path, func(t *testing.T) {
				var archive bytes.Buffer
				archived, err := ArchiveEntry(ctx, root, path, &archive)
				if err != nil {
					t.Fatal(err)
				}
				directory := t.TempDir()
				destination := filepath.Join(directory, "copy")
				if extracted, err := extract(ctx, &archive, destination); err != nil || !SameContents(archived, extracted) {
					t.Fatalf("archive changed contents: %v", err)
				}
				for relative, mtime := range expected {
					file := filepath.Join(destination, filepath.FromSlash(relative))
					if path != "files" {
						if relative != "early" {
							continue
						}
						file = destination
					}
					// Probe the destination too, instead of requiring nanosecond
					// support or assuming its precision matches the source.
					probe := filepath.Join(directory, "probe")
					if err := os.WriteFile(probe, nil, 0600); err != nil {
						t.Fatal(err)
					} else if err := os.Chtimes(probe, mtime, mtime); err != nil {
						t.Fatal(err)
					}
					probeInfo, err := os.Stat(probe)
					if err != nil {
						t.Fatal(err)
					} else if info, err := os.Stat(file); err != nil {
						t.Fatal(err)
					} else if !info.ModTime().Equal(probeInfo.ModTime()) {
						t.Errorf("%s: modification time %v, expected %v", relative, info.ModTime(), probeInfo.ModTime())
					}
				}
			})
		}
	}
}

// extractionFailureReader closes an extraction root after its file is created,
// when its contents are read, to make setting its modification time fail.
type extractionFailureReader struct {
	io.Reader
	beforeRead func()
}

func (r *extractionFailureReader) Read(data []byte) (int, error) {
	if r.beforeRead != nil {
		r.beforeRead()
		r.beforeRead = nil
	}
	return r.Reader.Read(data)
}

func TestExtractModificationTimeFailure(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "file", Size: 1, ModTime: time.Unix(1700000000, 0)}); err != nil {
		t.Fatal(err)
	} else if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	} else if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	input := &extractionFailureReader{Reader: &archive}
	reader := tar.NewReader(input)
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	input.beforeRead = func() {
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if entry, err := extractLeaf(reader, header, root, "file", true); entry != nil ||
		!errors.Is(err, os.ErrClosed) || ClassifyError(err) != ErrorKindOperation || !strings.Contains(err.Error(), "unable to preserve modification time") {
		t.Fatalf("modification time failure not reported: %+v, %v", entry, err)
	}
}
