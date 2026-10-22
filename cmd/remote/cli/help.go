package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/pflag"
)

// usage is the usage summary printed before the option list. It's kept short
// and covers running commands, which is all that most invocations need, so
// that people and agents unfamiliar with remote can read it in full. Details
// are left to help topics.
const usage = `Usage: remote [options] <command> [<args>...]
       remote [options] --script <file>|- [<args>...]
       remote [options] --detach [--log <file>] <command> [<args>...]
       remote [options] --flush
       remote [options] --diff [<path>...]
       remote [-s <session>] --jobs | --wait <id> | --kill <id>
       remote [-s <session>] --ignores [<path>...] | --pull <path>...
       remote --sessions

Run a command on the opposite endpoint of the Mutagen synchronization session
whose root contains the current directory, in the corresponding directory
there. Standard input, output, error, and the exit status are forwarded. The
session is synchronized before the command, so that it sees local changes, and
after it, so that its changes are present locally when remote exits.

Examples:
  remote pytest -x tests/
  remote sh -c 'make && ./bin/app'      (shell syntax needs a shell or --script)
  remote -e CONDA_ENV=torch python train.py
  remote --timeout 600 make bench       (terminate it after 10 minutes)
  remote --detach --log train.log python train.py
                                        (run it in the background: prints an ID)
  remote --wait <id>                    (wait for that job, then synchronize)
  remote --flush                        (only synchronize)
  remote --script - <<'EOF'             (run shell code as written, with bash)
  for p in a b; do python run.py --preset "$p"; done
  EOF

Your local shell expands variables, globs, and ~ before remote sees them, and
no remote shell interprets the arguments, so use sh -c '...' for anything that
should be expanded remotely, or --script - with a quoted here-document
(<<'EOF'), which your shell passes unchanged. Options must precede the command;
the arguments after --script's file are the script's. No terminal is
allocated, so interactive programs don't work.

Exit status is the command's own, or:
  255  remote failed, such as when no session was found or the connection was
       lost (it prints a remote: error saying why)
  127  command not found
  126  command couldn't be started, or the prologue failed
  124  --timeout expired
With --wait, it's the job's status. With --diff, it's 0 if the copies are
identical and 1 if they differ, and with --ignores, it's 1 if any given path
is ignored.

Options:
`

// helpTopics are the topics shown by remote --help <topic>, in the order in
// which they're listed.
var helpTopics = []struct {
	// name is the topic name.
	name string
	// summary describes the topic in the topic list.
	summary string
	// text is the topic's help text.
	text string
}{
	{"sessions", "which session and directory are used", `remote uses the session whose root most specifically contains the current
directory, and runs commands in the corresponding directory on its opposite
endpoint (remote pwd prints it). Among equally specific roots, such as one
directory synchronized to several machines, the session created first is used.
remote --sessions lists the connected sessions, marking the one used by default
and those whose roots contain the current directory.

  - --session selects a session by name or identifier instead, even if its
    roots don't contain the current directory: commands then run at the root
    of its opposite endpoint, or in --cwd (relative to that root), and require
    --cwd if another session's root contains the directory.
  - An exported MUTAGEN_REMOTE_SESSION only selects among the sessions
    containing the current directory, and is ignored if none of them matches
    it; outside every root, it selects a session as --session does.
  - If several sessions synchronize the current directory (their roots
    contain it and they don't ignore it), remote names them in a warning,
    unless the session was selected with --session or a matching
    MUTAGEN_REMOTE_SESSION.
  - Commands don't run in a directory that the session ignores (or inside
    one), since the session doesn't synchronize it, unless --cwd is given:
    --cwd . runs them in the corresponding directory anyway, such as in an
    ignored build directory that exists on both endpoints. remote --ignores
    lists the session's ignores.
  - Sessions that are paused, disconnected, or don't run commands submitted
    from this side can't be found, so inside the root of such a session that
    is nested in another session's root, the enclosing session is used: if it
    ignores the nested root, as it typically does, commands are refused, and
    otherwise they run in its copy of the directory. --session selecting the
    nested session makes them fail instead.
  - Once remote has located the session, it doesn't switch to another, even
    if that one disconnects.
`},
	{"sync", "synchronization, ignored paths, --pull, and --diff", `  - The session is synchronized before and after the command. Failure to
    synchronize only produces a warning. Keep these synchronizations: without
    the first, a command can silently run outdated code, while an unneeded one
    only costs a rescan of both roots, which typically takes well under a
    second, but longer for very large trees and slow connections. --no-flush
    skips both, so use it only for commands that neither read nor write
    synchronized files, such as nvidia-smi or reading an ignored log; when
    unsure, synchronize. --flush without a command only synchronizes. --timeout
    includes the time spent synchronizing.
  - --detach synchronizes only before starting the job, and --wait only after
    the job ends. --jobs, --kill, --ignores, and --pull don't synchronize.
  - After synchronizing, remote warns about conflicts (files modified on both
    endpoints, which stay unsynchronized until resolved; see
    remote --help conflicts) and other content that couldn't be synchronized,
    such as because of permission errors. Ignored paths are never synchronized
    and aren't reported.
  - Files that a detached job writes arrive through the session's regular
    synchronization, which remote --wait performs once the job ends, and
    remote --flush on demand.
  - Content written to ignored paths exists only on the endpoint that wrote
    it. remote --ignores lists the session's ignores, and
    remote --ignores <path>... shows whether each path is ignored, as the kind
    of entry that it is locally (exit status 1 if any is). remote --pull
    <path>... copies ignored files, directories, and symbolic links from the
    opposite endpoint, replacing local files that differ but never deleting
    local content. It refuses the root, paths that the session (or another
    session) synchronizes, which synchronization updates instead, and
    directories with synchronized content beneath them.
  - remote --diff shows how the remote copies of the given paths, or of the
    conflicting files, differ from the local copies, as a unified diff
    (- local, + remote). For a single-file session, use --session from outside
    its root. Large conflict reports may omit changes and say how many;
    specify paths to compare omitted files. --timeout also covers file reads.
`},
	{"environment", "environment variables and prologues", `  - Commands inherit the environment of the executing endpoint's Mutagen agent
    (or, on the daemon's system, of the daemon), which usually lacks
    interactive shell setup such as conda init. -e and --env-file set
    additional variables (your shell expands -e PATH=$PATH to the local PATH).
  - /bin/sh sources .remote-prologue.sh in the executing endpoint's sync root
    if it exists, otherwise that endpoint's configured remotePrologue, before
    commands submitted from inside the root (even with --cwd). It sees -e
    variables, and the command is looked up with the PATH that it sets.
    --no-prologue skips both. The workspace script replaces the configured
    script; an empty file does nothing, and deleting it restores the
    configured default. Edits take effect on the next command that sees them,
    without a session restart. The file follows normal sync rules; ignored
    files, failed flushes, and --no-flush can leave the executing copy
    outdated. Scripts must use POSIX shell syntax and need no executable
    permission. Failure prevents execution (status 126); prologues aren't
    supported on Windows.
  - Commands and prologues receive MUTAGEN_REMOTE_ENDPOINT=alpha or beta,
    identifying the executing endpoint. Use it to branch in a shared script.
    This overrides inherited values and those passed with -e or --env-file,
    even with --no-prologue or when submitting from outside the root.
    Detached jobs also receive MUTAGEN_REMOTE_JOB, set to their ID.

Example .remote-prologue.sh, activating a conda environment on beta only:
  case "$MUTAGEN_REMOTE_ENDPOINT" in
    beta) eval "$(~/miniconda3/bin/conda shell.posix hook)"
          conda activate "${CONDA_ENV:-base}" ;;
  esac
`},
	{"termination", "timeouts, interruptions, and lost connections", `--timeout takes seconds, or a duration with a unit, such as 10m or 1h30m.
Commands are terminated when it expires, when remote is interrupted, and when
the session stops synchronizing (such as when it's paused or reconnects, or the
daemon stops): their process group receives SIGTERM, then
SIGKILL 5 seconds later if any of it remains (on Windows, the command is killed
at once). If the session stops synchronizing, remote reports that the
connection to the executing endpoint was lost and exits with status 255,
without synchronizing afterwards; files that the command was writing may be
incomplete. Statuses 128+N mean that the command was killed by signal N.

remote waits for every process holding its output. Run commands that should
keep running after remote exits, or after the session stops synchronizing, as
detached jobs (see remote --help jobs), rather than in the background with
nohup.
`},
	{"scripts", "running shell code and other scripts with --script", `Usage: remote [options] --script <file>|- [<args>...]

--script runs a script instead of a command. remote reads it from a local file,
or from standard input for -, typically a here-document, whose contents your
shell passes unchanged if its delimiter is quoted:
  remote --script - --preset a <<'EOF'
  for seed in 1 2 3; do python train.py "$1" "$2" --seed "$seed"; done
  EOF

  - The opposite endpoint runs the script with the interpreter named by its #!
    line (such as #!/usr/bin/env python3), or with bash if it has none, looked
    up after the prologue is sourced. The arguments after the file, even ones
    that look like options, become the script's arguments ($1, $2, ...).
  - The script is copied to a temporary file there, which is removed when the
    script exits, so $0 doesn't name the original file. Run scripts that are
    in the synchronized directory with remote bash <path> instead.
  - With -, the script consumes standard input, so its commands receive none;
    with a file, input is forwarded as for commands. remote bash -s <<'EOF'
    also reads a script from standard input, but commands in it that read
    input then consume the rest of the script, and it can't run detached.
  - Scripts are limited to 512 KiB and aren't supported on Windows endpoints.
`},
	{"jobs", "detached jobs: --detach, --jobs, --wait, and --kill", `Usage: remote [options] --detach [--log <file>] <command> [<args>...]
       remote [options] --detach [--log <file>] --script <file>|- [<args>...]
       remote --jobs
       remote --wait <id> [--timeout <duration>] [--no-flush]
       remote --kill <id>

--detach synchronizes the session, starts the command (or script) as a detached
job on the opposite endpoint, prints the job's ID on standard output, and
exits:
  id=$(remote --detach --log train.log python train.py)
  remote --wait "$id"

  - Jobs keep running when remote exits and when the session stops
    synchronizing (such as when it's paused or reconnects, or the daemon
    stops), unlike other commands. They run in the directory and environment
    that other commands would, prologue included, in a new session and process
    group, with no standard input.
  - Output goes to --log FILE (relative to the command's directory; absolute
    and ~ paths are on the opposite endpoint), which is truncated first, or
    else to a log kept with the job's state in the Mutagen data directory
    there. remote warns if the session ignores the log, which then isn't
    synchronized (see remote --help sync).
  - remote --jobs lists the jobs started at the session's root on the opposite
    endpoint, newest first, with their state (running, exited N, killed N, or
    lost), running time, log, and command.
  - remote --wait ID waits for the job to end, synchronizes the session
    (unless --no-flush), and exits with the job's status. --timeout stops
    waiting (status 124) without affecting the job. If the connection to the
    executing endpoint is lost while waiting, remote retries for a minute. A
    job whose status couldn't be recorded, such as because it was killed with
    SIGKILL or its system restarted, is lost (status 255).
  - remote --kill ID terminates the job: its process group receives SIGTERM,
    then SIGKILL 5 seconds later if any of it remains. Processes that leave
    the group, such as daemons, aren't terminated.
  - Job state is kept for 7 days after jobs end. Logs given with --log are
    never removed.
  - Jobs end with the system or container that runs them, and can end with the
    processes that run the session's endpoint there: for example, when the
    Mutagen daemon runs as a systemd service, stopping the service terminates
    the jobs that the daemon runs, and on systems where logind's
    KillUserProcesses is enabled, jobs on SSH endpoints end with the SSH
    connection.
  - --timeout can't be combined with --detach; run the command with timeout
    instead. Detached jobs aren't supported on Windows endpoints.
`},
	{"conflicts", "resolving files modified on both endpoints", `Usage: remote --conflicts [--json]
       remote --conflict-checkout [--base <file>] [--json] <path>...
       remote --conflict-apply [--take local|remote | --delete]
              [--executable true|false] [--json] <path>...
       remote --conflict-discard [--json] <path>...

Conflicts are files modified on both endpoints, which stay unsynchronized until
resolved. remote resolves them in two-way-safe and one-way-safe sessions:
  remote --conflicts                    (list all conflicts)
  remote --conflict-checkout app.go     (download both copies for merging)
  (edit the merged file that checkout prints, then review all of it)
  remote --conflict-apply app.go        (write the merged file to both copies)

Notes:
  - remote --conflicts synchronizes, lists every conflict, how it can be
    resolved, and through which endpoint it's checked out, flags checked-out
    copies that changed since checkout and partially applied resolutions, and
    lists workspaces whose conflicts are no longer listed, with their
    locations: unfinished resolutions, and workspaces with nothing applied,
    whose conflicts were resolved some other way or ended by a change after
    checkout. Conflict operations accept several paths, with options before
    them.
  - Before checking out, stop anything that writes the conflicting paths on
    either endpoint, and leave them unchanged until apply finishes; changing
    them meanwhile isn't supported. Apply fails without writing a copy that
    changed before it ran, but can overwrite changes made while it runs. If
    they change anyway, stop whatever changed them, then discard the
    workspace and check out again. A change that ends the conflict (such as
    reverting or deleting one copy, so that synchronization copies the other
    over it) may leave one side's changes only in the workspace: compare the
    current file with its local and remote files before discarding it, and
    restore what's needed (on alpha in one-way-safe sessions, which never
    copy beta's changes to alpha). Each conflict is checked out once, through
    one endpoint, which alone can apply or discard it: checking it out again,
    through either endpoint, fails until then. A local checkout may be
    another agent's or the user's resolution in progress, so apply or discard
    only checkouts that you made.
  - --conflict-checkout downloads both complete copies into a workspace (in
    the Mutagen data directory) and prints their paths. For two files, it also
    looks for their common ancestor (the file that both endpoints last agreed
    on) in Git's HEAD commit and index, or in the file given with --base,
    verified by its digest. With it, checkout uses a text-based three-way merge
    model similar to Git's, comparing the ancestor and both current copies.
    Regions changed on one side or identically on both are merged; regions
    changed differently are marked with the local, base, and remote versions.
    Changes separated only by repeated lines (such as closing braces) are
    marked together, even if they look independent, since they can be aligned
    in several ways. Additional conservative checks may mark more conflicts
    than Git, and the results needn't match Git's. Without the ancestor, every
    region where the copies differ is marked. If consistency checks fail, every
    difference is marked, without the base version. Like Git, this model
    doesn't track editing operations, so even a merge without markers can
    misrepresent changes: lines removed and then restored look unchanged, a
    change made on both sides in different places can be kept twice, and moved
    lines, especially repeated ones, can be misplaced or kept too often or too
    rarely. Review the whole merged file against the copies and the ancestor
    before applying it, even if nothing is marked. Files too large to compare
    line by line or not valid UTF-8 are enclosed whole in one region, and
    binary copies get no merged file; checkout says which applies. Checkout
    fails if the copies changed since they were listed (the ancestor might not
    be theirs); check out again. To check out with --base after checking out
    without it, discard the workspace first. Conflicts involving directories
    require manual resolution; checkout and apply reject them.
  - For two files, --conflict-apply writes the merged file (or the copy chosen
    by --take, or deletion with --delete) to both copies, but refuses a merged
    file that still contains conflict markers, or one that's empty while
    neither copy is, which was probably truncated (such as by a command whose
    output was redirected to the file that it reads). For other conflicts
    (such as a symbolic link and a file), only --take is possible: it writes
    the copy taken, as checked out, to the other endpoint. Apply then
    synchronizes and checks that both copies match and no conflict or problem
    involves the path. The workspace, with the original copies, is then kept
    for 7 days (by the system clock, so a clock jump can delete it early) in
    case the resolution was wrong, as is one that --conflict-discard sets
    aside without changing either copy. Both release the checkout, so the
    conflict can be checked out again; --conflicts also releases checkouts
    whose workspaces are missing, such as after an interrupted checkout.
    Discard also sets aside a workspace that can't be read, which checkout
    and apply report.
  - Apply only replaces or deletes a copy that matches a checked-out copy
    (synchronization may copy one over the other), which the workspace keeps,
    or a result that it applied before, so it fails if either copy changed
    since checkout; then discard the workspace and check out again. Retry
    other failures rather than discarding, since a copy may already have the
    result, even if the conflict is no longer listed or the merged file was
    edited since. If nothing was applied and the conflict is no longer
    listed, apply refuses without changing either copy, since it was resolved
    some other way or ended by a change; compare the current file with the
    workspace's copies before discarding it. Copies differing in
    executability need --executable or --take. A conflict operation fails
    while another one on the same path is running on the same system. Writes
    of a copy wait for each other, since an apply that timed out may still be
    writing the remote copy, and apply fails without changing a copy that a
    later apply (through either endpoint) reached first; run --conflicts.

Options:
`},
}

// printHelp prints the usage summary, with the options other than conflict
// options and the list of help topics, or the help for the topic specified as
// an argument. The conflicts topic ends with the conflict options.
func printHelp(o *options, output io.Writer) error {
	if len(o.arguments) == 0 {
		fmt.Fprint(output, usage, flagUsages(o.flags, func(flag *pflag.Flag) bool {
			return o.conflictFlags.Lookup(flag.Name) == nil
		}))
		fmt.Fprintln(output, "\nHelp topics (remote --help <topic>):")
		for _, topic := range helpTopics {
			fmt.Fprintf(output, "  %-13s%s\n", topic.name, topic.summary)
		}
		return nil
	}
	names := make([]string, len(helpTopics))
	for t, topic := range helpTopics {
		if len(o.arguments) == 1 && o.arguments[0] == topic.name {
			fmt.Fprint(output, topic.text)
			if topic.name == "conflicts" {
				fmt.Fprint(output, o.conflictFlags.FlagUsagesWrapped(80))
			}
			return nil
		}
		names[t] = topic.name
	}
	return fmt.Errorf("unknown help topic %q (topics are %s)", strings.Join(o.arguments, " "), strings.Join(names, ", "))
}

// flagUsages formats the usage of the flags in the set that are selected by
// include, in the order in which they were defined.
func flagUsages(flags *pflag.FlagSet, include func(*pflag.Flag) bool) string {
	selected := pflag.NewFlagSet("", pflag.ContinueOnError)
	selected.SortFlags = false
	flags.VisitAll(func(flag *pflag.Flag) {
		if include(flag) {
			selected.AddFlag(flag)
		}
	})
	return selected.FlagUsagesWrapped(80)
}
