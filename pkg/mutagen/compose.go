package mutagen

import (
	"context"
	"errors"
	"fmt"

	moby "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"

	"github.com/compose-spec/compose-go/types"

	"github.com/docker/compose/v2/pkg/api"
)

// appendServiceByCopy appends a service definition to a slice of service
// definitions without any risk of overwriting additional capacity in the slice
// that might be in use elsewhere.
func appendServiceByCopy(services types.Services, service types.ServiceConfig) types.Services {
	result := make(types.Services, 0, len(services)+1)
	result = append(result, services...)
	result = append(result, service)
	return result
}

// composeService is a Mutagen-aware implementation of
// github.com/docker/compose/v2/pkg/api.Service that injects Mutagen services
// and dependencies into the project.
type composeService struct {
	// liaison is the parent Mutagen liaison.
	liaison *Liaison
	// service is the underlying Compose service.
	service api.Service
}

// Build implements github.com/docker/compose/v2/pkg/api.Service.Build.
func (s *composeService) Build(ctx context.Context, project *types.Project, options api.BuildOptions) error {
	return s.service.Build(ctx, project, options)
}

// Push implements github.com/docker/compose/v2/pkg/api.Service.Push.
func (s *composeService) Push(ctx context.Context, project *types.Project, options api.PushOptions) error {
	return s.service.Push(ctx, project, options)
}

// Pull implements github.com/docker/compose/v2/pkg/api.Service.Pull.
func (s *composeService) Pull(ctx context.Context, project *types.Project, options api.PullOptions) error {
	// Process Mutagen extensions for the project.
	if err := s.liaison.processProject(project); err != nil {
		return fmt.Errorf("unable to process project: %w", err)
	}

	// Cache the nominal service list.
	services := project.Services

	// Inject the Mutagen service into the project.
	project.Services = appendServiceByCopy(project.Services, s.liaison.mutagenService)

	// Invoke the underlying implementation.
	result := s.service.Pull(ctx, project, options)

	// Restore the service list.
	project.Services = services

	// Done.
	return result
}

// Create implements github.com/docker/compose/v2/pkg/api.Service.Create.
func (s *composeService) Create(ctx context.Context, project *types.Project, options api.CreateOptions) error {
	// Process Mutagen extensions for the project.
	if err := s.liaison.processProject(project); err != nil {
		return fmt.Errorf("unable to process project: %w", err)
	}

	// Cache the nominal service lists.
	services := project.Services
	disabledServices := project.DisabledServices

	// Create the Mutagen Compose sidecar service first. We do this for
	// consistency with Up and for the flag-related reasons outlined there (the
	// hidden start progress updates aren't an issue for Create).
	project.Services = types.Services{s.liaison.mutagenService}
	project.DisabledServices = nil
	mutagenCreateOptions := api.CreateOptions{
		Services:      []string{sidecarServiceName},
		IgnoreOrphans: true,
	}
	if err := s.service.Create(ctx, project, mutagenCreateOptions); err != nil {
		project.Services = services
		project.DisabledServices = disabledServices
		return fmt.Errorf("unable to create Mutagen Compose sidecar service: %w", err)
	}

	// Restore the service lists but keep the Mutagen service defined so that it
	// doesn't appear as an orphan service.
	project.Services = services
	project.DisabledServices = appendServiceByCopy(disabledServices, s.liaison.mutagenService)

	// Invoke the underlying implementation.
	result := s.service.Create(ctx, project, options)

	// Restore the service lists.
	project.DisabledServices = disabledServices

	// Done.
	return result
}

// Start implements github.com/docker/compose/v2/pkg/api.Service.Start.
func (s *composeService) Start(ctx context.Context, projectName string, options api.StartOptions) error {
	// Start the Mutagen Compose sidecar service first. We do this for
	// consistency with Up and for the flag-related reasons outlined there (the
	// hidden start progress updates aren't an issue for Start).
	mutagenStartOptions := api.StartOptions{
		AttachTo: []string{sidecarServiceName},
	}
	if err := s.service.Start(ctx, projectName, mutagenStartOptions); err != nil {
		return fmt.Errorf("unable to start Mutagen Compose sidecar service: %w", err)
	}

	// Invoke the underlying implementation.
	return s.service.Start(ctx, projectName, options)
}

// Restart implements github.com/docker/compose/v2/pkg/api.Service.Restart.
func (s *composeService) Restart(ctx context.Context, projectName string, options api.RestartOptions) error {
	return s.service.Restart(ctx, projectName, options)
}

// Stop implements github.com/docker/compose/v2/pkg/api.Service.Stop.
func (s *composeService) Stop(ctx context.Context, projectName string, options api.StopOptions) error {
	return s.service.Stop(ctx, projectName, options)
}

// Up implements github.com/docker/compose/v2/pkg/api.Service.Up.
func (s *composeService) Up(ctx context.Context, project *types.Project, options api.UpOptions) error {
	// Process Mutagen extensions for the project.
	if err := s.liaison.processProject(project); err != nil {
		return fmt.Errorf("unable to process project: %w", err)
	}

	// Cache the nominal service lists.
	services := project.Services
	disabledServices := project.DisabledServices

	// Bring up the Mutagen Compose sidecar service first. We do this for two
	// reasons: First, we don't want user-specified up flags (which might be
	// incompatible with or inappropriate for Mutagen operation) to affect the
	// Mutagen Compose sidecar service. Second, if the up operation is running
	// attached (which it is by default and in most usage), then only create
	// progress updates are displayed and start updates are hidden since they
	// would conflict with service logs. This is a problem because the progress
	// updates that Liaison.reconcileSessions emits (which are some of the
	// longest-running and most important) appear as part of the start updates.
	//
	// Conceptually, we want Mutagen to be on-par with volumes and networks and
	// other project infrastructure that's initialized pre-services (even though
	// Mutagen support is implemented, in part, by a service). There might be
	// some microscopic performance advantage to be gained by relying on service
	// dependencies to bring up Mutagen only when necessary, but that advantaged
	// is dwarfed by the disadvantages of hiding start up progress updates,
	// allowing Mutagen to be subject to user-specified flags, and the general
	// inconsistency that would arise when relying on depends_on (volumes and
	// networks, for example, are always created when any service starts,
	// regardless of whether or not it depends on them).
	//
	// To do this, we'll need to temporarily modify the service lists to include
	// only the Mutagen service, because although the underlying create call
	// will filter services if a list is specified in the create options, the
	// underlying start call has no such option field. In this case, we'll tell
	// the up operation to ignore orphans, since all other services at that
	// point would be orphans.
	//
	// We also have to perform a stop operation on the Mutagen service before
	// performing the up operation to ensure that session reconciliation occurs
	// if the service is already running. Fortunately this operation has no
	// effect or output if the Mutagen service doesn't yet exist, and no effect
	// if the Mutagen service is already stopped.
	project.Services = types.Services{s.liaison.mutagenService}
	project.DisabledServices = nil
	mutagenStopOptions := api.StopOptions{
		Services: []string{sidecarServiceName},
	}
	mutagenUpOptions := api.UpOptions{
		Create: api.CreateOptions{
			Services:      []string{sidecarServiceName},
			IgnoreOrphans: true,
		},
		Start: api.StartOptions{
			AttachTo: []string{sidecarServiceName},
		},
	}
	if err := s.service.Stop(ctx, project.Name, mutagenStopOptions); err != nil {
		project.Services = services
		project.DisabledServices = disabledServices
		return fmt.Errorf("unable to stop Mutagen Compose sidecar service: %w", err)
	} else if err = s.service.Up(ctx, project, mutagenUpOptions); err != nil {
		project.Services = services
		project.DisabledServices = disabledServices
		return fmt.Errorf("unable to bring up Mutagen Compose sidecar service: %w", err)
	}

	// Restore the service lists but keep the Mutagen service defined so that it
	// doesn't appear as an orphan service.
	project.Services = services
	project.DisabledServices = appendServiceByCopy(disabledServices, s.liaison.mutagenService)

	// Invoke the underlying implementation.
	result := s.service.Up(ctx, project, options)

	// Restore the service lists.
	project.DisabledServices = disabledServices

	// Done.
	return result
}

// Down implements github.com/docker/compose/v2/pkg/api.Service.Down.
func (s *composeService) Down(ctx context.Context, projectName string, options api.DownOptions) error {
	// Process Mutagen extensions for the project.
	if err := s.liaison.processProject(options.Project); err != nil {
		return fmt.Errorf("unable to process project: %w", err)
	}

	// Cache the nominal service list and inject the Mutagen service definition
	// if the project is non-nil.
	var services types.Services
	if options.Project != nil {
		services = options.Project.Services
		options.Project.Services = appendServiceByCopy(options.Project.Services, s.liaison.mutagenService)
	}

	// Invoke the underlying implementation.
	result := s.service.Down(ctx, projectName, options)

	// Restore the service list if the project is non-nil.
	if options.Project != nil {
		options.Project.Services = services
	}

	// Done.
	return result
}

// Logs implements github.com/docker/compose/v2/pkg/api.Service.Logs.
func (s *composeService) Logs(ctx context.Context, projectName string, consumer api.LogConsumer, options api.LogOptions) error {
	return s.service.Logs(ctx, projectName, consumer, options)
}

// Ps implements github.com/docker/compose/v2/pkg/api.Service.Ps.
func (s *composeService) Ps(ctx context.Context, projectName string, options api.PsOptions) ([]api.ContainerSummary, error) {
	// Perform a query to identify the Mutagen Compose sidecar container. We
	// allow it to not exist, but we don't allow multiple matches.
	containers, err := s.liaison.dockerCLI.Client().ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", fmt.Sprintf("%s=%s", api.ProjectLabel, projectName)),
			filters.Arg("label", fmt.Sprintf("%s=%s", sidecarRoleLabelKey, sidecarRoleLabelValue)),
		),
		All: true,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to query Mutagen sidecar container: %w", err)
	} else if len(containers) > 1 {
		return nil, errors.New("multiple Mutagen sidecar containers identified")
	} else if len(containers) == 1 {
		if err := s.liaison.listSessions(ctx, containers[0].ID); err != nil {
			return nil, err
		}
	}

	// Invoke the underlying implementation.
	return s.service.Ps(ctx, projectName, options)
}

// List implements github.com/docker/compose/v2/pkg/api.Service.List.
func (s *composeService) List(ctx context.Context, options api.ListOptions) ([]api.Stack, error) {
	return s.service.List(ctx, options)
}

// Convert implements github.com/docker/compose/v2/pkg/api.Service.Convert.
func (s *composeService) Convert(ctx context.Context, project *types.Project, options api.ConvertOptions) ([]byte, error) {
	return s.service.Convert(ctx, project, options)
}

// Kill implements github.com/docker/compose/v2/pkg/api.Service.Kill.
func (s *composeService) Kill(ctx context.Context, projectName string, options api.KillOptions) error {
	return s.service.Kill(ctx, projectName, options)
}

// RunOneOffContainer implements
// github.com/docker/compose/v2/pkg/api.Service.RunOneOffContainer.
func (s *composeService) RunOneOffContainer(ctx context.Context, project *types.Project, options api.RunOptions) (int, error) {
	return s.service.RunOneOffContainer(ctx, project, options)
}

// Remove implements github.com/docker/compose/v2/pkg/api.Service.Remove.
func (s *composeService) Remove(ctx context.Context, projectName string, options api.RemoveOptions) error {
	return s.service.Remove(ctx, projectName, options)
}

// Exec implements github.com/docker/compose/v2/pkg/api.Service.Exec.
func (s *composeService) Exec(ctx context.Context, projectName string, options api.RunOptions) (int, error) {
	return s.service.Exec(ctx, projectName, options)
}

// Copy implements github.com/docker/compose/v2/pkg/api.Service.Copy.
func (s *composeService) Copy(ctx context.Context, projectName string, options api.CopyOptions) error {
	return s.service.Copy(ctx, projectName, options)
}

// Pause implements github.com/docker/compose/v2/pkg/api.Service.Pause.
func (s *composeService) Pause(ctx context.Context, projectName string, options api.PauseOptions) error {
	return s.service.Pause(ctx, projectName, options)
}

// UnPause implements github.com/docker/compose/v2/pkg/api.Service.UnPause.
func (s *composeService) UnPause(ctx context.Context, projectName string, options api.PauseOptions) error {
	return s.service.UnPause(ctx, projectName, options)
}

// Top implements github.com/docker/compose/v2/pkg/api.Service.Top.
func (s *composeService) Top(ctx context.Context, projectName string, services []string) ([]api.ContainerProcSummary, error) {
	return s.service.Top(ctx, projectName, services)
}

// Events implements github.com/docker/compose/v2/pkg/api.Service.Events.
func (s *composeService) Events(ctx context.Context, projectName string, options api.EventsOptions) error {
	return s.service.Events(ctx, projectName, options)
}

// Port implements github.com/docker/compose/v2/pkg/api.Service.Port.
func (s *composeService) Port(ctx context.Context, projectName string, service string, port int, options api.PortOptions) (string, int, error) {
	return s.service.Port(ctx, projectName, service, port, options)
}

// Images implements github.com/docker/compose/v2/pkg/api.Service.Images.
func (s *composeService) Images(ctx context.Context, projectName string, options api.ImagesOptions) ([]api.ImageSummary, error) {
	return s.service.Images(ctx, projectName, options)
}
