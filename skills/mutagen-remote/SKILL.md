---
name: mutagen-remote
description: Run commands on the other side of a Mutagen synchronization session with the `remote` executable — tests, builds, training or benchmark scripts, GPU checks, or anything else that has to execute on the server or container holding the synced copy of the code. Use this whenever the working directory is synced by Mutagen and the task needs execution on the other machine (the user says "run it on the server/remote/GPU box/container", mentions `remote`, `mutagen sync`, or a conda environment on the server, or the local machine lacks the needed hardware, OS, or dependencies), even if they don't name the tool. Also use it when `remote` fails or warns.
---

# Running commands with `remote`

`remote <command> [args...]` runs a command on the other side of the Mutagen
session containing the current directory (so `cd` into the synced tree), in the
corresponding directory, and forwards stdio and the exit status. It
synchronizes before and after the command, so the command sees your edits and
its changes are present locally when it returns. Use only `remote`: the
`mutagen` CLI may not be installed where you run, and sessions are the user's
to manage.

- **Your shell runs first.** Variables, globs, `~`, pipes, and `&&` are
  processed locally, so `remote echo $HOME` prints the local home. Use
  `remote sh -c '...'` for anything that should be expanded remotely.
- **No terminal.** Use non-interactive flags, and `--timeout <seconds>` for
  anything that might hang. `remote` waits for every process holding its
  output, so detach long jobs with all output redirected:
  `remote sh -c 'nohup python train.py > train.log 2>&1 < /dev/null &'`, then
  `remote tail -n 40 train.log`.
- **Edit sources locally.** Let remote commands write only outputs, and avoid
  mutating `git` commands remotely: a file changed on both sides becomes a
  conflict and stops synchronizing.
- `--no-flush` skips synchronization for commands that don't touch synced
  files, such as `nvidia-smi`. `remote --flush` only synchronizes, such as
  before reading files that a detached job wrote.

`remote --help` lists the options and help topics. Read a reference only when
you need it:

- [troubleshooting.md](references/troubleshooting.md) if `remote` exits with
  255, 126, or 127, prints a `remote:` error or warning, or doesn't return; if
  a command is missing remotely (such as an inactive conda environment) or sees
  outdated files; or to run outside the current session or directory.
- [conflicts.md](references/conflicts.md) before resolving a conflict in any
  way, including by editing or deleting either copy.
