package containerautomation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/docker"
	"github.com/portainer/portainer/api/docker/consts"
	"github.com/portainer/portainer/api/docker/images"
	"github.com/portainer/portainer/api/internal/endpointutils"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/rs/zerolog/log"
)

const (
	// statusCheckTimeout bounds a single container image-status resolution
	// (container inspect + remote digest fetch).
	statusCheckTimeout = 30 * time.Second
	// recreateTimeout bounds a standalone recreate (pull + stop + create + start).
	// Pulls can be slow, so it is generous.
	recreateTimeout = 10 * time.Minute
)

// update runs a single auto-update pass over every reachable Docker endpoint.
// It is the scheduler entry point (poll timer): it delegates to runUpdatePass and
// always returns nil so the scheduler keeps the job regardless of the outcome.
func (s *Service) update() error {
	s.runUpdatePass()
	return nil
}

// runUpdatePass performs a single auto-update pass over every reachable Docker
// endpoint. It is guarded against overlapping runs by the updateRunning CAS: if a
// pass (scheduled or webhook-triggered) is already in progress it returns false
// WITHOUT running, so the caller can decide what to do (the poll timer drops the
// tick; the webhook worker waits and re-runs so a mid-pass kick is not lost).
// Errors are logged per endpoint/container so one failure does not abort the whole
// pass. Returns true when this call actually acquired the lock and ran the pass.
func (s *Service) runUpdatePass() bool {
	if !s.updateRunning.CompareAndSwap(false, true) {
		log.Debug().Msg("auto-update: previous run still in progress, skipping tick")
		return false
	}
	defer s.updateRunning.Store(false)

	settings, err := s.dataStore.Settings().Settings()
	if err != nil {
		log.Warn().Err(err).Msg("auto-update: unable to read settings")
		return true
	}

	// Defense-in-depth: auto-update may have been disabled during the webhook
	// debounce/backoff window (the handler's 409 gate and the scheduler removal on
	// Reload both act earlier, but neither covers a kick already in flight). Re-read
	// the live flag and no-op if it was turned off. Returns true (lock acquired,
	// nothing to retry).
	if !settings.ContainerAutomation.AutoUpdate.Enabled {
		log.Debug().Msg("auto-update: disabled, skipping pass")
		return true
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
		return true
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

	return true
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
// single endpoint. Every candidate is recreated individually with a re-pull of
// its image (Watchtower-style), so a compose stack member keeps its labels and
// stays part of its project without redeploying the owning stack.
func (s *Service) updateEndpoint(endpoint *portainer.Endpoint, scope string, opts updateOptions) {
	endpointID := int(endpoint.ID)

	// Swarm note (M4 limitation, mirrors auto-heal): we connect to the endpoint's
	// primary node only (nodeName ""). Containers scheduled on other Swarm nodes
	// are not updated here.
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

		candidates = append(candidates, UpdateCandidate{ID: c.ID, Name: containerName(c.Names), ImageID: c.ImageID, Image: c.Image, Labels: c.Labels})
	}

	// Recreate every candidate individually with a re-pull of its image, exactly
	// like the standalone path. A stack member keeps its compose labels through the
	// recreate, so it stays part of its project without redeploying the stack.
	for _, c := range candidates {
		s.updateStandalone(cli, endpoint, c, opts)
	}
}

// updateStandalone recreates a container with a re-pull of its image,
// then (when rollback is enabled and the container has a healthcheck) holds a
// health gate over the new container and rolls back to the previous image if it
// fails to become healthy. The old-image cleanup is deliberately ordered AFTER
// the health gate, so the rollback target is never removed before the update is
// confirmed good.
//
// Sequence: capture old image id + original ref + healthcheck -> recreate(pull)
// -> [health gate] -> on healthy: cleanup (if enabled); on unhealthy: rollback
// (never cleanup).
func (s *Service) updateStandalone(cli dockerClient, endpoint *portainer.Endpoint, c UpdateCandidate, opts updateOptions) {
	endpointID := int(endpoint.ID)

	// A recreated container keeps its compose labels, so a stack member stays part
	// of its project. Source the stack name from the compose-project label here so
	// the notification prints "Stack [name]" for a member (empty for a standalone
	// container, which the webhook formatter renders as "Container [name]").
	stackName := c.Labels[consts.ComposeStackNameLabel]

	// Loop-guard safety: the rolled-back map is keyed by endpoint+name (the only
	// identifier that survives a recreate). An unnamed container cannot be recorded
	// (recordRolledBack skips it), so with rollback enabled a container that keeps
	// failing its health gate would update->rollback every tick with NO suppression.
	// Skip the unnamed case when rollback is on so it cannot enter that
	// unsuppressable loop; detection/badge refresh already happened upstream and is
	// unaffected. (With rollback off there is no rollback to loop, so we proceed.)
	if skipUnnamedForRollback(opts.rollback, c.Name) {
		log.Info().Str("container_id", c.ID).Int("endpoint_id", endpointID).
			Msg("auto-update: skipping unnamed container, rollback is enabled but there is no stable name to key the loop guard")
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

	// Hold the original container for the rest of this pass: an auto-heal restart
	// landing in the middle of the recreate would fight it for the same container,
	// and the two would race over its lifecycle. The early returns above take no
	// hold — nothing was acted on there.
	//
	// Taking it BEFORE Recreate is load-bearing twice over. Recreate pulls the image
	// first and only then stops the container, so the original stays up — unhealthy,
	// and listed as such — for however long the pull takes. And when the recreate
	// fails, Recreate's restore defer renames the original back from "<name>-old" and
	// starts it again under its ORIGINAL id (api/docker/container.go), so the whole
	// restore path is covered by this hold too, but only because it was already taken
	// when Recreate was entered.
	//
	// It narrows the window rather than closing the race: auto-heal consults the hold
	// at one line and calls ContainerRestart at another, so a restart it decided on
	// just before this hold — or one already in flight when Recreate issues its stop
	// and rename — still lands mid-recreate. There is no hold in the other direction
	// either: auto-heal does not block an update.
	s.acquireUpdateHold(endpoint.ID, c.ID)
	defer s.releaseUpdateHold(endpoint.ID, c.ID)

	// Hold the container NAME as well, and from here rather than after the recreate,
	// because the new container's id does not exist until Recreate returns while the
	// new container itself is running well before that. Recreate creates it under the
	// ORIGINAL name (api/docker/container.go, step 6), starts it (step 8), and only
	// then removes the old container (step 9) and inspects the new one — so between
	// the start and the id hold taken below there is a real window (a blocking
	// removal plus a full inspect round-trip) in which the new container is running,
	// reporting health, and visible in auto-heal's health=unhealthy list. A container
	// with an aggressive healthcheck (interval 1s, retries 1, no start period, all
	// ordinary in compose files) turns unhealthy inside it; an auto-heal restart
	// there resets Docker health to "starting" and the gate waits instead of rolling
	// back — exactly what this interlock exists to prevent.
	//
	// The name is recreate-stable, so this hold covers the new container from birth,
	// and keeps covering it across the rollback recreate, which creates a third
	// container under the same name. The old container leaves the name (Recreate
	// renames it to "<name>-old", step 3) but stays covered by its id hold above.
	s.acquireUpdateHoldByName(endpoint.ID, c.Name)
	defer s.releaseUpdateHoldByName(endpoint.ID, c.Name)

	newContainer, err := s.containerService.Recreate(ctx, endpoint, c.ID, true, "", "")
	if err != nil {
		// Recreate preserves config and keeps the original container until the new one
		// has started, rolling back to the original from the moment it stops it, so an
		// ordinary recreate failure ends with the original running exactly as before.
		// Recreate verifies that by inspecting it; when the rollback did not fully land
		// it reports a *docker.RestoreError, which is an operator-visible problem rather
		// than a skipped update. Its OriginalRunning tells the two apart: nothing
		// running is an outage, a running container that did not get its name or its
		// networks back is a degradation that will not fix itself. StateUnknown is
		// neither: the container was not observed at all, so the operator is told to
		// go and look rather than told something that may not be true.
		var restoreErr *docker.RestoreError
		if errors.As(err, &restoreErr) {
			if restoreErr.StateUnknown {
				// Acted on as an outage — a workload that may be down is worth waking
				// somebody for — but never described as one.
				log.Error().Err(err).Str("container_id", c.ID).Str("container", c.Name).Int("endpoint_id", endpointID).
					Msg("auto-update: failed to recreate container and the state of the original could not be read, it needs checking by hand")
				s.notifier.Notify(Event{
					Kind: EventUpdateFailed, EndpointID: endpointID, ContainerID: c.ID, ContainerName: c.Name,
					StackName: stackName, Message: "failed to recreate container and the state of the original container could not be read, check it manually", Err: err,
					ServiceDown: true,
				})

				return
			}

			if !restoreErr.OriginalRunning {
				log.Error().Err(err).Str("container_id", c.ID).Str("container", c.Name).Int("endpoint_id", endpointID).
					Msg("auto-update: failed to recreate container and the original could not be restored, it is left down")
				s.notifier.Notify(Event{
					Kind: EventUpdateFailed, EndpointID: endpointID, ContainerID: c.ID, ContainerName: c.Name,
					StackName: stackName, Message: "failed to recreate container and the original container is left down, manual intervention required", Err: err,
					ServiceDown: true,
				})

				return
			}

			// Serving again, so ServiceDown stays false and this is a warning, the level
			// the notifier gives it too — but under the wrong name or without a network
			// it is not the service it was, and the next pass would find and recreate the
			// "-old" container instead of this one.
			log.Warn().Err(err).Str("container_id", c.ID).Str("container", c.Name).Int("endpoint_id", endpointID).
				Msg("auto-update: failed to recreate container, the original is running again but its name or networks were not restored")
			s.notifier.Notify(Event{
				Kind: EventUpdateFailed, EndpointID: endpointID, ContainerID: c.ID, ContainerName: c.Name,
				StackName: stackName, Message: "failed to recreate container and the original container was only partially restored (name or networks), manual intervention required", Err: err,
			})

			return
		}

		log.Warn().Err(err).Str("container_id", c.ID).Int("endpoint_id", endpointID).
			Msg("auto-update: failed to recreate container")
		s.notifier.Notify(Event{
			Kind: EventUpdateFailed, EndpointID: endpointID, ContainerID: c.ID, ContainerName: c.Name,
			StackName: stackName, Message: "failed to recreate container", Err: err,
		})
		return
	}

	log.Info().Str("container_id", c.ID).Int("endpoint_id", endpointID).
		Msg("auto-update: recreated container with updated image")
	newImage := ""
	if newContainer != nil {
		newImage = newContainer.Config.Image

		// Hold the new container by id now that it has one. An auto-heal restart
		// landing mid-gate resets Docker health to "starting", so the gate would keep
		// waiting instead of rolling back and could accept a failed update as a good
		// one. A restart preserves the container id, so this hold covers the gate
		// window and the rollback's operations on THIS id (the re-tag and the recreate
		// it hands this id to).
		//
		// It does not cover what the rollback then produces: the rollback's own
		// Recreate (see rollback) creates a THIRD container with a new id, which is
		// never held by id at all. What carries across that recreate is the name hold
		// taken above, still in scope here and released only when this pass returns.
		s.acquireUpdateHold(endpoint.ID, newContainer.ID)
		defer s.releaseUpdateHold(endpoint.ID, newContainer.ID)
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
			s.rollback(cli, endpoint, newContainer.ID, oldImageID, originalRef, c.Name, stackName)
			return
		case gateHealthy:
			// Confirmed healthy: fall through to emit "updated" and clean up.
		}
	}

	// Emit "updated" now: either there was no gate (emitted right after recreate,
	// as before), or the gate confirmed the new container is healthy.
	s.notifier.Notify(Event{
		Kind: EventUpdated, EndpointID: endpointID, ContainerID: newContainer.ID, ContainerName: c.Name,
		StackName: stackName, Image: newImage, OldDigest: oldImageID, NewDigest: newContainer.Image,
		Message: "updated container",
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

// updateHoldIDKey identifies a hold on one concrete container, by its id; a hold
// taken this way follows that container and only that container. The "id:"/"name:"
// namespaces keep a container id from ever colliding with a container name.
//
// Both keys are endpoint-scoped, like rollbackKey, which leaves two cases
// uncovered for two different reasons:
//
//   - Two environments of THIS Portainer pointing at the same Docker engine do not
//     interlock with each other. Nothing technical stops it — they share this
//     process and this map, and dropping the endpoint from the key would be enough
//     — it is a deliberate choice: the endpoint is the unit of automation
//     everywhere else in this package (scheduling, scope, retry accounting), and
//     such a pair already runs two independent auto-update passes over the same
//     containers. Holds are not what makes that configuration safe, so widening
//     them would not make it so.
//   - Two separate Portainer INSTANCES sharing an engine cannot interlock at all:
//     that would need coordination state outside this process.
func updateHoldIDKey(endpointID portainer.EndpointID, containerID string) string {
	return fmt.Sprintf("%d/id:%s", int(endpointID), containerID)
}

// updateHoldNameKey identifies a hold on whatever container currently owns a
// name. Like rollbackKey it survives a recreate (which assigns a new container id
// but preserves the name), so it covers a container that does not exist yet.
func updateHoldNameKey(endpointID portainer.EndpointID, name string) string {
	return fmt.Sprintf("%d/name:%s", int(endpointID), name)
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
func (s *Service) cleanupOldImage(cli dockerClient, endpoint *portainer.Endpoint, oldImageID string) {
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
