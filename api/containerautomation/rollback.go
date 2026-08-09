package containerautomation

import (
	"context"
	"errors"
	"regexp"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/docker"

	"github.com/docker/docker/api/types/container"
	"github.com/rs/zerolog/log"
	"go.podman.io/image/v5/docker/reference"
)

const (
	// defaultRollbackTimeout bounds how long the health gate waits for a freshly
	// updated standalone container to become healthy before rolling back.
	defaultRollbackTimeout = 120 * time.Second
	// rollbackPollInterval is the delay between two health probes of the new
	// container while the rollback window is open.
	rollbackPollInterval = 3 * time.Second
	// rollbackGateBuffer is added to the rollback timeout when deriving the inspect
	// context deadline, leaving room for the final probe to complete after the
	// decision deadline elapses.
	rollbackGateBuffer = 10 * time.Second
	// startPeriodBuffer is added to a container's healthcheck start_period when it
	// is longer than the rollback timeout, so the gate waits through the whole
	// start period (during which Docker reports "starting") plus a small grace
	// before deciding. Without it a legitimately slow-starting container would be
	// rolled back while it is still initializing normally.
	startPeriodBuffer = 15 * time.Second
	// maxConsecutiveInspectErrors is how many back-to-back inspect failures the
	// health gate tolerates before declaring the update failed. A single transient
	// Docker API blip must not trigger a false rollback, so the gate keeps polling
	// and only gives up once the failures are clearly not transient.
	maxConsecutiveInspectErrors = 3
	// updateRollbackCooldown is how long a standalone container whose update was
	// rolled back is skipped from updating to the SAME failed image again. It
	// breaks the update->rollback loop: without it a persistently-unhealthy new
	// image would be re-pulled and rolled back on every poll tick. A genuinely new
	// upstream image (a changed remote digest) is not blocked; the cooldown only
	// suppresses the exact target that just failed. It is generous because a broken
	// upstream image is normally fixed by a new push, which lifts the skip at once.
	updateRollbackCooldown = 24 * time.Hour
)

// rolledBackTarget records that a standalone container's update to a specific
// remote image was rolled back, so the same target is skipped until the cooldown
// elapses or the upstream digest changes.
type rolledBackTarget struct {
	// ref is the container's original image reference (the re-tag target), used to
	// re-resolve the current remote digest on later ticks.
	ref string
	// digest is the remote image digest that failed the health gate. A later tick
	// resolving a DIFFERENT digest (a new upstream push) is allowed through; the
	// same digest is skipped until the cooldown elapses. Empty when it could not be
	// resolved at rollback time, in which case the guard skips conservatively.
	digest string
	// at is when the rollback happened; the cooldown is measured from it.
	at time.Time
}

// decideUpdateSkip is the pure core of the update->rollback loop guard: given a
// recorded rolled-back target and the freshly-resolved current remote digest, it
// reports whether the standalone update must be skipped this tick. The skip holds
// only while the cooldown is open AND the remote still points at the same failed
// image; once the cooldown elapses the skip is lifted. An unknown recorded digest
// is skipped conservatively (we cannot prove the target changed). Mirrors the
// decideRestart pattern so it is unit-testable without Docker.
func decideUpdateSkip(rec rolledBackTarget, currentDigest string, now time.Time, cooldown time.Duration) bool {
	if now.Sub(rec.at) >= cooldown {
		return false
	}

	if rec.digest == "" {
		return true
	}

	return currentDigest == rec.digest
}

// rollbackOutcome is the decision produced from a single health sample.
type rollbackOutcome int

const (
	// rollbackContinue: still starting and before the deadline, keep polling.
	rollbackContinue rollbackOutcome = iota
	// rollbackHealthy: the new container is healthy, accept the update.
	rollbackHealthy
	// rollbackTrigger: the new container failed the health gate, roll back.
	rollbackTrigger
)

// gateResult is the terminal outcome of healthGate. It is a tri-state because a
// shutdown mid-gate must be distinguished from a genuine failure: only a real
// unhealthy/not-running/deadline outcome may roll back.
type gateResult int

const (
	// gateHealthy: the new container became healthy in time, accept the update.
	gateHealthy gateResult = iota
	// gateRollback: the new container failed the gate, roll back to the old image.
	gateRollback
	// gateAborted: the service base context was cancelled (server shutdown) while
	// the gate was open. The new container is left running as-is; no rollback and
	// no failure event, since we never observed an actual failure.
	gateAborted
)

// imageIDReference matches a content-addressable image id carried verbatim in a
// container's Config.Image when it was started from a bare id (e.g.
// "sha256:ab12…"). Such an id is not a tag and cannot be re-tagged, so it must
// not enable the health gate. A full bare hex id (no algorithm prefix) is
// already rejected by reference.ParseNormalizedNamed; this catches the
// algorithm-prefixed digest form, which otherwise parses as a bogus tag.
var imageIDReference = regexp.MustCompile(`^[a-z0-9]+:[0-9a-f]{64}$`)

// containerHealth is the minimal health signal the gate polls. It is built from
// a container inspect but kept independent of the Docker SDK so the decision
// logic can be unit-tested without a Docker engine.
type containerHealth struct {
	// Running reports whether the container is currently running. A container that
	// has exited within the window is a failed update.
	Running bool
	// Status is the Docker health status: "starting", "healthy", "unhealthy" or
	// "none"/"" when there is no healthcheck.
	Status string
}

// decideRollback is a pure decision over a single health sample taken at time
// `now`, given the rollback `deadline`. It is the testable core of the health
// gate: callers feed it successive samples and act on the outcome.
//
// Rules, in order:
//   - healthy -> accept the update (rollbackHealthy);
//   - unhealthy -> roll back immediately (Docker only reports unhealthy after the
//     configured retries fail, so it is a definitive signal);
//   - not running (crashed/exited post-start) -> roll back;
//   - still starting past the deadline -> roll back (never became healthy in time);
//   - otherwise keep waiting (rollbackContinue).
func decideRollback(h containerHealth, now, deadline time.Time) rollbackOutcome {
	switch h.Status {
	case string(container.Healthy):
		return rollbackHealthy
	case string(container.Unhealthy):
		return rollbackTrigger
	}

	if !h.Running {
		return rollbackTrigger
	}

	if !now.Before(deadline) {
		return rollbackTrigger
	}

	return rollbackContinue
}

// effectiveRollbackDeadline derives the health-gate deadline from the gate start
// time, the configured rollback timeout, and the container's healthcheck
// start_period. While a container is within its start_period Docker keeps
// reporting "starting" (it never reports unhealthy yet), so a start_period
// longer than the rollback timeout would otherwise trip a premature rollback
// while the container is initializing normally. The deadline is therefore the
// later of (start + timeout) and (start + start_period + buffer).
func effectiveRollbackDeadline(start time.Time, timeout, startPeriod time.Duration) time.Time {
	window := timeout
	if startPeriod > 0 {
		if d := startPeriod + startPeriodBuffer; d > window {
			window = d
		}
	}

	return start.Add(window)
}

// inspectErrorTolerated reports whether the health gate should keep polling after
// `consecutive` back-to-back inspect failures rather than declaring the update
// failed. Up to maxConsecutiveInspectErrors transient errors are tolerated; the
// counter is reset by the caller on any successful inspect.
func inspectErrorTolerated(consecutive int) bool {
	return consecutive <= maxConsecutiveInspectErrors
}

// hasHealthGate reports whether a container's healthcheck config yields a usable
// health signal. A nil config, an empty test, or an explicit {"NONE"} disable all
// mean Docker never reports healthy/unhealthy, so there is nothing to gate on.
func hasHealthGate(hc *container.HealthConfig) bool {
	if hc == nil || len(hc.Test) == 0 {
		return false
	}

	return hc.Test[0] != "NONE"
}

// isTagReference reports whether ref is a proper tag reference that the health
// gate can roll back. Rolling back re-tags the previous image id onto ref via
// ImageTag, which Docker rejects for a digest-pinned reference (repo@sha256:…)
// with "refusing to create a tag with a digest reference", and which is
// meaningless for a bare image id. Such containers are detected here so the gate
// is skipped instead of silently no-op'ing.
func isTagReference(ref string) bool {
	if ref == "" {
		return false
	}

	// Algorithm-prefixed image id (e.g. "sha256:<64 hex>"): a bare id, not a tag.
	if imageIDReference.MatchString(ref) {
		return false
	}

	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		// Unparseable (e.g. a full bare hex image id): not a usable tag target.
		return false
	}

	// A digest-pinned reference (with or without a tag) cannot be re-tagged.
	if _, ok := named.(reference.Canonical); ok {
		return false
	}

	return true
}

// healthGate polls the new container's health until it becomes healthy, fails, or
// the rollback window elapses, returning the terminal gateResult.
//
// The polling context is derived from the service base context, so a server
// shutdown ends the wait. A shutdown is reported as gateAborted (leave the new
// container in place, do not roll back): we never observed a real failure, and a
// rollback derived from the cancelled context would itself fail and emit a
// misleading "rollback failed" event on every shutdown during a gate window.
//
// Transient inspect failures (a brief Docker API blip) are tolerated: the gate
// keeps polling and only declares the update failed after more than
// maxConsecutiveInspectErrors consecutive failures, resetting on any success.
//
// Scheduling note (known limitation): this poll runs inside the sequential update
// tick, so N unhealthy standalone containers with rollback enabled can each hold
// the tick for up to their rollback window, delaying other containers/endpoints
// in the same tick. The overlap guard in update() still prevents ticks from
// piling up; this is accepted rather than re-architected (no per-container
// goroutine) to keep the update path simple and ordered.
func (s *Service) healthGate(cli dockerClient, containerID string, timeout, startPeriod time.Duration) gateResult {
	if timeout <= 0 {
		timeout = defaultRollbackTimeout
	}

	deadline := effectiveRollbackDeadline(time.Now(), timeout, startPeriod)

	ctx, cancel := context.WithDeadline(s.baseCtx, deadline.Add(rollbackGateBuffer))
	defer cancel()

	consecutiveErrors := 0
	for {
		inspect, err := cli.ContainerInspect(ctx, containerID)
		if err != nil {
			// Server shutdown cancelled the base context: abort without rolling back.
			if errors.Is(ctx.Err(), context.Canceled) || errors.Is(s.baseCtx.Err(), context.Canceled) {
				log.Debug().Str("container_id", containerID).
					Msg("auto-update: health gate aborted due to shutdown")

				return gateAborted
			}

			consecutiveErrors++
			if !inspectErrorTolerated(consecutiveErrors) {
				// Repeated failures: the container vanished or the engine is
				// unreachable, treat as a failed update so the rollback can restore
				// the previous image.
				log.Warn().Err(err).Str("container_id", containerID).Int("consecutive_errors", consecutiveErrors).
					Msg("auto-update: health gate inspect failed repeatedly, treating as unhealthy")

				return gateRollback
			}

			// Tolerate a transient blip: keep polling until the data resolves or the
			// deadline passes.
			log.Debug().Err(err).Str("container_id", containerID).Int("consecutive_errors", consecutiveErrors).
				Msg("auto-update: health gate inspect failed, retrying (transient)")

			select {
			case <-ctx.Done():
				return s.gateDeadlineResult()
			case <-time.After(rollbackPollInterval):
			}

			continue
		}
		consecutiveErrors = 0

		h := containerHealth{Running: inspect.State != nil && inspect.State.Running}
		if inspect.State != nil && inspect.State.Health != nil {
			h.Status = string(inspect.State.Health.Status)
		}

		switch decideRollback(h, time.Now(), deadline) {
		case rollbackHealthy:
			return gateHealthy
		case rollbackTrigger:
			return gateRollback
		}

		select {
		case <-ctx.Done():
			return s.gateDeadlineResult()
		case <-time.After(rollbackPollInterval):
		}
	}
}

// gateDeadlineResult maps a context-done gate exit to its outcome: a base-context
// cancellation (shutdown) aborts without rolling back, while a plain deadline
// (the container never became healthy in time) rolls back.
func (s *Service) gateDeadlineResult() gateResult {
	if errors.Is(s.baseCtx.Err(), context.Canceled) {
		log.Debug().Msg("auto-update: health gate aborted due to shutdown")

		return gateAborted
	}

	return gateRollback
}

// rollback restores the previous image after a failed health-gated update. It
// re-tags the old image id back onto the container's original reference (which
// the new image currently owns), then recreates the new container on that
// reference with no pull, so Recreate's full config-preservation + create-failure
// rollback is reused while resolving to the old image.
//
// Side effect: re-tagging moves `originalRef` from the new image to the old one,
// leaving the new (unhealthy) image untagged/dangling. It is intentionally left
// in place (not pruned) so an operator can inspect why the update failed.
//
// If any step fails the previous image cannot be safely restored, so the
// (unhealthy) new container is left running rather than destroyed, and a loud
// failure notification is emitted. The exception is a *docker.RestoreError from
// the rollback recreate: there the restore itself failed, nothing is left
// running, and the notification says so.
func (s *Service) rollback(cli dockerClient, endpoint *portainer.Endpoint, newContainerID, oldImageID, originalRef, containerName, stackName string) {
	endpointID := int(endpoint.ID)

	log.Warn().Str("container_id", newContainerID).Str("image", originalRef).Int("endpoint_id", endpointID).
		Msg("auto-update: new container failed the health gate, rolling back to the previous image")

	ctx, cancel := context.WithTimeout(s.baseCtx, recreateTimeout)
	defer cancel()

	// Re-tag the previous image id back onto the original reference. After the
	// update the reference points at the new image; this moves it back so Recreate
	// resolves the old image without a pull.
	if err := cli.ImageTag(ctx, oldImageID, originalRef); err != nil {
		log.Error().Err(err).Str("image_id", oldImageID).Str("image", originalRef).Int("endpoint_id", endpointID).
			Msg("auto-update: rollback failed to re-tag the previous image, leaving the unhealthy container in place")
		s.notifier.Notify(Event{
			Kind: EventUpdateFailed, EndpointID: endpointID, ContainerID: newContainerID, ContainerName: containerName,
			StackName: stackName, Image: originalRef, Message: "rollback failed: could not re-tag previous image", Err: err,
		})

		return
	}

	if _, err := s.containerService.Recreate(ctx, endpoint, newContainerID, false, "", ""); err != nil {
		// A *docker.RestoreError means the rollback recreate could not put back even
		// the container it had torn down: unlike a plain recreate failure, nothing is
		// left running, so the operator must not be told the unhealthy container is
		// still serving.
		var restoreErr *docker.RestoreError
		if errors.As(err, &restoreErr) {
			log.Error().Err(err).Str("container_id", newContainerID).Str("image", originalRef).Int("endpoint_id", endpointID).
				Msg("auto-update: rollback recreate failed and the container could not be restored, it is left down")
			s.notifier.Notify(Event{
				Kind: EventUpdateFailed, EndpointID: endpointID, ContainerID: newContainerID, ContainerName: containerName,
				StackName: stackName, Image: originalRef, Message: "rollback failed and the container is left down, manual intervention required", Err: err,
			})

			return
		}

		log.Error().Err(err).Str("container_id", newContainerID).Str("image", originalRef).Int("endpoint_id", endpointID).
			Msg("auto-update: rollback recreate failed, leaving the unhealthy container in place")
		s.notifier.Notify(Event{
			Kind: EventUpdateFailed, EndpointID: endpointID, ContainerID: newContainerID, ContainerName: containerName,
			StackName: stackName, Image: originalRef, Message: "rollback failed: could not recreate on previous image", Err: err,
		})

		return
	}

	log.Warn().Str("container_id", newContainerID).Str("image", originalRef).Int("endpoint_id", endpointID).
		Msg("auto-update: rolled back to the previous image after a failed update")
	s.notifier.Notify(Event{
		Kind: EventRollback, EndpointID: endpointID, ContainerID: newContainerID, ContainerName: containerName,
		StackName: stackName, Image: originalRef, Message: "rolled back to previous image after failed health check",
	})

	// Record the failed target so the next poll does not immediately re-pull the
	// same broken image and roll back again (the update->rollback loop). Recorded
	// only after a SUCCESSFUL rollback; a changed remote digest later lifts the skip.
	s.recordRolledBack(endpoint, containerName, originalRef)
}
