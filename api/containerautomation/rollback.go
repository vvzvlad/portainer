package containerautomation

import (
	"context"
	"time"

	portainer "github.com/portainer/portainer/api"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/rs/zerolog/log"
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
)

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

// hasHealthGate reports whether a container's healthcheck config yields a usable
// health signal. A nil config, an empty test, or an explicit {"NONE"} disable all
// mean Docker never reports healthy/unhealthy, so there is nothing to gate on.
func hasHealthGate(hc *container.HealthConfig) bool {
	if hc == nil || len(hc.Test) == 0 {
		return false
	}

	return hc.Test[0] != "NONE"
}

// healthGate polls the new container's health until it becomes healthy, fails, or
// the rollback window elapses. It returns true when the update is healthy and may
// proceed, false when the container must be rolled back. The polling context is
// derived from the service base context, so a server shutdown ends the wait and
// is treated as a non-healthy outcome (conservative: prefer rollback over leaving
// an unverified container in place).
func (s *Service) healthGate(cli *dockerclient.Client, containerID string, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = defaultRollbackTimeout
	}

	deadline := time.Now().Add(timeout)

	ctx, cancel := context.WithDeadline(s.baseCtx, deadline.Add(rollbackGateBuffer))
	defer cancel()

	for {
		inspect, err := cli.ContainerInspect(ctx, containerID)
		if err != nil {
			// The container vanished or the engine is unreachable: treat as a failed
			// update so the rollback path can restore the previous image.
			log.Warn().Err(err).Str("container_id", containerID).
				Msg("auto-update: health gate inspect failed, treating as unhealthy")

			return false
		}

		h := containerHealth{Running: inspect.State != nil && inspect.State.Running}
		if inspect.State != nil && inspect.State.Health != nil {
			h.Status = string(inspect.State.Health.Status)
		}

		switch decideRollback(h, time.Now(), deadline) {
		case rollbackHealthy:
			return true
		case rollbackTrigger:
			return false
		}

		select {
		case <-ctx.Done():
			// Deadline reached (or shutdown) while still starting: roll back.
			return false
		case <-time.After(rollbackPollInterval):
		}
	}
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
// failure notification is emitted.
func (s *Service) rollback(cli *dockerclient.Client, endpoint *portainer.Endpoint, newContainerID, oldImageID, originalRef string) {
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
			Kind: EventUpdateFailed, EndpointID: endpointID, ContainerID: newContainerID,
			Image: originalRef, Message: "rollback failed: could not re-tag previous image", Err: err,
		})

		return
	}

	if _, err := s.containerService.Recreate(ctx, endpoint, newContainerID, false, "", ""); err != nil {
		log.Error().Err(err).Str("container_id", newContainerID).Str("image", originalRef).Int("endpoint_id", endpointID).
			Msg("auto-update: rollback recreate failed, leaving the unhealthy container in place")
		s.notifier.Notify(Event{
			Kind: EventUpdateFailed, EndpointID: endpointID, ContainerID: newContainerID,
			Image: originalRef, Message: "rollback failed: could not recreate on previous image", Err: err,
		})

		return
	}

	log.Warn().Str("container_id", newContainerID).Str("image", originalRef).Int("endpoint_id", endpointID).
		Msg("auto-update: rolled back to the previous image after a failed update")
	s.notifier.Notify(Event{
		Kind: EventRollback, EndpointID: endpointID, ContainerID: newContainerID,
		Image: originalRef, Message: "rolled back to previous image after failed health check",
	})
}
