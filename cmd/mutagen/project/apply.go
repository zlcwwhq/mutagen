package project

import (
	"github.com/spf13/cobra"

	"github.com/mutagen-io/mutagen/cmd"
)

// applyMain is the entry point for the apply command.
func applyMain(_ *cobra.Command, _ []string) error {
	return startOrApply(applyConfiguration.sessionCreationConfiguration, true, applyConfiguration.dryRun)
}

// applyCommand is the apply command.
var applyCommand = &cobra.Command{
	Use:   "apply",
	Short: "Apply project configuration incrementally",
	Long: `Apply project configuration incrementally, starting the project if needed.

Sessions are matched by type and name within the project. Unchanged sessions
retain their identifiers, synchronization history, caches, and paused state.
New sessions are created, removed sessions are terminated, and sessions with
changed endpoints or configurations are terminated and recreated. This includes
changes to the remote and remotePrologue settings of synchronization sessions,
even though they only affect remote commands. Recreated sessions retain their
paused state unless --paused is specified, which pauses all newly created
sessions. Renaming a session removes it and creates a new one.

Recreated synchronization sessions, like those created under a new name, start
with fresh history, so they can't tell which endpoint changed a file. Files that
differ between the endpoints become conflicts in two-way-safe and one-way-safe
modes and are overwritten with alpha's version in two-way-resolved and
one-way-replica modes. Files that exist on only one endpoint are copied to the
other, which undoes unsynchronized deletions, except that one-way-safe keeps
files that exist only on beta and one-way-replica deletes them.

To protect changes that haven't been synchronized, apply flushes the running
synchronization sessions that it replaces or removes before terminating any
session, together with the running sessions (including retained ones) that share
their workspaces, repeatedly, until changes have propagated across them, so
that new sessions don't copy back files whose deletion hadn't reached every
workspace. Sessions share a workspace if their endpoints are equal or nested on
the same configured host. Connected sessions are flushed in D+1 rounds, where D
is the largest number of steps between two of them (two rounds for a star), or
in as many rounds as there are sessions if their workspaces form a cycle,
while a session that shares no workspace is flushed once. If a flush fails,
nothing is terminated or created: fix the session and apply again, or terminate
it with "mutagen sync terminate" and apply again if its unsynchronized changes
don't matter (a retained session can instead be paused, since it keeps its
history). Changes that a flush leaves unsynchronized (such as those blocked by
problems) and changes made while apply runs aren't protected, so stop processes
that modify synchronized files before replacing or renaming their sessions.
Paused sessions are neither flushed nor resumed, so changes made since a session
was paused aren't protected either: resume it before applying to synchronize
them first.

Docker environment variables (such as DOCKER_HOST) captured by endpoint URLs are
not compared. Retained sessions keep the environment they were created with,
while new and recreated pair sessions use the current environment. In a star,
new or recreated edges reuse the center's endpoint from the star's retained
edges, including its captured environment, so the center retains one identity.

Creation hooks run only when starting a project. Applying changes to an existing
project does not run lifecycle hooks. flushOnCreate applies only to sessions
created by this invocation. Use the same --no-global-configuration setting as
when starting the project.

Changes are not transactional. If an operation fails, completed changes remain
in place; fix the problem and run apply again to reconcile the remaining changes.
Use --dry-run to preview changes without modifying sessions or running hooks.`,
	Args:         cmd.DisallowArguments,
	RunE:         applyMain,
	SilenceUsage: true,
}

// applyConfiguration stores configuration for the apply command.
var applyConfiguration struct {
	sessionCreationConfiguration
	// dryRun indicates whether to print changes without applying them.
	dryRun bool
}

func init() {
	// Wire up flags shared with start.
	registerSessionCreationFlags(applyCommand, &applyConfiguration.sessionCreationConfiguration, "Create new and replacement sessions pre-paused")

	// Wire up dry run flags.
	applyCommand.Flags().BoolVar(&applyConfiguration.dryRun, "dry-run", false, "Preview changes without applying them")
}
