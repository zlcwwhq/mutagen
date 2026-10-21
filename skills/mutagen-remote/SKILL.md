---
name: mutagen-remote
description: Run commands on the other side of a Mutagen synchronization session with the `remote` executable — tests, builds, training or benchmark scripts, GPU checks, or anything else that has to execute on the server or container holding the synced copy of the code. Use this whenever the working directory is synced by Mutagen and the task needs execution on the other machine (the user says "run it on the server/remote/GPU box/container", mentions `remote`, `mutagen sync`, or a conda environment on the server, or the local machine lacks the needed hardware, OS, or dependencies), even if they don't name the tool. Also use it when `remote` fails (exit status 255, 126, or 127, or a `remote:` error).
---

# Running commands with `remote`

`remote <command> [args...]` runs a command on the opposite endpoint of the
Mutagen sync session containing the current directory, in the corresponding
directory, and forwards stdio and the exit status. `remote --help` covers the
options, exit statuses, and prologue behavior. Use only `remote`, including for
synchronization (`remote --flush`): the `mutagen` CLI may not be installed where
you run, and sessions are the user's to manage.

What the help can't convey is how working across two synchronized copies goes
wrong:

- **Synchronization happens around each command.** `remote` flushes the
  session before and after the command, so it sees your edits and its file
  changes are present locally when it returns (a `remote: warning:` means that
  a flush failed). Files that a detached job writes later arrive through normal
  synchronization; run `remote --flush` before reading them. `--no-flush` skips
  both flushes for commands that don't touch synced files, such as `nvidia-smi`.
- **The current directory picks the session.** Commands run in the remote
  directory corresponding to the current one, through the session whose root
  contains it, so `cd` into the synced tree instead of selecting sessions. From
  outside every synced root, `-s <name-or-id>` runs at that session's remote
  root (or in `--cwd`); `-s` naming a session whose root doesn't contain the
  current directory requires `--cwd` if another session's root does.
  `MUTAGEN_REMOTE_SESSION` never overrides the session containing the current
  directory. Commands refuse to run in a directory that the session ignores.
  Inside a session nested in another synced root, the enclosing session is
  used while the nested one is paused or disconnected: commands are refused if
  it ignores the nested root, and otherwise run in its copy; `-s <nested>`
  fails instead, and `remote pwd` shows where commands run.
- **Your shell runs first.** Variables, globs, `~`, pipes, and `&&` are
  processed locally, and nothing is reinterpreted remotely: `remote echo $HOME`
  prints the local home, while `remote sh -c 'echo $HOME'` prints the remote
  one.
- **Remote changes come back.** Files that the command creates, rewrites, or
  deletes in the synced tree change locally too. A file modified on both sides
  becomes a conflict and stops synchronizing (`remote` warns about it, and in
  two-way-safe and one-way-safe sessions `remote --conflicts` lists every
  conflict), so edit sources locally, let remote commands write only outputs,
  and avoid mutating `git` commands remotely.
- **Environment setup can live in the workspace.** For repeated activation,
  create or edit `.remote-prologue.sh` at the sync root, preserving existing
  setup. It replaces the session's configured prologue and synchronizes like
  other files. Use `MUTAGEN_REMOTE_ENDPOINT` (`alpha` or `beta`, the executing
  side) for different environments on each endpoint. See the
  [environment setup guidance](references/troubleshooting.md#commands-missing-from-the-remote-environment)
  for diagnosis, examples, and validation.
- **No terminal.** Use non-interactive flags, set `--timeout` for anything that
  might hang, and detach long jobs with all output redirected (otherwise
  `remote` waits for them):
  `remote sh -c 'nohup python train.py > train.log 2>&1 < /dev/null &'`, then
  `remote tail -n 40 train.log`.
- **Commands stop when synchronization stops.** If the session is paused or
  reconnects while a command runs, the command is terminated (SIGTERM, then
  SIGKILL 5 seconds later, as for `--timeout`), and `remote` exits with 255,
  saying that the connection was lost, without synchronizing afterwards. Files
  that it was writing may be incomplete; rerun it once `remote --flush`
  succeeds. Detached jobs keep running.

If `remote` exits with 255, 126, or 127, prints a `remote:` error or warning, a
command sees outdated files, or a command is missing from the remote
environment (such as an inactive conda environment), read
[references/troubleshooting.md](references/troubleshooting.md).

## Resolving conflicts

In two-way-safe (the default) and one-way-safe sessions, use the
[conflict workflow](references/conflicts.md) to download both copies of a
conflict, merge them locally, and apply the result to both sides. Use
`remote --conflicts` rather than flush warnings as the complete list, including
partially applied resolutions, which you finish by applying again, and
workspaces whose conflicts are no longer listed. From checkout until apply
finishes, leave the conflicting paths unchanged on both sides and edit only the
workspace; if they changed anyway, follow the workflow's recovery steps, since
one side's changes may then exist only in the workspace, which you must compare
with the current file before discarding it: discarded workspaces are deleted
after 7 days. A conflict is checked out once, through one side, which alone can
apply or discard it: leave conflicts checked out through the other side to it,
and don't edit their paths. A conflict checked out on your side may be another
agent's or the user's resolution in progress: apply or discard only conflicts
that you checked out, and ask the user about others.
With a common ancestor, merging uses a text-based three-way model similar to
Git's, with additional conservative checks. Even without conflict markers, it
can take lines that were removed and then restored for unchanged, keep twice a
change that both sides made in different places, or misread moves, especially
of repeated lines such as blank lines or closing braces: review the whole
merged file against both copies and the ancestor, when available, before
applying it. Preserve both sides' intent when it is clear, and ask the user
when it's ambiguous, especially without a common ancestor.
Conflicts involving directories require manual resolution; checkout and apply
reject them, including existing workspaces. Report these to the user.
