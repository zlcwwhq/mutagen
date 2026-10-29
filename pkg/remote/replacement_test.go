package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pauseReplacement pauses the first replacement that reaches a stage (see
// ReplaceHook) until the returned function, which may be called more than
// once, resumes it. The returned channel is closed once it's paused.
func pauseReplacement(t *testing.T, stage string) (<-chan struct{}, func()) {
	t.Helper()
	var first atomic.Bool
	paused, resumed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(resumed) }) }
	ReplaceHook = func(reached, _, _ string) {
		if reached == stage && first.CompareAndSwap(false, true) {
			close(paused)
			<-resumed
		}
	}
	t.Cleanup(func() {
		resume()
		ReplaceHook = nil
	})
	return paused, resume
}

// replaceContents performs a replacement of an entry whose expected entries
// are files with the specified contents, with a file with the specified
// contents.
func replaceContents(ctx context.Context, root, path, result string, sequence uint64, expected ...string) error {
	replacement := &Replacement{Path: path, Result: contentsEntry(result), Sequence: sequence}
	for _, contents := range expected {
		replacement.Expected = append(replacement.Expected, contentsEntry(contents))
	}
	return ReplaceFile(ctx, root, replacement, strings.NewReader(result))
}

func TestReplaceOrdering(t *testing.T) {
	// An apply whose client stops waiting, such as on timeout, may leave its
	// replacement running, since the opposite endpoint only notices later, and
	// a retry may take a checked-out copy, which the earlier replacement also
	// accepts. Replacements are thus ordered by their sequence numbers, so the
	// earlier one fails once the retry's has been admitted, rather than
	// overwrite its result.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("remote\n"), 0600); err != nil {
		t.Fatal(err)
	}
	paused, resume := pauseReplacement(t, "locking")
	stale := make(chan error, 1)
	go func() {
		stale <- replaceContents(ctx, root, "file", "merged\n", 1, "local\n", "remote\n")
	}()
	<-paused
	if err := replaceContents(ctx, root, "file", "local\n", 2, "local\n", "remote\n", "merged\n"); err != nil {
		t.Fatal(err)
	}
	resume()
	if err := <-stale; !errors.Is(err, ErrSuperseded) || errors.Is(err, ErrStale) {
		t.Fatal("earlier replacement not rejected as superseded:", err)
	} else if data, _ := os.ReadFile(path); string(data) != "local\n" {
		t.Fatalf("later replacement overwritten: %q", data)
	}
}

func TestReplaceOrderingAcrossRoots(t *testing.T) {
	// Nested sessions' roots may both contain an entry, if the outer session
	// doesn't ignore the inner root, and a file may be a root itself, so
	// replacements of an entry are ordered together whichever root they reach
	// it through: an earlier replacement through the outer root fails once a
	// later one through the inner root was admitted, rather than overwrite its
	// result.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	ctx := context.Background()
	for _, test := range []struct {
		name string
		// later is the root and path through which the later replacement
		// reaches the entry, given the outer root.
		later func(outer string) (string, string)
	}{
		{"directory root", func(outer string) (string, string) {
			return filepath.Join(outer, "sub"), "file"
		}},
		{"file root", func(outer string) (string, string) {
			return filepath.Join(outer, "sub", "file"), "."
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			outer := t.TempDir()
			path := filepath.Join(outer, "sub", "file")
			if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			} else if err := os.WriteFile(path, []byte("remote\n"), 0600); err != nil {
				t.Fatal(err)
			}
			paused, resume := pauseReplacement(t, "locking")
			stale := make(chan error, 1)
			go func() {
				stale <- replaceContents(ctx, outer, "sub/file", "merged\n", 1, "local\n", "remote\n")
			}()
			<-paused
			root, relative := test.later(outer)
			if err := replaceContents(ctx, root, relative, "local\n", 2, "local\n", "remote\n", "merged\n"); err != nil {
				t.Fatal(err)
			}
			resume()
			if err := <-stale; !errors.Is(err, ErrSuperseded) || errors.Is(err, ErrStale) {
				t.Fatal("earlier replacement through the outer root not rejected as superseded:", err)
			} else if data, _ := os.ReadFile(path); string(data) != "local\n" {
				t.Fatalf("later replacement through the inner root overwritten: %q", data)
			}
		})
	}
}

func TestReplacementLockTargets(t *testing.T) {
	// Replacements are locked and ordered by the entries that they reach,
	// rather than by the roots and paths through which they reach them, since
	// nested sessions' roots may both contain an entry, a file may be a root
	// itself, and roots may be reached through symbolic links. Links are
	// resolved like opening the roots (or their parents) resolves them, except
	// for the entry itself, which replacements replace rather than follow,
	// while different entries at the same path relative to different roots are
	// locked separately.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	ctx := context.Background()
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "sub")
	other := filepath.Join(base, "other")
	alias := filepath.Join(base, "alias")
	for _, directory := range []string{inner, other} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{outer, inner, other} {
		if err := os.WriteFile(filepath.Join(directory, "file"), []byte("remote\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	links := os.Symlink(outer, alias)
	if links == nil {
		links = os.Symlink("file", filepath.Join(inner, "link"))
	}
	type location struct {
		root, path string
	}
	for _, test := range []struct {
		name          string
		first, second location
		shared, links bool
	}{
		{"nested root", location{outer, "sub/file"}, location{inner, "file"}, true, false},
		{"file root", location{filepath.Join(inner, "file"), "."}, location{outer, "sub/file"}, true, false},
		{"nested root itself", location{inner, "."}, location{outer, "sub"}, true, false},
		{"same path in the outer root", location{outer, "file"}, location{inner, "file"}, false, false},
		{"same path in another root", location{other, "file"}, location{inner, "file"}, false, false},
		{"linked root", location{filepath.Join(alias, "sub"), "file"}, location{inner, "file"}, true, true},
		{"linked outer root", location{alias, "sub/file"}, location{filepath.Join(inner, "file"), "."}, true, true},
		{"linked file root", location{filepath.Join(alias, "sub", "file"), "."}, location{outer, "sub/file"}, true, true},
		{"link root", location{filepath.Join(inner, "link"), "."}, location{outer, "sub/link"}, true, true},
		{"link root and its target", location{filepath.Join(inner, "link"), "."}, location{inner, "file"}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.links && links != nil {
				t.Skip("symbolic links unavailable:", links)
			}
			first, err := lockReplacement(ctx, test.first.root, test.first.path)
			if err != nil {
				t.Fatal(err)
			}
			waiting, stop := context.WithTimeout(ctx, 100*time.Millisecond)
			second, err := lockReplacement(waiting, test.second.root, test.second.path)
			stop()
			if err == nil {
				defer second.unlock()
			}
			first.unlock()
			if test.shared {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("replacement of the entry through another root not excluded:", err)
				} else if second, err = lockReplacement(ctx, test.second.root, test.second.path); err != nil {
					t.Fatal(err)
				}
				defer second.unlock()
				if second.record != first.record {
					t.Fatalf("replacements of the entry recorded separately: %s, %s", first.record, second.record)
				}
			} else if err != nil {
				t.Fatal("replacement of another entry excluded:", err)
			} else if second.record == first.record {
				t.Fatal("replacements of different entries recorded together:", first.record)
			}
		})
	}
}

func TestReplaceSequences(t *testing.T) {
	// A replacement is refused, without changing its entry, once a replacement
	// with a later sequence number was admitted for the entry, even if the
	// entry is one of its expected entries, whether the later one replaced the
	// entry, found that it already had its result, or found it stale. Sequence
	// numbers are recorded by entry in the Mutagen data directory, along with
	// the root and path through which the latest admitted replacement reached
	// the entry.
	data := t.TempDir()
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	ctx := context.Background()
	roots := [2]string{t.TempDir(), t.TempDir()}
	write := func(root, name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(root, name string) string {
		data, _ := os.ReadFile(filepath.Join(root, name))
		return string(data)
	}
	replace := func(root, name, result string, sequence uint64) error {
		return replaceContents(ctx, root, name, result, sequence, "local\n", "remote\n")
	}
	write(roots[0], "file", "remote\n")
	if err := replace(roots[0], "file", "local\n", 5); err != nil {
		t.Fatal(err)
	} else if err := replace(roots[0], "file", "remote\n", 4); !errors.Is(err, ErrSuperseded) || errors.Is(err, ErrStale) {
		t.Fatal("earlier replacement not refused:", err)
	} else if read(roots[0], "file") != "local\n" {
		t.Fatal("refused replacement changed the entry")
	} else if err := replace(roots[0], "file", "local\n", 6); err != nil {
		t.Fatal("replacement with the result present failed:", err)
	} else if err := replace(roots[0], "file", "remote\n", 5); !errors.Is(err, ErrSuperseded) {
		t.Fatal("earlier replacement admitted after a later one found its result present:", err)
	}
	write(roots[0], "file", "edited\n")
	if err := replace(roots[0], "file", "local\n", 7); !errors.Is(err, ErrStale) {
		t.Fatal("stale entry replaced:", err)
	}
	write(roots[0], "file", "remote\n")
	if err := replace(roots[0], "file", "local\n", 6); !errors.Is(err, ErrSuperseded) {
		t.Fatal("earlier replacement admitted after a later one found the entry stale:", err)
	} else if read(roots[0], "file") != "remote\n" {
		t.Fatal("refused replacement changed the entry")
	}

	// Other entries, including those at the same path in other roots, are
	// ordered separately.
	write(roots[0], "other", "remote\n")
	write(roots[1], "file", "remote\n")
	if err := replace(roots[0], "other", "local\n", 1); err != nil {
		t.Fatal("replacement of another entry refused:", err)
	} else if err := replace(roots[1], "file", "local\n", 1); err != nil {
		t.Fatal("replacement of an entry in another root refused:", err)
	}

	// Each entry has a lock file and a record of its latest sequence number.
	locks, _ := filepath.Glob(filepath.Join(data, replacementsDirectoryName, "*.lock"))
	files, _ := filepath.Glob(filepath.Join(data, replacementsDirectoryName, "*.json"))
	var records []replacementRecord
	for _, file := range files {
		var record replacementRecord
		if data, err := os.ReadFile(file); err != nil {
			t.Fatal(err)
		} else if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	expected := []replacementRecord{{roots[0], "file", 7}, {roots[0], "other", 1}, {roots[1], "file", 1}}
	for _, record := range expected {
		if !slices.Contains(records, record) {
			t.Fatalf("sequence number not recorded: %+v in %+v", record, records)
		}
	}
	if len(locks) != len(expected) || len(records) != len(expected) {
		t.Fatalf("unexpected replacement state: %v, %+v", locks, records)
	}
}

func TestReplaceExclusion(t *testing.T) {
	// A replacement keeps its entry locked until it has finished writing, even
	// once its client stopped waiting for it, which the executor only notices
	// later, so that later replacements of the entry wait for it, until their
	// own clients stop waiting, and supersede it once it's done. A replacement
	// refused because a later one was admitted is reported as such.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("remote\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	replace := func(ctx context.Context, result string, sequence uint64) error {
		client, server := net.Pipe()
		go Serve(ctx, server, root, "", false, "")
		replacement := &Replacement{
			Path:     "file",
			Expected: []*Entry{contentsEntry("local\n"), contentsEntry("remote\n"), contentsEntry("merged\n")},
			Result:   contentsEntry(result), Sequence: sequence,
		}
		return Replace(ctx, client, replacement, strings.NewReader(result))
	}
	read := func() string {
		data, _ := os.ReadFile(path)
		return string(data)
	}
	paused, resume := pauseReplacement(t, "writing")
	abandoned, abandon := context.WithCancel(ctx)
	pending := make(chan error, 1)
	go func() {
		pending <- replace(abandoned, "merged\n", 1)
	}()
	<-paused
	abandon()
	if err := <-pending; !errors.Is(err, context.Canceled) {
		t.Fatal("abandoned replacement not cancelled:", err)
	}
	waiting, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	if err := replace(waiting, "local\n", 2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("replacement not excluded by a pending one:", err)
	} else if data := read(); data != "remote\n" {
		t.Fatalf("entry replaced while a replacement was pending: %q", data)
	}
	resume()
	if err := replace(ctx, "local\n", 3); err != nil {
		t.Fatal(err)
	} else if data := read(); data != "local\n" {
		t.Fatalf("later replacement not written: %q", data)
	} else if err := replace(ctx, "merged\n", 2); !errors.Is(err, ErrSuperseded) || errors.Is(err, ErrStale) ||
		!strings.Contains(err.Error(), "reached the copy first") {
		t.Fatal("superseded replacement not reported:", err)
	} else if data := read(); data != "local\n" {
		t.Fatalf("superseded replacement changed the entry: %q", data)
	}
}

func TestReplacementLockAcrossProcesses(t *testing.T) {
	// POSIX record locks don't exclude other locks held by the same process,
	// so a helper process holds the lock until its input closes.
	if root := os.Getenv("MUTAGEN_TEST_REPLACEMENT_LOCK"); root != "" {
		lock, err := lockReplacement(context.Background(), root, "file")
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("locked")
		io.Copy(io.Discard, os.Stdin)
		lock.unlock()
		os.Exit(0)
	}

	// Executors that a reconnection replaced may still be finishing a
	// replacement, and resolutions' clients replace local copies, so a
	// replacement in another process excludes others, which wait for it until
	// their contexts are done, without changing the entry.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("remote\n"), 0600); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestReplacementLockAcrossProcesses$")
	helper.Env = append(os.Environ(), "MUTAGEN_TEST_REPLACEMENT_LOCK="+root)
	input, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	} else if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("helper unable to lock: %q: %v", line, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := replaceContents(ctx, root, "file", "local\n", 1, "remote\n"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("replacement not excluded by another process:", err)
	} else if data, _ := os.ReadFile(path); string(data) != "remote\n" {
		t.Fatalf("entry replaced while another process held its lock: %q", data)
	}
	input.Close()
	if err := helper.Wait(); err != nil {
		t.Fatal("helper failed:", err)
	} else if err := replaceContents(context.Background(), root, "file", "local\n", 1, "remote\n"); err != nil {
		t.Fatal("replacement failed once the lock was released:", err)
	} else if data, _ := os.ReadFile(path); string(data) != "local\n" {
		t.Fatalf("result not written: %q", data)
	}
}

func TestWriteFileDurably(t *testing.T) {
	// Files are replaced with private files containing the data, without
	// leaving temporary files behind.
	directory := t.TempDir()
	path := filepath.Join(directory, "file")
	for _, data := range []string{"first", "second"} {
		if err := WriteFileDurably(path, []byte(data)); err != nil {
			t.Fatal(err)
		} else if written, err := os.ReadFile(path); err != nil || string(written) != data {
			t.Fatalf("file not written: %q, %v", written, err)
		} else if entries, err := os.ReadDir(directory); err != nil || len(entries) != 1 {
			t.Fatal("temporary files left behind:", entries, err)
		} else if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
			t.Fatal("file not private:", err)
		}
	}
	if err := WriteFileDurably(filepath.Join(directory, "missing", "file"), nil); err == nil {
		t.Fatal("file written in a missing directory")
	}
}
