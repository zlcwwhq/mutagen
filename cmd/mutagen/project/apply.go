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
changed endpoints or configurations are terminated and recreated. Recreated
sessions retain their paused state unless --paused is specified, which pauses
all newly created sessions. Renaming a session removes it and creates a new one.

Docker environment variables (such as DOCKER_HOST) captured by endpoint URLs are
not compared. Retained sessions keep the environment they were created with,
while new and recreated sessions use the current environment.

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
