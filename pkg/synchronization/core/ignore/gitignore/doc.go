// Package gitignore supports gitignore-style ignore syntax and semantics. It
// parses and matches the patterns in the rule files that a Git working tree
// uses to decide which paths to leave untracked ("<directory>/.gitignore" files
// and the root repository's ".git/info/exclude" file) without invoking Git.
//
// The implementation follows the documented behavior of gitignore(5). It
// deliberately differs from Git by always being case-sensitive, which keeps
// results identical on every endpoint.
package gitignore
