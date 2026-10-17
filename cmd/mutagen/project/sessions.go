package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"google.golang.org/grpc"

	"github.com/mutagen-io/mutagen/cmd/mutagen/daemon"
	"github.com/mutagen-io/mutagen/cmd/mutagen/forward"
	"github.com/mutagen-io/mutagen/cmd/mutagen/sync"

	"github.com/mutagen-io/mutagen/pkg/configuration/global"
	"github.com/mutagen-io/mutagen/pkg/filesystem/locking"
	"github.com/mutagen-io/mutagen/pkg/forwarding"
	"github.com/mutagen-io/mutagen/pkg/grpcutil"
	"github.com/mutagen-io/mutagen/pkg/identifier"
	"github.com/mutagen-io/mutagen/pkg/project"
	"github.com/mutagen-io/mutagen/pkg/selection"
	forwardingsvc "github.com/mutagen-io/mutagen/pkg/service/forwarding"
	synchronizationsvc "github.com/mutagen-io/mutagen/pkg/service/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
)

// startOrApply starts or reconciles project sessions while holding the project
// lock. A dry run computes the same plan without retaining a new lock file,
// running hooks, or modifying sessions. Like other project commands, it starts
// the daemon if necessary to read the state of a running project's sessions.
func startOrApply(options sessionCreationConfiguration, apply, dryRun bool) error {
	// Compute the name of the configuration file and ensure that our working
	// directory is that in which the file resides. This is required for
	// relative paths (including relative synchronization paths and relative
	// Unix Domain Socket paths) to be resolved relative to the project
	// configuration file.
	configurationFileName := project.DefaultConfigurationFileName
	if options.projectFile != "" {
		var directory string
		directory, configurationFileName = filepath.Split(options.projectFile)
		if directory != "" {
			if err := os.Chdir(directory); err != nil {
				return fmt.Errorf("unable to switch to target directory: %w", err)
			}
		}
	}

	// Compute the lock path.
	lockPath := configurationFileName + project.LockFileExtension

	// Track whether or not we should remove the lock file on return.
	var removeLockFileOnReturn bool

	// Create a locker and defer its closure and potential removal. On Windows
	// systems, we have to handle this removal after the file is closed.
	locker, err := locking.NewLocker(lockPath, 0600)
	if err != nil {
		return fmt.Errorf("unable to create project locker: %w", err)
	}
	defer func() {
		locker.Close()
		if removeLockFileOnReturn && runtime.GOOS == "windows" {
			os.Remove(lockPath)
		}
	}()

	// Acquire the project lock and defer its release and potential removal. On
	// Windows systems, we can't remove the lock file if it's locked or even
	// just opened, so we handle removal for Windows systems after we close the
	// lock file (see above). In this case, we truncate the lock file before
	// releasing it to ensure that any other process that opens or acquires the
	// lock file before we manage to remove it will simply see an empty lock
	// file, which it will ignore or attempt to remove.
	if err := locker.Lock(true); err != nil {
		return fmt.Errorf("unable to acquire project lock: %w", err)
	}
	defer func() {
		if removeLockFileOnReturn {
			if runtime.GOOS == "windows" {
				locker.Truncate(0)
			} else {
				os.Remove(lockPath)
			}
		}
		locker.Unlock()
	}()

	// Read the project identifier and distinguish creation from reconciliation.
	buffer := &bytes.Buffer{}
	if _, err := buffer.ReadFrom(locker); err != nil {
		return fmt.Errorf("unable to read project lock: %w", err)
	}
	projectIdentifier := buffer.String()
	running := projectIdentifier != ""
	if running {
		if !apply {
			return errors.New("project already running")
		} else if !identifier.IsValid(projectIdentifier) {
			return errors.New("invalid project identifier found in project lock")
		}
	} else {
		// Remove the empty lock if validation fails or this is a dry run.
		removeLockFileOnReturn = true
		projectIdentifier, err = identifier.New(identifier.PrefixProject)
		if err != nil {
			return fmt.Errorf("unable to generate project identifier: %w", err)
		}
	}

	// Load the project configuration.
	configuration, err := project.LoadConfiguration(configurationFileName)
	if err != nil {
		return fmt.Errorf("unable to load configuration file: %w", err)
	}

	// Unless disabled, attempt to load configuration from the global
	// configuration file. We allow it to not exist.
	var globalConfiguration *global.Configuration
	if !options.noGlobalConfiguration {
		globalConfigurationPath, err := global.ConfigurationPath()
		if err != nil {
			return fmt.Errorf("unable to compute path to global configuration file: %w", err)
		}
		globalConfiguration, err = global.LoadConfiguration(globalConfigurationPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("unable to load global configuration: %w", err)
			}
			globalConfiguration = nil
		}
	}

	// Compute and validate all desired specifications before modifying
	// sessions.
	specifications, err := project.NewSessionSpecifications(configuration, globalConfiguration, projectIdentifier, options.paused)
	if err != nil {
		return err
	}

	// A new project's dry run needs no daemon. Other operations need either
	// the current session state or a connection for creating sessions.
	var daemonConnection *grpc.ClientConn
	if running || !dryRun {
		daemonConnection, err = daemon.Connect(true, true)
		if err != nil {
			return fmt.Errorf("unable to connect to daemon: %w", err)
		}
		defer daemonConnection.Close()
	}

	// Query only sessions carrying this project's identifier.
	var forwardingStates []*forwarding.State
	var synchronizationStates []*synchronization.State
	if running {
		forwardingStates, synchronizationStates, err = listProjectSessions(daemonConnection, projectIdentifier)
		if err != nil {
			return err
		}
	}

	// Compute the entire plan before any destructive operation. This also
	// catches ambiguous names before either session type is modified.
	plan, err := project.NewPlan(specifications, forwardingStates, synchronizationStates)
	if err != nil {
		return err
	}
	if apply {
		plan.Print(os.Stdout)
	}
	if dryRun {
		return nil
	}

	// Retain the project identifier before running hooks or creating resources,
	// so that an interrupted or partially successful operation can be retried.
	if !running {
		if _, err := locker.Write([]byte(projectIdentifier)); err != nil {
			return fmt.Errorf("unable to write project identifier: %w", err)
		}
		removeLockFileOnReturn = false

		// Creation hooks belong to project startup, not incremental updates.
		for _, command := range configuration.BeforeCreate {
			fmt.Println(">", command)
			if err := runInShell(command); err != nil {
				return fmt.Errorf("pre-create command failed: %w", err)
			}
		}
	}

	// Apply the plan. On failure the lock remains, and the next apply computes
	// a fresh plan from the sessions that actually exist.
	if err := plan.Apply(&sessionOperations{daemonConnection}); err != nil {
		return err
	}

	// Finish project startup hooks only on the initial invocation.
	if !running {
		for _, command := range configuration.AfterCreate {
			fmt.Println(">", command)
			if err := runInShell(command); err != nil {
				return fmt.Errorf("post-create command failed: %w", err)
			}
		}
	}

	// Success.
	return nil
}

// listProjectSessions retrieves and validates both types of project sessions.
func listProjectSessions(connection *grpc.ClientConn, projectIdentifier string) ([]*forwarding.State, []*synchronization.State, error) {
	selection := &selection.Selection{
		LabelSelector: fmt.Sprintf("%s=%s", project.LabelKey, projectIdentifier),
	}
	forwardingResponse, err := forwardingsvc.NewForwardingClient(connection).List(context.Background(), &forwardingsvc.ListRequest{Selection: selection})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to list forwarding sessions: %w", grpcutil.PeelAwayRPCErrorLayer(err))
	} else if err := forwardingResponse.EnsureValid(); err != nil {
		return nil, nil, fmt.Errorf("invalid forwarding list response: %w", err)
	}
	synchronizationResponse, err := synchronizationsvc.NewSynchronizationClient(connection).List(context.Background(), &synchronizationsvc.ListRequest{Selection: selection})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to list synchronization sessions: %w", grpcutil.PeelAwayRPCErrorLayer(err))
	} else if err := synchronizationResponse.EnsureValid(); err != nil {
		return nil, nil, fmt.Errorf("invalid synchronization list response: %w", err)
	}
	return forwardingResponse.SessionStates, synchronizationResponse.SessionStates, nil
}

// sessionOperations implements project.SessionOperations using the daemon and
// the command line session helpers.
type sessionOperations struct {
	// connection is the daemon connection.
	connection *grpc.ClientConn
}

// TerminateForwarding implements project.SessionOperations.TerminateForwarding.
func (o *sessionOperations) TerminateForwarding(identifiers []string) error {
	return forward.TerminateWithSelection(o.connection, &selection.Selection{Specifications: identifiers})
}

// TerminateSynchronization implements
// project.SessionOperations.TerminateSynchronization.
func (o *sessionOperations) TerminateSynchronization(identifiers []string) error {
	return sync.TerminateWithSelection(o.connection, &selection.Selection{Specifications: identifiers})
}

// CreateForwarding implements project.SessionOperations.CreateForwarding.
func (o *sessionOperations) CreateForwarding(specification *forwardingsvc.CreationSpecification) error {
	_, err := forward.CreateWithSpecification(o.connection, specification)
	return err
}

// CreateSynchronization implements
// project.SessionOperations.CreateSynchronization.
func (o *sessionOperations) CreateSynchronization(specification *synchronizationsvc.CreationSpecification) (string, error) {
	return sync.CreateWithSpecification(o.connection, specification)
}

// FlushSynchronization implements project.SessionOperations.FlushSynchronization.
func (o *sessionOperations) FlushSynchronization(identifiers []string) error {
	return sync.FlushWithSelection(o.connection, &selection.Selection{Specifications: identifiers}, false)
}
