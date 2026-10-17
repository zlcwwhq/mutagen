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
- **Your shell runs first.** Variables, globs, `~`, pipes, and `&&` are
  processed locally, and nothing is reinterpreted remotely: `remote echo $HOME`
  prints the local home, while `remote sh -c 'echo $HOME'` prints the remote
  one.
- **Remote changes come back.** Files that the command creates, rewrites, or
  deletes in the synced tree change locally too. A file modified on both sides
  becomes a conflict and stops synchronizing (`remote` warns about it, and
  `remote --diff` shows both versions), so edit sources locally, let remote
  commands write only outputs, and avoid mutating `git` commands remotely.
- **No terminal.** Use non-interactive flags, set `--timeout` for anything that
  might hang, and detach long jobs with all output redirected (otherwise
  `remote` waits for them):
  `remote sh -c 'nohup python train.py > train.log 2>&1 < /dev/null &'`, then
  `remote tail -n 40 train.log`.

If `remote` exits with 255, 126, or 127, prints a `remote:` error or warning, a
command sees outdated files, or a command is missing from the remote
environment (such as an inactive conda environment), read
[references/troubleshooting.md](references/troubleshooting.md).
