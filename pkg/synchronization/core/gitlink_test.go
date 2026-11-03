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

// TestSnapshotSeedsEnsureValid tests that Snapshot.EnsureValid validates seeds.
func TestSnapshotSeedsEnsureValid(t *testing.T) {
	// Define the content of a population and a snapshot that has it. The
	// snapshot's content is allowed to differ from the population's, apart from
	// the seed, since the endpoint may have changed since.
	seed := testingSubmodule(testingSeeded(tGL0, "seed"), map[string]*Entry{"file": tF1})
	content := &Entry{Contents: map[string]*Entry{
		"sub":       testingSubmodule(testingSeeded(tGL0, "seed"), map[string]*Entry{"file": tF2, "untracked": tU}),
		"directory": nested("sub", testingSubmodule(testingSeeded(tGL1, "other"), nil)),
		"plain":     tD1,
		"unseeded":  testingSubmodule(tGL0, nil),
		"file":      tF1,
	}}

	// Verify valid seeds.
	valid := []map[string]*Entry{
		nil,
		{},
		{"sub": seed},
		{"directory/sub": testingSubmodule(testingSeeded(tGL1, "other"), nil)},
	}
	for _, seeds := range valid {
		if err := (&Snapshot{Content: content, Seeds: seeds}).EnsureValid(); err != nil {
			t.Errorf("valid seeds %v classified as invalid: %v", seeds, err)
		}
	}

	// Verify invalid seeds.
	invalid := map[string]map[string]*Entry{
		"root path":                       {"": seed},
		"absolute path":                   {"/sub": seed},
		"trailing slash":                  {"sub/": seed},
		"empty component":                 {"directory//sub": testingSubmodule(testingSeeded(tGL1, "other"), nil)},
		"nil seed":                        {"sub": nil},
		"unsynchronizable seed":           {"sub": testingSubmodule(testingSeeded(tGL0, "seed"), map[string]*Entry{"untracked": tU})},
		"invalid seed":                    {"sub": nested("sub", tGL0)},
		"seed without submodule":          {"sub": tD1},
		"seed that's a file":              {"sub": tF1},
		"seed that's a GitLink entry":     {"sub": testingSeeded(tGL0, "seed")},
		"seed without seed":               {"sub": testingSubmodule(tGL0, nil)},
		"seed with different seed":        {"sub": testingSubmodule(testingSeeded(tGL0, "different"), nil)},
		"seed for missing content":        {"missing": seed},
		"seed for plain directory":        {"plain": testingSubmodule(testingSeeded(tGL0, "seed"), nil)},
		"seed for submodule without seed": {"unseeded": testingSubmodule(testingSeeded(tGL0, "seed"), nil)},
		"seed for file":                   {"file": seed},
		"seed for path beneath file":      {"file/sub": seed},
		"seed for submodule's Git entry":  {"sub/.git": seed},
		"seed with traversal":             {"plain/../sub": seed},
		"one valid and one invalid seed":  {"sub": seed, "plain": seed},
	}
	for description, seeds := range invalid {
		if (&Snapshot{Content: content, Seeds: seeds}).EnsureValid() == nil {
			t.Errorf("%s: invalid seeds classified as valid", description)
		}
	}
}

// TestSnapshotWithSeeds tests that Snapshot.WithSeeds copies every other field
// of the snapshot, including any that are added in the future, without
// modifying the original, and that seeds don't participate in equality.
func TestSnapshotWithSeeds(t *testing.T) {
	// Create a snapshot with every field populated.
	original := &Snapshot{
		Content:                tD1,
		PreservesExecutability: true,
		DecomposesUnicode:      true,
		Directories:            1,
		Files:                  2,
		SymbolicLinks:          3,
		TotalFileSize:          4,
		Warnings:               []*Problem{{Path: "path", Error: "warning"}},
		Seeds:                  map[string]*Entry{"old": tD1},
	}
	reflected := original.ProtoReflect()
	fields := reflected.Descriptor().Fields()
	seedsField := fields.ByName("seeds")
	for f := 0; f < fields.Len(); f++ {
		if !reflected.Has(fields.Get(f)) {
			t.Fatalf("field %s not populated by test", fields.Get(f).Name())
		}
	}

	// Add new seeds and verify the result.
	seeds := map[string]*Entry{"new": tD2}
	result := original.WithSeeds(seeds)
	if result == original {
		t.Fatal("snapshot not copied")
	}
	reflectedResult := result.ProtoReflect()
	for f := 0; f < fields.Len(); f++ {
		field := fields.Get(f)
		if field == seedsField {
			continue
		} else if !reflectedResult.Get(field).Equal(reflected.Get(field)) {
			t.Errorf("field %s not copied", field.Name())
		}
	}
	if len(result.Seeds) != 1 || result.Seeds["new"] != tD2 {
		t.Errorf("incorrect seeds in result: %v", result.Seeds)
	}
	if len(original.Seeds) != 1 || original.Seeds["old"] != tD1 {
		t.Errorf("original seeds modified: %v", original.Seeds)
	}

	// Seeds can be removed.
	if result := original.WithSeeds(nil); result.Seeds != nil || len(result.Warnings) != 1 {
		t.Errorf("incorrect result of removing seeds: %v", result)
	}

	// Seeds don't participate in equality.
	if !original.Equal(result) || !result.Equal(original) {
		t.Error("snapshots differing only in seeds not considered equal")
	}

	// Adding warnings retains seeds.
	if withWarnings := result.WithWarnings(nil); withWarnings.Seeds["new"] != tD2 {
		t.Errorf("seeds not retained when adding warnings: %v", withWarnings.Seeds)
	}
}

// TestSnapshotSeedsSerialization tests that seeds and the fields of GitLink
// entries survive the serialization that's used to transmit snapshots from
// remote endpoints and to store ancestors.
func TestSnapshotSeedsSerialization(t *testing.T) {
	link := &Entry{Kind: EntryKind_GitLink, Digest: tGL0.Digest, Url: tGitURL, Seed: "seed", AutoCRLF: "input", Eol: "lf"}
	original := &Snapshot{
		Content: nested("sub", testingSubmodule(link, map[string]*Entry{"file": tF2})),
		Seeds:   map[string]*Entry{"sub": testingSubmodule(link, map[string]*Entry{"file": tF1})},
	}
	marshaling := proto.MarshalOptions{Deterministic: true}
	data, err := marshaling.Marshal(original)
	if err != nil {
		t.Fatal("unable to marshal snapshot:", err)
	}
	snapshot := &Snapshot{}
	if err := proto.Unmarshal(data, snapshot); err != nil {
		t.Fatal("unable to unmarshal snapshot:", err)
	} else if err := snapshot.EnsureValid(); err != nil {
		t.Fatal("unmarshaled snapshot invalid:", err)
	} else if !proto.Equal(snapshot, original) {
		t.Errorf("snapshot changed by serialization: %v != %v", snapshot, original)
	}
}

// TestSeedChanges tests SeedChanges.
func TestSeedChanges(t *testing.T) {
	// Define the contents of populations.
	population := testingSubmodule(testingSeeded(tGL0, "first"), map[string]*Entry{"file": tF1, "nested": tD0})
	nestedPopulation := testingSubmodule(testingSeeded(tGL1, "second"), map[string]*Entry{"file": tF2})

	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// ancestor is the ancestor.
		ancestor *Entry
		// seeds are the seeds reported by each endpoint.
		seeds []map[string]*Entry
		// expectedChanges are the expected changes, in order.
		expectedChanges []*Change
		// expected is the expected result of applying the changes.
		expected *Entry
	}{
		{
			description: "no seeds",
			ancestor:    tD1,
		},
		{
			description: "empty seeds",
			ancestor:    tD1,
			seeds:       []map[string]*Entry{nil, {}},
		},
		{
			description:     "submodule in existing directory",
			ancestor:        tD1,
			seeds:           []map[string]*Entry{{"sub": population}},
			expectedChanges: []*Change{{Path: "sub", New: population}},
			expected:        &Entry{Contents: map[string]*Entry{"file": tF1, "sub": population}},
		},
		{
			description:     "submodule replacing existing content",
			ancestor:        nested("sub", testingSubmodule(testingSeeded(tGL0, "old"), map[string]*Entry{"other": tF2})),
			seeds:           []map[string]*Entry{nil, {"sub": population}},
			expectedChanges: []*Change{{Path: "sub", New: population}},
			expected:        nested("sub", population),
		},
		{
			description: "missing ancestor",
			seeds:       []map[string]*Entry{{"a/b/sub": population}},
			expectedChanges: []*Change{
				{Path: "", New: tD0},
				{Path: "a", New: tD0},
				{Path: "a/b", New: tD0},
				{Path: "a/b/sub", New: population},
			},
			expected: nested("a", nested("b", nested("sub", population))),
		},
		{
			description: "missing parents",
			ancestor:    tD1,
			seeds:       []map[string]*Entry{{"a/sub": population}},
			expectedChanges: []*Change{
				{Path: "a", New: tD0},
				{Path: "a/sub", New: population},
			},
			expected: &Entry{Contents: map[string]*Entry{"file": tF1, "a": nested("sub", population)}},
		},
		{
			description: "file parent",
			ancestor:    nested("a", tF1),
			seeds:       []map[string]*Entry{{"a/sub": population}},
			expectedChanges: []*Change{
				{Path: "a", New: tD0},
				{Path: "a/sub", New: population},
			},
			expected: nested("a", nested("sub", population)),
		},
		{
			description: "file root",
			ancestor:    tF1,
			seeds:       []map[string]*Entry{{"sub": population}},
			expectedChanges: []*Change{
				{Path: "", New: tD0},
				{Path: "sub", New: population},
			},
			expected: nested("sub", population),
		},
		{
			description:     "existing parent retains other contents",
			ancestor:        nested("a", tD1),
			seeds:           []map[string]*Entry{{"a/sub": population}},
			expectedChanges: []*Change{{Path: "a/sub", New: population}},
			expected:        nested("a", &Entry{Contents: map[string]*Entry{"file": tF1, "sub": population}}),
		},
		{
			description: "nested seeds from different endpoints",
			ancestor:    tD0,
			seeds:       []map[string]*Entry{{"sub/nested": nestedPopulation}, {"sub": population}},
			expectedChanges: []*Change{
				{Path: "sub", New: population},
				{Path: "sub/nested", New: nestedPopulation},
			},
			expected: nested("sub", testingSubmodule(testingSeeded(tGL0, "first"), map[string]*Entry{
				"file":   tF1,
				"nested": nestedPopulation,
			})),
		},
		{
			description: "nested seeds from the same endpoint",
			ancestor:    tD0,
			seeds:       []map[string]*Entry{{"sub/nested": nestedPopulation, "sub": population}},
			expectedChanges: []*Change{
				{Path: "sub", New: population},
				{Path: "sub/nested", New: nestedPopulation},
			},
			expected: nested("sub", testingSubmodule(testingSeeded(tGL0, "first"), map[string]*Entry{
				"file":   tF1,
				"nested": nestedPopulation,
			})),
		},
		{
			description: "nested seed without its parent's population",
			ancestor:    nested("sub", testingSubmodule(tGL0, map[string]*Entry{"nested": tD0, "file": tF2})),
			seeds:       []map[string]*Entry{nil, {"sub/nested": nestedPopulation}},
			expectedChanges: []*Change{
				{Path: "sub/nested", New: nestedPopulation},
			},
			expected: nested("sub", testingSubmodule(tGL0, map[string]*Entry{"nested": nestedPopulation, "file": tF2})),
		},
		{
			description: "independent seeds",
			ancestor:    tD0,
			seeds:       []map[string]*Entry{{"b": nestedPopulation}, {"a": population}},
			expectedChanges: []*Change{
				{Path: "a", New: population},
				{Path: "b", New: nestedPopulation},
			},
			expected: &Entry{Contents: map[string]*Entry{"a": population, "b": nestedPopulation}},
		},
	}

	// Process test cases.
	for _, test := range tests {
		// Compute the changes and verify them, including their order.
		original := test.ancestor.Copy(EntryCopyBehaviorDeep)
		changes := SeedChanges(test.ancestor, test.seeds...)
		if !proto.Equal(test.ancestor, original) {
			t.Errorf("%s: ancestor modified", test.description)
		}
		if len(changes) != len(test.expectedChanges) {
			t.Errorf("%s: incorrect number of changes: %v != %v", test.description, changes, test.expectedChanges)
			continue
		}
		for c, change := range changes {
			expected := test.expectedChanges[c]
			if change.Path != expected.Path || change.Old != nil || !change.New.Equal(expected.New, true) {
				t.Errorf("%s: incorrect change at index %d: %v != %v", test.description, c, change, expected)
			}
		}
		if test.expectedChanges == nil {
			if changes != nil {
				t.Errorf("%s: non-nil changes without seeds", test.description)
			}
			continue
		}

		// Apply the changes and verify the result, which has to be valid, and
		// which has to retain seeds.
		result, err := Apply(test.ancestor, changes)
		if err != nil {
			t.Errorf("%s: unable to apply changes: %v", test.description, err)
			continue
		} else if err := result.EnsureValid(true); err != nil {
			t.Errorf("%s: result invalid: %v", test.description, err)
		} else if !result.Equal(test.expected, true) {
			t.Errorf("%s: incorrect result: %v != %v", test.description, result, test.expected)
		}
		seeds := result.GitLinkSeeds()
		slices.Sort(seeds)
		var expectedSeeds []string
		for _, endpointSeeds := range test.seeds {
			for _, seed := range endpointSeeds {
				expectedSeeds = append(expectedSeeds, seed.GitLinkSeeds()...)
			}
		}
		slices.Sort(expectedSeeds)
		if !slices.Equal(seeds, slices.Compact(expectedSeeds)) {
			t.Errorf("%s: incorrect seeds in result: %v != %v", test.description, seeds, expectedSeeds)
		}

		// Applying the changes again, as the synchronization loop does when it
		// folds them into the cycle's ancestor changes, changes nothing.
		if again, err := Apply(result, changes); err != nil {
			t.Errorf("%s: unable to apply changes again: %v", test.description, err)
		} else if !proto.Equal(again, result) {
			t.Errorf("%s: applying changes again changed result: %v != %v", test.description, again, result)
		}
	}
}

// TestSeedChangesRecoverPopulation tests the scenario that seeds exist for: a
// session stops after beta is populated with a clone of a submodule that alpha
// has, but before the ancestor is saved, while alpha's submodule has a file
// with uncommitted changes, a tracked file that's been deleted, and a new
// file. With the population's content substituted into the ancestor, alpha's
// changes propagate to beta without conflicts and without undoing the deletion
// on alpha, exactly as they would have if the session hadn't stopped. Without
// it, the modification conflicts with the clone and the deletion is undone.
func TestSeedChangesRecoverPopulation(t *testing.T) {
	// Define the content. The ancestor doesn't have the submodule at all, since
	// it's never been saved with it.
	ancestor := nested("other", tF1)
	clone := testingSubmodule(testingSeeded(tGL0, "seed"), map[string]*Entry{
		"x": tF1,
		"y": tF1,
		"z": tD1,
	})
	alpha := &Entry{Contents: map[string]*Entry{
		"other": tF1,
		"sub": testingSubmodule(tGL0, map[string]*Entry{
			"x":         tF2,
			"z":         tD1,
			"new":       tF3,
			"untracked": tU,
		}),
	}}
	beta := &Entry{Contents: map[string]*Entry{"other": tF1, "sub": clone}}
	seeds := map[string]*Entry{"sub": clone}

	// Beta's snapshot, which reports the seed, is valid.
	if err := (&Snapshot{Content: beta, Seeds: seeds}).EnsureValid(); err != nil {
		t.Fatal("beta's snapshot invalid:", err)
	}

	// Substitute the population into the ancestor.
	seedChanges := SeedChanges(ancestor, nil, seeds)
	seeded, err := Apply(ancestor, seedChanges)
	if err != nil {
		t.Fatal("unable to apply seed changes:", err)
	} else if err := seeded.EnsureValid(true); err != nil {
		t.Fatal("seeded ancestor invalid:", err)
	}

	// Verify that alpha's changes propagate to beta in every mode.
	expectedBetaChanges := []*Change{
		{Path: "sub/x", Old: tF1, New: tF2},
		{Path: "sub/y", Old: tF1},
		{Path: "sub/new", New: tF3},
	}
	for _, mode := range allModes {
		ancestorChanges, alphaChanges, betaChanges, conflicts := ReconcileGitMode(seeded, alpha, beta, mode)
		if len(ancestorChanges) != 0 {
			t.Errorf("%s: unexpected ancestor changes: %v", mode.Description(), ancestorChanges)
		}
		if len(alphaChanges) != 0 {
			t.Errorf("%s: unexpected alpha changes: %v", mode.Description(), alphaChanges)
		}
		if !testingChangeListsEqual(betaChanges, expectedBetaChanges) {
			t.Errorf("%s: incorrect beta changes: %v != %v", mode.Description(), betaChanges, expectedBetaChanges)
		}
		if len(conflicts) != 0 {
			t.Errorf("%s: unexpected conflicts: %v", mode.Description(), conflicts)
		}

		// The ancestor that the synchronization loop computes from the seed
		// changes (applied again), the reconciliation, and the results of the
		// transitions, is valid and absorbs the seed.
		changes := append(append([]*Change{}, seedChanges...), ancestorChanges...)
		for _, change := range betaChanges {
			changes = append(changes, &Change{Path: change.Path, New: change.New})
		}
		if result, err := Apply(seeded, changes); err != nil {
			t.Errorf("%s: unable to compute new ancestor: %v", mode.Description(), err)
		} else if err := result.EnsureValid(true); err != nil {
			t.Errorf("%s: new ancestor invalid: %v", mode.Description(), err)
		} else if seeds := result.GitLinkSeeds(); !slices.Equal(seeds, []string{"seed"}) {
			t.Errorf("%s: seed not absorbed by new ancestor: %v", mode.Description(), seeds)
		}
	}

	// Verify that, without the substitution, the modification conflicts with
	// the clone and the deletion is undone by recreating the file on alpha.
	_, alphaChanges, betaChanges, conflicts := ReconcileGitMode(
		ancestor, alpha, beta, SynchronizationMode_SynchronizationModeTwoWaySafe,
	)
	expectedAlphaChanges := []*Change{{Path: "sub/y", New: tF1}}
	expectedBetaChanges = []*Change{{Path: "sub/new", New: tF3}}
	expectedConflicts := []*Conflict{{
		Root:         "sub/x",
		AlphaChanges: []*Change{{Path: "sub/x", New: tF2}},
		BetaChanges:  []*Change{{Path: "sub/x", New: tF1}},
	}}
	if !testingChangeListsEqual(alphaChanges, expectedAlphaChanges) {
		t.Errorf("incorrect alpha changes without substitution: %v != %v", alphaChanges, expectedAlphaChanges)
	}
	if !testingChangeListsEqual(betaChanges, expectedBetaChanges) {
		t.Errorf("incorrect beta changes without substitution: %v != %v", betaChanges, expectedBetaChanges)
	}
	if !testingConflictListsEqual(conflicts, expectedConflicts) {
		t.Errorf("incorrect conflicts without substitution: %v != %v", conflicts, expectedConflicts)
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
