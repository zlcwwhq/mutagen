package core

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestGitLinkKind tests the properties of the GitLink entry kind.
func TestGitLinkKind(t *testing.T) {
	// GitLink entries are synchronizable.
	if !EntryKind_GitLink.synchronizable() {
		t.Error("GitLink entry kind not synchronizable")
	}

	// They round-trip through their text representation.
	if text, err := EntryKind_GitLink.MarshalText(); err != nil {
		t.Fatal("unable to marshal GitLink entry kind:", err)
	} else if string(text) != "gitlink" {
		t.Errorf("incorrect text for GitLink entry kind: %s", text)
	}
	var kind EntryKind
	if err := kind.UnmarshalText([]byte("gitlink")); err != nil {
		t.Fatal("unable to unmarshal GitLink entry kind:", err)
	} else if kind != EntryKind_GitLink {
		t.Errorf("incorrect GitLink entry kind: %v", kind)
	}
}

// TestGitLinkEnsureValid tests Entry.EnsureValid for GitLink entries and for
// the fields that only GitLink entries may have.
func TestGitLinkEnsureValid(t *testing.T) {
	// Define the fields that only GitLink entries may have, along with a
	// function that creates a copy of an entry with one of them set.
	fields := map[string]func(*Entry){
		"URL":      func(e *Entry) { e.Url = tGitURL },
		"seed":     func(e *Entry) { e.Seed = "seed" },
		"autocrlf": func(e *Entry) { e.AutoCRLF = "true" },
		"eol":      func(e *Entry) { e.Eol = "lf" },
	}
	withField := func(entry *Entry, field string) *Entry {
		result := entry.Copy(EntryCopyBehaviorDeep)
		fields[field](result)
		return result
	}

	// Define the entries that should be valid, which are validated with the
	// synchronizability requirement, unless they're phantom directories.
	complete := &Entry{
		Kind:     EntryKind_GitLink,
		Digest:   tGL0.Digest,
		Url:      tGitURL,
		Seed:     "seed",
		AutoCRLF: "input",
		Eol:      "crlf",
	}
	valid := map[string]*Entry{
		"submodule":                      testingSubmodule(tGL0, map[string]*Entry{"file": tF1}),
		"submodule without contents":     testingSubmodule(tGL0, nil),
		"submodule with all fields":      testingSubmodule(complete, nil),
		"nested submodule":               nested("directory", testingSubmodule(tGL1, nil)),
		"submodule within submodule":     testingSubmodule(tGL0, map[string]*Entry{"nested": testingSubmodule(tGL1, nil)}),
		"GitLink entry by itself":        tGL0,
		"phantom directory with GitLink": {Kind: EntryKind_PhantomDirectory, Contents: map[string]*Entry{GitLinkName: tGL0}},
	}
	for description, entry := range valid {
		synchronizable := entry.Kind != EntryKind_PhantomDirectory
		if err := entry.EnsureValid(synchronizable); err != nil {
			t.Errorf("%s: incorrectly classified as invalid: %v", description, err)
		}
	}

	// Define the entries that should be invalid in either case.
	invalid := map[string]*Entry{
		"GitLink entry with another name":            nested("sub", tGL0),
		"GitLink entry with differently cased name":  nested(".GIT", tGL0),
		"GitLink entry beneath another name":         nested("directory", nested("sub", tGL0)),
		"GitLink entry with another name in phantom": {Kind: EntryKind_PhantomDirectory, Contents: map[string]*Entry{"sub": tGL0}},
		"GitLink entry with empty contents":          {Kind: EntryKind_GitLink, Digest: tGL0.Digest, Url: tGitURL, Contents: map[string]*Entry{}},
		"GitLink entry with contents":                {Kind: EntryKind_GitLink, Digest: tGL0.Digest, Url: tGitURL, Contents: map[string]*Entry{"file": tF1}},
		"executable GitLink entry":                   {Kind: EntryKind_GitLink, Digest: tGL0.Digest, Url: tGitURL, Executable: true},
		"GitLink entry with target":                  {Kind: EntryKind_GitLink, Digest: tGL0.Digest, Url: tGitURL, Target: "target"},
		"GitLink entry with problem":                 {Kind: EntryKind_GitLink, Digest: tGL0.Digest, Url: tGitURL, Problem: "problem"},
		"GitLink entry without digest":               {Kind: EntryKind_GitLink, Url: tGitURL},
		"GitLink entry with empty digest":            {Kind: EntryKind_GitLink, Digest: []byte{}, Url: tGitURL},
		"GitLink entry without URL":                  {Kind: EntryKind_GitLink, Digest: tGL0.Digest},
		"submodule with invalid GitLink entry":       testingSubmodule(&Entry{Kind: EntryKind_GitLink, Digest: tGL0.Digest}, nil),
	}
	for field := range fields {
		invalid["directory with "+field] = withField(tD0, field)
		invalid["directory containing file with "+field] = nested("file", withField(tF1, field))
		invalid["file with "+field] = withField(tF1, field)
		invalid["symbolic link with "+field] = withField(tSR, field)
		invalid["untracked content with "+field] = withField(tU, field)
		invalid["problematic content with "+field] = withField(tP1, field)
		invalid["phantom directory with "+field] = withField(tPD0, field)
	}
	for description, entry := range invalid {
		for _, synchronizable := range []bool{false, true} {
			if entry.EnsureValid(synchronizable) == nil {
				t.Errorf("%s: incorrectly classified as valid (synchronizable: %t)", description, synchronizable)
			}
		}
	}
}

// TestGitLinkCopy tests that Entry.Copy retains all of the fields of GitLink
// entries, with every copy behavior.
func TestGitLinkCopy(t *testing.T) {
	link := &Entry{
		Kind:     EntryKind_GitLink,
		Digest:   tGL0.Digest,
		Url:      tGitURL,
		Seed:     "seed",
		AutoCRLF: "true",
		Eol:      "lf",
	}
	verify := func(description string, copied *Entry) {
		t.Helper()
		if !proto.Equal(copied, link) {
			t.Errorf("%s: GitLink entry fields not copied: %v", description, copied)
		}
	}
	for _, behavior := range []EntryCopyBehavior{
		EntryCopyBehaviorDeep,
		EntryCopyBehaviorDeepPreservingLeaves,
		EntryCopyBehaviorShallow,
		EntryCopyBehaviorSlim,
	} {
		verify("GitLink entry", link.Copy(behavior))
	}
	directory := testingSubmodule(link, map[string]*Entry{"nested": nested("directory", testingSubmodule(link, nil))})
	for _, behavior := range []EntryCopyBehavior{EntryCopyBehaviorDeep, EntryCopyBehaviorDeepPreservingLeaves} {
		copied := directory.Copy(behavior)
		verify("submodule's GitLink entry", copied.Contents[GitLinkName])
		verify("nested submodule's GitLink entry", copied.Contents["nested"].Contents["directory"].Contents[GitLinkName])
	}

	// Taking the synchronizable portion of a submodule retains them too.
	verify("synchronizable GitLink entry", link.synchronizable())
	synchronizable := testingSubmodule(link, map[string]*Entry{"untracked": tU}).synchronizable()
	verify("synchronizable submodule's GitLink entry", synchronizable.Contents[GitLinkName])
	if len(synchronizable.Contents) != 1 {
		t.Errorf("unsynchronizable content retained: %v", synchronizable.Contents)
	}
}

// TestGitLinkEqual tests that Entry.Equal only uses the commit to identify a
// GitLink entry.
func TestGitLinkEqual(t *testing.T) {
	// Entries that only differ in the other fields are equal.
	variants := []*Entry{
		{Kind: EntryKind_GitLink, Digest: tGL0.Digest, Url: "https://example.com/other.git"},
		testingSeeded(tGL0, "seed"),
		{Kind: EntryKind_GitLink, Digest: tGL0.Digest, Url: tGitURL, AutoCRLF: "true"},
		{Kind: EntryKind_GitLink, Digest: tGL0.Digest, Url: tGitURL, Eol: "crlf"},
	}
	for _, variant := range variants {
		for _, deep := range []bool{false, true} {
			if !tGL0.Equal(variant, deep) || !variant.Equal(tGL0, deep) {
				t.Errorf("GitLink entries differing only in %v not equal (deep: %t)", variant, deep)
			}
		}
		if !testingSubmodule(tGL0, nil).Equal(testingSubmodule(variant, nil), true) {
			t.Errorf("submodules differing only in %v not equal", variant)
		}
	}

	// Entries with different commits or kinds are different.
	if tGL0.Equal(tGL1, false) {
		t.Error("GitLink entries with different commits equal")
	} else if tGL0.Equal(&Entry{Kind: EntryKind_File, Digest: tGL0.Digest}, false) {
		t.Error("GitLink entry equal to file with same digest")
	} else if testingSubmodule(tGL0, nil).Equal(testingSubmodule(tGL1, nil), true) {
		t.Error("submodules with different commits equal")
	} else if !testingSubmodule(tGL0, nil).Equal(testingSubmodule(tGL1, nil), false) {
		t.Error("submodules with different commits not shallowly equal")
	}
}

// TestGitLinkHelpers tests the functions that locate GitLink entries and their
// submodules within entry hierarchies.
func TestGitLinkHelpers(t *testing.T) {
	// Define a hierarchy with submodules at several levels, along with
	// directories that aren't submodules for various reasons.
	hierarchy := &Entry{Contents: map[string]*Entry{
		"sub": testingSubmodule(testingSeeded(tGL0, "first"), map[string]*Entry{
			"nested": testingSubmodule(tGL1, map[string]*Entry{"file": tF1}),
			"file":   tF2,
		}),
		"directory": nested("deep", testingSubmodule(testingSeeded(tGL2, "second"), nil)),
		"plain":     &Entry{Contents: map[string]*Entry{GitLinkName: tU, "file": tF1}},
		"file":      tF1,
	}}

	// Verify IsSubmodule.
	submodules := map[*Entry]bool{
		nil:                         false,
		hierarchy:                   false,
		hierarchy.Contents["sub"]:   true,
		hierarchy.Contents["plain"]: false,
		hierarchy.Contents["file"]:  false,
		hierarchy.Contents["sub"].Contents[GitLinkName]:                                    false,
		{Kind: EntryKind_PhantomDirectory, Contents: map[string]*Entry{GitLinkName: tGL0}}: false,
		{Contents: map[string]*Entry{GitLinkName: tF1}}:                                    false,
	}
	for entry, expected := range submodules {
		if entry.IsSubmodule() != expected {
			t.Errorf("incorrect submodule classification for %v: %t", entry, !expected)
		}
	}

	// Verify GitLinkPaths, including that it's relative to the entry.
	expectedPaths := []string{"directory/deep/.git", "sub/.git", "sub/nested/.git"}
	if paths := hierarchy.GitLinkPaths(); !slices.Equal(paths, expectedPaths) {
		t.Errorf("incorrect GitLink paths: %v != %v", paths, expectedPaths)
	}
	if paths := hierarchy.Contents["sub"].GitLinkPaths(); !slices.Equal(paths, []string{".git", "nested/.git"}) {
		t.Errorf("incorrect relative GitLink paths: %v", paths)
	}
	if paths := (*Entry)(nil).GitLinkPaths(); paths != nil {
		t.Errorf("GitLink paths for nil entry: %v", paths)
	}

	// Verify Submodules, which can only identify submodules below the entry.
	expectedSubmodules := map[string]*Entry{
		"sub":            hierarchy.Contents["sub"].Contents[GitLinkName],
		"sub/nested":     tGL1,
		"directory/deep": hierarchy.Contents["directory"].Contents["deep"].Contents[GitLinkName],
	}
	if result := hierarchy.Submodules(); len(result) != len(expectedSubmodules) {
		t.Errorf("incorrect submodules: %v", result)
	} else {
		for path, link := range expectedSubmodules {
			if result[path] != link {
				t.Errorf("incorrect submodule at %s: %v", path, result[path])
			}
		}
	}
	if result := hierarchy.Contents["sub"].Submodules(); len(result) != 1 || result["nested"] != tGL1 {
		t.Errorf("incorrect relative submodules: %v", result)
	}
	if result := tD1.Submodules(); result != nil {
		t.Errorf("submodules found without GitLink entries: %v", result)
	}

	// Verify GitLinkSeeds, which only includes non-empty seeds.
	seeds := hierarchy.GitLinkSeeds()
	slices.Sort(seeds)
	if !slices.Equal(seeds, []string{"first", "second"}) {
		t.Errorf("incorrect seeds: %v", seeds)
	}
	if seeds := tD1.GitLinkSeeds(); seeds != nil {
		t.Errorf("seeds found without GitLink entries: %v", seeds)
	}

	// Verify gitLinkDirectory.
	directories := []struct {
		path      string
		directory string
		ok        bool
	}{
		{"", "", false},
		{".git", "", false},
		{"sub/.git", "sub", true},
		{"a/b/.git", "a/b", true},
		{"sub", "", false},
		{"sub/.gitignore", "", false},
		{"sub/.GIT", "", false},
		{"sub/.git/x", "", false},
	}
	for _, test := range directories {
		if directory, ok := gitLinkDirectory(test.path); directory != test.directory || ok != test.ok {
			t.Errorf("incorrect directory for %q: %q, %t", test.path, directory, ok)
		}
	}

	// Verify lookup.
	lookups := map[string]*Entry{
		"":                hierarchy,
		"sub":             hierarchy.Contents["sub"],
		"sub/nested/file": tF1,
		"sub/.git":        hierarchy.Contents["sub"].Contents[GitLinkName],
		"missing":         nil,
		"file/beneath":    nil,
		"missing/beneath": nil,
	}
	for path, expected := range lookups {
		if result := lookup(hierarchy, path); result != expected {
			t.Errorf("incorrect lookup result for %q: %v", path, result)
		}
	}
	if result := lookup(nil, "sub"); result != nil {
		t.Errorf("lookup in nil entry found %v", result)
	}
}

// TestGitLinkLocationValidation tests that a GitLink entry is only valid where
// it can be a submodule's Git entry when it's at the top level of a snapshot or
// a change, while an archive can hold one, since archives also wrap the results
// of transitions, such as those that attach or follow.
func TestGitLinkLocationValidation(t *testing.T) {
	if err := (&Snapshot{Content: tGL0}).EnsureValid(); err == nil {
		t.Error("snapshot with GitLink root considered valid")
	}
	if err := (&Archive{Content: tGL0}).EnsureValid(true); err != nil {
		t.Error("archive with GitLink root considered invalid:", err)
	}
	for _, path := range []string{"", ".git", "sub", "sub/.gitx"} {
		for _, change := range []*Change{{Path: path, New: tGL0}, {Path: path, Old: tGL0}} {
			if err := change.EnsureValid(true); err == nil {
				t.Errorf("change of GitLink at %q considered valid", path)
			}
		}
	}
	for _, path := range []string{"sub/.git", "a/b/.git"} {
		if err := (&Change{Path: path, Old: tGL0, New: tGL0}).EnsureValid(true); err != nil {
			t.Errorf("change of GitLink at %q considered invalid: %v", path, err)
		}
	}
}
