// Package gitignore supports gitignore-style ignore syntax and semantics. It
// evaluates the rule files that a Git working tree uses to decide which paths
// to leave untracked ("<directory>/.gitignore" files and the root repository's
// ".git/info/exclude" file) directly against the filesystem, without invoking
// Git or reading anything from outside the synchronization root.
//
// The implementation follows the documented behavior of gitignore(5). It
// deliberately differs from Git in a few ways that keep results identical on
// every endpoint: it never consults global or system configuration, it never
// reads rule files from above the synchronization root, it never follows
// symbolic links when reading rule files, and it is always case-sensitive.
package gitignore
