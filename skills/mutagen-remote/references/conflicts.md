# Resolving conflicts

`remote` resolves conflicts between two files in sessions using the two-way-safe
synchronization mode (the default). For other conflicts, see "Conflicts and
unsynchronized content" in [troubleshooting.md](troubleshooting.md).

1. Run `remote --conflicts`. It synchronizes and lists every conflict, with
   paths relative to the current directory. Lines marked as something remote
   can't resolve (directories, symbolic links) need the manual approach.
2. Run `remote --conflict-checkout <path>`. It prints the paths of three files
   in a workspace outside the synced tree: `Local` and `Remote` (both complete
   copies) and `Merged`. Checking out again prints the same workspace and keeps
   your edits.
3. Edit `Merged`. Regions where the copies differ are enclosed in markers:

   ```
   <<<<<<< local <token>
   (local lines)
   ======= <token>
   (remote lines)
   >>>>>>> remote <token>
   ```

   There is no common base version, so neither side's additions or deletions
   are known to be newer: combine both sides' intended changes and remove all
   markers. If the project uses version control, the last committed version
   (such as `git show HEAD:<path>`) often approximates the base and shows
   which side changed a region. Very large files are enclosed in a single
   region; use ordinary file tools to inspect manageable ranges. Binary copies
   get no merged file: create it, or choose a copy with `--take`. If checkout
   says that a copy doesn't end with a newline, the markers may have added one
   at the end of `Merged`; remove it unless the result should end with one.
4. Run `remote --conflict-apply <path>`. It writes `Merged` to both copies,
   synchronizes, and removes the workspace once both copies have the result
   and no conflict involves the file. Instead, `--take local` or
   `--take remote` uses an original copy unchanged, and `--delete` deletes
   both copies. If the copies differ in executability, add `--executable true`
   or `--executable false` (or use `--take`).
5. Run the project's relevant checks.

If apply reports that a copy changed since checkout, it didn't overwrite that
change (if it also says that it updated the remote copy, that copy already has
your result): run `remote --conflict-discard <path>` (which changes neither
copy), check out again, and merge the new versions. Retry any other failed
apply (after fixing `Merged` if needed), even if `remote --conflicts` no longer
lists the file: synchronization may have copied one original over a partially
applied result, which removes the conflict without applying your result. A
copy that already has the result is left alone, a copy with a result applied
before is replaced, and the workspace remains until both copies have the
result. Run conflict operations on a file one at a time: one that finds
another in progress fails without changing anything.

`--json` prints results as JSON: the listing as `{"Conflicts": [{"Path",
"Resolvable"}]}`, a checkout as `{"Local", "Remote", "Merged", "Binary"}`.
