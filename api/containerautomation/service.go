// Package containerautomation provides native container automation that runs as
// background scheduler jobs. M1 implements auto-heal (restarting Docker
// containers whose healthcheck reports "unhealthy", replacing the
// willfarrell/autoheal sidecar); M4 adds auto-update (periodically detecting
// outdated images and applying updates, replacing the containrrr/watchtower
// sidecar).
package containerautomation

import (
	"sync"
	"sync/atomic"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/dataservices"
	"github.com/portainer/portainer/api/docker"
	dockerclient "github.com/portainer/portainer/api/docker/client"
	"github.com/portainer/portainer/api/docker/images"
	"github.com/portainer/portainer/api/scheduler"
	"github.com/portainer/portainer/api/stacks/deployments"

	"github.com/rs/zerolog/log"
)

const (
	// defaultCheckInterval is used when the configured auto-heal interval is empty or unparseable.
	defaultCheckInterval = 30 * time.Second
	// defaultPollInterval is used when the configured auto-update interval is empty or unparseable.
	// It is conservative (hours) to stay within registry rate limits and rely on the 24h status cache.
	defaultPollInterval = 6 * time.Hour
)

// Service manages the lifecycle of the auto-heal and auto-update scheduler jobs
// and keeps the per-container retry state in memory across ticks.
type Service struct {
	scheduler     *scheduler.Scheduler
	dataStore     dataservices.DataStore
	clientFactory *dockerclient.ClientFactory

	// Dependencies used by the auto-update job (M4).
	digestClient     *images.DigestClient
	containerService *docker.ContainerService
	stackDeployer    deployments.StackDeployer
	gitService       portainer.GitService

	mu          sync.Mutex
	healJobID   string
	updateJobID string

	// running guards against overlapping heal ticks.
	running atomic.Bool
	// updateRunning guards against overlapping update ticks.
	updateRunning atomic.Bool

	retryMu sync.Mutex
	retries map[string]retryState
}

// NewService creates a new container automation service. Call Start to schedule
// the jobs according to the persisted settings. The stackDeployer, gitService
// and containerService are used by the auto-update job; they may be nil only in
// tests that do not exercise auto-update.
func NewService(
	scheduler *scheduler.Scheduler,
	dataStore dataservices.DataStore,
	clientFactory *dockerclient.ClientFactory,
	containerService *docker.ContainerService,
	stackDeployer deployments.StackDeployer,
	gitService portainer.GitService,
) *Service {
	return &Service{
		scheduler:        scheduler,
		dataStore:        dataStore,
		clientFactory:    clientFactory,
		digestClient:     images.NewClientWithRegistry(images.NewRegistryClient(dataStore), clientFactory),
		containerService: containerService,
		stackDeployer:    stackDeployer,
		gitService:       gitService,
		retries:          make(map[string]retryState),
	}
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
