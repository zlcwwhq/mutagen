package project

import (
	"github.com/spf13/cobra"

	"github.com/mutagen-io/mutagen/cmd"
)

// startMain is the entry point for the start command.
func startMain(_ *cobra.Command, _ []string) error {
	return startOrApply(startConfiguration, false, false)
}

// startCommand is the start command.
var startCommand = &cobra.Command{
	Use:          "start",
	Short:        "Start project sessions",
	Args:         cmd.DisallowArguments,
	RunE:         startMain,
	SilenceUsage: true,
}

// startConfiguration stores configuration for the start command.
var startConfiguration sessionCreationConfiguration

// sessionCreationConfiguration stores flags shared by start and apply.
type sessionCreationConfiguration struct {
	// help indicates whether or not to show help information and exit.
	help bool
	// projectFile is the path to the project file, if non-default.
	projectFile string
	// paused indicates whether or not to create sessions in a pre-paused state.
	paused bool
	// noGlobalConfiguration specifies whether or not the global configuration
	// file should be ignored.
	noGlobalConfiguration bool
}

// registerSessionCreationFlags wires up the flags shared by start and apply.
func registerSessionCreationFlags(command *cobra.Command, configuration *sessionCreationConfiguration, pausedUsage string) {
	// Grab a handle for the command line flags.
	flags := command.Flags()

	// Disable alphabetical sorting of flags in help output.
	flags.SortFlags = false

	// Manually add a help flag to override the default message. Cobra will
	// still implement its logic automatically.
	flags.BoolVarP(&configuration.help, "help", "h", false, "Show help information")

	// Wire up project file flags.
	flags.StringVarP(&configuration.projectFile, "project-file", "f", "", "Specify project file")

	// Wire up paused flags.
	flags.BoolVarP(&configuration.paused, "paused", "p", false, pausedUsage)

	// Wire up general configuration flags.
	flags.BoolVar(&configuration.noGlobalConfiguration, "no-global-configuration", false, "Ignore the global configuration file")
}

func init() {
	// Wire up command line flags.
	registerSessionCreationFlags(startCommand, &startConfiguration, "Create the session pre-paused")
}
