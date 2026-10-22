# Recovering conflict resolutions

These steps continue the [conflict workflow](conflicts.md) when apply fails or
a conflicting path changed after checkout. A workspace may then hold the only
copy of one side's changes, so follow them before discarding one.

## When apply fails

If apply says that the merged file is empty, it changed neither copy: rebuild
`Merged` from `Local`, `Remote`, and `Base`, or, if the message suggests it
(nothing was applied yet), discard the workspace and check out again, which
recreates it. If the result really should be empty, discard the workspace and
empty both copies, which then agree.

If apply reports that a copy changed since checkout, it didn't change that copy
(if it also says that it updated the remote copy, that copy already has your
result): run `remote --conflict-discard <path>` (which changes neither copy),
check out again, and merge the new versions. Retry any other failed apply,
including one that was interrupted (`remote --conflicts` marks it as partially
applied), after fixing `Merged` if needed: synchronization may have copied
one original over a partially applied result, which removes the conflict
without applying your result. A copy that already has the result is left
alone, a copy with a result applied before is replaced, and the workspace
remains until both copies match. Don't discard a partially applied resolution
instead: a copy may keep the result, or one side's original may then exist
only in the workspace, as discard warns. An apply that timed out may still be
writing the remote copy, and a retry waits for that write to finish. If apply
reports that a later apply of the conflict reached a copy first, it didn't
change that copy: another apply (such as one through the other side) got there
first, so run `remote --conflicts` to see what's left. If apply reports a
problem that synchronization has with the path (such as a permission error),
it updated the copies but couldn't verify the resolution: fix the problem if
you can (or report it to the user), then retry. If nothing was applied and the
conflict is no longer listed, apply refuses without changing either copy, since
the conflict was resolved some other way or ended by a change: follow the
recovery steps below before discarding the workspace. If apply says that the
conflict is checked out through the other side, that side resolves it: discard
this workspace. Existing workspaces involving directories can only be
discarded, not applied. A conflict operation on a path fails while another one
on it is in progress on the same side, without changing anything.

## When a conflicting path changed

If a conflicting path changed after checkout anyway (by you, the user, or a
tool on either side), first find and stop whatever changed it, such as an
editor, a watcher or formatter, a remote job, or a `git` command, or it will
change again. The synchronized files remain authoritative. Discarding changes
neither copy and keeps the workspace, whose location it prints (`Backup` with
`--json`), but the workspace may hold the only copy of one side's changes, and
discarded workspaces are deleted after 7 days, so recover what you need from
it first. Run `remote --conflicts`, then:

- If the conflict is still listed (flagged as changed, or apply reported that a
  copy changed or that the path conflicts again), discard, check out again, and
  merge the new copies. Diffing the discarded workspace's `local` or `remote`
  with the new copy shows what changed, and its `merged` file has your earlier
  decisions to reuse. If apply also said that it updated the remote copy, that
  copy has your result, and the new conflict is between it and the changed
  local copy.
- If the conflict is no longer listed, and either nothing was applied or
  applying an unfinished resolution again reports that a copy changed, it was
  resolved some other way, or the change ended it: reverting or deleting one
  copy, for example, lets synchronization copy the other one over it, so that
  side's changes now exist only in the workspace. Checking out again fails,
  since nothing conflicts. Before discarding, compare the current file with
  the workspace's `local` and `remote` (`remote --conflicts` shows where the
  workspace is). If it matches one of them, the other side's changes were
  overwritten, and a complete `merged` file is the result to restore. If it
  matches neither, someone wrote a new version: compare it with both and with
  `merged`. Restore anything lost by editing the file normally (it's no longer
  a conflict; in one-way-safe sessions, see below), check with
  `remote --diff <path>`, which synchronizes first, that both copies have it,
  then discard the workspace. Leave the file as it is if the user resolved the
  conflict this way on purpose, and ask the user when the intended content is
  unclear.
- In one-way-safe sessions, only alpha's changes propagate: synchronization
  copies alpha's copy over a reverted or deleted beta copy, but if alpha's
  copy was deleted, it leaves beta's copy as it is, unsynchronized, and it
  never copies beta's changes to alpha. `remote --diff <path>` shows whether
  the copies differ. Restore lost content on alpha: edit the file if your side
  is alpha (`remote --no-prologue printenv MUTAGEN_REMOTE_ENDPOINT` prints the
  other side's name); otherwise, write it there with
  `remote tee <path> < <file> > /dev/null`, or tell the user what to restore.
  Then check with `remote --diff <path>`: if the copies still differ,
  `remote --conflicts` lists any new conflict between them, which you resolve
  as usual.
- Changes made while apply was writing a copy may have been overwritten without
  an error, and can't be recovered.
- If checkout or apply says that it's unable to read the workspace, discard it,
  and if anything was applied through it, check the copies against its files
  (discard prints where it keeps them).
