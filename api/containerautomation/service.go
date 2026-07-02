// Package containerautomation provides native container automation that runs as
// background scheduler jobs. M1 implements auto-heal (restarting Docker
// containers whose healthcheck reports "unhealthy", replacing the
// willfarrell/autoheal sidecar); M4 adds auto-update (periodically detecting
// outdated images and applying updates, replacing the containrrr/watchtower
// sidecar).
package containerautomation

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/dataservices"
	"github.com/portainer/portainer/api/docker"
	dockerclient "github.com/portainer/portainer/api/docker/client"
	"github.com/portainer/portainer/api/docker/images"
	"github.com/portainer/portainer/api/scheduler"

	"github.com/rs/zerolog/log"
)

const (
	// defaultCheckInterval is used when the configured auto-heal interval is empty or unparseable.
	defaultCheckInterval = 30 * time.Second
	// defaultPollInterval is used when the configured auto-update interval is empty or unparseable.
	// It is conservative (hours) to stay within registry rate limits; the image-status cache is
	// short-lived (keyed by the local imageID), so each poll re-checks the remote digest.
	defaultPollInterval = 6 * time.Hour
)

// Service manages the lifecycle of the auto-heal and auto-update scheduler jobs
// and keeps the per-container retry state in memory across ticks.
type Service struct {
	// baseCtx is the application shutdown context. It is the base for every
	// per-operation timeout context, so a server shutdown cancels in-flight heal
	// restarts and update redeploys instead of letting them run detached.
	baseCtx context.Context

	scheduler     *scheduler.Scheduler
	dataStore     dataservices.DataStore
	clientFactory *dockerclient.ClientFactory

	// Dependencies used by the auto-update job (M4).
	digestClient *images.DigestClient
	// containerService is the recreate seam (satisfied by *docker.ContainerService);
	// an interface so the standalone update/rollback recreate step can be faked in
	// tests. See containerRecreator.
	containerService containerRecreator

	// notifier receives automation events (update/rollback/failure/heal). The
	// default is logNotifier; the field is the seam external senders plug into.
	notifier Notifier

	mu          sync.Mutex
	healJobID   string
	updateJobID string

	// running guards against overlapping heal ticks.
	running atomic.Bool
	// updateRunning guards against overlapping update ticks.
	updateRunning atomic.Bool

	retryMu sync.Mutex
	retries map[string]retryState

	// rolledBackMu guards rolledBack.
	rolledBackMu sync.Mutex
	// rolledBack records standalone containers whose update was rolled back, keyed
	// by endpoint+name, so the auto-update job does not immediately re-pull the
	// same failed image and roll back again on the next tick (the update->rollback
	// loop guard, mirroring the auto-heal retries map).
	//
	// This state is in-memory only and is NOT persisted: after a Portainer restart
	// the map is empty, so at most one extra update->rollback cycle per restart is
	// possible before the guard re-records the failed target. Persisting it would
	// require a datastore schema (key + digest + timestamp) and is intentionally out
	// of scope here; the cooldown-bounded single extra cycle is an acceptable
	// trade-off against that complexity.
	rolledBack map[string]rolledBackTarget
}

// NewService creates a new container automation service. Call Start to schedule
// the jobs according to the persisted settings. baseCtx is the application
// shutdown context: it bounds the job operation contexts so a shutdown cancels
// any in-flight heal/update. containerService is used by the auto-update job; it
// may be nil only in tests that do not exercise auto-update.
func NewService(
	baseCtx context.Context,
	scheduler *scheduler.Scheduler,
	dataStore dataservices.DataStore,
	clientFactory *dockerclient.ClientFactory,
	containerService *docker.ContainerService,
) *Service {
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	return &Service{
		baseCtx:          baseCtx,
		scheduler:        scheduler,
		dataStore:        dataStore,
		clientFactory:    clientFactory,
		digestClient:     images.NewClientWithRegistry(images.NewRegistryClient(dataStore), clientFactory),
		containerService: containerService,
		// Compose the always-on log notifier with the optional webhook notifier.
		// The webhook reads the current settings per-event from the datastore, so a
		// URL change in the UI takes effect without a restart; logNotifier keeps the
		// existing structured log output unchanged.
		notifier:   multiNotifier{logNotifier{}, newWebhookNotifier(dataStore)},
		retries:    make(map[string]retryState),
		rolledBack: make(map[string]rolledBackTarget),
	}
}

// AutomationEnabledForEndpoint reports whether container automation (auto-heal and
// auto-update) should run for an environment. It is the per-endpoint opt-out (M5)
// layered on top of the global switch: an environment participates unless it has
// been explicitly disabled. The zero value (not disabled) preserves the
// pre-M5 behavior for every existing environment.
func AutomationEnabledForEndpoint(endpoint *portainer.Endpoint) bool {
	return endpoint != nil && !endpoint.ContainerAutomationDisabled
}

// Start schedules the enabled jobs according to the persisted settings.
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.start()
}

// Reload re-applies the current settings: it stops the running jobs and starts
// fresh ones with the new intervals, or leaves them stopped if disabled. It is
// safe to call after a settings update.
//
// Note: stopping a job unschedules future ticks but does not interrupt a tick
// already in progress. An in-flight heal/update pass runs to completion on its
// original (pre-reload) context and is only cancelled by a server shutdown (via
// baseCtx); the new interval takes effect from the next scheduled tick. The
// overlap guards (running/updateRunning) and the per-map mutexes keep this safe
// against data races, so this is a deliberate behavioural nuance, not a bug.
func (s *Service) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stop()
	s.start()

	return nil
}

// start (re)schedules the enabled jobs from settings. Caller must hold s.mu.
func (s *Service) start() {
	settings, err := s.dataStore.Settings().Settings()
	if err != nil {
		log.Warn().Err(err).Msg("container automation: unable to read settings, jobs not scheduled")
		return
	}

	s.startHeal(settings)
	s.startUpdate(settings)
}

// startHeal schedules the auto-heal job if enabled. Caller must hold s.mu.
func (s *Service) startHeal(settings *portainer.Settings) {
	if s.healJobID != "" {
		return
	}

	autoHeal := settings.ContainerAutomation.AutoHeal
	if !autoHeal.Enabled {
		return
	}

	interval, err := time.ParseDuration(autoHeal.CheckInterval)
	if err != nil || interval <= 0 {
		log.Warn().Str("interval", autoHeal.CheckInterval).Dur("default", defaultCheckInterval).
			Msg("auto-heal: invalid check interval, falling back to default")
		interval = defaultCheckInterval
	}

	s.healJobID = s.scheduler.StartJobEvery(interval, s.heal)
	log.Info().Dur("interval", interval).Msg("auto-heal: job scheduled")
}

// startUpdate schedules the auto-update job if enabled. Caller must hold s.mu.
func (s *Service) startUpdate(settings *portainer.Settings) {
	if s.updateJobID != "" {
		return
	}

	autoUpdate := settings.ContainerAutomation.AutoUpdate
	if !autoUpdate.Enabled {
		return
	}

	interval, err := time.ParseDuration(autoUpdate.PollInterval)
	if err != nil || interval <= 0 {
		log.Warn().Str("interval", autoUpdate.PollInterval).Dur("default", defaultPollInterval).
			Msg("auto-update: invalid poll interval, falling back to default")
		interval = defaultPollInterval
	}

	s.updateJobID = s.scheduler.StartJobEvery(interval, s.update)
	log.Info().Dur("interval", interval).Msg("auto-update: job scheduled")
}

// stop cancels the running jobs, if any. Caller must hold s.mu.
func (s *Service) stop() {
	if s.healJobID != "" {
		if err := s.scheduler.StopJob(s.healJobID); err != nil {
			log.Warn().Err(err).Msg("auto-heal: could not stop the job")
		}

		s.healJobID = ""
	}

	if s.updateJobID != "" {
		if err := s.scheduler.StopJob(s.updateJobID); err != nil {
			log.Warn().Err(err).Msg("auto-update: could not stop the job")
		}

		s.updateJobID = ""
	}
}

// scope returns the configured auto-heal scope, defaulting to "labeled".
func (s *Service) scope() string {
	settings, err := s.dataStore.Settings().Settings()
	if err != nil {
		return ScopeLabeled
	}

	if settings.ContainerAutomation.AutoHeal.Scope == ScopeAll {
		return ScopeAll
	}

	return ScopeLabeled
}

// getRetry returns the retry state for a container (zero value if unknown).
func (s *Service) getRetry(containerID string) retryState {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()

	return s.retries[containerID]
}

// setRetry stores the retry state for a container.
func (s *Service) setRetry(containerID string, state retryState) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()

	s.retries[containerID] = state
}

// getRolledBack returns the rolled-back target for a key and whether it exists.
func (s *Service) getRolledBack(key string) (rolledBackTarget, bool) {
	s.rolledBackMu.Lock()
	defer s.rolledBackMu.Unlock()

	rec, ok := s.rolledBack[key]

	return rec, ok
}

// setRolledBack records a rolled-back target for a key.
func (s *Service) setRolledBack(key string, rec rolledBackTarget) {
	s.rolledBackMu.Lock()
	defer s.rolledBackMu.Unlock()

	s.rolledBack[key] = rec
}

// clearRolledBack drops the rolled-back record for a key (cooldown elapsed or a
// new upstream image lifted the skip).
func (s *Service) clearRolledBack(key string) {
	s.rolledBackMu.Lock()
	defer s.rolledBackMu.Unlock()

	delete(s.rolledBack, key)
}

// pruneRolledBack drops rolled-back records whose cooldown has fully elapsed, so
// the map cannot grow unbounded. It mirrors pruneRetries.
func (s *Service) pruneRolledBack(now time.Time) {
	s.rolledBackMu.Lock()
	defer s.rolledBackMu.Unlock()

	for key, rec := range s.rolledBack {
		if now.Sub(rec.at) >= updateRollbackCooldown {
			delete(s.rolledBack, key)
		}
	}
}

// pruneRetries drops retry state for containers whose retry window has fully
// elapsed since their last restart. A container is kept regardless of whether it
// appeared in the current tick: one that briefly leaves the unhealthy filter
// (e.g. while "starting" right after a restart) must not lose its accounting, or
// the cooldown / max-retries storm guard would be defeated. A container that has
// recovered and stayed quiet for longer than the window is cleaned up (fresh
// budget next incident, no unbounded growth).
func (s *Service) pruneRetries(now time.Time) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()

	for id, state := range s.retries {
		if now.Sub(state.lastRestart) >= retryWindow {
			delete(s.retries, id)
		}
	}
}
