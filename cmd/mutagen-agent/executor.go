package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/mutagen-io/mutagen/cmd"

	"github.com/mutagen-io/mutagen/pkg/agent"
	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// executorMain is the entry point for the executor command.
func executorMain(_ *cobra.Command, _ []string) error {
	// Cancel on termination signals so that registrations are cleaned up and
	// running commands are terminated.
	ctx, cancel := signal.NotifyContext(context.Background(), cmd.TerminationSignals...)
	defer cancel()

	// Set up a logger on the standard error stream.
	logLevel := logging.LevelInfo
	if executorConfiguration.logLevel != "" {
		if l, ok := logging.NameToLevel(executorConfiguration.logLevel); !ok {
			return fmt.Errorf("invalid log level specified: %s", executorConfiguration.logLevel)
		} else {
			logLevel = l
		}
	}
	logger := logging.NewLogger(logLevel, os.Stderr)

	// Create a stream using standard input/output.
	stream := newStdioStream()

	// Perform agent and version handshakes in the background so that
	// termination signals are observed even though standard input/output can't
	// be closed to interrupt them.
	handshakeErrors := make(chan error, 1)
	go func() {
		if err := agent.ServerHandshake(stream); err != nil {
			handshakeErrors <- fmt.Errorf("server handshake failed: %w", err)
		} else if err := mutagen.ServerVersionHandshake(stream); err != nil {
			handshakeErrors <- fmt.Errorf("version handshake error: %w", err)
		} else {
			handshakeErrors <- nil
		}
	}()
	select {
	case err := <-handshakeErrors:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return errors.New("terminated by signal during handshake")
	}

	// Serve the executor until disconnection or termination. The remote
	// executable is installed as a link to the agent if needed.
	if err := remote.ServeChannel(ctx, logger, stream, agent.InstallRemoteExecutable); err != nil {
		return fmt.Errorf("executor terminated: %w", err)
	}
	return nil
}

// executorCommand is the executor command.
var executorCommand = &cobra.Command{
	Use:          agent.CommandExecutor,
	Short:        "Run the agent in remote command executor mode",
	Args:         cmd.DisallowArguments,
	RunE:         executorMain,
	SilenceUsage: true,
}

// executorConfiguration stores configuration for the executor command.
var executorConfiguration struct {
	// help indicates whether or not to show help information and exit.
	help bool
	// logLevel indicates the log level to use.
	logLevel string
}

func init() {
	// Grab a handle for the command line flags.
	flags := executorCommand.Flags()

	// Disable alphabetical sorting of flags in help output.
	flags.SortFlags = false

	// Manually add a help flag to override the default message. Cobra will
	// still implement its logic automatically.
	flags.BoolVarP(&executorConfiguration.help, "help", "h", false, "Show help information")

	// Wire up logging flags.
	flags.StringVar(&executorConfiguration.logLevel, agent.FlagLogLevel, "", "Set the log level")
}
