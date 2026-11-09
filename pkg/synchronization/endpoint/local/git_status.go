package local

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mutagen-io/mutagen/pkg/git"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// gitStatusTimeout is the maximum amount of time that the Git commands that
// examine the working tree of a single submodule can take (see
// submoduleStatus). Their results only improve reconciliation, so they
// mustn't be able to hold up a synchronization cycle indefinitely, but they
// compare the contents of files whose metadata has changed, which can take a
// while in a large repository.
const gitStatusTimeout = 30 * time.Second

// gitStatusArguments are the arguments of the command that lists the paths in
// a repository whose content differs from that of the commit that's checked
// out (see submoduleStatus). Untracked files aren't listed, since those that
// matter are found by their absence from the index, and listing them would
// mean looking at every directory. Nested submodules aren't examined, since
// their content belongs to their own repositories, and renames aren't
// detected, so that both paths of a rename are listed as they are.
var gitStatusArguments = []string{
	"status",
	"--porcelain=v2",
	"-z",
	"--branch",
	"--untracked-files=no",
	"--ignore-submodules=all",
	"--no-renames",
}

// contentAt returns the entry at a path within an entry hierarchy, or nil if
// there's none.
func contentAt(root *core.Entry, path string) *core.Entry {
	entry := root
	for _, component := range strings.Split(path, "/") {
		if entry == nil {
			return nil
		}
		entry = entry.Contents[component]
	}
	return entry
}

// submoduleStatuses determines the statuses (see core.SubmoduleStatus) of the
// submodules in a snapshot's content that have a commit checked out other
// than the one that the ancestor has for them, or for which the ancestor
// doesn't have one, given the ancestor's GitLink entries, keyed by the paths
// of their submodules. Examining a submodule's working tree means comparing
// its files with the commit, so it's only done for those submodules, which
// are the only ones whose statuses reconciliation uses, and which are
// usually there for a cycle or two after a commit is checked out. A submodule
// whose working tree can't be examined has no status, which only means that
// reconciliation uses the ancestor's content for it, so this only fails if
// the context is cancelled.
func (g *gitProvider) submoduleStatuses(ctx context.Context, content *core.Entry, ancestorLinks map[string]*core.Entry) (map[string]*core.SubmoduleStatus, error) {
	var statuses map[string]*core.SubmoduleStatus
	for path, link := range content.Submodules() {
		if ancestorLink := ancestorLinks[path]; ancestorLink != nil && bytes.Equal(ancestorLink.Digest, link.Digest) {
			continue
		}
		status, err := g.submoduleStatus(ctx, path, contentAt(content, path))
		if ctx.Err() != nil {
			return nil, fmt.Errorf("examination of submodule cancelled: %w", ctx.Err())
		} else if err != nil {
			g.logger.Debugf("Unable to examine working tree of submodule at \"%s\": %v", path, err)
			continue
		}
		if statuses == nil {
			statuses = make(map[string]*core.SubmoduleStatus)
		}
		statuses[path] = status
	}
	return statuses, nil
}

// submoduleStatus determines the status of the submodule at a path, whose
// content in a snapshot is specified, which is a description of the files and
// symbolic links in that content that Git doesn't consider to be the content
// of the commit that's checked out. Those that it does consider to be the
// commit's are those that the index tracks without any flags that make Git
// skip comparing their content (those marked as assumed to be unchanged, and
// those outside of a sparse checkout), and that Git doesn't list as differing
// from the commit (in the index, which covers staged changes, or in the
// working tree, after the filters and line ending conversions that the
// repository is configured for). Anything else, such as a file that the
// repository doesn't track, counts as changed. The commands run after the scan
// that yielded the content, and the status is only valid if the same commit was
// still checked out when Git compared the files.
func (g *gitProvider) submoduleStatus(ctx context.Context, path string, submodule *core.Entry) (*core.SubmoduleStatus, error) {
	// Bound the duration of the commands.
	ctx, cancel := context.WithTimeout(ctx, gitStatusTimeout)
	defer cancel()
	directory := filepath.Join(g.root, filepath.FromSlash(path))
	link := submodule.Contents[core.GitLinkName]

	// List the paths that the index tracks without flags. Each record consists
	// of a tag, a space, and the path, and the tag of a path without flags is
	// H (lowercase for paths that are assumed to be unchanged, and S for those
	// outside of a sparse checkout).
	listing, err := git.Run(ctx, directory, "ls-files", "-z", "-v")
	if err != nil {
		return nil, fmt.Errorf("unable to list tracked files: %w", err)
	}
	unchanged := make(map[string]bool)
	for _, record := range strings.Split(listing, "\x00") {
		if tracked, ok := strings.CutPrefix(record, "H "); ok {
			unchanged[tracked] = true
		}
	}

	// Remove the paths that Git lists as differing from the commit. Each
	// record of a changed path consists of fields separated by spaces, the last
	// of which is the path, which may contain spaces: 8 fields precede it for
	// ordinary changes (1), 10 for unmerged paths (u), and 9 for renames and
	// copies (2), which are followed by a record with the original path.
	// Records for untracked (?) and ignored (!) paths only consist of the
	// path. Headers (#) include the commit that's checked out.
	output, err := git.Run(ctx, directory, gitStatusArguments...)
	if err != nil {
		return nil, fmt.Errorf("unable to compare working tree with commit: %w", err)
	}
	var commit string
	records := strings.Split(output, "\x00")
	for r := 0; r < len(records); r++ {
		record := records[r]
		if record == "" {
			continue
		} else if value, ok := strings.CutPrefix(record, "# branch.oid "); ok {
			commit = value
			continue
		} else if strings.HasPrefix(record, "# ") {
			continue
		}
		var fields int
		switch record[0] {
		case '1':
			fields = 9
		case '2':
			fields = 10
		case 'u':
			fields = 11
		case '?', '!':
			fields = 2
		default:
			return nil, fmt.Errorf("unexpected output from Git: %q", record)
		}
		components := strings.SplitN(record, " ", fields)
		if len(components) != fields {
			return nil, fmt.Errorf("unexpected output from Git: %q", record)
		}
		delete(unchanged, components[fields-1])
		if record[0] == '2' {
			if r++; r == len(records) {
				return nil, errors.New("unexpected end of output from Git")
			}
			delete(unchanged, records[r])
		}
	}

	// Verify that the commit is the one that was scanned.
	if commit != hex.EncodeToString(link.Digest) {
		return nil, fmt.Errorf("commit changed since the scan (from %x to %s)", link.Digest, commit)
	}

	// Collect the paths of the files and symbolic links that aren't unchanged.
	var changed []string
	submodule.WalkSubmodule(func(path string, entry *core.Entry) {
		if entry.Kind != core.EntryKind_Directory && !unchanged[path] {
			changed = append(changed, path)
		}
	})
	sort.Strings(changed)

	// Done.
	return &core.SubmoduleStatus{Commit: link.Digest, Changed: changed}, nil
}
