package ignore

// IgnoreStatus encodes the different potential ignoredness states of content
// during filesystem traversal.
type IgnoreStatus uint8

const (
	// IgnoreStatusNominal indicates that content is neither explicitly ignored
	// nor unignored by any rule in an ignorer. In this case, the content is
	// typically traversed and processed, except in cases where an ignore
	// previously applied to parent content.
	IgnoreStatusNominal IgnoreStatus = iota
	// IgnoreStatusIgnored indicates that content is explicitly ignored.
	IgnoreStatusIgnored
	// IgnoreStatusUnignored indicates that content is explicitly unignored.
	IgnoreStatusUnignored
)

// Ignorer performs ignore evaluation during filesystem traversal. It takes the
// path being traversed and an indication of whether or not traversal should
// continue to child entries due to the possibility of content being unignored
// at a lower depth. Ignorer implementations need not be safe concurrent usage
// by multiple Goroutines.
type Ignorer interface {
	// Ignore determines whether or not a filesystem entry should be ignored
	// based on its path and nature as a directory. It returns the ignore status
	// for the entry, as well as a traversal continuation directive. The
	// traversal continuation directive should be true only if (a) the path is a
	// directory, (b) the ignore status is nominal (in which case it might be
	// ignore-masked) or ignored, and (c) an inverted ignore indicates that
	// content beneath the entry could be explicitly unignored. The path
	// provided to Ignore will be relative to the synchronization root and
	// suitable for use with the fastpath package. The Ignore method should
	// always return the same results for a given set of arguments.
	Ignore(path string, directory bool) (IgnoreStatus, bool)
}

// IgnoreCacheKey represents a key in an ignore cache.
type IgnoreCacheKey struct {
	// Path is the path used for testing ignore status.
	Path string
	// Directory is whether or not that path was a directory.
	Directory bool
}

// IgnoreCacheValue represents a value in an ignore cache.
type IgnoreCacheValue struct {
	// Status is the ignore status.
	Status IgnoreStatus
	// ContinueTraversal indicates whether or not to continue traversal in the
	// event that the content is ignored (due to either an explicit or implicit
	// (ignore-masked) ignore).
	ContinueTraversal bool
}

// IgnoreCache provides an efficient mechanism to avoid recomputing ignores.
type IgnoreCache map[IgnoreCacheKey]IgnoreCacheValue

// NonUTF8Ignorer is an ignorer that can evaluate paths whose last component
// isn't valid UTF-8. The specifications of other ignorers are valid UTF-8 and
// aren't meant to match such names, so scans don't evaluate content with such
// names with them, and they record it as problematic content (unless it's
// ignore masked), since its name can't be synchronized. Rules that match bytes
// rather than characters, as gitignore rules do, can ignore such content, which
// scans then record as untracked, like any other ignored content.
type NonUTF8Ignorer interface {
	Ignorer
	// EvaluatesNonUTF8 returns true if Ignore can evaluate paths whose last
	// component isn't valid UTF-8.
	EvaluatesNonUTF8() bool
}
