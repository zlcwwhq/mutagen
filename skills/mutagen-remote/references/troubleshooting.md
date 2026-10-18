# Troubleshooting `remote`

Diagnose with `remote` alone: the `mutagen` CLI may not be installed where you
run, and sessions (their state, configuration, and conflicts) are managed by the
user. When a fix requires changing a session, tell the user what's wrong and
what to change.

## Where things are

- On the daemon's machine, `remote` is installed alongside Mutagen. On SSH and
  Docker endpoints, it's at `~/.mutagen/agents/<version>/remote` and usually
  isn't on `PATH`.
- `remote --flush` checks routing and synchronization without running a
  command.

## Error messages

| Message | Cause | Fix |
| --- | --- | --- |
| `no connected session with remote commands enabled contains <dir>` (255) | Outside every synced root, or the session is paused, disconnected, or doesn't execute commands on the other side (they can be enabled in one direction only) | `cd` into the synced directory; otherwise tell the user |
| `<alpha\|beta> is not connected for remote commands` (255) | Session just started or is reconnecting | Retry after a few seconds; if it persists, tell the user |
| `multiple sessions contain ...` / `multiple sessions match ...` (255) | The directory is synced by the sessions listed in the message | `remote -s <name-or-id> ...` or `export MUTAGEN_REMOTE_SESSION=<name>` |
| `both endpoints of session ... are on this system` (255) | `--session` used outside both roots of a local session | Run from inside the root you want to submit from |
| `registered by Mutagen X, but this is Mutagen Y` (255) | `remote` from a different Mutagen version | Use the `remote` installed with version X |
| `warning: unable to synchronize before/after the command` | The session stopped synchronizing during a flush, or `--timeout` expired | The command still ran; retry with `remote --flush`, and tell the user if it keeps failing |
| `synchronization stopped before the flush completed` (255 with `--flush`) | Same as above | Same as above |
| `unable to find command` (127) | Not on the remote `PATH` | See "Commands missing from the remote environment" |
| `remote: prologue <path> failed with exit status N` (126) | The prologue returned non-zero, had a syntax error, or exited | Inspect with `remote --no-prologue cat <path>`; repair a workspace override locally, preserving existing setup |
| `unable to access prologue` (126) | A prologue is missing or inaccessible, or an override is a dangling link | Use `--no-prologue` to inspect the reported path; repair the workspace override, or tell the user if their configured script is unavailable |
| `unable to access working directory` (126) | The corresponding directory doesn't exist remotely | See "Conflicts and unsynchronized content", or use `--cwd` |

Statuses 128+N mean that the remote command was killed by signal N.

## Conflicts and unsynchronized content

After each flush, `remote` warns about content that it couldn't synchronize,
with paths relative to the current directory:

- `these conflicts aren't synchronized until resolved`: the path changed on
  both sides since the last synchronization, so neither copy propagates.
  `remote --diff` shows how the copies differ (`-` local, `+` remote). Resolve
  conflicts with the [conflict workflow](conflicts.md). If `remote --conflicts`
  says that the session's mode doesn't support resolution, or lists the
  conflict as one that remote can't resolve, first check whether it involves
  a directory. Checkout and apply reject directory conflicts, including
  existing workspaces; preserve the copies and report the conflict to the user
  for manual resolution. For other unsupported conflicts involving content
  that can't be synchronized or read, fix unreadable content if you can.
  Otherwise, copy both versions somewhere outside the synced tree first, put
  the content you want in one copy (merging both sides' changes if needed),
  delete the other copy (`rm <path>` or `remote rm <path>`), and run
  `remote --flush`. Nothing checks that the deleted copy is unchanged, so do
  this promptly. If it's unclear which changes to keep, show the user the diff
  and ask.
- `this content couldn't be synchronized`: the side in parentheses couldn't
  read or write the path, for example because of permissions. Fix it on that
  side if you can, or tell the user.

Ignored paths are never synchronized and aren't reported; if a file is missing
remotely without a warning, it's probably ignored (tell the user).

Each flush rescans both roots, which can take seconds for very large trees. Use
`--no-flush` for commands that don't depend on recent edits, and suggest that
the user ignore large generated directories in the session.

For a session that synchronizes a single file, run `remote -s <name-or-id>
--diff` from outside its root, or pass the local file's absolute path after
`--diff`. File/directory conflicts are supported: children beneath a file are
treated as missing on that endpoint. Large reports may omit change details and
show an omitted count; use `remote --diff <path>` to compare specific omitted
files. `--timeout` also covers reading and hashing files during comparison.

## Commands missing from the remote environment

Remote commands inherit the Mutagen agent's non-interactive environment, so
`.bashrc` setup such as `conda init` usually hasn't run. Diagnose with
`remote sh -c 'echo "$PATH"; command -v python'`, and repeat it with
`--no-prologue` to see what the endpoint's prologue, if any, changes. Then pick
one:

- Use the prologue: it's sourced before the command is looked up and sees `-e`
  variables, so it may support selecting an environment (the example below
  reads `CONDA_ENV`).
- Activate inline:
  `remote sh -c 'eval "$(~/miniconda3/bin/conda shell.posix hook)" && conda activate torch && python train.py'`.
- Use an absolute path to the executable.

Avoid `-e PATH=$PATH`: your local shell expands it to the local `PATH`.

For repeated environment setup, create or edit `.remote-prologue.sh` at the
synchronization root. Inspect an existing file first and preserve its setup;
check the current prologue's effects before replacing them. No session change
or restart is needed. This POSIX shell script is sourced before each eligible
command and completely replaces the session's configured prologue. It needs
no executable permission.

Discover the executing side with
`remote --no-prologue sh -c 'printf "%s\n" "$MUTAGEN_REMOTE_ENDPOINT"'`.
`remote` supplies `alpha` or `beta` to scripts and commands, overriding inherited
values and values passed with `-e` or `--env-file`. Since the file synchronizes
to both endpoints, use this variable for setup that differs between them:

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

Adapt the paths and environments to what you have verified on each endpoint;
only add the branches that need setup. Shared initialization can go outside
the `case`. An empty file or a branch that does nothing still replaces the
configured prologue. Delete the override to restore the session default.

Validate edits with `remote sh -c 'command -v python; python --version'` (or the
relevant tool). The default flush transfers edits before execution; ignored
files, sync failures, or `--no-flush` can leave the executing copy outdated.
If the script fails, commands stop with status 126 and don't fall back to the
session default. Use `--no-prologue` for diagnosis and repair, then validate
with prologues enabled again.

Only the synchronization root is checked, including with `--cwd`; relative
paths inside the script use the command's working directory. `--no-prologue`
skips both workspace and configured scripts, as does submission from outside
the root with `--session`. Prologues aren't supported on Windows endpoints.

## Commands that don't return

- A background process still holds stdout or stderr: redirect all output and
  stdin when detaching (`> log 2>&1 < /dev/null &`).
- The command is waiting for input: `remote` forwards stdin, so pass
  `< /dev/null` or use the program's non-interactive flags.
- Use `--timeout` to bound commands that might hang; it kills the remote
  process group and exits with 124.
