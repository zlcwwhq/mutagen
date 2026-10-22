# Resolving conflicts

`remote` resolves conflicts in sessions using the two-way-safe (the default) or
one-way-safe synchronization mode. For conflicts that it can't resolve, see
"Conflicts and unsynchronized content" in
[troubleshooting.md](troubleshooting.md).

Resolution requires that the conflicting paths don't change from checkout
until apply finishes: before checking out, stop anything that writes them on
either side, such as editors, watchers, and remote jobs, and don't edit,
delete, or restore them (with `git checkout`, for example) in the meantime.
Edit the files in the workspace instead. Apply refuses copies that changed
before it runs, but it can overwrite changes made while it runs. Each conflict
is checked out once, through one side, which alone can apply or discard it:
checking it out again, from either side, fails until then. Checkouts aren't
tied to who made them, so a conflict checked out on your side may be another
agent's or the user's resolution in progress: apply or discard only conflicts
that you checked out, and ask the user about others.

1. Run `remote --conflicts`. It synchronizes and lists every conflict, with
   paths relative to the current directory. Conflicts that aren't between two
   files say which copies `--take` can keep. Lines marked as something remote
   can't resolve need the manual approach. Conflicts involving directories
   are rejected by checkout and apply; report these to the user. Conflicts
   checked out through the other side belong to whoever checked them out
   there: leave them alone, and ask the user if they seem abandoned. A
   conflict checked out here whose copy changed since checkout must be
   discarded and checked out again, and one marked as partially applied must
   be applied again (step 4), since a copy may already have the result.
   Workspaces whose conflicts are no longer listed come at the end, with their
   locations. Finish partially applied resolutions first (step 4). Workspaces
   with nothing applied belong to conflicts that were resolved some other way,
   such as by the user, or ended by a change after checkout, which may have
   left one side's changes only in the workspace: apply refuses them, so
   follow the
   [recovery steps](conflict-recovery.md#when-a-conflicting-path-changed)
   before discarding them. Paths still checked out through the other side,
   though no longer listed as conflicts, may be unfinished resolutions: leave
   them unchanged.
2. Run `remote --conflict-checkout <path>...`. For each path, it prints the
   paths of the files in a workspace outside the synced tree: `Local` and
   `Remote` (both complete copies) and, for two files, `Merged` and, if found,
   `Base`. Each conflict can only be checked out once: checking out again
   fails, printing the existing workspace's location and what to do with it,
   and so does checking out a conflict that the other side checked out. If
   you didn't check out the existing workspace, leave it to whoever did, or
   ask the user: discarding it would let a new checkout reuse its files.
   If checkout says that the copies changed since conflicts were listed (such
   as by an edit while it ran), check out again.
3. For two files, edit `Merged`. Conflicting regions are enclosed in markers:

   ```
   <<<<<<< local <token>
   (local lines)
   ||||||| base <token>
   (common ancestor's lines, if merged with Base)
   ======= <token>
   (remote lines)
   >>>>>>> remote <token>
   ```

   - **With `Base`** (the file that both sides last agreed on, verified by its
     digest), checkout uses a text-based three-way merge model similar to
     Git's. It merges regions changed on one side or identically on both and
     marks regions changed differently on both sides; checkout says how many.
     Changes separated only by repeated lines (such as closing braces) are
     marked together, even if they look independent, since they can be
     aligned in several ways. Additional conservative checks may mark more
     conflicts than Git, and results aren't guaranteed to match Git's. This
     model compares snapshots without tracking editing operations, so even a
     merge with no markers can misrepresent changes. Check especially lines
     that were removed and then restored, which look unchanged (if both sides
     deleted a function and one side restored it, the merge deletes it);
     changes that both sides made in different places, which can be kept
     twice; and moved lines, especially repeated ones such as blank lines or
     closing braces, which can be misplaced or kept too often or too rarely.
     Compare the whole merged file with `Local`, `Remote`, and `Base` before
     applying it, combine the intended changes, and remove the markers. Ask
     the user when intent is unclear. If checkout says that
     the changes couldn't be merged reliably (such as a line that each side
     moved elsewhere), every region where the copies differ is marked
     instead, without the base's lines.
   - **Without `Base`**, every region where the copies differ is marked, and
     neither side's additions or deletions are known to be newer. Checkout
     tries Git's `HEAD` commit and index; if the ancestor is elsewhere (another
     commit, a stash, a backup), save it to a file and check out with
     `--base <file>` (after `--conflict-discard`, if already checked out).
     Otherwise, infer intent from context, and ask the user when unsure.

   Files that are very large, have too many lines, or aren't valid UTF-8
   can't be merged line by line, so the merged file is a single region with
   each complete copy (and `Base`), and checkout says why: compare each side
   with `Base` (or, without it, the copies with each other) and combine both
   sides' changes rather than keep either copy whole, using ordinary file
   tools to inspect manageable ranges. Binary copies get no merged file:
   create it, or choose a copy with `--take`. If checkout says that a copy
   doesn't end with a newline, the markers may have added one at the end of
   `Merged`; remove it unless the result should end with one.

   Don't redirect a command's output to `Merged` while the command reads it
   (as in `sed ... Merged > Merged` or `grep -v ... Merged > Merged`): the
   shell empties `Merged` before the command runs. Write the output to another
   file, check it, and then move it over `Merged`, or edit `Merged` directly.
   Apply refuses an empty `Merged` unless a copy is empty.

   For other supported conflicts (such as a symbolic link on one side and a
   file on the other), there's nothing to merge: decide which copy to keep.
   To keep content from both, take one copy, then edit it once apply has
   finished; the other copy remains in the workspace's backup.
4. Run `remote --conflict-apply <path>...`. For two files, it writes `Merged`
   to both copies; `--take local` or `--take remote` uses an original copy
   unchanged instead, and `--delete` deletes both copies. If the copies differ
   in executability, add `--executable true` or `--executable false` (or use
   `--take`). For other conflicts, `--take local` or `--take remote` is
   required: it writes the copy taken, as checked out, to the other side.
   Apply then synchronizes and sets the workspace aside once both copies match
   and no conflict or problem involves the path, keeping it for 7 days (the
   output says where) in case the resolution turns out to be wrong. The 7 days
   follow the system clock: backups are named by the time they were set aside
   and deleted by it, so a clock jump can delete them early.
5. Run the project's relevant checks.

If apply fails, a conflicting path changed after checkout, or
`remote --conflicts` lists a workspace whose conflict is no longer listed, read
[conflict-recovery.md](conflict-recovery.md) before retrying or discarding
anything: one side's changes may exist only in the workspace.

Applying and discarding release the checkout, so that either side can check
the conflict out again. If discard reports that it couldn't release the
checkout (for example, while the session is disconnected), discard again
later; `remote --conflicts` also releases checkouts whose workspaces are
missing, such as those of interrupted checkouts.

Conflict operations accept several paths, with options before them, so that
many conflicts can be resolved the same way at once, such as
`remote --conflict-apply --take local <path>...` after checking them out.
`--json` prints results as JSON: the listing as `{"Conflicts": [{"Path",
"Resolvable", "Files", "Take", "CheckedOut", "Changed", "Applied"}],
"Pending": [{"Path", "Applied", "Workspace"}], "RemotePending":
[<path>...]}`, where `CheckedOut` is `"local"` or `"remote"` (checked out
through the other side) for checked-out conflicts, `Changed` marks local
checkouts whose copies changed since checkout, `Applied` marks partially
applied resolutions (apply them again), `Pending` lists workspaces whose
conflicts are no longer listed, with their locations (`Workspace`; those
without `Applied` need the
[recovery steps](conflict-recovery.md#when-a-conflicting-path-changed) before
discarding), and `RemotePending` lists paths no longer listed as conflicts that
the other side has still checked out. Other operations print one line per path, such as a
checkout's `{"Path", "Local", "Remote", "Base", "Merged", "Files", "Binary",
"Regions", "Take"}`, a discard's `{"Path", "Discarded", "Applied", "Backup"}`
(`Applied` if results were applied through the workspace, so that a copy may
have one, or one side's original may exist only in the backup), or an
`"Error"` for a path that failed.
