package synchronization

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/encoding"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// TestUpdateReplicaRecord tests the computation of the paths that beta has
// untracked and the paths of exposed content.
func TestUpdateReplicaRecord(t *testing.T) {
	type contents = map[string]*core.Entry
	untracked := &core.Entry{Kind: core.EntryKind_Untracked}
	problematic := &core.Entry{Kind: core.EntryKind_Problematic, Problem: "unreadable"}
	directory := func(contents contents) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: contents}
	}
	tests := []struct {
		// name is the name of the test.
		name string
		// previous is the previous record.
		previous *ReplicaRecord
		// alpha is alpha's content.
		alpha *core.Entry
		// beta is beta's content.
		beta *core.Entry
		// expected is the expected record.
		expected *ReplicaRecord
	}{
		{
			name:     "untracked content recorded",
			previous: &ReplicaRecord{},
			beta: directory(contents{
				"data": untracked,
				"sub":  directory(contents{"build": untracked, ".git": untracked, "file": testingFile}),
				"repo": directory(contents{"git~1": untracked}),
			}),
			expected: &ReplicaRecord{Untracked: []string{"data", "sub/build"}},
		},
		{
			name:     "untracked content exposed",
			previous: &ReplicaRecord{Untracked: []string{"data", "gone", "sub/build"}},
			alpha:    directory(contents{"sub": directory(nil)}),
			beta:     directory(contents{"data": testingFile, "sub": directory(contents{"build": directory(contents{"x": untracked})})}),
			expected: &ReplicaRecord{Untracked: []string{"sub/build/x"}, Exposed: []string{"data", "sub/build"}},
		},
		{
			name:     "content identical to alpha's isn't exposed",
			previous: &ReplicaRecord{Untracked: []string{"data", "other"}, Exposed: []string{"exposed"}},
			alpha:    directory(contents{"data": testingFile, "other": testingOtherFile, "exposed": testingFile}),
			beta:     directory(contents{"data": testingFile, "other": testingFile, "exposed": testingFile}),
			expected: &ReplicaRecord{Exposed: []string{"other"}},
		},
		{
			name:     "content untracked on alpha isn't identical",
			previous: &ReplicaRecord{Untracked: []string{"data"}},
			alpha:    directory(contents{"data": untracked}),
			beta:     directory(contents{"data": testingFile}),
			expected: &ReplicaRecord{Exposed: []string{"data"}},
		},
		{
			name:     "exposed content released",
			previous: &ReplicaRecord{Exposed: []string{"deleted", "ignored", "parent/ignored", "replaced/file"}},
			beta: directory(contents{
				"ignored":  untracked,
				"parent":   untracked,
				"replaced": testingFile,
			}),
			expected: &ReplicaRecord{Untracked: []string{"ignored", "parent"}},
		},
		{
			name:     "paths beneath problematic content kept",
			previous: &ReplicaRecord{Untracked: []string{"a/data"}, Exposed: []string{"a/exposed", "b"}},
			beta:     directory(contents{"a": problematic, "b": problematic}),
			expected: &ReplicaRecord{Untracked: []string{"a/data"}, Exposed: []string{"a/exposed", "b"}},
		},
		{
			name:     "no content on beta",
			previous: &ReplicaRecord{Untracked: []string{"data"}, Exposed: []string{"exposed"}},
			expected: &ReplicaRecord{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, changed := updateReplicaRecord(test.previous, test.alpha, test.beta)
			if !slices.Equal(record.Untracked, test.expected.Untracked) {
				t.Errorf("incorrect untracked paths: %v != %v", record.Untracked, test.expected.Untracked)
			}
			if !slices.Equal(record.Exposed, test.expected.Exposed) {
				t.Errorf("incorrect exposed paths: %v != %v", record.Exposed, test.expected.Exposed)
			}
			expectChanged := !slices.Equal(test.previous.Untracked, test.expected.Untracked) ||
				!slices.Equal(test.previous.Exposed, test.expected.Exposed)
			if changed != expectChanged {
				t.Errorf("incorrect change indication: %t", changed)
			}
			if err := record.EnsureValid(); err != nil {
				t.Error("invalid record:", err)
			}
		})
	}
}

// TestWithholdExposedChanges tests that the transitions that would remove or
// replace exposed content are withheld, and only those.
func TestWithholdExposedChanges(t *testing.T) {
	type contents = map[string]*core.Entry
	directory := func(contents contents) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: contents}
	}
	exposed := []string{"data", "deep/exposed"}
	tests := []struct {
		// name is the name of the test.
		name string
		// transition is the transition.
		transition *core.Change
		// affected are the exposed paths that the transition affects.
		affected []string
	}{
		{"removal", &core.Change{Path: "data", Old: testingFile}, []string{"data"}},
		{"replacement", &core.Change{Path: "data", Old: testingFile, New: testingOtherFile}, []string{"data"}},
		{"removal beneath", &core.Change{Path: "data/file", Old: testingFile}, []string{"data"}},
		{"modification beneath", &core.Change{Path: "data/a/b", Old: testingFile, New: testingOtherFile}, []string{"data"}},
		{"creation beneath", &core.Change{Path: "data/new", New: testingFile}, nil},
		{"removal above", &core.Change{Path: "deep", Old: directory(contents{"exposed": testingFile})}, []string{"deep/exposed"}},
		{"replacement of root", &core.Change{Old: directory(contents{"data": testingFile, "deep": directory(contents{"exposed": testingFile})}), New: testingFile}, []string{"data", "deep/exposed"}},
		{"removal above without exposed content", &core.Change{Path: "deep", Old: directory(contents{"other": testingFile})}, nil},
		{"sibling", &core.Change{Path: "database", Old: testingFile}, nil},
		{"other path", &core.Change{Path: "deep/other", Old: testingFile}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unaffected := &core.Change{Path: "unaffected", Old: testingFile}
			retained, problems := withholdExposedChanges(exposed, []*core.Change{test.transition, unaffected})
			expected := []*core.Change{test.transition, unaffected}
			if test.affected != nil {
				expected = []*core.Change{unaffected}
			}
			if !slices.Equal(retained, expected) {
				t.Errorf("incorrect retained transitions: %v", retained)
			}
			var affected []string
			for _, problem := range problems {
				if problem.Warning || problem.Error != exposedContentProblem {
					t.Error("incorrect problem:", problem)
				}
				affected = append(affected, problem.Path)
			}
			if !slices.Equal(affected, test.affected) {
				t.Errorf("incorrect affected paths: %v != %v", affected, test.affected)
			}
		})
	}

	// Without exposed content, nothing is withheld.
	transitions := []*core.Change{{Path: "data", Old: testingFile}}
	if retained, problems := withholdExposedChanges(nil, transitions); !slices.Equal(retained, transitions) || problems != nil {
		t.Errorf("transitions withheld without exposed content: %v, %v", retained, problems)
	}

	// Content is only reported once, even if several transitions affect it.
	_, problems := withholdExposedChanges(exposed, []*core.Change{
		{Path: "data/a", Old: testingFile},
		{Path: "data/b", Old: testingFile},
	})
	if len(problems) != 1 {
		t.Error("exposed content not reported once:", problems)
	}
}

// TestLoadReplicaRecord tests that a record that doesn't exist is empty, and
// that one that can't be loaded or that's invalid is an error.
func TestLoadReplicaRecord(t *testing.T) {
	directory := t.TempDir()

	// A missing record is empty.
	if record, err := loadReplicaRecord(filepath.Join(directory, "missing")); err != nil {
		t.Error("unable to load missing record:", err)
	} else if len(record.Untracked) != 0 || len(record.Exposed) != 0 {
		t.Error("missing record not empty:", record)
	}

	// A record round-trips.
	path := filepath.Join(directory, "record")
	saved := &ReplicaRecord{Untracked: []string{"a", "b/c"}, Exposed: []string{"d"}}
	if err := encoding.MarshalAndSaveProtobuf(path, saved); err != nil {
		t.Fatal("unable to save record:", err)
	}
	if record, err := loadReplicaRecord(path); err != nil {
		t.Error("unable to load record:", err)
	} else if !slices.Equal(record.Untracked, saved.Untracked) || !slices.Equal(record.Exposed, saved.Exposed) {
		t.Error("incorrect record:", record)
	}

	// Corrupt and invalid records are errors.
	if err := os.WriteFile(path, []byte{0xff, 0xff}, 0600); err != nil {
		t.Fatal("unable to corrupt record:", err)
	} else if _, err := loadReplicaRecord(path); err == nil {
		t.Error("corrupt record loaded")
	}
	for _, invalid := range []*ReplicaRecord{
		{Untracked: []string{"b", "a"}},
		{Exposed: []string{"a", "a"}},
		{Exposed: []string{""}},
	} {
		if err := encoding.MarshalAndSaveProtobuf(path, invalid); err != nil {
			t.Fatal("unable to save record:", err)
		} else if _, err := loadReplicaRecord(path); err == nil {
			t.Error("invalid record loaded:", invalid)
		}
	}

	// A record that can't be read is an error.
	if _, err := loadReplicaRecord(directory); err == nil {
		t.Error("unreadable record loaded")
	}
}
