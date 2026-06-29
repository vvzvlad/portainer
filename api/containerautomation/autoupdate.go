package containerautomation

import (
	"context"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/docker/images"
	"github.com/portainer/portainer/api/internal/endpointutils"
	"github.com/portainer/portainer/api/stacks/deployments"
	"github.com/portainer/portainer/api/stacks/stackutils"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/rs/zerolog/log"
)

const (
	// statusCheckTimeout bounds a single container image-status resolution
	// (container inspect + remote digest fetch).
	statusCheckTimeout = 30 * time.Second
	// recreateTimeout bounds a standalone recreate (pull + stop + create + start).
	// Pulls can be slow, so it is generous.
	recreateTimeout = 10 * time.Minute
	// stackRedeployTimeout bounds a single stack redeploy-with-pull.
	stackRedeployTimeout = 15 * time.Minute
)

// update runs a single auto-update pass over every reachable Docker endpoint.
// It is registered with the scheduler and guarded against overlapping ticks by
// the Service. Errors are logged per endpoint/container so one failure does not
// abort the whole pass; it always returns nil so the scheduler keeps the job.
func (s *Service) update() error {
	if !s.updateRunning.CompareAndSwap(false, true) {
		log.Debug().Msg("auto-update: previous run still in progress, skipping tick")
		return nil
	}
	defer s.updateRunning.Store(false)

	settings, err := s.dataStore.Settings().Settings()
	if err != nil {
		log.Warn().Err(err).Msg("auto-update: unable to read settings")
		return nil
	}

	scope := ScopeLabeled
	if settings.ContainerAutomation.AutoUpdate.Scope == ScopeAll {
		scope = ScopeAll
	}

	cleanup := settings.ContainerAutomation.AutoUpdate.Cleanup

	endpoints, err := s.dataStore.Endpoint().Endpoints()
	if err != nil {
		log.Warn().Err(err).Msg("auto-update: unable to list environments")
		return nil
	}

	for i := range endpoints {
		endpoint := &endpoints[i]

		// Native Docker endpoints only: Kubernetes is not applicable and
		// Edge/async endpoints are not reachable synchronously from the scheduler.
		if !endpointutils.IsDockerEndpoint(endpoint) || endpointutils.IsEdgeEndpoint(endpoint) {
			continue
		}

		s.updateEndpoint(endpoint, scope, cleanup)
	}

	return nil
}

// updateEndpoint applies image updates to the in-scope, outdated containers of a
// single endpoint, routing each container to the standalone / stack / external
// apply path. Stack-managed candidates are grouped so each owning stack is
// redeployed at most once per tick.
func (s *Service) updateEndpoint(endpoint *portainer.Endpoint, scope string, cleanup bool) {
	endpointID := int(endpoint.ID)

	// Swarm note (M4 limitation, mirrors auto-heal): we connect to the endpoint's
	// primary node only (nodeName ""). Containers scheduled on other Swarm nodes
	// are not updated here; stacks are redeployed cluster-wide by the swarm engine.
	clientTimeout := endpointTimeout
	cli, err := s.clientFactory.CreateClient(endpoint, "", &clientTimeout)
	if err != nil {
		log.Warn().Err(err).Int("endpoint_id", endpointID).Msg("auto-update: unable to create Docker client")
		return
	}
	defer cli.Close()

	listCtx, cancel := context.WithTimeout(context.Background(), endpointTimeout)
	defer cancel()

	// Running containers only: a stopped container has nothing to update now and
	// would be started by a bare recreate.
	containers, err := cli.ContainerList(listCtx, container.ListOptions{All: false})
	if err != nil {
		log.Warn().Err(err).Int("endpoint_id", endpointID).Msg("auto-update: unable to list containers")
		return
	}

	// Collect the in-scope, outdated, non-monitor-only containers as candidates;
	// monitor-only ones are still status-checked so the badge cache stays warm.
	var candidates []UpdateCandidate
	for _, c := range containers {
		if !InUpdateScope(scope, c.Labels) {
			continue
		}

		// Resolve the image status. This also refreshes the package-level status
		// cache that backs the badge, so monitor-only containers are still checked.
		statusCtx, statusCancel := context.WithTimeout(context.Background(), statusCheckTimeout)
		status, err := s.digestClient.ContainerImageStatus(statusCtx, c.ID, endpoint, "")
		statusCancel()
		if err != nil {
			// Pull / registry-auth / network failure: leave the running container
			// untouched, never recreate on a failed check.
			log.Warn().Err(err).Str("container_id", c.ID).Int("endpoint_id", endpointID).
				Msg("auto-update: image status check failed, leaving container untouched")
			continue
		}

		if status != images.Outdated {
			continue
		}

		// Monitor-only: detect-only, never auto-apply (status already cached above).
		if IsMonitorOnly(c.Labels) {
			log.Info().Str("container_id", c.ID).Int("endpoint_id", endpointID).
				Msg("auto-update: outdated image detected but container is monitor-only, not applying")
			continue
		}

		candidates = append(candidates, UpdateCandidate{ID: c.ID, ImageID: c.ImageID, Labels: c.Labels})
	}

	// Route and de-duplicate: one redeploy per stack per tick.
	grouped := groupContainersForUpdate(candidates, s.stackLookupForEndpoint(endpoint.ID))

	for _, ext := range grouped.External {
		log.Debug().Str("container_id", ext.ID).Int("endpoint_id", endpointID).
			Msg("auto-update: outdated externally-managed compose container, detect only")
	}

	for _, c := range grouped.Standalone {
		s.updateStandalone(cli, endpoint, c, cleanup)
	}

	for _, st := range grouped.Stacks {
		s.updateStack(endpoint, st)
	}
}

// stackLookupForEndpoint builds a compose-project-name -> Portainer compose stack
// resolver for a single endpoint. Only Docker Compose stacks on this endpoint
// match; a same-named swarm/kubernetes stack is treated as external (mirrors
// M3's resolveContainerUpdatePath).
func (s *Service) stackLookupForEndpoint(endpointID portainer.EndpointID) func(project string) *StackMatch {
	stacks, err := s.dataStore.Stack().ReadAll()
	if err != nil {
		log.Warn().Err(err).Int("endpoint_id", int(endpointID)).
			Msg("auto-update: unable to read stacks, treating compose containers as external")
		return func(string) *StackMatch { return nil }
	}

	byName := make(map[string]*StackMatch)
	for i := range stacks {
		st := &stacks[i]
		if st.EndpointID != endpointID || st.Type != portainer.DockerComposeStack {
			continue
		}

		byName[st.Name] = &StackMatch{StackID: int(st.ID), IsGit: st.WorkflowID != 0}
	}

	return func(project string) *StackMatch {
		return byName[project]
	}
}

// updateStandalone recreates a standalone container with a re-pull of its image.
// On a successful update it optionally removes the now-dangling old image.
func (s *Service) updateStandalone(cli *dockerclient.Client, endpoint *portainer.Endpoint, c UpdateCandidate, cleanup bool) {
	oldImageID := c.ImageID

	ctx, cancel := context.WithTimeout(context.Background(), recreateTimeout)
	defer cancel()

	newContainer, err := s.containerService.Recreate(ctx, endpoint, c.ID, true, "", "")
	if err != nil {
		// Recreate preserves config and rolls back on a create failure; a pull or
		// create failure leaves the original container running.
		log.Warn().Err(err).Str("container_id", c.ID).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: failed to recreate standalone container")
		return
	}

	log.Info().Str("container_id", c.ID).Int("endpoint_id", int(endpoint.ID)).
		Msg("auto-update: recreated standalone container with updated image")

	if cleanup && newContainer != nil && newContainer.Image != oldImageID {
		s.cleanupOldImage(cli, endpoint, oldImageID)
	}
}

// cleanupOldImage attempts a conservative removal of the previous image after a
// standalone update. The removal is NOT forced: Docker refuses to delete an
// image that still carries tags or is referenced by any container, so this only
// succeeds when the old image has become genuinely dangling (untagged and
// unused). It never touches a tagged image still in use.
func (s *Service) cleanupOldImage(cli *dockerclient.Client, endpoint *portainer.Endpoint, oldImageID string) {
	if oldImageID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), endpointTimeout)
	defer cancel()

	if _, err := cli.ImageRemove(ctx, oldImageID, image.RemoveOptions{Force: false, PruneChildren: false}); err != nil {
		log.Debug().Err(err).Str("image_id", oldImageID).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: old image not removed (still tagged or in use)")
		return
	}

	log.Info().Str("image_id", oldImageID).Int("endpoint_id", int(endpoint.ID)).
		Msg("auto-update: removed dangling old image after update")
}

// updateStack redeploys a Portainer-managed compose stack with a re-pull so its
// containers are recreated by the stack engine and stay part of the stack. It is
// called at most once per stack per tick.
//
//   - git stacks: routed through the blessed git redeploy path
//     (RedeployWhenChanged), which re-clones, force-pulls and keeps the stack's
//     git/deployment bookkeeping consistent. Limitation: an image-only update
//     (same compose manifest, newer upstream digest) is picked up on the next
//     git change or via the manual "Update now" path, not by this tick.
//   - file stacks: the deployer is driven directly with forcePullImage=true,
//     applying the image update immediately.
func (s *Service) updateStack(endpoint *portainer.Endpoint, st StackUpdate) {
	ctx, cancel := context.WithTimeout(context.Background(), stackRedeployTimeout)
	defer cancel()

	if st.IsGit {
		if err := deployments.RedeployWhenChanged(ctx, portainer.StackID(st.StackID), s.stackDeployer, s.dataStore, s.gitService); err != nil {
			log.Warn().Err(err).Int("stack_id", st.StackID).Int("endpoint_id", int(endpoint.ID)).
				Msg("auto-update: failed to redeploy git stack")
			return
		}

		log.Info().Int("stack_id", st.StackID).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: triggered git stack redeploy")
		return
	}

	stack, err := s.dataStore.Stack().Read(portainer.StackID(st.StackID))
	if err != nil {
		log.Warn().Err(err).Int("stack_id", st.StackID).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: unable to read stack for redeploy")
		return
	}

	// Registries: the daemon is a system actor with no user context, so it uses
	// every configured registry (the admin path of the manual deploy helper)
	// rather than hand-rolling auth.
	registries, err := s.dataStore.Registry().ReadAll()
	if err != nil {
		log.Warn().Err(err).Int("stack_id", st.StackID).
			Msg("auto-update: unable to read registries for stack redeploy")
		return
	}

	// prune=false (conservative: do not remove resources the user may rely on),
	// forcePullImage=true (the whole point), forceRecreate=false.
	if stackutils.IsRelativePathStack(stack) {
		err = s.stackDeployer.DeployRemoteComposeStack(ctx, stack, endpoint, registries, false, true, false)
	} else {
		err = s.stackDeployer.DeployComposeStack(ctx, stack, endpoint, registries, false, true, false)
	}

	if err != nil {
		log.Warn().Err(err).Int("stack_id", st.StackID).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: failed to redeploy compose stack with re-pull")
		return
	}

	log.Info().Int("stack_id", st.StackID).Int("endpoint_id", int(endpoint.ID)).
		Msg("auto-update: redeployed compose stack with updated images")
}
