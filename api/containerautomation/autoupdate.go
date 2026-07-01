package containerautomation

import (
	"context"
	"fmt"
	"strings"
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

	opts := updateOptions{
		cleanup:         settings.ContainerAutomation.AutoUpdate.Cleanup,
		rollback:        settings.ContainerAutomation.AutoUpdate.RollbackOnFailure,
		rollbackTimeout: parseRollbackTimeout(settings.ContainerAutomation.AutoUpdate.RollbackTimeout),
	}

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

		// Per-endpoint opt-out (M5): skip environments where automation is disabled,
		// independently of the global switch. Zero value participates, so existing
		// installs are unaffected.
		if !AutomationEnabledForEndpoint(endpoint) {
			log.Debug().Int("endpoint_id", int(endpoint.ID)).
				Msg("auto-update: automation disabled for this environment, skipping")
			continue
		}

		s.updateEndpoint(endpoint, scope, opts)
	}

	// Drop rolled-back records whose cooldown has fully elapsed (mirrors auto-heal's
	// pruneRetries), so the loop-guard map cannot grow unbounded.
	s.pruneRolledBack(time.Now())

	return nil
}

// updateOptions carries the per-pass auto-update toggles resolved from settings.
type updateOptions struct {
	// cleanup removes the now-dangling old image after a confirmed-good update.
	cleanup bool
	// rollback enables the health gate + rollback of a failed standalone update.
	rollback bool
	// rollbackTimeout bounds how long the health gate waits before rolling back.
	rollbackTimeout time.Duration
}

// parseRollbackTimeout resolves the configured rollback timeout, falling back to
// the default when empty or unparseable.
func parseRollbackTimeout(raw string) time.Duration {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultRollbackTimeout
	}

	return d
}

// updateEndpoint applies image updates to the in-scope, outdated containers of a
// single endpoint, routing each container to the standalone / stack / external
// apply path. Stack-managed candidates are grouped so each owning stack is
// redeployed at most once per tick.
func (s *Service) updateEndpoint(endpoint *portainer.Endpoint, scope string, opts updateOptions) {
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

	listCtx, cancel := context.WithTimeout(s.baseCtx, endpointTimeout)
	defer cancel()

	// Running containers only: a stopped container has nothing to update now and
	// would be started by a bare recreate.
	containers, err := cli.ContainerList(listCtx, container.ListOptions{All: false})
	if err != nil {
		log.Warn().Err(err).Int("endpoint_id", endpointID).Msg("auto-update: unable to list containers")
		return
	}

	// Collect the in-scope, outdated, non-monitor-only containers as candidates.
	// An in-scope monitor-only container is still status-checked (keeping its badge
	// cache warm) but never auto-applied. This only covers in-scope containers: in
	// "labeled" scope a monitor-only container without the enable label is filtered
	// out below before any status check, so its badge is not refreshed here.
	var candidates []UpdateCandidate
	for _, c := range containers {
		if !InUpdateScope(scope, c.Labels) {
			continue
		}

		// Resolve the image status. This also refreshes the package-level status
		// cache that backs the badge, so in-scope monitor-only containers are still
		// checked even though they are never auto-applied.
		statusCtx, statusCancel := context.WithTimeout(s.baseCtx, statusCheckTimeout)
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

		candidates = append(candidates, UpdateCandidate{ID: c.ID, Name: containerName(c.Names), ImageID: c.ImageID, Labels: c.Labels})
	}

	// Route and de-duplicate: one redeploy per stack per tick.
	grouped := groupContainersForUpdate(candidates, s.stackLookupForEndpoint(endpoint.ID))

	for _, ext := range grouped.External {
		log.Debug().Str("container_id", ext.ID).Int("endpoint_id", endpointID).
			Msg("auto-update: outdated externally-managed compose container, detect only")
	}

	for _, c := range grouped.Standalone {
		s.updateStandalone(cli, endpoint, c, opts)
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

// updateStandalone recreates a standalone container with a re-pull of its image,
// then (when rollback is enabled and the container has a healthcheck) holds a
// health gate over the new container and rolls back to the previous image if it
// fails to become healthy. The old-image cleanup is deliberately ordered AFTER
// the health gate, so the rollback target is never removed before the update is
// confirmed good.
//
// Sequence: capture old image id + original ref + healthcheck -> recreate(pull)
// -> [health gate] -> on healthy: cleanup (if enabled); on unhealthy: rollback
// (never cleanup).
func (s *Service) updateStandalone(cli *dockerclient.Client, endpoint *portainer.Endpoint, c UpdateCandidate, opts updateOptions) {
	endpointID := int(endpoint.ID)

	// Loop-guard safety: the rolled-back map is keyed by endpoint+name (the only
	// identifier that survives a recreate). An unnamed container cannot be recorded
	// (recordRolledBack skips it), so with rollback enabled a container that keeps
	// failing its health gate would update->rollback every tick with NO suppression.
	// Skip the unnamed case when rollback is on so it cannot enter that
	// unsuppressable loop; detection/badge refresh already happened upstream and is
	// unaffected. (With rollback off there is no rollback to loop, so we proceed.)
	if skipUnnamedForRollback(opts.rollback, c.Name) {
		log.Info().Str("container_id", c.ID).Int("endpoint_id", endpointID).
			Msg("auto-update: skipping unnamed standalone container, rollback is enabled but there is no stable name to key the loop guard")
		return
	}

	// Update->rollback loop guard: if this container's update was rolled back
	// recently and the remote still points at the SAME failed image, skip it until
	// the cooldown elapses. A genuinely new upstream image (a changed remote digest)
	// is not blocked.
	rollbackMapKey := rollbackKey(endpoint.ID, c.Name)
	if rec, ok := s.getRolledBack(rollbackMapKey); ok && s.shouldSkipRolledBack(rollbackMapKey, rec) {
		log.Info().Str("container_id", c.ID).Str("container", c.Name).Str("image", rec.ref).Int("endpoint_id", endpointID).
			Msg("auto-update: skipping update, a recent rollback failed on this image and the remote is unchanged (cooldown)")
		return
	}

	// Capture the pre-update image identity for a possible rollback. The container
	// list gives us the old image id; an inspect adds the original reference (re-tag
	// target), whether a usable healthcheck exists, and the healthcheck start_period
	// (which must be waited out before deciding). We only health-gate when rollback
	// is enabled, the container has a healthcheck, we resolved both the old image id
	// and its reference, and that reference is a proper tag (a digest-pinned or bare
	// image id cannot be re-tagged, so the gate could never roll back).
	oldImageID := c.ImageID
	var originalRef string
	var startPeriod time.Duration
	healthGated := false
	if opts.rollback {
		// Bound the inspect like every other engine call so a hung/unreachable engine
		// cannot block the whole sequential tick until shutdown.
		inspectCtx, inspectCancel := context.WithTimeout(s.baseCtx, endpointTimeout)
		inspect, err := cli.ContainerInspect(inspectCtx, c.ID)
		inspectCancel()
		if err != nil {
			log.Warn().Err(err).Str("container_id", c.ID).Int("endpoint_id", endpointID).
				Msg("auto-update: unable to inspect container before update, proceeding without a health gate")
		} else {
			originalRef = inspect.Config.Image
			if oldImageID == "" {
				oldImageID = inspect.Image
			}
			if hc := inspect.Config.Healthcheck; hc != nil {
				startPeriod = hc.StartPeriod
			}

			switch {
			case !hasHealthGate(inspect.Config.Healthcheck):
				log.Info().Str("container_id", c.ID).Int("endpoint_id", endpointID).
					Msg("auto-update: container has no healthcheck, updating without a rollback gate")
			case oldImageID == "" || originalRef == "":
				log.Info().Str("container_id", c.ID).Int("endpoint_id", endpointID).
					Msg("auto-update: unable to resolve previous image identity, updating without a rollback gate")
			case !isTagReference(originalRef):
				log.Info().Str("container_id", c.ID).Str("image", originalRef).Int("endpoint_id", endpointID).
					Msg("auto-update: health gate skipped, image is digest-pinned and cannot be rolled back")
			default:
				healthGated = true
			}
		}
	}

	ctx, cancel := context.WithTimeout(s.baseCtx, recreateTimeout)
	defer cancel()

	newContainer, err := s.containerService.Recreate(ctx, endpoint, c.ID, true, "", "")
	if err != nil {
		// Recreate preserves config and rolls back on a create failure; a pull or
		// create failure leaves the original container running.
		log.Warn().Err(err).Str("container_id", c.ID).Int("endpoint_id", endpointID).
			Msg("auto-update: failed to recreate standalone container")
		s.notifier.Notify(Event{
			Kind: EventUpdateFailed, EndpointID: endpointID, ContainerID: c.ID, ContainerName: c.Name,
			Message: "failed to recreate standalone container", Err: err,
		})
		return
	}

	log.Info().Str("container_id", c.ID).Int("endpoint_id", endpointID).
		Msg("auto-update: recreated standalone container with updated image")
	newImage := ""
	if newContainer != nil {
		newImage = newContainer.Config.Image
	}

	// Health gate: roll back if the new container does not become healthy in time.
	// The old image is preserved (not cleaned up) until the gate confirms health,
	// so the rollback target is still available. The "updated" event is held until
	// the gate confirms health, so an observer never sees a misleading
	// "updated" -> "rollback" sequence for the same container; on the rollback path
	// only EventRollback (or update-failed) is emitted.
	if healthGated {
		switch s.healthGate(cli, newContainer.ID, opts.rollbackTimeout, startPeriod) {
		case gateAborted:
			// Server shutdown mid-gate: leave the new container in place, do not roll
			// back and do not emit an event (we never observed a real failure).
			return
		case gateRollback:
			s.rollback(cli, endpoint, newContainer.ID, oldImageID, originalRef, c.Name)
			return
		case gateHealthy:
			// Confirmed healthy: fall through to emit "updated" and clean up.
		}
	}

	// Emit "updated" now: either there was no gate (emitted right after recreate,
	// as before), or the gate confirmed the new container is healthy.
	s.notifier.Notify(Event{
		Kind: EventUpdated, EndpointID: endpointID, ContainerID: newContainer.ID, ContainerName: c.Name,
		Image: newImage, OldDigest: oldImageID, NewDigest: newContainer.Image,
		Message: "updated standalone container",
	})

	if opts.cleanup && newContainer != nil && newContainer.Image != oldImageID {
		s.cleanupOldImage(cli, endpoint, oldImageID)
	}
}

// containerName returns a container's primary name without the leading slash, or
// "" when none is reported. The name is stable across a recreate (Recreate
// assigns a new container ID but preserves the name), so it keys the rolled-back
// loop-guard map.
func containerName(names []string) string {
	if len(names) == 0 {
		return ""
	}

	return strings.TrimPrefix(names[0], "/")
}

// skipUnnamedForRollback reports whether a standalone update must be skipped
// because rollback is enabled but the container has no stable name to key the
// loop guard. The rolled-back map is keyed by endpoint+name (the only identifier
// that survives a recreate); without a name the guard cannot record a failed
// target, so a repeatedly-failing update would loop update->rollback every tick
// with no suppression. When rollback is off there is nothing to loop, so an
// unnamed container is still allowed to update.
func skipUnnamedForRollback(rollback bool, name string) bool {
	return rollback && name == ""
}

// rollbackKey identifies a standalone container in the rolled-back map by its
// endpoint and (recreate-stable) name. A recreate assigns a new container ID, so
// the ID cannot key state across an update; the name is preserved.
func rollbackKey(endpointID portainer.EndpointID, name string) string {
	return fmt.Sprintf("%d/%s", int(endpointID), name)
}

// resolveRemoteDigest fetches the current remote image digest for a reference. It
// tells whether a rolled-back container's upstream target is still the same
// failed image (skip) or a new push (retry).
func (s *Service) resolveRemoteDigest(ctx context.Context, ref string) (string, error) {
	img, err := images.ParseImage(images.ParseImageOptions{Name: ref})
	if err != nil {
		return "", err
	}

	dig, err := s.digestClient.RemoteDigest(ctx, img)
	if err != nil {
		return "", err
	}

	return dig.String(), nil
}

// recordRolledBack stores the failed target after a successful rollback so the
// next poll skips re-pulling the same broken image. The failed remote digest is
// resolved now (the registry is reachable, the image was just pulled); if it
// cannot be resolved the record is still stored with an empty digest and the
// guard skips conservatively until the cooldown elapses.
func (s *Service) recordRolledBack(endpoint *portainer.Endpoint, name, ref string) {
	if name == "" {
		// Without a stable key we cannot reliably match the container next tick.
		log.Debug().Str("image", ref).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: rolled-back container has no name, loop guard not recorded")
		return
	}

	ctx, cancel := context.WithTimeout(s.baseCtx, statusCheckTimeout)
	digest, err := s.resolveRemoteDigest(ctx, ref)
	cancel()
	if err != nil {
		log.Debug().Err(err).Str("image", ref).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: could not resolve failed remote digest, loop guard will skip conservatively until cooldown")
	}

	s.setRolledBack(rollbackKey(endpoint.ID, name), rolledBackTarget{ref: ref, digest: digest, at: time.Now()})
}

// shouldSkipRolledBack reports whether a standalone container must be skipped this
// tick to avoid the update->rollback loop, clearing the record once the skip no
// longer applies (cooldown elapsed or a new upstream image). It resolves the
// current remote digest so a genuinely new image is never blocked.
func (s *Service) shouldSkipRolledBack(key string, rec rolledBackTarget) bool {
	now := time.Now()

	// Fast paths that avoid a registry call: cooldown elapsed -> clear & proceed;
	// no recorded digest -> skip conservatively while the cooldown is open.
	if now.Sub(rec.at) >= updateRollbackCooldown {
		s.clearRolledBack(key)
		return false
	}
	if rec.digest == "" {
		return true
	}

	ctx, cancel := context.WithTimeout(s.baseCtx, statusCheckTimeout)
	currentDigest, err := s.resolveRemoteDigest(ctx, rec.ref)
	cancel()
	if err != nil {
		// Cannot confirm the upstream target changed: stay conservative and skip to
		// avoid re-entering the loop, until the cooldown elapses.
		log.Debug().Err(err).Str("image", rec.ref).
			Msg("auto-update: cannot resolve remote digest for a rolled-back container, skipping until cooldown")
		return true
	}

	if decideUpdateSkip(rec, currentDigest, now, updateRollbackCooldown) {
		return true
	}

	// New upstream image (changed digest): the failed target is gone, clear the
	// record and let the update proceed.
	s.clearRolledBack(key)
	return false
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

	ctx, cancel := context.WithTimeout(s.baseCtx, endpointTimeout)
	defer cancel()

	if _, err := cli.ImageRemove(ctx, oldImageID, image.RemoveOptions{Force: false, PruneChildren: false}); err != nil {
		log.Debug().Err(err).Str("image_id", oldImageID).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: old image not removed (still tagged or in use)")
		return
	}

	log.Info().Str("image_id", oldImageID).Int("endpoint_id", int(endpoint.ID)).
		Msg("auto-update: removed dangling old image after update")
}

// updateStack applies an image update to a Portainer-managed compose stack so its
// containers are recreated by the stack engine and stay part of the stack. It is
// called at most once per stack per tick.
//
//   - git stacks: detect-only here. A git stack's source of truth is its commit;
//     this tick's trigger is an image-only update (same compose manifest, newer
//     upstream digest), which the git redeploy path (RedeployWhenChanged) would
//     short-circuit without applying — while still doing a real git fetch every
//     tick. So we skip git stacks: the image update lands on the stack's next git
//     change or via a manual "Update now", and we do not fetch git every tick.
//   - file stacks: the deployer is driven directly with forcePullImage=true,
//     applying the image update immediately.
func (s *Service) updateStack(endpoint *portainer.Endpoint, st StackUpdate) {
	if st.IsGit {
		// Detect-only: leave git bookkeeping to the git redeploy path. Logged at
		// debug so it does not repeat at info on every tick (it would otherwise
		// fire for an unchanged git stack indefinitely).
		log.Debug().Int("stack_id", st.StackID).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: outdated git stack image detected, detect only (applied on next git change or manual update)")
		return
	}

	ctx, cancel := context.WithTimeout(s.baseCtx, stackRedeployTimeout)
	defer cancel()

	stack, err := s.dataStore.Stack().Read(portainer.StackID(st.StackID))
	if err != nil {
		log.Warn().Err(err).Int("stack_id", st.StackID).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: unable to read stack for redeploy")
		return
	}

	// Resolve registries the same way the established userless/system redeploy does
	// (RedeployWhenChanged): scope them to the stack author's access on the endpoint
	// and refresh ECR tokens, so an ECR-backed stack authenticates with fresh
	// credentials instead of the stale token a raw ReadAll() would pass.
	registries, err := deployments.ResolveStackRegistries(s.dataStore, stack, endpoint.ID)
	if err != nil {
		log.Warn().Err(err).Int("stack_id", st.StackID).Int("endpoint_id", int(endpoint.ID)).
			Msg("auto-update: unable to resolve registries for stack redeploy")
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
	s.notifier.Notify(Event{
		Kind: EventUpdated, EndpointID: int(endpoint.ID), StackID: st.StackID,
		Message: "redeployed compose stack with updated images",
	})
}
