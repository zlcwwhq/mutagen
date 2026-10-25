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
new and replaced sessions use the current environment.

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
session. If a flush fails, nothing is terminated or created: fix the session and
apply again, or, if its unsynchronized changes don't matter, terminate it with
`mutagen sync terminate` and apply again. Changes that a flush leaves
unsynchronized, such as those blocked by problems, aren't protected, and neither
are changes made while `apply` runs, so stop processes that modify synchronized
files before replacing or renaming their sessions. Paused sessions are neither
flushed nor resumed, so changes made since a session was paused may be
overwritten (`two-way-resolved`, `one-way-replica`) or become conflicts
(`two-way-safe`, `one-way-safe`) once its replacement is resumed or a session
created under a new name synchronizes them. The plan notes such sessions; resume
them before applying to synchronize their changes first.

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


## Remote commands

The `remote` executable runs a command on the opposite endpoint of a
synchronization session, in the directory corresponding to the current one:
invoked inside alpha's root, it runs the command on beta, and invoked inside
beta's root, it runs the command on alpha. Commands are routed through the
Mutagen daemon over its own agent connections, so the endpoints don't need to
reach each other and the daemon can run on a third system.

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

`remote` uses the session whose root most specifically contains the current
directory. Use `--session` (`-s`) to select a session by name or identifier
instead, which also allows invocation outside its roots: commands submitted from
outside the selected session's roots run at the root of its opposite endpoint,
or in the directory specified by `--cwd`. If another session's root contains the
current directory, such commands require `--cwd`, since they would otherwise run
at the selected session's root rather than in a directory corresponding to the
current one. An exported `MUTAGEN_REMOTE_SESSION` only selects among the
sessions whose roots contain the current directory, such as to choose between
them, and is ignored if none of them matches it; outside every session's
root, it selects a session as `--session` does. Once `remote` has located the
session, it uses that session for the rest of the invocation, rather than
switching to another session if it disconnects in the meantime.

If several sessions' roots contain the current directory equally
specifically, such as when one directory is synchronized to several machines,
the session created first is used. `remote --sessions` lists the sessions
connected for remote commands on the current system, separating those whose
roots contain the current directory and marking the one used by default. Each
entry shows `local root`, the execution host and configured root (`runs on`),
and the mapped execution `cwd`. Home-relative paths use `~/` for the executing
user's home and are not resolved during listing; old registrations with no
execution metadata show `unknown`. Ignored directories and selections outside
a root are annotated. When several sessions synchronize the current directory
(their roots contain it,
and they don't ignore it), `remote` names them in a warning, unless the session
was selected with `--session` or a matching `MUTAGEN_REMOTE_SESSION`. Sessions
are named by their names, or by their identifiers if they're unnamed or share
their names, as `--session` accepts them.

Commands don't run in a directory that the session ignores, or inside one, since
the session doesn't synchronize it, unless `--cwd` is given: `--cwd .` runs them
in the corresponding directory anyway, such as in an ignored build directory
that exists on both endpoints. `remote --ignores` lists the session's ignores.

`remote` can't find sessions that are paused, disconnected, or that don't run
commands submitted from the current endpoint. Inside the root of such a session
that's nested in another session's root, the enclosing session is used instead:
if it ignores the nested root, as it typically does, commands are refused, and
otherwise they run in its copy of the directory. `--session` selecting the
nested session makes them fail instead. `--flush` and `--diff` still use the
enclosing session.

`--cwd` overrides the working directory (relative values are resolved against
the corresponding directory, or against the opposite endpoint's root when
submitting from outside the session's root). `-e NAME=VALUE` sets a variable,
`-e NAME` forwards a local one, and `--env-file` loads literal `NAME=VALUE`
lines. Quote home paths, such as `remote -C '~/project' pwd`, so your shell
doesn't expand `~` into the local home. If an explicit absolute path under your
local home is missing on the executing side, the error suggests this spelling
conditionally; it never rewrites the path. Otherwise, commands inherit the
environment of the agent (or, on the daemon's system, of the daemon).

Every command receives `MUTAGEN_REMOTE_ENDPOINT=alpha` or `beta`, identifying
the endpoint where it executes. This value overrides inherited values and
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
submitted from outside the root with `--session` don't use it. These rules apply
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
setup, lost connections, protocol failures, and an unreachable session return
255. An invalid explicit session selection returns 125. Read the `remote:`
diagnostic before retrying a 125 failure. Commands may themselves return 125
or 255, so exit status alone cannot always identify the source. Timeouts remain
124, startup/working-directory/prologue failures 126, and missing commands 127;
signals use 128+N. `--diff` and `--ignores` use 1 for their results and 125/255
for failures. In batches, interruption or timeout takes precedence, followed by
connection failures, then operation failures. This changes scripts that used
255 for all failures of remote itself. No pseudo-terminal is allocated.
Commands can only be routed while the session is connected, and the daemon connects command channels in the background, so
SSH authentication must work non-interactively. `remote --help` summarizes
usage, and `remote --help <topic>` covers sessions, synchronization, scripts,
detached jobs, environment setup, termination, and conflicts in detail.

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

`remote` flushes the session (as `mutagen sync flush` does) before the command,
so that it sees local changes, and after it, so that its changes are present
locally when `remote` exits. This works from either endpoint, since the daemon
performs the flush. If flushing fails, then `remote` prints a warning and
continues with the command's exit status; `--no-flush` skips both flushes, and
`remote --flush` only flushes. `--timeout` takes seconds, or a duration with a
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
operations. A final line summarizes where the time went, such as
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
(`--wait` returns 125). Job
state is kept for 7 days after jobs end. Jobs end with the system or container
that runs them, and can end with the processes that run the endpoint, such as
when a daemon running as a systemd service is stopped, or, on systems where
logind's `KillUserProcesses` is enabled, with the SSH connection. Detached jobs
aren't supported on Windows or AIX endpoints.

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
`remote --pull <path>...` copies ignored files, directories, and
symbolic links from the opposite endpoint, replacing local files that differ
but never deleting local content. It refuses paths that the session (or
another session) synchronizes, and directories with synchronized content
beneath them. Other sessions are recognized whether or not they run remote
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

After each flush, `remote` also warns about content that the session couldn't
synchronize: conflicts (paths modified on both endpoints, which aren't
synchronized until resolved) and problems such as permission errors.
`remote --diff` shows how the remote copies of conflicting files, or of the
specified paths, differ from the local copies as a unified diff, and exits with
status 1 if any differ. Single-file sessions are supported: use `--session`
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
`--delete` require an explicit checkout.

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
