package containerautomation

import (
	"context"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/internal/endpointutils"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/rs/zerolog/log"
)

const (
	// retryWindow is the rolling window over which max restarts per container are counted.
	retryWindow = 10 * time.Minute
	// restartCooldown is the minimum delay between two restarts of the same container,
	// giving its healthcheck time to recover before we try again.
	restartCooldown = 60 * time.Second
	// endpointTimeout bounds the container-list call for a single endpoint.
	endpointTimeout = 30 * time.Second
	// restartTimeoutBuffer is added on top of a container's stop-timeout to derive
	// the deadline of its own restart context, leaving room for the engine to kill
	// and start the container after the graceful stop window elapses.
	restartTimeoutBuffer = 15 * time.Second
)

// retryState tracks restart accounting for a single container across ticks.
type retryState struct {
	attempts    int
	windowStart time.Time
	lastRestart time.Time
}

// retryPolicy holds the cooldown/window parameters applied to a container.
type retryPolicy struct {
	maxRetries int
	window     time.Duration
	cooldown   time.Duration
}

// decideRestart is a pure function that decides whether an unhealthy container
// should be restarted now, given its current retry state and policy. It returns
// the decision and the updated state to persist.
//
// Rules, in order:
//   - reset the window (and attempts) when the window has elapsed;
//   - deny while still within the cooldown since the last restart;
//   - deny once the max number of restarts in the current window is reached;
//   - otherwise restart, incrementing the attempt counter.
func decideRestart(state retryState, policy retryPolicy, now time.Time) (bool, retryState) {
	if state.windowStart.IsZero() || now.Sub(state.windowStart) >= policy.window {
		state.windowStart = now
		state.attempts = 0
	}

	if !state.lastRestart.IsZero() && now.Sub(state.lastRestart) < policy.cooldown {
		return false, state
	}

	if state.attempts >= policy.maxRetries {
		return false, state
	}

	state.attempts++
	state.lastRestart = now

	return true, state
}

// heal runs a single auto-heal pass over every reachable Docker endpoint.
// It is registered with the scheduler and guarded against overlapping ticks by
// the Service. Errors are logged per endpoint/container so one failure does not
// abort the whole pass; it always returns nil so the scheduler keeps the job.
func (s *Service) heal() error {
	if !s.running.CompareAndSwap(false, true) {
		log.Debug().Msg("auto-heal: previous run still in progress, skipping tick")
		return nil
	}
	defer s.running.Store(false)

	scope := s.scope()

	endpoints, err := s.dataStore.Endpoint().Endpoints()
	if err != nil {
		log.Warn().Err(err).Msg("auto-heal: unable to list environments")
		return nil
	}

	for i := range endpoints {
		endpoint := &endpoints[i]

		// M1 scope: native Docker endpoints only. Kubernetes is not applicable and
		// Edge/async endpoints are not reachable synchronously from the scheduler.
		if !endpointutils.IsDockerEndpoint(endpoint) || endpointutils.IsEdgeEndpoint(endpoint) {
			continue
		}

		// Per-endpoint opt-out (M5): skip environments where automation is disabled,
		// independently of the global switch. Zero value participates, so existing
		// installs are unaffected.
		if !AutomationEnabledForEndpoint(endpoint) {
			log.Debug().Int("endpoint_id", int(endpoint.ID)).
				Msg("auto-heal: automation disabled for this environment, skipping")
			continue
		}

		s.healEndpoint(endpoint, scope)
	}

	// Drop retry state only for containers whose retry window has fully elapsed
	// since their last restart. A container that briefly leaves the unhealthy
	// filter (e.g. while "starting" after a restart) keeps its accounting, so the
	// cooldown / max-retries storm guard survives flapping.
	s.pruneRetries(time.Now())

	return nil
}

// healEndpoint restarts the in-scope unhealthy containers of a single endpoint.
func (s *Service) healEndpoint(endpoint *portainer.Endpoint, scope string) {
	endpointID := int(endpoint.ID)

	// Swarm note (M1 limitation): we connect to the endpoint's primary node only
	// (nodeName ""). Containers scheduled on other Swarm nodes are not healed here;
	// per-node iteration is deferred to a later milestone.
	clientTimeout := endpointTimeout
	cli, err := s.clientFactory.CreateClient(endpoint, "", &clientTimeout)
	if err != nil {
		log.Warn().Err(err).Int("endpoint_id", endpointID).Msg("auto-heal: unable to create Docker client")
		return
	}
	defer cli.Close()

	listCtx, cancel := context.WithTimeout(s.baseCtx, endpointTimeout)
	defer cancel()

	// List running unhealthy containers only (All:false). Docker keeps
	// Health.Status=="unhealthy" on stopped containers, so listing with All:true
	// would let us "restart" (i.e. start) an intentionally-stopped container.
	listFilters := filters.NewArgs(filters.Arg("health", "unhealthy"))
	containers, err := cli.ContainerList(listCtx, container.ListOptions{All: false, Filters: listFilters})
	if err != nil {
		log.Warn().Err(err).Int("endpoint_id", endpointID).Msg("auto-heal: unable to list containers")
		return
	}

	for _, c := range containers {
		if !InScope(scope, c.Labels) {
			continue
		}

		policy := retryPolicy{
			maxRetries: MaxRetries(c.Labels),
			window:     retryWindow,
			cooldown:   restartCooldown,
		}

		ok, newState := decideRestart(s.getRetry(c.ID), policy, time.Now())
		s.setRetry(c.ID, newState)
		if !ok {
			log.Debug().Str("container_id", c.ID).Int("endpoint_id", endpointID).
				Msg("auto-heal: restart skipped (cooldown or max retries reached)")
			continue
		}

		timeout := StopTimeout(c.Labels)

		// Each restart gets its own context, bounded by the container's stop-timeout
		// plus a buffer, so one slow restart cannot starve the others and a hung
		// engine call is bounded independently of the list deadline.
		restartTimeout := time.Duration(timeout)*time.Second + restartTimeoutBuffer
		restartCtx, restartCancel := context.WithTimeout(s.baseCtx, restartTimeout)
		err := cli.ContainerRestart(restartCtx, c.ID, container.StopOptions{Timeout: &timeout})
		restartCancel()
		if err != nil {
			log.Warn().Err(err).Str("container_id", c.ID).Int("endpoint_id", endpointID).
				Msg("auto-heal: failed to restart unhealthy container")
			continue
		}

		log.Info().Str("container_id", c.ID).Int("endpoint_id", endpointID).Int("attempt", newState.attempts).
			Msg("auto-heal: restarted unhealthy container")
		s.notifier.Notify(Event{
			Kind: EventHealRestarted, EndpointID: endpointID, ContainerID: c.ID, ContainerName: containerName(c.Names),
			Message: "restarted unhealthy container",
		})
	}
}
