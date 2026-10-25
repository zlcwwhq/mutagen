package remote

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
)

// Snapshots describe an entry of any kind, and archives transfer them in tar
// format. An archive's first entry is the snapshotted entry itself, named by
// archiveTop, followed by a directory's contents in depth-first order with
// names sorted, named by their slash-separated paths relative to it.
// Mutagen's temporary files are excluded. Contents other than files,
// directories, and symbolic links (such as named pipes and sockets) have no
// data and are archived as named pipes, which aren't extracted.
const archiveTop = "."

// treeHasher computes a directory's digest from the entries in its archive, in
// archive order, so that a snapshot identifies a directory's complete
// contents. Executability is included, since directory digests are only
// compared between snapshots taken on the same endpoint, or as transferred.
type treeHasher struct {
	hash.Hash
}

// add adds an entry with the specified archive name.
func (h treeHasher) add(name string, entry *Entry) {
	fmt.Fprintf(h, "%s\x00%s\x00", entry.Kind, name)
	switch entry.Kind {
	case EntryFile:
		fmt.Fprintf(h, "%d\x00%t\x00%s\x00", entry.Size, entry.Executable, entry.Digest)
	case EntrySymbolicLink:
		fmt.Fprintf(h, "%s\x00", entry.Target)
	}
}

// digest returns the hexadecimal digest of the entries added.
func (h treeHasher) digest() string {
	return fmt.Sprintf("%x", h.Sum(nil))
}

// snapshot reads the entry at a path, without following a final symbolic
// link, and writes it to an archive if one is specified. A directory's digest
// covers its complete contents. A missing entry yields a missing entry (and
// no archive entries). Files must not change while being read.
func snapshot(ctx context.Context, root *os.Root, path string, archive *tar.Writer) (*Entry, error) {
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Entry{Kind: EntryMissing}, nil
	} else if err != nil {
		return nil, err
	} else if !info.IsDir() {
		return snapshotLeaf(ctx, root, path, archiveTop, info, archive)
	}
	tree := treeHasher{sha256.New()}
	if err := snapshotDirectory(ctx, root, path, archiveTop, tree, archive); err != nil {
		return nil, err
	}
	return &Entry{Kind: EntryDirectory, Digest: tree.digest()}, nil
}

// snapshotLeaf reads an entry other than a directory, described by information
// read at its path, and writes it to an archive with the specified name if one
// is specified.
func snapshotLeaf(ctx context.Context, root *os.Root, path, name string, info os.FileInfo, archive *tar.Writer) (*Entry, error) {
	header := &tar.Header{Name: name, Mode: int64(info.Mode().Perm())}
	var entry *Entry
	switch mode := info.Mode(); {
	case mode.IsRegular():
		var output io.Writer = io.Discard
		if archive != nil {
			header.Typeflag, header.Size = tar.TypeReg, info.Size()
			// Keep the default format, which rounds modification times to
			// seconds. PAX subsecond times add 1 KiB per file, doubling the
			// archive size for small files.
			header.ModTime = info.ModTime()
			if err := archive.WriteHeader(header); err != nil {
				return nil, err
			}
			output = archive
		}
		entry, _, err := copyFile(ctx, root, path, output)
		if err == nil && entry.Kind != EntryFile {
			err = errors.New("file changed while being read")
		}
		return entry, err
	case mode&fs.ModeSymlink != 0:
		target, err := root.Readlink(path)
		if err != nil {
			return nil, err
		}
		target = normalizeTarget(target)
		header.Typeflag, header.Linkname = tar.TypeSymlink, target
		entry = &Entry{Kind: EntrySymbolicLink, Target: target}
	default:
		header.Typeflag = tar.TypeFifo
		entry = &Entry{Kind: EntryOther}
	}
	if archive != nil {
		if err := archive.WriteHeader(header); err != nil {
			return nil, err
		}
	}
	return entry, nil
}

// snapshotDirectory adds a directory, named in archives as specified, and its
// contents to a tree digest and writes them to an archive if one is specified.
func snapshotDirectory(ctx context.Context, root *os.Root, directory, name string, tree treeHasher, archive *tar.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tree.add(name, &Entry{Kind: EntryDirectory})
	if archive != nil {
		if err := archive.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0700}); err != nil {
			return err
		}
	}
	file, err := root.Open(directory)
	if err != nil {
		return err
	}
	contents, err := file.ReadDir(-1)
	file.Close()
	if err != nil {
		return err
	}
	slices.SortFunc(contents, func(a, b fs.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	for _, content := range contents {
		if strings.HasPrefix(content.Name(), filesystem.TemporaryNamePrefix) {
			continue
		}
		childPath, childName := filepath.Join(directory, content.Name()), path.Join(name, content.Name())
		info, err := root.Lstat(childPath)
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("directory changed while being read")
		} else if err != nil {
			return err
		} else if info.IsDir() {
			if err := snapshotDirectory(ctx, root, childPath, childName, tree, archive); err != nil {
				return err
			}
			continue
		}
		entry, err := snapshotLeaf(ctx, root, childPath, childName, info, archive)
		if err != nil {
			return fmt.Errorf("unable to read %s: %w", childName, err)
		}
		tree.add(childName, entry)
	}
	return nil
}

// Snapshot reads the entry at a slash-separated path relative to a root (see
// openFile), including a directory's complete contents in its digest.
func Snapshot(ctx context.Context, directory, path string) (*Entry, error) {
	root, path, _, err := openFile(directory, path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return snapshot(ctx, root, path, nil)
}

// ArchiveEntry writes an archive of the entry at a slash-separated path
// relative to a root (see openFile) to output and returns its snapshot. Regular
// file modification times are preserved to the nearest second and don't
// participate in snapshots or content comparisons.
func ArchiveEntry(ctx context.Context, directory, path string, output io.Writer) (*Entry, error) {
	root, path, _, err := openFile(directory, path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	archive := tar.NewWriter(output)
	entry, err := snapshot(ctx, root, path, archive)
	if err != nil {
		return nil, err
	}
	return entry, archive.Close()
}

// errInvalidArchive indicates that an archive doesn't have the expected form.
var errInvalidArchive = ConnectionError(errors.New("invalid archive"))

// ExtractArchive extracts an archive (see ArchiveEntry) to a destination path,
// at which nothing may exist, and returns the snapshot that it describes, which
// is computed as received rather than read back. Extraction is confined to the
// destination. Extracted files are private, with their archived modification
// times, to the precision supported by the destination filesystem.
func ExtractArchive(ctx context.Context, input io.Reader, destination string) (*Entry, error) {
	return extractArchive(ctx, input, destination, true)
}

// ExtractSharedArchive extracts an archive as ExtractArchive does, but with the
// permissions that programs typically request, which the umask restricts, as
// for content that the user creates, rather than privately.
func ExtractSharedArchive(ctx context.Context, input io.Reader, destination string) (*Entry, error) {
	return extractArchive(ctx, input, destination, false)
}

// extractArchive implements ExtractArchive and ExtractSharedArchive.
func extractArchive(ctx context.Context, input io.Reader, destination string, private bool) (*Entry, error) {
	reader := tar.NewReader(&entryReader{ctx: ctx, reader: input})
	header, err := reader.Next()
	if err == io.EOF {
		return &Entry{Kind: EntryMissing}, nil
	} else if err != nil {
		return nil, ConnectionError(err)
	} else if header.Name != archiveTop {
		return nil, errInvalidArchive
	}
	var entry *Entry
	if header.Typeflag == tar.TypeDir {
		entry, err = extractDirectory(reader, destination, private)
	} else if entry, err = extractLeaf(reader, header, nil, destination, private); err == nil {
		if _, err = reader.Next(); err == io.EOF {
			err = nil
		} else if err == nil {
			err = errInvalidArchive
		} else {
			err = ConnectionError(err)
		}
	}
	if err != nil {
		return nil, err
	}
	// Consume the archive's padding, so that the sender isn't blocked.
	_, err = io.Copy(io.Discard, input)
	return entry, ConnectionError(err)
}

// extractLeaf extracts an archive entry other than a directory to a native
// path relative to a root, or to an absolute path if root is nil, privately if
// requested.
func extractLeaf(reader *tar.Reader, header *tar.Header, root *os.Root, path string, private bool) (*Entry, error) {
	switch header.Typeflag {
	case tar.TypeReg:
		permissions := os.FileMode(0600)
		if !private {
			permissions = 0666
		}
		if header.Mode&0111 != 0 {
			permissions |= (permissions & 0666) >> 1
		}
		var file *os.File
		var err error
		if root != nil {
			file, err = root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, permissions)
		} else {
			file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, permissions)
		}
		if err != nil {
			return nil, err
		}
		hasher := sha256.New()
		size, err := io.Copy(io.MultiWriter(file, hasher), connectionReader{reader})
		// Restore the archived modification time before flushing and
		// publishing the newly created file. This path is confined to the
		// extraction directory and was created exclusively above, so no
		// existing symbolic link or hard link is modified.
		if err == nil {
			if root != nil {
				err = root.Chtimes(path, header.ModTime, header.ModTime)
			} else {
				err = os.Chtimes(path, header.ModTime, header.ModTime)
			}
			if err != nil {
				err = fmt.Errorf("unable to preserve modification time: %w", err)
			}
		}
		// Flush the file, so that an extracted copy, such as a checked-out
		// one, survives a power loss or an operating system crash once the
		// directory containing it is flushed too (see SyncDirectory).
		if err == nil {
			err = file.Sync()
		}
		if err = errors.Join(err, file.Close()); err != nil {
			return nil, err
		}
		return &Entry{Kind: EntryFile, Size: size, Digest: fmt.Sprintf("%x", hasher.Sum(nil)), Executable: header.Mode&0111 != 0}, nil
	case tar.TypeSymlink:
		// Targets are slash-separated (see normalizeTarget), and Go converts
		// them to backslashes when creating links on Windows.
		var err error
		if root != nil {
			err = root.Symlink(header.Linkname, path)
		} else {
			err = os.Symlink(header.Linkname, path)
		}
		if err != nil {
			return nil, err
		}
		return &Entry{Kind: EntrySymbolicLink, Target: header.Linkname}, nil
	case tar.TypeFifo:
		return &Entry{Kind: EntryOther}, nil
	}
	return nil, errInvalidArchive
}

// extractDirectory extracts a directory's contents from an archive, whose
// directory entry has been read, to a destination path, privately if
// requested.
func extractDirectory(reader *tar.Reader, destination string, private bool) (*Entry, error) {
	permissions := os.FileMode(0700)
	if !private {
		permissions = 0777
	}
	if err := os.Mkdir(destination, permissions); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	tree := treeHasher{sha256.New()}
	tree.add(archiveTop, &Entry{Kind: EntryDirectory})
	// Validate archive structure before performing filesystem operations, so
	// malformed entries are protocol failures while actual filesystem errors
	// remain operation failures. Parents must have been explicit directories;
	// symbolic links and duplicate entries can never redirect extraction.
	directories := map[string]bool{archiveTop: true}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return &Entry{Kind: EntryDirectory, Digest: tree.digest()}, nil
		} else if err != nil {
			return nil, ConnectionError(err)
		} else if header.Name == archiveTop || !validRelativePath(header.Name) {
			return nil, errInvalidArchive
		}
		if _, duplicate := directories[header.Name]; duplicate || !directories[path.Dir(header.Name)] {
			return nil, errInvalidArchive
		}
		directories[header.Name] = header.Typeflag == tar.TypeDir
		native := filepath.FromSlash(header.Name)
		entry := &Entry{Kind: EntryDirectory}
		if header.Typeflag == tar.TypeDir {
			err = root.Mkdir(native, permissions)
		} else {
			entry, err = extractLeaf(reader, header, root, native, private)
		}
		if err != nil {
			return nil, fmt.Errorf("unable to extract %s: %w", header.Name, err)
		}
		tree.add(header.Name, entry)
	}
}
