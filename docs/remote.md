# Remote commands

`remote` runs a command on the other side of a synchronization session, in the
directory that corresponds to the current one. It synchronizes the session
before the command, so that the command sees local edits, and after it, so that
the command's output files are present locally when `remote` exits. Standard
streams and the exit status are forwarded.

```sh
cd ~/project/service
remote go test ./...           # runs in ~/project/service on the server
remote -e NODE_ENV=test --env-file .env.remote npm test
remote --timeout 2m sh -c 'make build && make test'
id=$(remote --detach --log train.log python train.py)
remote --wait "$id" --follow
```

Commands travel over the daemon's agent connections, so the daemon can run on a
third system, and endpoints needn't reach each other.

`remote --help` summarizes usage, and `remote --help <topic>` covers each area
in full detail: `nodes`, `sync`, `environment`, `termination`, `scripts`,
`jobs`, `conflicts`, and `conflict-details`.


## Contents

- [Enabling remote commands](#enabling-remote-commands)
- [Nodes](#nodes)
- [Running commands](#running-commands)
- [Prologues](#prologues)
- [Synchronization](#synchronization)
- [Timeouts and termination](#timeouts-and-termination)
- [Detached jobs](#detached-jobs)
- [Ignored paths](#ignored-paths)
- [Conflicts](#conflicts)
- [Exit statuses](#exit-statuses)


## Enabling remote commands

The `remote` setting of a synchronization session names the endpoints that
execute commands: `true` (both), `alpha`, `beta`, or `false` (the default). Set
it for a session or under `defaults` in `mutagen.yml`, under `sync.defaults` in
global configuration, or with `--remote` for `mutagen sync create`:

```yaml
sync:
  code:
    alpha: "."
    beta: "server:~/project"
    remote: true
```

```sh
mutagen sync create --remote=true ~/project server:~/project
```

Stars use `star.remote` instead; see [Star workspaces](stars.md).

Any process running as your user on a system that submits commands can run
commands on the executing endpoint. Only enable execution on an endpoint if you
trust the systems that submit commands to it as much as the endpoint itself.

**Where `remote` is installed.** On the daemon's system, `remote` is installed
alongside `mutagen`. On SSH and Docker endpoints, the agent installs it next to
itself as a link to itself (a symbolic link, or a hard link on Windows),
normally at `~/.mutagen/agents/<version>/remote`. Invoke it by that path or put
that directory on `PATH`. Links to it must also be named `remote`.

The daemon connects command channels in the background, so SSH authentication
must work non-interactively. Commands can only be routed while the session is
connected.


## Nodes

`remote` sends each command to a destination **node**, which is a workspace on
a system. A synchronization pair has two nodes, `alpha` and `beta`. A
[star](stars.md) uses the node names that it declares.

```sh
remote --nodes                 # the nodes that this workspace can reach
remote --nodes --json
remote --node beta make test   # select a node
```

`--node` can be omitted when there's exactly one other node. Each call stays on
the node it selected and never switches to another one after a failure. Repeat
`--node` for later operations on the same node, such as `--wait` and `--kill`
for a detached job. The commands that `remote` suggests include it.

The current directory selects the source workspace: the most specific connected
workspace that contains it.

- Independent pairs whose roots are the same directory are ambiguous, and
  commands from there fail with `ambiguous source node`. Declare them as one
  star instead, with the shared directory as the center.
- Outside every root, an explicit `--node` (or `--nodes`) requires a single,
  unambiguous source workspace on this system. Commands then run in the
  destination root, or in `--cwd`, and skip its prologue. This is also how to
  use `--diff` and other file operations on a pair whose roots are single
  files.

`--session`, `-s`, `--sessions`, and `MUTAGEN_REMOTE_SESSION` are deprecated and
fail with status 125. Unset the variable and use `--nodes` and `--node`
instead.


## Running commands

Options must precede the command. Arguments are passed as they are, without a
shell, so shell syntax needs an explicit shell (`remote sh -c '...'`) or
`--script`. Your local shell still expands variables, globs, and `~` first, so
quote what should be expanded remotely. No pseudo-terminal is allocated.

### Working directory

Commands run in the directory that corresponds to the current one. `--cwd`
(`-C`) overrides it. Relative values are resolved against the corresponding
directory, or against the destination root when submitting from outside the
roots. Absolute and `~` paths belong to the executing system, so quote `~` to
keep your shell from expanding it locally: `remote -C '~/project' pwd`. If an
explicit absolute path under your local home is missing on the executing side,
the error suggests this spelling; `remote` never rewrites the path.

Directories that the session ignores require an explicit `--cwd`, and their
contents still aren't synchronized. In [Git mode](git-mode.md), this includes
directories that `.gitignore` rules ignore.

### Environment

Commands inherit the environment of the agent on the executing endpoint (or,
on the daemon's system, of the daemon), which usually lacks interactive shell
setup such as `conda init`. Use a [prologue](#prologues) for that.

- `-e NAME=VALUE` sets a variable, and `-e NAME` forwards its local value.
- `--env-file FILE` reads literal `NAME=VALUE` lines.

`remote` sets these variables, overriding inherited and submitted values:

| Variable | Value |
| :--- | :--- |
| `MUTAGEN_REMOTE_NODE` | The executing node's name (`alpha` or `beta` for a pair) |
| `MUTAGEN_REMOTE_ENDPOINT` | `alpha` or `beta`: the executing endpoint's role in its session |
| `MUTAGEN_REMOTE_JOB` | The job ID, for [detached jobs](#detached-jobs) only |

In a star, the center is alpha on every edge and every other node is beta, so
shared scripts should use `MUTAGEN_REMOTE_NODE`.

### Output buffering

Without a terminal, many programs buffer their output. For Python, use
`remote -e PYTHONUNBUFFERED=1 python train.py` or `remote python -u train.py`.
For programs that use glibc stdio, `remote stdbuf -oL <command>` requests line
buffering. Colors are a separate matter: use the program's own option to force
them.

### Scripts

`--script` runs a local script file, or one read from standard input with `-`,
such as a here-document. Quote its delimiter (`<<'EOF'`) so that your shell
leaves the contents alone:

```sh
remote --script - <<'EOF'
for p in a b; do python run.py --preset "$p"; done
EOF
```

The executing endpoint copies the script to a temporary file and runs it with
the interpreter named by its `#!` line, or with `bash` if it has none. The
interpreter is looked up after the prologue. Arguments after the file are the
script's, even ones that look like options. A script read from standard input
leaves its commands no input. Scripts are limited to 512 KiB and aren't
supported on Windows endpoints. See `remote --help scripts`.


## Prologues

A prologue is a POSIX shell script that `/bin/sh` sources on the executing
endpoint before each command, such as one that activates a conda environment.

Configure it with `remotePrologue` (or `--remote-prologue` for
`mutagen sync create`). Since paths usually differ between systems, it can be
set per endpoint, under `configurationAlpha` or `configurationBeta` (or with
`--remote-prologue-alpha` and `--remote-prologue-beta`). Absolute and `~` paths
are interpreted on the executing system, and other paths relative to its
synchronization root. Quote `~` in these flags so that your shell doesn't
expand it locally.

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
# ~/.config/remote-prologue.sh on the server
eval "$("$HOME/miniconda3/bin/conda" shell.posix hook)"
conda activate "${CONDA_ENV:-base}"
```

In a star, prologues are configured per node; see [Star workspaces](stars.md).

### Workspace prologue

A `.remote-prologue.sh` file at the synchronization root replaces the
configured prologue. The executing endpoint checks for it before each command,
so edits take effect without restarting the session. An empty file does
nothing, and deleting it restores the configured prologue. Only the root is
checked. One file can serve both endpoints:

```sh
# .remote-prologue.sh at the synchronization root
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

The branch for the executing endpoint runs, so `remote pytest` submitted on
alpha runs the `beta` branch. A branch that does nothing still replaces the
configured prologue: the two aren't combined.

The file is synchronized like any other, so ignores, failed flushes, or
`--no-flush` can leave the executing endpoint with an older copy, or with its
configured default.

### Rules

- Prologues apply to commands submitted from inside the session's root,
  including those with `--cwd`. Commands submitted from outside the roots with
  an explicit `--node` skip them, and so does `--no-prologue`.
- The prologue sees variables set with `-e` and `--env-file`, and the command
  is looked up with the `PATH` that it sets, so
  `remote -e CONDA_ENV=test pytest` works even if `pytest` exists only in that
  environment.
- If the prologue can't be loaded, fails, or exits the shell, the command
  doesn't run, and `remote` exits with status 126. An invalid workspace
  prologue doesn't fall back to the configured one; use `--no-prologue` while
  repairing it.
- Prologues aren't supported on Windows endpoints.


## Synchronization

`remote` flushes the session that the command crosses before and after the
command, as `mutagen sync flush` does. The daemon performs the flush, so this
works from either endpoint. If the flush around a command, `--detach`, or
`--wait` fails, `remote` prints a warning and carries on. For `--flush`
without a command, `--diff`, and conflict operations, it's an error.

| Operation | Flushes |
| :--- | :--- |
| A command or script | Before and after |
| `--detach` | Before starting the job |
| `--wait` | After the job ends |
| `--flush` without a command | Only flushes |
| `--diff` | Before comparing |
| `--conflicts`, `--conflict-checkout` | Before listing |
| `--conflict-apply` | Before and after writing |
| `--nodes`, `--jobs`, `--kill`, `--ignores`, `--pull`, `--conflict-discard` | Never |

`--no-flush` skips the flushes of a command, `--detach`, or `--wait`. Skipping
the first can silently run outdated code, while an unneeded flush only costs a
rescan of both roots. So use `--no-flush` only for commands that neither read
nor write synchronized files, such as `nvidia-smi`. `--timeout` includes the
time spent flushing.

**Progress.** Flushes that take more than 3 seconds show their progress on
standard error: on a terminal as a line updated in place, and elsewhere only
with `--progress`, as `remote:` lines printed when the phase changes and every
10 seconds. `--progress=false` hides it. A final line shows where the time
went:

```
remote: synchronized in 44s: scanning 31s; staging on alpha (local) 12s, 1 file, 5.0 MiB
```

**Warnings.** After each flush, `remote` warns about content that the session
couldn't synchronize: [conflicts](#conflicts), and problems such as permission
errors. In [Git mode](git-mode.md), it lists what isn't synchronized by design
separately. Ignored paths aren't reported.


## Timeouts and termination

`--timeout` takes seconds or a duration with a unit, such as `10m`. A command
is terminated when:

- the timeout expires (status 124),
- `remote` is interrupted,
- the session stops synchronizing, such as when it's paused, reconnects after
  an error, or is replaced by `mutagen project apply`, or when the daemon
  stops.

The executing endpoint sends SIGTERM to the command's process group and SIGKILL
5 seconds later to any process that remains. On Windows endpoints, the command
is killed at once, without its child processes.

When the session stops synchronizing, `remote` reports that the connection to
the executing endpoint was lost and exits with status 255, without flushing
afterwards. Files that the command was writing may be incomplete, and are
synchronized as they are once synchronization resumes. Run commands that must
survive this as [detached jobs](#detached-jobs).


## Detached jobs

A detached job keeps running when `remote` exits and when the session stops
synchronizing.

```sh
id=$(remote --detach --log train.log python train.py)
remote --jobs                          # list jobs and their states
remote --wait "$id" --follow           # print the log while waiting
remote --wait "$id" --tail 20 --timeout 5m
remote --kill "$id"
```

- `--detach` starts the command or script and prints the job's ID. Jobs run in
  the directory and environment that commands would, prologue included, in a
  new session and process group, without input.
- `--log FILE` receives the job's output. It's relative to the command's
  directory and must be a regular file (truncated first) or a character device
  such as `/dev/null`. Without it, the log is kept with the job's state in the
  executing endpoint's Mutagen data directory.
- `--jobs` lists the jobs started at the session's root.
- `--wait ID` waits for the job to end, flushes, and exits with the job's
  status. `--timeout` stops waiting (status 124) without affecting the job.
  `--tail N` also prints the last N log lines, and `--follow` prints the log as
  it's written.
- `--kill ID` sends SIGTERM to the job's process group, and SIGKILL 5 seconds
  later to any process that remains.

A job ends when its command exits. Processes that it leaves running in the
background aren't part of the job any more, so scripts that start background
processes should `wait` for them. A job whose status couldn't be recorded, such
as after SIGKILL or a restart of its system, is reported as lost, and `--wait`
returns 125. Job state is kept for 7 days after a job ends.

Jobs end with the system or container that runs them. They can also end with
the processes that run the endpoint: stopping a daemon that runs as a systemd
service terminates the jobs that it runs, and where logind's
`KillUserProcesses` is enabled, jobs on SSH endpoints end with the SSH
connection. Detached jobs aren't supported on Windows or AIX endpoints. See
`remote --help jobs`.


## Ignored paths

Content that commands write to ignored paths exists only on the executing
endpoint.

- `remote --ignores` lists the session's ignores, and
  `remote --ignores <path>...` says whether each path is ignored, with status 1
  if any is.
- `remote --pull <path>...` copies ignored files, directories, and symbolic
  links from the target node. It replaces local files that differ, never
  deletes local content, and preserves the modification times of regular files
  (to whole seconds). It refuses paths that a session synchronizes, and
  directories with synchronized content beneath them, since synchronization
  updates those.

`--pull` never writes inside a `.git` entry, and only adds one where none
exists locally. `remote --help sync` covers the details: how symbolic links,
Unicode normalization, permissions, and other sessions are handled.

In [Git mode](git-mode.md), `--ignores` also lists the rule files that the
session's ignores come from and says which of them ignores each path, and
`--pull` copies what they ignore.


## Conflicts

A conflict is a path that both endpoints modified. The session doesn't
synchronize it until it's resolved, and `remote` warns about it after each
flush without preventing commands.

`remote --diff` shows how the remote copies of the conflicting files, or of the
given paths, differ from the local ones, as a unified diff, and exits with
status 1 if any differ. When one endpoint has replaced a directory with a file,
the directory's former children are compared as missing entries.

In `two-way-safe` and `one-way-safe` sessions, `remote` can also resolve
conflicts. Local and remote mean the submitting and the opposite endpoint.

```sh
remote --conflict-apply --take local src/app.go   # keep the local copy
remote --conflict-apply --take remote src/app.go  # keep the remote copy

# Or merge both copies by hand:
remote --conflicts                    # list conflicts and checkouts
remote --conflict-checkout src/app.go # download both copies into a workspace
# Edit the merged file that checkout prints, resolving its conflict markers.
remote --conflict-apply src/app.go    # write it to both copies and verify
```

- Stop anything that writes the conflicting paths, on both endpoints, until
  apply finishes. Apply refuses copies that changed since checkout, but can
  overwrite edits made while it runs.
- Review the whole merged file, even if it has no conflict markers.
- A conflict is checked out through one endpoint, and an existing checkout may
  be someone else's work in progress: only apply or discard checkouts that you
  made.
- Directories require manual resolution. Other entry types can be resolved
  with `--take`.
- A failed apply keeps its workspace for a retry. Successful apply and
  `--conflict-discard` keep the original copies as backups.

`remote --conflicts` exits with status 0 whenever it produces a listing. With
nothing to list, it prints `No conflicts.`, unless some content couldn't be
synchronized, which can hide conflicts: then it says that conflicts can't be
ruled out. Conflict operations accept several paths and `--json`. See
`remote --help conflicts` and `remote --help conflict-details`.


## Exit statuses

`remote` exits with the command's or job's own status. Its own results and
failures use these statuses:

| Status | Meaning |
| :--- | :--- |
| 1 | `--diff`: copies differ. `--ignores`: a path is ignored |
| 124 | Timeout |
| 125 | `remote`'s own failure: invalid arguments or input, a failed operation, an unknown node, a node that only a star's center reaches, a star problem, or a lost job |
| 126 | The command couldn't start, such as because of its working directory or prologue |
| 127 | Command not found |
| 128+N | Killed by signal N |
| 255 | Connection setup failed, the connection was lost, a protocol failure, or an unreachable node |

Commands can return 125 or 255 themselves, so read the `remote:` diagnostic
before retrying. When a batch fails in several ways, an interruption or timeout
takes precedence, then connection failures, then operation failures.
