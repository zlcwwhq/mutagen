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
| `remote: prologue <path> failed with exit status N` (126) | The prologue returned non-zero, had a syntax error, or exited | Confirm with `--no-prologue`, inspect it with `remote cat <path>`, and tell the user if it's broken |
| `unable to access prologue` (126) | The configured prologue doesn't exist on the remote side | Use `--no-prologue`, and tell the user |
| `unable to access working directory` (126) | The corresponding directory doesn't exist remotely | See "Conflicts and unsynchronized content", or use `--cwd` |

Statuses 128+N mean that the remote command was killed by signal N.

## Conflicts and unsynchronized content

After each flush, `remote` warns about content that it couldn't synchronize,
with paths relative to the current directory:

- `these conflicts aren't synchronized until one side's copy is deleted`: the
  path changed on both sides since the last synchronization, so neither copy
  propagates. `remote --diff` shows how each conflicting file's remote copy
  differs from the local one (`-` local, `+` remote); pass paths to compare
  specific files instead. To resolve a conflict, put the content you want in
  one copy (merging both sides' changes if needed), delete the other copy
  (`rm <path>` or `remote rm <path>`), and run `remote --flush`. If it's unclear
  which changes to keep, show the user the diff and ask.
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

If you keep activating an environment inline, suggest a prologue to the user. A
prologue is a POSIX shell script that the executing endpoint sources before
each command. Paths usually differ between machines, so it's set per endpoint
in the session configuration (here, beta executes):

```yaml
# mutagen.yml
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

Absolute and `~` paths are interpreted on the executing endpoint, and relative
ones against its root. Prologues aren't supported on Windows endpoints.

## Commands that don't return

- A background process still holds stdout or stderr: redirect all output and
  stdin when detaching (`> log 2>&1 < /dev/null &`).
- The command is waiting for input: `remote` forwards stdin, so pass
  `< /dev/null` or use the program's non-interactive flags.
- Use `--timeout` to bound commands that might hang; it kills the remote
  process group and exits with 124.
