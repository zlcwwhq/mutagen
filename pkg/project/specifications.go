package project

import (
	"fmt"
	"sort"

	"github.com/mutagen-io/mutagen/pkg/configuration/global"
	"github.com/mutagen-io/mutagen/pkg/forwarding"
	"github.com/mutagen-io/mutagen/pkg/selection"
	forwardingsvc "github.com/mutagen-io/mutagen/pkg/service/forwarding"
	synchronizationsvc "github.com/mutagen-io/mutagen/pkg/service/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// SessionSpecifications contains validated session specifications for a
// project.
type SessionSpecifications struct {
	// Forwarding contains forwarding session specifications sorted by name.
	Forwarding []*forwardingsvc.CreationSpecification
	// Synchronization contains synchronization session specifications sorted
	// by name.
	Synchronization []*synchronizationsvc.CreationSpecification
	// FlushOnCreate records flush-on-creation behavior by synchronization
	// session name.
	FlushOnCreate map[string]bool
}

// NewSessionSpecifications computes the session specifications for a project,
// layering session configurations on top of project defaults and (if non-nil)
// global configuration. Relative local endpoint paths are resolved against the
// working directory, which should be the project directory. All validation
// that can be performed before session creation is completed here, so that
// invalid configuration is detected before any sessions are modified.
func NewSessionSpecifications(
	configuration *Configuration,
	globalConfiguration *global.Configuration,
	projectIdentifier string,
	paused bool,
) (*SessionSpecifications, error) {
	// If provided, use the global configuration as the base for our core
	// session configurations.
	globalConfigurationForwarding := &forwarding.Configuration{}
	globalConfigurationSynchronization := &synchronization.Configuration{}
	if globalConfiguration != nil {
		globalConfigurationForwarding = globalConfiguration.Forwarding.Defaults.ToInternal()
		if err := globalConfigurationForwarding.EnsureValid(false); err != nil {
			return nil, fmt.Errorf("invalid global forwarding configuration: %w", err)
		}
		globalConfigurationSynchronization = globalConfiguration.Synchronization.Defaults.ToInternal()
		if err := globalConfigurationSynchronization.EnsureValid(false); err != nil {
			return nil, fmt.Errorf("invalid global synchronization configuration: %w", err)
		}
	}

	// Extract and validate forwarding defaults.
	var defaultSource, defaultDestination string
	defaultConfigurationForwarding := &forwarding.Configuration{}
	defaultConfigurationSource := &forwarding.Configuration{}
	defaultConfigurationDestination := &forwarding.Configuration{}
	if defaults, ok := configuration.Forwarding["defaults"]; ok {
		defaultSource = defaults.Source
		defaultDestination = defaults.Destination
		defaultConfigurationForwarding = defaults.Configuration.ToInternal()
		if err := defaultConfigurationForwarding.EnsureValid(false); err != nil {
			return nil, fmt.Errorf("invalid default forwarding configuration: %w", err)
		}
		defaultConfigurationSource = defaults.ConfigurationSource.ToInternal()
		if err := defaultConfigurationSource.EnsureValid(true); err != nil {
			return nil, fmt.Errorf("invalid default forwarding source configuration: %w", err)
		}
		defaultConfigurationDestination = defaults.ConfigurationDestination.ToInternal()
		if err := defaultConfigurationDestination.EnsureValid(true); err != nil {
			return nil, fmt.Errorf("invalid default forwarding destination configuration: %w", err)
		}
	}

	// Extract and validate synchronization defaults.
	var defaultAlpha, defaultBeta string
	var defaultFlushOnCreate FlushOnCreateBehavior
	defaultConfigurationSynchronization := &synchronization.Configuration{}
	defaultConfigurationAlpha := &synchronization.Configuration{}
	defaultConfigurationBeta := &synchronization.Configuration{}
	if defaults, ok := configuration.Synchronization["defaults"]; ok {
		defaultAlpha = defaults.Alpha
		defaultBeta = defaults.Beta
		defaultFlushOnCreate = defaults.FlushOnCreate
		defaultConfigurationSynchronization = defaults.Configuration.ToInternal()
		if err := defaultConfigurationSynchronization.EnsureValid(false); err != nil {
			return nil, fmt.Errorf("invalid default synchronization configuration: %w", err)
		}
		defaultConfigurationAlpha = defaults.ConfigurationAlpha.ToInternal()
		if err := defaultConfigurationAlpha.EnsureValid(true); err != nil {
			return nil, fmt.Errorf("invalid default synchronization alpha configuration: %w", err)
		}
		defaultConfigurationBeta = defaults.ConfigurationBeta.ToInternal()
		if err := defaultConfigurationBeta.EnsureValid(true); err != nil {
			return nil, fmt.Errorf("invalid default synchronization beta configuration: %w", err)
		}
	}

	// Merge global and default configurations, with defaults taking priority.
	defaultConfigurationForwarding = forwarding.MergeConfigurations(
		globalConfigurationForwarding,
		defaultConfigurationForwarding,
	)
	defaultConfigurationSynchronization = synchronization.MergeConfigurations(
		globalConfigurationSynchronization,
		defaultConfigurationSynchronization,
	)

	// Generate forward session creation specifications.
	var forwardingSpecifications []*forwardingsvc.CreationSpecification
	for name, session := range configuration.Forwarding {
		// Ignore defaults.
		if name == "defaults" {
			continue
		}

		// Verify that the name is valid.
		if err := selection.EnsureNameValid(name); err != nil {
			return nil, fmt.Errorf("invalid forwarding session name (%s): %v", name, err)
		}

		// Compute URLs.
		source := session.Source
		if source == "" {
			source = defaultSource
		}
		destination := session.Destination
		if destination == "" {
			destination = defaultDestination
		}

		// Parse URLs.
		sourceURL, err := url.Parse(source, url.Kind_Forwarding, true)
		if err != nil {
			return nil, fmt.Errorf("unable to parse forwarding source URL (%s): %v", source, err)
		}
		destinationURL, err := url.Parse(destination, url.Kind_Forwarding, false)
		if err != nil {
			return nil, fmt.Errorf("unable to parse forwarding destination URL (%s): %v", destination, err)
		}

		// Compute configuration.
		configuration := session.Configuration.ToInternal()
		if err := configuration.EnsureValid(false); err != nil {
			return nil, fmt.Errorf("invalid forwarding session configuration for %s: %v", name, err)
		}
		configuration = forwarding.MergeConfigurations(defaultConfigurationForwarding, configuration)

		// Compute source-specific configuration.
		sourceConfiguration := session.ConfigurationSource.ToInternal()
		if err := sourceConfiguration.EnsureValid(true); err != nil {
			return nil, fmt.Errorf("invalid forwarding session source configuration for %s: %v", name, err)
		}
		sourceConfiguration = forwarding.MergeConfigurations(defaultConfigurationSource, sourceConfiguration)

		// Compute destination-specific configuration.
		destinationConfiguration := session.ConfigurationDestination.ToInternal()
		if err := destinationConfiguration.EnsureValid(true); err != nil {
			return nil, fmt.Errorf("invalid forwarding session destination configuration for %s: %v", name, err)
		}
		destinationConfiguration = forwarding.MergeConfigurations(defaultConfigurationDestination, destinationConfiguration)

		// Record the specification.
		forwardingSpecifications = append(forwardingSpecifications, &forwardingsvc.CreationSpecification{
			Source:                   sourceURL,
			Destination:              destinationURL,
			Configuration:            configuration,
			ConfigurationSource:      sourceConfiguration,
			ConfigurationDestination: destinationConfiguration,
			Name:                     name,
			Labels: map[string]string{
				LabelKey: projectIdentifier,
			},
			Paused: paused,
		})
	}

	// Generate synchronization session creation specifications and keep track
	// of those that we should flush on creation.
	var synchronizationSpecifications []*synchronizationsvc.CreationSpecification
	flushOnCreate := make(map[string]bool)
	for name, session := range configuration.Synchronization {
		// Ignore defaults.
		if name == "defaults" {
			continue
		}

		// Verify that the name is valid.
		if err := selection.EnsureNameValid(name); err != nil {
			return nil, fmt.Errorf("invalid synchronization session name (%s): %v", name, err)
		}

		// Compute URLs.
		alpha := session.Alpha
		if alpha == "" {
			alpha = defaultAlpha
		}
		beta := session.Beta
		if beta == "" {
			beta = defaultBeta
		}

		// Parse URLs.
		alphaURL, err := url.Parse(alpha, url.Kind_Synchronization, true)
		if err != nil {
			return nil, fmt.Errorf("unable to parse synchronization alpha URL (%s): %v", alpha, err)
		}
		betaURL, err := url.Parse(beta, url.Kind_Synchronization, false)
		if err != nil {
			return nil, fmt.Errorf("unable to parse synchronization beta URL (%s): %v", beta, err)
		}

		// Compute configuration.
		configuration := session.Configuration.ToInternal()
		if err := configuration.EnsureValid(false); err != nil {
			return nil, fmt.Errorf("invalid synchronization session configuration for %s: %v", name, err)
		}
		configuration = synchronization.MergeConfigurations(defaultConfigurationSynchronization, configuration)

		// Compute alpha-specific configuration.
		alphaConfiguration := session.ConfigurationAlpha.ToInternal()
		if err := alphaConfiguration.EnsureValid(true); err != nil {
			return nil, fmt.Errorf("invalid synchronization session alpha configuration for %s: %v", name, err)
		}
		alphaConfiguration = synchronization.MergeConfigurations(defaultConfigurationAlpha, alphaConfiguration)

		// Compute beta-specific configuration.
		betaConfiguration := session.ConfigurationBeta.ToInternal()
		if err := betaConfiguration.EnsureValid(true); err != nil {
			return nil, fmt.Errorf("invalid synchronization session beta configuration for %s: %v", name, err)
		}
		betaConfiguration = synchronization.MergeConfigurations(defaultConfigurationBeta, betaConfiguration)

		// Validate ignores in the same way as the daemon does when creation is
		// requested. They can only be validated once the configuration layers
		// have been merged.
		if _, err := synchronization.NewIgnorer(configuration, synchronization.DefaultVersion); err != nil {
			return nil, fmt.Errorf("invalid synchronization session ignores for %s: %w", name, err)
		}

		// Record the specification.
		synchronizationSpecifications = append(synchronizationSpecifications, &synchronizationsvc.CreationSpecification{
			Alpha:              alphaURL,
			Beta:               betaURL,
			Configuration:      configuration,
			ConfigurationAlpha: alphaConfiguration,
			ConfigurationBeta:  betaConfiguration,
			Name:               name,
			Labels: map[string]string{
				LabelKey: projectIdentifier,
			},
			Paused: paused,
		})

		// Compute and store flush-on-creation behavior.
		if session.FlushOnCreate.IsDefault() {
			flushOnCreate[name] = defaultFlushOnCreate.FlushOnCreate()
		} else {
			flushOnCreate[name] = session.FlushOnCreate.FlushOnCreate()
		}
	}

	// Expand the workspace tree after validating the ordinary sessions. Refuse
	// name collisions and extra synchronization edges touching its workspaces.
	tree, err := treeSpecifications(configuration.Tree, defaultConfigurationSynchronization, projectIdentifier, paused)
	if err != nil {
		return nil, fmt.Errorf("invalid workspace tree: %w", err)
	}
	for _, edge := range tree {
		for _, ordinary := range synchronizationSpecifications {
			if edge.Name == ordinary.Name {
				return nil, fmt.Errorf("synchronization name %s is reserved by the tree", edge.Name)
			}
			for _, a := range []*url.URL{edge.Alpha, edge.Beta} {
				for _, b := range []*url.URL{ordinary.Alpha, ordinary.Beta} {
					if synchronization.TreeEndpointsOverlap(a, b) {
						return nil, fmt.Errorf("session %s overlaps a workspace tree node", ordinary.Name)
					}
				}
			}
		}
	}
	synchronizationSpecifications = append(synchronizationSpecifications, tree...)

	// Use a deterministic creation order independent of YAML map iteration.
	sort.Slice(forwardingSpecifications, func(i, j int) bool {
		return forwardingSpecifications[i].Name < forwardingSpecifications[j].Name
	})
	sort.Slice(synchronizationSpecifications, func(i, j int) bool {
		return synchronizationSpecifications[i].Name < synchronizationSpecifications[j].Name
	})

	// Success.
	return &SessionSpecifications{
		Forwarding:      forwardingSpecifications,
		Synchronization: synchronizationSpecifications,
		FlushOnCreate:   flushOnCreate,
	}, nil
}
