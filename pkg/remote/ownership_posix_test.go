//go:build !windows

package remote

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestReplacePreservesOwnership(t *testing.T) {
	// Replacing a copy keeps its owner and group. Only root can give files to
	// other users, but others can use any group that they belong to.
	uid, gid := os.Geteuid(), -1
	if uid == 0 {
		uid, gid = 12345, 23456
	} else if groups, err := os.Getgroups(); err == nil {
		for _, group := range groups {
			if group != os.Getegid() {
				gid = group
				break
			}
		}
	}
	if gid < 0 {
		t.Skip("no other group to give the copy")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "file")
	if err := os.WriteFile(path, []byte("original\n"), 0640); err != nil {
		t.Fatal(err)
	} else if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	original := &Entry{Kind: EntryFile, Size: 9, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("original\n")))}
	merged := &Entry{Kind: EntryFile, Size: 7, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("merged\n")))}
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	replacement := &Replacement{Path: "file", Expected: []*Entry{original}, Result: merged, Sequence: 1}
	if err := ReplaceFile(context.Background(), directory, replacement, strings.NewReader("merged\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != uid || int(stat.Gid) != gid {
		t.Errorf("replaced copy owned by %d:%d instead of %d:%d", stat.Uid, stat.Gid, uid, gid)
	} else if info.Mode().Perm() != 0640 {
		t.Errorf("replaced copy has permissions %v instead of 0640", info.Mode().Perm())
	}
}

// ownedInfo is file information with another owner and group.
type ownedInfo struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (i *ownedInfo) Sys() any {
	return i.stat
}

func TestPreserveOwnershipFallbacks(t *testing.T) {
	// Ownership is reproduced as far as permitted, but failures other than a
	// lack of permission fail the replacement.
	defer func() { chownFile = (*os.File).Chown }()
	file, err := os.Create(filepath.Join(t.TempDir(), "staged"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	staged := *info.Sys().(*syscall.Stat_t)
	other := staged
	other.Uid, other.Gid = staged.Uid+1, staged.Gid+1
	sameGroup := staged
	sameGroup.Uid = staged.Uid + 1
	uid, gid, sameGid := int(other.Uid), int(other.Gid), int(sameGroup.Gid)
	type call struct{ uid, gid int }
	for _, test := range []struct {
		description string
		replaced    syscall.Stat_t
		errors      []error
		calls       []call
		fails       bool
	}{
		{"same ownership", staged, nil, nil, false},
		{"owner and group", other, []error{nil}, []call{{uid, gid}}, false},
		{"group only", other, []error{syscall.EPERM, nil}, []call{{uid, gid}, {-1, gid}}, false},
		{"neither", other, []error{syscall.EPERM, syscall.EPERM}, []call{{uid, gid}, {-1, gid}}, false},
		{"unmapped", other, []error{syscall.EINVAL, syscall.EINVAL}, []call{{uid, gid}, {-1, gid}}, false},
		{"group already kept", sameGroup, []error{syscall.EPERM}, []call{{uid, sameGid}}, false},
		{"failure", other, []error{syscall.EIO}, []call{{uid, gid}}, true},
		{"group failure", other, []error{syscall.EPERM, syscall.EIO}, []call{{uid, gid}, {-1, gid}}, true},
	} {
		var calls []call
		chownFile = func(_ *os.File, uid, gid int) error {
			calls = append(calls, call{uid, gid})
			if len(calls) > len(test.errors) {
				t.Fatalf("%s: unexpected change of ownership to %d:%d", test.description, uid, gid)
			}
			return test.errors[len(calls)-1]
		}
		err := preserveOwnership(file, &ownedInfo{info, &test.replaced})
		if (err != nil) != test.fails {
			t.Errorf("%s: unexpected result: %v", test.description, err)
		} else if !slices.Equal(calls, test.calls) {
			t.Errorf("%s: changed ownership as %v instead of %v", test.description, calls, test.calls)
		}
	}
}
