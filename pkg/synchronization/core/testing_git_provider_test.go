package core

// tGitURL is the URL of the repository of the GitLink entries for testing.
const tGitURL = "https://example.com/submodule.git"

// tGL0 is a GitLink entry for testing.
var tGL0 = &Entry{Kind: EntryKind_GitLink, Digest: []byte{0xc0, 0x00}, Url: tGitURL}

// tGL1 is an alternative GitLink entry (with a different commit) for testing.
var tGL1 = &Entry{Kind: EntryKind_GitLink, Digest: []byte{0xc1, 0x01}, Url: tGitURL}

// tGL2 is an alternative GitLink entry (with a different commit) for testing.
var tGL2 = &Entry{Kind: EntryKind_GitLink, Digest: []byte{0xc2, 0x02}, Url: tGitURL}

// testingSubmodule creates a submodule for testing: a directory whose Git entry
// is the specified GitLink entry and whose other contents are as specified.
func testingSubmodule(link *Entry, contents map[string]*Entry) *Entry {
	result := &Entry{Contents: map[string]*Entry{GitLinkName: link}}
	for name, entry := range contents {
		result.Contents[name] = entry
	}
	return result
}

// testingSeeded creates a copy of a GitLink entry that carries a seed.
func testingSeeded(link *Entry, seed string) *Entry {
	result := link.Copy(EntryCopyBehaviorSlim)
	result.Seed = seed
	return result
}
