// Package gitignore supports gitignore-style ignore syntax and semantics. It
// evaluates the rule files that a Git working tree uses to decide which paths
// to leave untracked ("<directory>/.gitignore" files and the "info/exclude"
// files of the root repository and of any nested repositories) directly
// against the filesystem, without invoking Git.
//
// Like Git, it evaluates the paths beneath a nested repository (a directory
// other than the root that contains a ".git" entry, such as a submodule) for
// that repository alone: the rules of the directories above it don't apply
// beneath it, and its own exclude file applies instead. Those rules still
// decide whether its directory is ignored as a whole, and a failure to read
// them still applies beneath it. Nested repositories are recognized by their
// ".git" entries, which can be directories or files that point to Git
// directories, without checking whether Git would accept them, and a symbolic
// link named ".git" doesn't count.
//
// The implementation follows the documented behavior of gitignore(5) and is
// checked against the behavior of the git executable by black-box tests. It
// deliberately differs from Git in a few ways that keep results identical on
// every endpoint: it never consults global or system configuration, it never
// reads rule files from above the synchronization root, it never follows
// symbolic links when reading rule files, and it is always case-sensitive.
package gitignore
