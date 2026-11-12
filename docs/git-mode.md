# Git mode

Git mode makes a session follow the conventions of Git working trees:

- `.gitignore` files decide what's synchronized, so `ignore.paths` no longer
  needs to repeat them.
- `.git` entries are never synchronized.
- Submodules have their files synchronized, and the other endpoint follows the
  commit that they have checked out.

It's disabled by default.


## Contents

- [Enabling Git mode](#enabling-git-mode)
- [Ignores](#ignores)
- [Changing rules](#changing-rules)
- [Submodules](#submodules)
- [Nested repositories](#nested-repositories)
- [Warnings](#warnings)
- [Remote commands](#remote-commands)
- [Requirements and limitations](#requirements-and-limitations)


## Enabling Git mode

Set `git: enabled` for a session, under `defaults`, in global configuration, or
under `star.sync`, or pass `--git` to `mutagen sync create`:

```yaml
sync:
  code:
    alpha: "."
    beta: "server:~/project"
    git: enabled
    remote: true
    ignore:
      paths: ["!.env", ".venv/"]
```

```sh
mutagen sync create --git --remote=true . server:~/project
```

- It applies to the whole session and can't be set for one endpoint. In a star,
  `star.sync` or the defaults apply it to every edge; nodes can't set it
  individually.
- It implies `ignore.vcs: true`, and is rejected together with a request to
  synchronize VCS directories (`--no-ignore-vcs` or `ignore.vcs: false`).
- Changing it in a project recreates the session at the next
  `mutagen project apply`, like other configuration changes.
- `mutagen sync list -l` shows the mode.

To migrate a session that used `ignore.vcs: true`, replace that with
`git: enabled`, remove the `ignore.paths` patterns that repeat `.gitignore`
files, and apply the project.


## Ignores

The session ignores what Git would:

- the rules of the `.gitignore` files at every level beneath the
  synchronization root,
- the rules of the root repository's `.git/info/exclude`,
- inside a [nested repository](#nested-repositories), including a submodule,
  that repository's own rules.

Global excludes (`core.excludesFile`) and files above the root aren't read.
Rules always match case-sensitively, so that both endpoints agree.

Each endpoint reads the rule files in its own copy of the tree, anew at each
scan, so changes apply at the next scan. `.git/info/exclude` is inside `.git`,
which is never synchronized, so it only applies on endpoints that have the
repository. Put ignores that both endpoints should share in `.gitignore`.

**`ignore.paths` takes precedence.** A pattern such as `!.env` synchronizes a
file that a rule ignores, and a rule can't unignore what a pattern ignores.

**`.git` is never synchronized.** Entries named `.git` (files, as used by
submodules and worktrees, as well as directories) can't be unignored, and
neither can names that Windows or macOS treat as `.git`, such as `.git.` and
`git~1`, which Git refuses to track. A submodule's commit is synchronized
instead; see [Submodules](#submodules).

**Tracked files that rules ignore** aren't synchronized either. The endpoint
that has the repository reports a problem that suggests the `ignore.paths`
pattern that unignores them: `!path`, or `!directory` for a file inside an
ignored directory.

**Each endpoint also applies its own rules to what it receives,** including
paths that it doesn't have yet. So a `.git/info/exclude` on one endpoint keeps
the other endpoint's copies of what it excludes away:

- An excluded path isn't written and is reported as a warning, while the rest
  of its directory still is. If `*.log` excludes `pkg/debug.log`, then
  `pkg/a.go` still arrives.
- Rule files themselves are still delivered, even into an excluded directory.
- If the endpoint already has the excluded path, the session simply doesn't
  track it, and there's nothing to warn about.
- A pattern such as `!pkg/debug.log` in `ignore.paths` overrides the exclusion.


## Changing rules

**A rule change can't delete files.** If a rule starts to ignore files that
were synchronized, they stay on both endpoints and the session stops tracking
them. Removing the rule resumes synchronizing them.

**Rule files arrive first.** Since each endpoint reads its own copy of the rule
files, the endpoints can briefly disagree about them. So a cycle that has rule
files to deliver or remove does only that, on both endpoints, and every other
change waits for the next cycle. Content that the new rules ignore, such as a
`data/` directory that you prepared on a server, is thus neither copied back
nor deleted before the rules arrive.

**`one-way-replica` keeps exposed content.** A `one-way-replica` session would
otherwise remove or overwrite whatever beta has that differs from alpha. So it
keeps content on beta that beta's rules ignored until a change of rules exposed
it:

- the session removing a `.gitignore` that only beta has, or replacing beta's
  copy with alpha's,
- alpha removing a rule,
- an edit of beta's `.git/info/exclude`.

Beta reports a problem for that content, also after a restart, until it's
gone, identical to alpha's, or ignored again (such as in beta's
`.git/info/exclude`, or with `ignore.paths`). Content that beta creates
elsewhere is still removed.

**Unusable rule files.** If a rule file can't be written, the session reports
the problem, and nothing else synchronizes until it's fixed. If a rule file
can't be read, the session reports the problem and doesn't synchronize its
directory until it's fixed.


## Submodules

A directory other than the root whose `.git` belongs to a repository with a
commit checked out and an `origin` remote, such as a submodule, has its files
synchronized like any other directory's, uncommitted changes included. Its
`.git` isn't: branches, stashes, the index, and unpushed commits stay on the
endpoint that has them. Instead, the commit that it has checked out is
synchronized.

### Following commits

When one endpoint checks out another commit, the other follows by moving its
`HEAD` (detached) and its index to that commit, without touching any file. The
files then arrive like any other changes. A branch that the following endpoint
had checked out doesn't move.

If the following endpoint doesn't have the commit, it fetches it in the
background, from its own `origin` or else from the other endpoint's, without
holding up synchronization. While that fails, it retries at growing intervals,
from 10 seconds to 5 minutes. **So commits must be pushed before the other
endpoint can follow them.** Until it has the commit, its `HEAD` stays behind,
with a warning. Once it has the commit, a session that watches for changes
follows right away, and one that doesn't at its next flush.

`HEAD` also stays behind, with a warning, while a Git operation is in progress
there, such as a merge, a rebase, or a commit whose message is being edited. It
follows once the operation is finished or aborted.

### Missing submodules

An endpoint that doesn't have the submodule, or has an empty directory in its
place (as `git clone` without `--recurse-submodules` and `git submodule deinit`
leave), gets a clone from the `origin` URL of the other endpoint's copy, with
that commit checked out on a detached `HEAD`.

- If the commit isn't available there, the clone checks out the default branch,
  with a warning.
- The other endpoint's uncommitted changes and nested submodules arrive in the
  following cycles, which a flush waits for.
- Credentials aren't copied: the endpoint clones with its own.
- Clones and fetches only use `https`, `http`, `ssh` (including `host:path`),
  and `git` URLs. A submodule whose `origin` is a local path or a `file://` URL
  can't be cloned or fetched from, and the problem or warning says so
  (`transport 'file' not allowed`). If `GIT_ALLOW_PROTOCOL` is set in the
  environment of the daemon or agent, its list applies instead.

An endpoint that has the directory but no `.git` gets one attached, with the
other endpoint's commit, and its files are left as they are.

### Different commits

If both endpoints have checked out different commits since they last agreed,
merging their files path by path would mix two commits:

- In `two-way-safe` sessions, nothing in the submodule is synchronized, and
  `remote --conflicts` lists a conflict, until both have the same commit
  checked out.
- `one-way-safe` sessions synchronize its files, but list a conflict for its
  commit.
- `two-way-resolved` sessions follow alpha's commit and resolve files as usual.

Only Git resolves such a conflict: check out the same commit on both endpoints,
or delete the submodule on the endpoint whose commit and changes you want to
discard (beta, in `one-way-safe` sessions), which then gets a clone of the
other's commit.

Once both have the same commit, a file that Git considers changed from it
(modified, deleted, or untracked) on only one endpoint propagates to the other,
even if the commit changed it. Only a file that both have changed, or whose
bytes differ although Git considers both unchanged, is a conflict. The same
holds for a session that starts with both at the same commit.

### Deleting submodules

Deleting a submodule isn't synchronized. The endpoint that still has it reports
a problem; delete it there too.

### One-way modes

A submodule that only beta has stays there, including one opposite an empty
directory on alpha, and so does a `.git` that beta has in a directory that alpha
has without one. `one-way-replica`, which would otherwise delete them, reports a
problem on beta until you delete them there or alpha gets them too, as it does
for [exposed content](#changing-rules). `one-way-replica` overwrites uncommitted
changes in beta's submodules like any other content of beta's.

### Line endings and filters

Clones that Mutagen makes use the other endpoint's `core.autocrlf` and
`core.eol` settings, so that their files have the same bytes. Existing
checkouts with different line-ending settings differ in their text files and
conflict like any files that differ: pin line endings in `.gitattributes`, or
align the settings.

If a filter that Git requires (such as Git LFS without credentials) fails while
a clone is checked out, the clone gets the content stored in the repository
instead (such as LFS pointers), with a warning, and the other endpoint's files
arrive as changes.


## Nested repositories

Nested repositories other than submodules, those without commits or without an
`origin`, are synchronized as ordinary directories, without their `.git`.
`.gitmodules` is synchronized as an ordinary file and isn't interpreted.

Inside any nested repository, including a submodule, its own rules apply, as they do for Git run in it:
its `.gitignore` files and its exclude file. The rules above it only decide
whether the directory is ignored as a whole. So a root `.gitignore` with
`*.log` doesn't ignore `sub/debug.log` if `sub` has a `.git`. An endpoint where
the directory has no `.git` treats it as an ordinary directory, to which the
rules above it apply, and doesn't write what they exclude.

Only a `.git` that Git accepts gives a directory rules of its own. These
directories aren't synchronized until they're fixed, which is reported as a
problem:

- a directory whose `.git` Git can't use, such as an empty, damaged, or
  dangling one,
- a directory whose `.git` is a symbolic link (Git mode follows one only for
  the root),
- a directory whose `origin` URL the other endpoint couldn't use.


## Warnings

`mutagen sync list` reports what Git mode doesn't synchronize: tracked files
and incoming paths that rules ignore. What's left out by design is a warning
rather than a problem. `remote` prints it after each flush, under its own
heading (`this content isn't synchronized, by design`), without preventing
commands, and it doesn't affect what `remote --conflicts` says about
conflicts.


## Remote commands

`remote` follows the session's mode. It reads the rules from the tree on the
system where it runs:

- `remote --ignores` lists where the session's ignores come from, and
  `remote --ignores <path>...` says which of them ignores each path.
- `remote --pull` copies what rules ignore. If a rule file can't be read,
  `--pull` refuses, since it can't tell whether the session synchronizes the
  path.
- Directories that rules ignore require an explicit `--cwd`.

See [Remote commands](remote.md#ignored-paths).


## Requirements and limitations

- Rules follow Git 2.52 and later. Older versions of Git treat patterns like
  `foo**/bar` differently, so they can disagree with the session about them.
- Submodules require Git 2.29 or later on both endpoints. Without Git, a nested
  repository is reported as a problem rather than synchronized.
- If the other endpoint already has a directory without a `.gitignore`, the
  first scan hashes all of its content, but doesn't transfer it.
- While a `.gitignore` itself is in conflict, each endpoint applies its own
  copy, so neither writes content that its copy ignores.
- A flush waits for one further cycle after delivering rule files. If relaxing
  a rule exposes another rule file, the content behind it needs a second flush
  or the next filesystem event.
- If one endpoint deletes a directory and the other has ignored content in it,
  that's still a conflict, as without Git mode. `__pycache__` directories that
  commands run with `remote` create are a common case.
