# Mutagen

Mutagen is a new kind of remote development tool that enables your existing
local tools to work with code in remote environments like cloud servers and
containers. It does this by providing high-performance real-time
[file synchronization](https://mutagen.io/documentation/synchronization) and
flexible [network forwarding](https://mutagen.io/documentation/forwarding).
It supports synchronization and forwarding between
[local systems](https://mutagen.io/documentation/transports/local),
[SSH-accessible locations](https://mutagen.io/documentation/transports/ssh),
and [Docker containers](https://mutagen.io/documentation/transports/docker).


## Getting started

The best way to understand Mutagen and its features is to read the
[Overview](https://mutagen.io/documentation/introduction) and
[Getting started](https://mutagen.io/documentation/introduction/getting-started)
guides. You can find information about all of Mutagen's features in the
[documentation](https://mutagen.io/documentation).


## Applying project configuration

Use `mutagen project apply` to apply changes to `mutagen.yml` without restarting
unchanged sessions:

```sh
mutagen project apply --dry-run
mutagen project apply
```

The command starts the project if needed and matches existing sessions by type
and name within that project. It creates new sessions, terminates removed
sessions, and recreates sessions whose endpoints or merged configurations have
changed, including the `remote` and `remotePrologue` settings of
synchronization sessions, even though these only affect remote commands.
Unchanged sessions retain their identifiers, synchronization history, scan
caches, and paused state. Renaming a session removes the old session and
creates a new one. Docker environment variables such as `DOCKER_HOST` are not
compared: retained sessions keep the environment they were created with, while
new and replaced pair sessions use the current environment. In a star,
retained edges determine the center's endpoint, including for new or replaced
edges.

Replaced synchronization sessions, like sessions created under a new name,
start with fresh history and scan caches. Without a record of the last
synchronized state, they can't tell which endpoint changed a file:

- Files that differ between the endpoints become conflicts in `two-way-safe`
  (the default) and `one-way-safe` modes, and are overwritten with alpha's
  version in `two-way-resolved` and `one-way-replica` modes.
- Files that exist on only one endpoint are copied to the other, which undoes
  deletions that weren't synchronized, except that `one-way-safe` leaves files
  that exist only on beta in place and `one-way-replica` deletes them.

To protect changes that haven't been synchronized, `apply` flushes the running
synchronization sessions that it replaces or removes before terminating any
session, together with the running sessions that share their workspaces,
including retained ones, repeatedly, until changes have propagated across them.
A flush only makes its own session's endpoints agree, so a change that a later
flush brings into a shared workspace, such as a star's center, takes another
round to reach the other endpoints of sessions flushed before; otherwise, the
new sessions would copy back files whose deletion hadn't reached every
workspace. Sessions share a workspace if their endpoints are equal or nested on
the same configured host (aliases and remote symbolic links aren't recognized).
A group of sessions connected this way is flushed in D+1 rounds, where D is the
largest number of steps between two of them: two rounds for a star and k rounds
for a chain of k sessions, or k rounds for k sessions whose workspaces form a
cycle, since sessions that ignore a change or synchronize in one direction can
force it the long way around. A session that shares no workspace is flushed
once. If a flush fails, nothing is terminated or created: fix the session and
apply again, or, if its unsynchronized changes don't matter, terminate it with
`mutagen sync terminate` and apply again. A retained session can instead be
paused with `mutagen sync pause`, since it keeps its history. Changes that a
flush leaves unsynchronized, such as those blocked by problems, aren't
protected, and neither are changes made while `apply` runs, so stop processes
that modify synchronized files before replacing or renaming their sessions.
Paused sessions are neither flushed nor resumed, so changes made since a session
was paused may be overwritten (`two-way-resolved`, `one-way-replica`) or become
conflicts (`two-way-safe`, `one-way-safe`) once its replacement is resumed or a
session created under a new name synchronizes them. The plan notes such
sessions; resume them before applying to synchronize their changes first.

Use `-f path/to/project.yml` for another project file. Relative endpoint paths
are resolved against its directory. Global configuration is included unless
`--no-global-configuration` is specified; use this flag consistently with
`project start` and previous `apply` invocations. `--paused` pauses newly created
and replacement sessions without changing retained sessions. Replacements also
preserve the previous session's paused state.

Creation hooks run only when starting a project. Applying changes to an existing
project does not run lifecycle hooks. `flushOnCreate` applies only to newly
created, unpaused sessions. Changes are not transactional: if an operation fails,
completed changes remain and another `apply` reconciles the remaining sessions.
Startup hooks are not replayed when retrying an existing project. The command
runs once; it does not watch the YAML file for changes.


## Installation

You can find
[installation instructions](https://mutagen.io/documentation/introduction/installation)
in the [Mutagen documentation](https://mutagen.io/documentation).

Releases of this fork, which include `remote`, are available on its
[releases page](https://github.com/zlcwwhq/mutagen/releases). Each platform's
bundle contains `mutagen` and `remote`. The agents that `mutagen` installs on
endpoints are released once for all platforms, as
`mutagen-agents_v<version>.tar.gz`: save it as `mutagen-agents.tar.gz` in the
same directory as `mutagen`, or in `../libexec` if `mutagen` is in a `bin`
directory. Replace it whenever you upgrade `mutagen`, which refuses to install
agents from another release's agent bundle. Releases up to
v0.19.0-zlcwwhq.0.0.7 include `mutagen-agents.tar.gz` in each bundle instead.
Only the daemon's system needs an installation. The `mutagen-setup` agent skill
(see below) automates this.


## Git mode

Git mode makes a session follow the conventions of Git working trees, so that
`.gitignore` files decide what's synchronized instead of `ignore.paths` that
repeat them, and `.git` entries are never synchronized. It's disabled by
default. Enable it with `git: enabled` for a session, under `defaults`, in
global configuration, or under `star.sync`, or with `--git` for `mutagen sync
create`:

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

It applies to the whole session, so it can't be set for one endpoint, and stars
inherit it like other synchronization settings: `star.sync` or the defaults
apply it to every edge, while nodes can't set it individually. Git mode implies
`ignore.vcs: true` and is rejected together with a request to propagate VCS
directories (`--no-ignore-vcs`, or `ignore.vcs: false`). Changing it in a
project recreates the session when you apply the project, like other
configuration changes. `mutagen sync list -l` shows the mode.

**Ignores.** The session ignores what Git would: the rules of the `.gitignore`
files at every level beneath the synchronization root, and of the root
repository's `.git/info/exclude`, while nested repositories follow their own
rules (see below). Global excludes (`core.excludesFile`) and
directories above the root aren't read, and rules always match
case-sensitively, so that both endpoints agree. Each endpoint reads the files
in its own copy of the tree, and a scan reads them anew, so changes to them
apply at the next scan. `.git/info/exclude` is inside `.git`, which is never
synchronized, so it only applies on endpoints that have the repository: put
ignores that both endpoints share in `.gitignore`. `ignore.paths` takes
precedence over the files: a pattern such as `!.env` synchronizes a file that a
rule ignores, and a rule can't unignore what a pattern ignores. Entries named
`.git`, files (as used by submodules and worktrees) as well as directories, are
never synchronized, and no pattern can unignore them (a submodule's commit is
synchronized instead, see below). The same goes for names that Windows or macOS
treat as `.git`, such as `.git.` and `git~1`, which Git refuses to track. Files
that Git tracks but rules ignore aren't synchronized either: the endpoint that
has the repository reports a problem that suggests the `ignore.paths` pattern
to unignore them (`!path`, or `!directory` for a file inside an ignored
directory).

A rule change can't delete files. If a rule starts to ignore files that were
synchronized, they stay on both endpoints, and the session stops tracking them,
and removing the rule resumes synchronizing them. Since each endpoint reads its
own copy of the rule files, endpoints can briefly disagree about them. So a
cycle that has rule files to deliver or remove does only that, on both
endpoints, and every other change waits for the next cycle. Content that the new
rules ignore, such as a `data/` directory that you prepared on a server, is thus
neither copied back nor deleted before the rules arrive. A `one-way-replica`
session would remove or overwrite whatever beta has that differs from alpha's,
so it keeps content on beta that beta's rules ignored until a change of rules
exposed it: the session removing a `.gitignore` that only beta has, or
replacing beta's copy with alpha's, alpha removing a rule, or an edit of beta's
`.git/info/exclude`. Beta reports a problem for that content, also after a
restart, until it's gone, identical to alpha's, or ignored again (such as in
beta's `.git/info/exclude`, or with `ignore.paths`), while content that beta
creates elsewhere is still removed. If a rule file can't be written, the
session reports the problem, and nothing else synchronizes until it's fixed. If
a rule file can't be read, then the session reports the problem and doesn't
synchronize its directory until it's fixed.

An endpoint also applies its own rules to what it receives, including paths
that it doesn't have yet, so a `.git/info/exclude` on one endpoint keeps the
other endpoint's copies of what it excludes away. An excluded path isn't written
and is reported as a warning, while the rest of its directory still is written:
if `*.log` excludes `pkg/debug.log`, then `pkg/a.go` still arrives. Rule files
themselves are still delivered, even into an excluded directory. If the endpoint
already has the excluded path, then there's nothing to warn about, since the
session simply doesn't track it. A pattern such as `!pkg/debug.log` in
`ignore.paths` overrides the exclusion.

**Submodules.** A directory other than the root whose `.git` entry belongs to a
repository with a commit checked out and an `origin` remote, such as a
submodule, has its files synchronized like any other directory's, uncommitted
changes included, but not its `.git`: branches, stashes, the index, and commits
that aren't pushed stay on the endpoint that has them. Instead, the commit that
it has checked out is synchronized:

- When one endpoint checks out another commit, the other follows by moving its
  `HEAD`, detached, and its index to that commit without touching any file, and
  the files arrive like any other changes. A branch that it had checked out
  doesn't move. If it doesn't have the commit, it fetches it in the
  background, from its own `origin` or else from the other endpoint's, without
  holding up synchronization, and retries at growing intervals (from 10 seconds
  to 5 minutes) while that fails, so commits have to be pushed before the other
  endpoint can follow them. Until it has the commit, its `HEAD` stays behind,
  which is reported as a warning. Once it has the commit, a session that
  watches for changes follows right away, and one that doesn't at its next
  flush. `HEAD` also stays behind, with a warning, while a Git operation is in
  progress there, such as a merge, a rebase, or a commit whose message is
  being edited, and follows once it's finished or aborted.
- An endpoint that doesn't have the submodule, or has an empty directory in its
  place (as `git clone` without `--recurse-submodules` and `git submodule
  deinit` leave), gets a clone from the `origin` URL of the other endpoint's
  copy, with that commit checked out on a detached `HEAD`. If the commit isn't
  available there, the clone checks out the default branch, with a warning. The
  other endpoint's uncommitted changes and nested submodules arrive in the
  following cycles, which a flush waits for. Credentials aren't copied: the
  endpoint clones with its own. Clones and fetches only use `https`, `http`,
  `ssh` (including `host:path`), and `git` URLs, so a submodule whose `origin`
  is a local path or a `file://` URL can't be cloned or fetched from, and the
  problem or warning says so (`transport 'file' not allowed`). If
  `GIT_ALLOW_PROTOCOL` is set in the environment of the daemon or agent, then
  its list applies instead.
- An endpoint that has the directory but no `.git` gets one attached, with the
  other endpoint's commit, and its files are left as they are.
- If both endpoints have checked out different commits since they last agreed,
  then merging their files path by path would mix two commits. So, in
  `two-way-safe` sessions, nothing in the submodule is synchronized, and
  `remote --conflicts` lists a conflict, until both have the same commit checked
  out. Deleting the submodule on one endpoint also resolves it: that endpoint's
  commit and changes are discarded, and it gets a clone of the other's commit.
  `two-way-resolved` sessions follow alpha's commit and resolve files as usual.
- Deleting a submodule isn't synchronized: the endpoint that still has it
  reports a problem, and you delete it there too.
- In one-way modes, a submodule that only beta has stays there, including one
  opposite an empty directory on alpha, and so does a `.git` that beta has in a
  directory that alpha has without one. `one-way-replica`, which would otherwise
  delete them, reports a problem on beta until you delete them there or alpha
  gets them too, as it does for content that a change of rules exposes on beta
  (see above). `one-way-replica` overwrites uncommitted changes in beta's
  submodules like any other content of beta's.

Clones that Mutagen makes use the other endpoint's `core.autocrlf` and
`core.eol` settings, so that their files have the same bytes. Existing
checkouts with different line ending settings differ in their text files and
conflict like any files that differ: pin line endings in `.gitattributes`, or
align the settings. If a filter that Git requires (such as that of Git LFS
without credentials) fails while a clone is checked out, then the clone has the
content stored in the repository instead (such as LFS pointers), with a
warning, and the other endpoint's files arrive as changes.

**Nested repositories.** Other nested repositories, those without commits or
without an `origin`, are synchronized as ordinary directories, without their
`.git`. `.gitmodules` is synchronized as an ordinary file and not interpreted.
Inside a nested repository, its own rules apply, as they do for Git run in it:
its `.gitignore` files and its exclude file, but not the rules above it, which
only decide whether the directory is ignored as a whole. So a root `.gitignore`
with `*.log` doesn't ignore `sub/debug.log` if `sub` has a `.git`. An endpoint
where the directory has no `.git` treats it as an ordinary directory, to which
the rules above it apply, and doesn't write what they exclude (see above). Only
a `.git` entry that Git accepts gives a directory rules of its own. A directory
whose `.git` entry Git can't use (such as an empty, damaged, or dangling one),
whose `.git` is a symbolic link (which Git mode follows only for the root), or
whose `origin` URL the other endpoint couldn't use, isn't synchronized until
that's fixed, which is reported as a problem.

**Warnings.** What Git mode doesn't synchronize is reported by
`mutagen sync list`, and `remote` prints it after each flush without preventing
commands: tracked files or incoming paths that rules ignore (see above). What's
left out by design is listed as a warning rather than a problem, which `remote`
reports under its own heading (`this content isn't synchronized, by design`) and
which doesn't affect what `remote --conflicts` says about conflicts.

**Requirements and limitations.**

- If the other endpoint already has a directory without a `.gitignore`, the
  first scan hashes all of its content, but doesn't transfer it. While a
  `.gitignore` itself is in conflict, each endpoint applies its own copy, so
  neither writes content that its copy ignores.
- Rules follow Git 2.52 and later; older versions of Git treat patterns like
  `foo**/bar` differently, so they can disagree with the session about them.
- Submodules require Git 2.29 or later on both endpoints. Without Git, a nested
  repository is reported as a problem rather than synchronized.
- A flush waits for one further cycle after delivering rule files. If relaxing a
  rule exposes another rule file, the content behind it needs a second flush or
  the next filesystem event.
- If one endpoint deletes a directory and the other has ignored content in it,
  that's still a conflict, as in sessions without Git mode, such as
  `__pycache__` directories that commands run with `remote` create.
- To migrate a session that used `ignore.vcs: true`, replace it with
  `git: enabled`, remove the `ignore.paths` patterns that repeat `.gitignore`
  files, and apply the project.

`remote` follows the session's mode. It reads the rules from the tree on the
system where it runs, so `remote --ignores` lists where the session's ignores
come from, and `remote --ignores <path>...` says which of them ignores a path;
`--pull` copies what rules ignore. Directories that rules ignore require an
explicit `--cwd`. If a rule file can't be read, then `--pull` refuses, since it
can't tell whether the session synchronizes the path.

## Star workspaces

A project can declare one star of directory workspaces, managed by one daemon:
a center node joined to every other node by its own synchronization session.

```yaml
star:
  remote: true
  sync:
    ignore:
      paths: ["/mutagen.yml.lock", ".venv/", "__pycache__/"]
  center: local
  nodes:
    local:
      endpoint: "."
    server:
      endpoint: "server:~/project"
    gpu:
      endpoint: "gpu:~/project"
      mode: one-way-safe
      remotePrologue: "~/.config/gpu-prologue.sh"
```

Apply it with `mutagen project apply --dry-run` and `mutagen project apply`.
Each node identifies one system and directory. `center` is required and names
one of the nodes, and there must be at least one other node. Names start with
an ASCII letter, contain letters, digits, or dashes, end with a letter or digit,
and have at most 63 characters; `defaults` is reserved. The star expands into
one ordinary synchronization session for each node other than the center, named
`star-<node>`, whose alpha is the center and whose beta is the node; `mutagen
project list` displays them. Each session records its membership (star, center,
and node) as a dedicated field rather than in labels, so it survives daemon
restarts and can't be set or changed with `mutagen sync create --label`;
`mutagen sync list -l` shows it. Unchanged edges retain their history when
applying configuration changes. New or replaced edges reuse the center's
endpoint from the star's retained edges, including its captured Docker
environment, so that all edges still identify the same center.

Any node can be the center, with any type of endpoint, but only the center
reaches every node, so put it where you usually work, such as on the system
where your agents run. Mutagen relays every session's data through the daemon,
so every endpoint must be reachable from the daemon, and endpoints needn't reach
each other. When the center isn't on the daemon's system, the daemon relays its
edges as it relays a pair whose endpoints are both remote. Either way, each
command crosses a single edge, without intermediate copies.

Settings under `star.sync` extend global and project synchronization defaults,
including ignores and [Git mode](#git-mode), and apply to every edge. A node's `mode` sets the
synchronization mode of its own edge (`two-way-safe`, `two-way-resolved`,
`one-way-safe`, or `one-way-replica`), taking precedence over `star.sync.mode`
and the defaults; edges use `two-way-safe` if nothing selects a mode. The
center is alpha on every edge, so it wins conflicts in `two-way-resolved` mode,
and one-way modes flow from the center to the node: changes made on such a node,
including files that commands write there, don't reach the center. `mode` can't
be set on the center. Pair-specific `configurationAlpha` and `configurationBeta`
defaults don't apply. Configure prologues on nodes, including the center:
`remotePrologue` in global or project synchronization defaults doesn't apply to
star nodes.

The nodes of a star must not overlap one another, including the center: the
daemon refuses star edges whose nodes would overlap, and `mutagen project start`
and `apply` refuse such declarations. They also refuse sessions declared in the
same project file that overlap star workspaces. Sessions created outside the
project aren't checked, and overlapping them behaves like overlapping pairs:
the most specific workspace containing a directory is the source for `remote`,
workspaces with the same root are ambiguous, and changes propagate through both
sessions. Detection compares configured hosts and paths and can't see through
host aliases or remote symbolic links, so don't use them to give one workspace
multiple identities.

`star.remote: true` explicitly permits commands between the center and every
other node. Its default is false, regardless of `sync.defaults.remote`. Select
a node by name:

```sh
remote --nodes
remote --nodes --json
remote --node gpu pytest -x                    # from the center
remote --node server --timeout 2m make test    # from the center
remote make test                               # from server/gpu: on the center
```

From the center, `--node` selects any node, and can only be omitted if the star
has a single other node. From any other node, the center is the only
destination, so `--node` can be omitted. Selecting another node there fails
with status 125: stars have no multi-hop routes, so run the command from the
center. Every command crosses exactly one edge, which is an ordinary pair
session, and follows the same rules as for a pair (see below): the edge is
flushed before and after commands, conflicts produce warnings rather than
preventing commands, and detached jobs, `--no-flush`, `--flush`, `--pull`,
`--diff`, `--ignores`, and conflict resolution work as they do for pairs.
An operation depends only on its own edge: pausing, replacing, removing, or
adding other nodes doesn't affect it, and no operation uses another edge. As
for a pair, the edge must be connected for commands at both of its ends: while
it isn't connected at the center, such as while it reconnects, the node is
unavailable, and operations on it from the center fail with status 255, so
retry once it reconnects. Commands use the corresponding relative directory;
`--cwd` overrides it. Repeat `--node` for later operations on the same node,
such as `--wait` and `--kill` for a detached job; the commands that `remote`
suggests, such as when it starts a job, include it.
`MUTAGEN_REMOTE_NODE` identifies the executing node; use it in prologues shared
by several nodes instead of `MUTAGEN_REMOTE_ENDPOINT`, since the center is alpha
on every edge and every other node is beta.

`remote --nodes` lists the nodes that the current node can reach: from the
center, every node, including paused or disconnected ones, and from another
node, itself and the center. It marks the current node and the center, and shows
each node's host and root, the mode of the edge to it, and its availability: a
node is available while it executes commands, as the remote setting of the edge
to it permits, and that edge is connected for commands at both ends. Operations
on a node that doesn't execute commands fail with status 125. `--nodes --json`
provides machine-readable output. Discovery
neither synchronizes files nor executes commands; available doesn't imply
conflict-free. Terminating an edge only removes its node. The daemon refuses
star edges that disagree with the star's existing edges on the center or its
endpoint, or whose node already has an edge. If the sessions that it loads
disagree on the center's endpoint anyway, `remote --nodes` reports the star
problem, and new operations from the center fail with status 125 until the star
is repaired, such as by terminating the edge that disagrees and applying the
project again, while operations already in progress and other nodes keep using
their own edges. `remote --conflicts --json` reports an `edges` array with a
single entry, which names the edge's `alpha` and `beta` nodes and the listing's
`source` and `target`, and contains the `listing`. `remote --help nodes`
summarizes target selection.

Earlier releases declared workspace trees under `tree:`, which is no longer
accepted: `mutagen project start` and `apply` refuse project files that
contain it. To migrate a tree to a star:

- Replace `tree:` with `star:`, remove each node's `parent`, and name a node,
  such as the former root, with `center:`.
- Apply the project. `mutagen project apply` removes the former `tree-<node>`
  sessions, flushing running ones first, and creates `star-<node>` sessions,
  which start with fresh synchronization history.
- The migration relies on `apply` flushing the tree's sessions together,
  repeatedly, until changes such as deletions have reached every node (see
  [Applying project configuration](#applying-project-configuration)). Paused
  sessions aren't flushed, so resume paused `tree-<node>` sessions first.
- Commands between two nodes other than the center no longer exist; run them
  from the center.
- Star commands follow pair semantics: conflicts produce warnings instead of
  preventing commands, and no command synchronizes several edges.


## Remote commands

The `remote` executable selects a destination **node**, a workspace on a
system. A standalone synchronization pair is exposed as two nodes named `alpha`
and `beta`; a declared star uses its configured names. `remote --nodes` lists
the nodes that can be reached, and `remote --node NAME` selects a target. When
there is exactly one other node, the name can be omitted. Commands run in
the corresponding directory and use the daemon's agent connections, so the
daemon can run on a third system and endpoints needn't reach each other.

Remote commands are enabled per session with the `remote` configuration
parameter, whose value names the endpoints that execute commands: `true`
(both), `alpha`, `beta`, or `false` (the default). It can be specified with the
`--remote` flag of `mutagen sync create`, under a session (or `defaults`) in
`mutagen.yml` project files, or under `sync.defaults` in global configuration:

```yaml
sync:
  code:
    alpha: "."
    beta: "server:~/project"
    remote: true
```

```sh
mutagen sync create --remote=true ~/project server:~/project
cd ~/project/service
remote go test ./...                    # runs in ~/project/service on server
remote -e NODE_ENV=test --env-file .env.remote npm test
remote --timeout 2m sh -c 'make build && make test'
remote --script - <<'EOF'               # shell code as written, run with bash
for p in a b; do python run.py --preset "$p"; done
EOF
id=$(remote --detach --log train.log python train.py)   # a detached job
remote --wait "$id"                     # waits for it, then synchronizes
```

On the daemon's system, `remote` is installed alongside `mutagen`. On SSH and
Docker endpoints, the agent installs it next to itself as a link to itself (a
symbolic link, or a hard link on Windows), normally at
`~/.mutagen/agents/<version>/remote`. Invoke it by
that path, or put that directory on `PATH`; links to it must also be named
`remote`.

The current directory identifies the most specific connected source workspace.
The edges of a star are one source at its center. Independent pairs sharing
the same root are ambiguous: declare them as one star instead of relying on
creation order. Earlier versions used the session created first; such setups
now fail with `ambiguous source node` until they're declared as a star, such
as with the shared directory as the center and each machine as a node. Outside
every root, an explicit `--node` (or `--nodes`) is
accepted only if one source workspace is unambiguous on this system. Commands
then use the destination root, or `--cwd`, and skip its prologue. This also
allows file operations on an implicit pair whose roots are single files.

**`--session`, `-s`, `--sessions`, and `MUTAGEN_REMOTE_SESSION` are deprecated
and rejected with status 125.** Unset the environment variable and use
`--nodes` and `--node NAME`. They are not aliases. Sessions remain the daemon's
internal synchronization edges, managed through `mutagen sync` and projects.

Ignored directories require an explicit `--cwd`; their files still aren't
synchronized. In [Git mode](#git-mode), that includes directories that
`.gitignore` rules ignore. `remote --ignores` inspects the source's ignore rules.
Each call pins its selected target and doesn't switch to another node after a
failure.

`--cwd` overrides the working directory (relative values are resolved against
the corresponding directory, or against the opposite endpoint's root when
submitting from outside the session's root). `-e NAME=VALUE` sets a variable,
`-e NAME` forwards a local one, and `--env-file` loads literal `NAME=VALUE`
lines. Quote home paths, such as `remote -C '~/project' pwd`, so your shell
doesn't expand `~` into the local home. If an explicit absolute path under your
local home is missing on the executing side, the error suggests this spelling
conditionally; it never rewrites the path. Otherwise, commands inherit the
environment of the agent (or, on the daemon's system, of the daemon).

Every command receives `MUTAGEN_REMOTE_NODE`, the executing node name (also
`alpha` or `beta` for an implicit pair). `MUTAGEN_REMOTE_ENDPOINT=alpha` or
`beta` additionally identifies its role on the underlying synchronization edge. This value overrides inherited values and
those submitted with `-e` or `--env-file`, including when prologues are skipped.
Detached jobs also receive `MUTAGEN_REMOTE_JOB`, set to their ID.

The `remotePrologue` configuration parameter (or the `--remote-prologue` flag of
`mutagen sync create`) names a POSIX shell script that the executing endpoint
sources before each command, such as one that activates a conda environment.
Since paths usually differ between systems, it can also be specified per
endpoint (under `configurationAlpha` or `configurationBeta`, or with
`--remote-prologue-alpha` and `--remote-prologue-beta`). Absolute and `~` paths
are interpreted on the executing endpoint's system (so quote `~` in flags to
prevent your shell from expanding it locally), while other paths are resolved
against its synchronization root:

```yaml
sync:
  code:
    alpha: "."
    beta: "server:~/project"
    remote: beta
    configurationBeta:
      remotePrologue: "~/.config/remote-prologue.sh"
```

```sh
# ~/.config/remote-prologue.sh on server (sourced by /bin/sh)
eval "$("$HOME/miniconda3/bin/conda" shell.posix hook)"
conda activate "${CONDA_ENV:-base}"
```

To configure initialization directly in the workspace, create
`.remote-prologue.sh` at the synchronization root. The executing endpoint checks
for this file before each command and uses it instead of its configured
`remotePrologue`. The script needs no executable permission. Edits take effect
without restarting the session, an empty file performs no initialization, and
deleting the file restores the configured default. Only the root is checked;
scripts in subdirectories or the directory specified by `--cwd` aren't used.

The same file can configure both endpoints using `MUTAGEN_REMOTE_ENDPOINT`:

```sh
# .remote-prologue.sh at the synchronization root (sourced by /bin/sh)
case "$MUTAGEN_REMOTE_ENDPOINT" in
  alpha)
    . "$HOME/venvs/local/bin/activate"
    ;;
  beta)
    eval "$("$HOME/miniconda3/bin/conda" shell.posix hook)"
    conda activate "${CONDA_ENV:-gpu}"
    ;;
esac
```

For example, `remote pytest` submitted on alpha selects the beta branch. Shared
initialization can go outside the `case`. A branch that does nothing still
replaces the configured prologue; scripts aren't automatically combined.
The override follows normal synchronization rules: the default flush before a
command transfers local edits, but ignored files, synchronization failures, or
`--no-flush` can leave the executing endpoint using an older copy or its
configured default.

The prologue applies to commands submitted from inside the session root,
including those using `--cwd`, unless `--no-prologue` is specified; commands
submitted from outside the roots with explicit `--node` don't use it. These rules apply
to both workspace overrides and configured prologues. It sees variables
set with `-e` and `--env-file`, and the command is located (using the `PATH` it
sets) only after it has been sourced, so `remote -e CONDA_ENV=test pytest` works
even if `pytest` exists only in that environment. If the prologue can't be
loaded, fails, or exits the shell, then the command isn't run and `remote`
exits with status 126. An invalid override doesn't fall back to the configured
script; use `--no-prologue` to run commands while repairing it.
Prologues aren't supported on Windows endpoints.

Options must precede the command, and arguments are passed without shell
interpretation, so shell syntax requires an explicit shell or `--script`.
Standard streams and the command or job's own exit status are preserved.
Remote's own argument, input, or operation failures return 125; connection
setup, lost connections, protocol failures, and an unreachable node return
255. An unknown node, a node that only a star's center reaches, or a star
problem returns 125. Read the `remote:`
diagnostic before retrying a 125 failure. Commands may themselves return 125
or 255, so exit status alone cannot always identify the source. Timeouts remain
124, startup/working-directory/prologue failures 126, and missing commands 127;
signals use 128+N. `--diff` and `--ignores` use 1 for their results and 125/255
for failures. In batches, interruption or timeout takes precedence, followed by
connection failures, then operation failures. This changes scripts that used
255 for all failures of remote itself. No pseudo-terminal is allocated.
Commands can only be routed while the session is connected, and the daemon
connects command channels in the background, so SSH authentication must work
non-interactively. `remote --help` summarizes usage, and `remote --help
<topic>` covers nodes, synchronization, scripts, detached jobs, environment
setup, termination, and conflicts in detail.

Programs may buffer output without a terminal. For immediate Python output,
use `remote -e PYTHONUNBUFFERED=1 python train.py` or
`remote python -u train.py`. For programs using glibc stdio,
`remote stdbuf -oL <command>` requests line buffering; it doesn't affect
programs that manage their own buffers. Buffering and colors are independent:
use the target program's own force-color option when colors are wanted.
See `remote --help environment` for these examples and environment setup.

`remote --script <file>` runs a local script, or one read from standard input
with `-`, such as a here-document whose quoted delimiter (`<<'EOF'`) keeps the
local shell from expanding anything in it. The executing endpoint copies it to
a temporary file and runs it with the interpreter named by its `#!` line, or
with `bash` if it has none, which is looked up after the prologue is sourced.
The arguments after the file are the script's, even ones that look like
options. A script read from standard input leaves its commands no input.
Scripts are limited to 512 KiB and aren't supported on Windows endpoints.

`remote` flushes the session that the command crosses, such as a star's edge,
as `mutagen sync flush` does: before the command, so that it sees local
changes, and after it, so that its changes are present locally when `remote`
exits. This works from either endpoint, since the daemon performs the flush.
If flushing fails, then `remote` prints a warning and continues with the
command's exit status; `--no-flush` skips both flushes, and `remote --flush`
only flushes. `--timeout` takes seconds, or a duration with a
unit such as `10m`, and includes the time spent flushing. Skipping the flush
before a command can silently run outdated code, while an unneeded flush only
costs a rescan of both roots, so use `--no-flush` only for commands that
neither read nor write synchronized files. `--detach` flushes only before
starting a job and `--wait` only after it ends, while `--jobs`, `--kill`,
`--ignores`, and `--pull` don't flush. `--kill` accepts `--no-flush`, which
has no effect.

Flushes that take over 3 seconds show their progress on standard error,
following git's convention: on a terminal, as a line updated in place, and
elsewhere only with `--progress`, as separate `remote:` lines printed when the
phase changes and every 10 seconds, which suits commands run in the background
or under a line-based monitor; `--progress=false` disables it. This applies to
every flush, including those of `--wait`, `--diff`, and the conflict
operations.
A final line summarizes where the time went, such as
`remote: synchronized in 44s: scanning 31s; staging on alpha (local) 12s, 1 file, 5.0 MiB`:
scanning both roots (a flush rescans them in full), staging changed files on an
endpoint (transferring their contents to it; the size is approximate),
applying changes, and so on. Phases can repeat if the session was already
synchronizing when the flush was requested. If the daemon doesn't report
progress, `remote` shows only the elapsed time.

Commands are terminated when `--timeout` expires, when `remote` is interrupted,
and when the session stops synchronizing, such as when it's paused, reconnects
after an error, or is replaced by `mutagen project apply`, or when the daemon
stops. The executing endpoint sends SIGTERM to the command's process group, so
that processes can exit cleanly, and SIGKILL 5 seconds later to any that remain.
(On Windows endpoints, the command is killed immediately, without its child
processes.) When the session stops synchronizing, `remote` reports that the
connection to the executing endpoint was lost and exits with status 255 without
synchronizing afterwards; files that the command was writing may be left
incomplete, and are synchronized as such once synchronization resumes. Commands
that must outlive these events can run as detached jobs.

`remote --detach` starts the command (or script) as a detached job, which keeps
running when `remote` exits and when the session stops synchronizing, and
prints its ID. Jobs run in the directory and environment that commands would,
in a new session and process group, without input, and write their output to
the file given with `--log` (relative to the command's directory), which must
be a regular file (truncated first) or a character device such as `/dev/null`,
not a FIFO, or to a log kept with the job's state in the executing endpoint's
Mutagen data directory.
`remote --jobs` lists the jobs started at the session's root, `remote --wait
<id>` waits for a job to end, flushes, and exits with its status (`--timeout`
stops waiting without affecting the job), and `remote --kill <id>` sends
SIGTERM to its process group, and SIGKILL 5 seconds later to any processes that
remain, even if `remote --kill` is interrupted meanwhile; repeating it while
the job is being terminated doesn't send SIGTERM again. A job ends when its
command exits: processes that the command leaves running in the background
aren't part of the job afterwards, so `--wait` doesn't wait for them and
`--kill` doesn't terminate them, and scripts that start background processes
should `wait` for them. On Linux endpoints, `--jobs`, `--wait`, and `--kill`
list those that remain in an ended job's process group, which `remote -- kill
<pid>...` terminates; on other endpoints, they only report that the group may
still have processes. A job whose status couldn't be recorded, such as because
it was killed with SIGKILL or its system restarted, is reported as lost
(`--wait` returns 125). Job state is kept for 7 days after jobs end. Jobs end
with the system or container that runs them, and can end with the processes
that run the endpoint, such as when a daemon running as a systemd service is
stopped, or, on systems where logind's `KillUserProcesses` is enabled, with the
SSH connection. Detached jobs aren't supported on Windows or AIX endpoints.

`remote --wait <id> --tail N` also prints the final N log lines to standard
output, including logs outside the root or in ignored paths, once the job ends,
or, if `--timeout` expires while the job runs, before reporting that it's still
running and exiting with status 124, so that `remote --wait <id> --tail N
--timeout T` checks on a job for at most T. To receive the tail in time, such a
wait ends up to a second before the timeout (half of the time left for timeouts
under two seconds). Zero disables output, and negative counts are invalid.
Output is limited to the final 1 MiB, with a warning if the requested lines are
cut (possibly within a line). Missing, unreadable, or non-regular logs, such as
`/dev/null`, cause a warning without changing the job's exit status. Reading
the log runs no commands or prologues; `--no-flush` still skips
synchronization.

`remote --wait <id> --follow` prints the job's log to standard output as it's
written, unchanged, from its beginning, or from its final N lines with
`--tail N` (`--tail 0` prints only new output), and exits as `--wait` does once
the job ends and the rest of its output has been printed; what processes that
the job left running write afterward isn't printed. The executing endpoint
checks the log for new output every 100 ms. If the connection is lost,
`remote` resumes following where it stopped. It warns if the log becomes
shorter, such as when it's truncated, and then follows it from its beginning,
and if another file has replaced the log when it resumes, which it doesn't
follow. Logs that can't be followed cause a warning without changing the job's
exit status. Executing endpoints of earlier versions support neither following
nor tails when `--timeout` expires: `remote` then warns and waits as before,
printing the final N lines with `--tail N` once the job ends.

Content that commands write to ignored paths exists only on the executing
endpoint. `remote --ignores <path>...` shows whether the session ignores each
path. Missing local paths are marked `not present locally`, with conditional
results for a file or directory; access errors are reported separately.
`remote --pull <path>...` copies ignored files, directories, and symbolic links
from the target node, replacing local files that differ but never deleting
local content. The target is selected as for commands before anything is
copied, so a pull that names an unknown or unreachable node, or omits a node
where several are reachable, copies nothing. It refuses paths that the session
(or another session) synchronizes, and directories with synchronized content
beneath them. It never writes inside a `.git` entry, and only adds one where
none exists locally, since a repository's metadata can't be merged file by file
(both endpoints can have their own copy of a submodule's). Other sessions are recognized whether or not they run remote
commands, but only while their endpoints on this system are connected and run
by this version of Mutagen or a later one: `--pull` can't tell whether paused
or reconnecting sessions, or sessions of earlier versions, synchronize a path.
Both options count a path as ignored only if the session ignores it in every
Unicode normalization form, and, if symbolic links among its local parents lead
elsewhere in the root, only if the session also ignores where they lead.
`--pull` only follows links that the session ignores, and never one at the path
itself, which a pulled file or link replaces. Regular files, including files
inside pulled directories, retain the executing copy's modification time,
truncated to whole seconds, so a copy is never newer than its source. Clock
skew between endpoints is preserved. Directory and symbolic-link timestamps,
atime, and ctime aren't preserved. Added and replaced files get default
permissions, executable if the remote copy is. Files with identical contents
but different mtime are atomically replaced, keeping their local permissions
(but not hard links, owner, or extended attributes), and counted as
`metadata updated`; another pull leaves matching files unchanged.

In [Git mode](#git-mode), `--ignores` also lists where the session's rules come
from (`.gitignore` files and `.git/info/exclude`, which `remote` reads from the
tree on its own system) and says which of them ignores each path. `--pull`
copies content that they ignore.

After each flush, `remote` also warns about content that the session couldn't
synchronize: conflicts (paths modified on both endpoints, which aren't
synchronized until resolved) and problems such as permission errors.
`remote --diff` shows how the remote copies of conflicting files, or of the
specified paths, differ from the local copies as a unified diff, and exits with
status 1 if any differ. Single-file pairs are supported: use explicit `--node`
from outside the file root. When one endpoint has replaced a directory with a
file, its former children are compared as missing entries. Large conflict
reports may omit change details; warnings give the omitted counts, and explicit
paths can still be compared with `remote --diff <path>`. `--timeout` also bounds
reading and hashing files during comparison.

In two-way-safe and one-way-safe sessions, `remote` can also resolve
conflicts:

```sh
remote --conflict-apply --take local src/app.go   # keep the submitting copy
remote --conflict-apply --take remote src/app.go  # keep the opposite copy

# Or merge both copies manually:
remote --conflicts                    # list actual changes and checkout state
remote --conflict-checkout src/app.go # download both copies into a workspace
# Edit the printed merged file, resolving its conflict markers.
remote --conflict-apply src/app.go    # write it to both copies and verify
```

`--take local|remote` automatically checks out an unclaimed conflict when no
workspace exists, then applies and verifies it in one invocation. Local and
remote are relative to the submitting endpoint. Existing workspaces retain
their original copies and validation; another endpoint's checkout is refused.
A side that can't be taken, or `--executable` for copies that aren't both
files, is refused before anything is checked out. Plain merged-file apply and
`--delete` require an explicit checkout. Text listings describe at most five
changes per side for each conflict and count the rest; `--json` includes more.
`remote --conflicts` exits with status 0 whenever it produces a listing. With
nothing to list, it prints `No conflicts.`, unless content couldn't be
synchronized, which can hide conflicts: it then warns about that content and
says that conflicts can't be ruled out.

Stop writers on both endpoints until apply finishes. A checkout belongs to one
endpoint, and an existing workspace may be another person's work; only apply
or discard checkouts that you made. Apply refuses copies changed since checkout,
but concurrent edits while it writes can be overwritten. Review the entire
merged file, even when it has no conflict markers. Directories require manual
resolution; other supported types can be resolved with `--take local|remote`.

Failed apply retains its workspace for retry, including partially applied
results. Successful apply and discard keep the original copies as backups.
See `remote --help conflicts` for the short workflow and
`remote --help conflict-details` for merge limitations, retention, and recovery
when copies change or an operation is interrupted. Conflict operations accept
several paths and `--json` for machine-readable results.

Any process running as your user on a submitting endpoint's system can run
commands on the executing endpoint, so only enable remote commands toward
endpoints whose systems you trust as much as the executing endpoint itself.

The [mutagen-remote](https://github.com/zlcwwhq/mutagen-remote-skill) agent
skill teaches coding agents to use `remote`, covering synchronization, quoting,
prologues, exit statuses, long-running jobs, and conflicts. Its plugin also
includes the `mutagen-setup` skill, which sets Mutagen up for a project: it
installs or upgrades this fork from its latest release, starts the daemon,
writes `mutagen.yml` with the alpha and beta endpoints, starts the sessions,
and makes `remote` available on the endpoints. On each system where agents
submit commands or set up Mutagen, install the plugin in Claude Code:

```
/plugin marketplace add zlcwwhq/mutagen-remote-skill
/plugin install mutagen-remote@mutagen-remote-skill
```

Or in Codex:

```sh
codex plugin marketplace add zlcwwhq/mutagen-remote-skill
codex plugin add mutagen-remote@mutagen-remote-skill
```

To keep it updated, enable auto-update for the marketplace in Claude Code's
`/plugin`; Codex updates it automatically when it starts. In Codex's default
sandbox, `remote` can't connect to the daemon unless the sandbox allows network
access. The skill's README describes the sandbox setting, the alternatives, an
example of setting Mutagen up with `mutagen-setup`, and how its versions relate
to `remote`. This repository references the matching version of the skills as
the `skills/mutagen-remote` Git submodule.


## Community

The [Mutagen Community Slack Workspace](https://mutagen.io/slack) is the place
to go for discussion, questions, and ideas.

For updates about the project and its releases, you can
[follow Mutagen on Twitter](https://twitter.com/mutagen_io).


## Status

Mutagen is built and tested on Windows, macOS, and Linux, and it's available for
[many more platforms](https://github.com/mutagen-io/mutagen/releases/latest).

| Tests                               | Report card                         |
| :---------------------------------: | :---------------------------------: |
| [![Tests][tests-badge]][tests-link] | [![Report card][rc-badge]][rc-link] |

[tests-badge]: https://github.com/mutagen-io/mutagen/workflows/CI/badge.svg "Test status"
[tests-link]: https://github.com/mutagen-io/mutagen/actions "Test status"
[rc-badge]: https://goreportcard.com/badge/github.com/mutagen-io/mutagen "Report card status"
[rc-link]: https://goreportcard.com/report/github.com/mutagen-io/mutagen "Report card status"


## License

For license information, please see the [LICENSE](LICENSE) file.


## Contributing

If you'd like to contribute to Mutagen, please see the
[contribution documentation](CONTRIBUTING.md).


## Security

Mutagen takes security very seriously. If you believe you have found a security
issue with Mutagen, please practice responsible disclosure practices and send an
email directly to [security@docker.com](mailto:security@docker.com) instead of
opening a GitHub issue. For more information, please see the
[security documentation](SECURITY.md).


## Versioning and support

Mutagen uses [semantic versioning](https://semver.org/) for tracking releases.
Before Mutagen reaches version 1.0, each minor release series will be supported
for one month after the first release in the next minor release series (e.g.
v0.10.x will be supported until one month after the release of v0.11.0).

The builds for each minor release series are pinned to the same Go minor release
and dependency versions used for developing that series (though patch releases
will be incorporated if they contain security fixes).

We reserve the right to break or bend these rules if necessary for the sake of
security, e.g. discontinuing support for a minor release to which a security fix
cannot be backported or upgrading Go minor versions for a release series to
incorporate important security fixes. Features marked as experimental may also
see breakage when necessary, though we will endeavor to keep this to a minimum.


## Building

Please see the [build instructions](BUILDING.md).


## External projects

Users have built a number of cool projects to extend and integrate Mutagen into
their workflows:

- [**Mutagen Helper**](https://github.com/gfi-centre-ouest/mutagen-helper) is a
  tool that makes the orchestration of synchronization sessions even easier by
  letting you define sessions with configuration files that live inside your
  codebase. Thanks to [**@Toilal**](https://github.com/Toilal)!
- [**docker-magento-mutagen**](https://github.com/mage2click/docker-magento-mutagen)
  (from [Mage2click](https://mage2.click/)) is a Docker-based development
  environment for Magento that uses Mutagen for file synchronization. Thanks to
  [**@u-maxx**](https://github.com/u-maxx) and
  [**@shkoliar**](https://github.com/shkoliar)!
- [**MutagenMon**](https://github.com/rualark/MutagenMon) is a Python-based GUI
  for monitoring Mutagen sessions. Thanks to
  [**@rualark**](https://github.com/rualark)!
- [**mutagenmon**](https://github.com/andrewmed/mutagenmon) is an alternative
  (Go-based) native GUI for Mac, for monitoring Mutagen sessions, provides apple
  notarized builds.


## Unrelated projects

Please note that this project is unrelated to the
[Mutagen](https://github.com/quodlibet/mutagen) Python module used for handling
audio metadata.
