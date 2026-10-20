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
before it runs, but it can overwrite changes made while it runs. Resolve each
conflict from one side only: workspaces are kept on the side that checked them
out, and the check that refuses a concurrent conflict operation on a path
doesn't cover operations started on the other side.

1. Run `remote --conflicts`. It synchronizes and lists every conflict, with
   paths relative to the current directory. Conflicts that aren't between two
   files say which copies `--take` can keep. Lines marked as something remote
   can't resolve need the manual approach. Conflicts involving directories
   are rejected by checkout and apply; report these to the user. Workspaces
   whose conflicts are no longer listed come at the end. Finish resolutions
   that were applied but left unfinished first (step 4). Workspaces with
   nothing applied belong to conflicts that were resolved some other way, such
   as by the user or from the other side: apply refuses them, so check that
   the copies are what the user wants, then discard them.
2. Run `remote --conflict-checkout <path>...`. For each path, it prints the
   paths of the files in a workspace outside the synced tree: `Local` and
   `Remote` (both complete copies) and, for two files, `Merged` and, if found,
   `Base`. Checking out again prints the same workspace and keeps your edits
   (with `--base`, it fails instead; discard the workspace first).
   If checkout says that the copies changed since conflicts were listed (such
   as by an edit while it ran), check out again.
3. For two files, edit `Merged`. Conflicting regions are enclosed in markers:

   ```
   <<<<<<< local <token>
   (local lines)
   ||||||| base <token>
   (common ancestor's lines, only if Base was found)
   ======= <token>
   (remote lines)
   >>>>>>> remote <token>
   ```

   - **With `Base`** (the file that both sides last agreed on, verified by its
     digest), changes made on only one side are already merged, and only
     regions changed differently on both sides are marked; checkout says how
     many. Compare each side with the base to see what each changed, combine
     both changes, and remove the markers. Review a clean merge before
     applying it.
   - **Without `Base`**, every region where the copies differ is marked, and
     neither side's additions or deletions are known to be newer. Checkout
     tries Git's `HEAD` commit and index; if the ancestor is elsewhere (another
     commit, a stash, a backup), save it to a file and check out with
     `--base <file>` (after `--conflict-discard`, if already checked out).
     Otherwise, infer intent from context, and ask the user when unsure.

   Very large files are enclosed in a single region; use ordinary file tools
   to inspect manageable ranges. Binary copies get no merged file: create it,
   or choose a copy with `--take`. If checkout says that a copy doesn't end
   with a newline, the markers may have added one at the end of `Merged`;
   remove it unless the result should end with one.

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
   output says where) in case the resolution turns out to be wrong.
5. Run the project's relevant checks.

If apply reports that a copy changed since checkout, it didn't change that copy
(if it also says that it updated the remote copy, that copy already has your
result): run `remote --conflict-discard <path>` (which changes neither copy),
check out again, and merge the new versions. Retry any other failed apply,
including one that was interrupted (`remote --conflicts` lists it as
unfinished), after fixing `Merged` if needed: synchronization may have copied
one original over a partially applied result, which removes the conflict
without applying your result. A copy that already has the result is left
alone, a copy with a result applied before is replaced, and the workspace
remains until both copies match. If apply reports a problem that
synchronization has with the path (such as a permission error), it updated the
copies but couldn't verify the resolution: fix the problem if you can (or
report it to the user), then retry. If nothing was applied and the conflict is
no longer listed, apply refuses without changing either copy, since the
conflict was resolved some other way: discard the workspace (its copies remain
in the backup). Existing workspaces involving directories can only be
discarded, not applied. A conflict operation on a path fails while another one
on it is in progress on the same side, without changing anything.

Conflict operations accept several paths, with options before them, so that
many conflicts can be resolved the same way at once, such as
`remote --conflict-apply --take local <path>...` after checking them out.
`--json` prints results as JSON: the listing as `{"Conflicts": [{"Path",
"Resolvable", "Files", "Take", "CheckedOut"}], "Pending": [{"Path",
"Applied"}]}`, where `Pending` lists workspaces whose conflicts are no longer
listed and `Applied` marks unfinished resolutions (the others are to be
discarded), and other operations as one line per path, such as a checkout's
`{"Path", "Local", "Remote", "Base", "Merged", "Files", "Binary", "Regions",
"Take", "Pending", "Applied"}` or an `"Error"` for a path that failed.
