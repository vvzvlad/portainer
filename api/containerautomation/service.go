// Package containerautomation provides native container automation that runs as
// a background scheduler job. M1 implements auto-heal: restarting Docker
// containers whose healthcheck reports "unhealthy", replacing the
// willfarrell/autoheal sidecar.
package containerautomation

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/portainer/portainer/api/dataservices"
	dockerclient "github.com/portainer/portainer/api/docker/client"
	"github.com/portainer/portainer/api/scheduler"

	"github.com/rs/zerolog/log"
)

// defaultCheckInterval is used when the configured interval is empty or unparseable.
const defaultCheckInterval = 30 * time.Second

// Service manages the lifecycle of the auto-heal scheduler job and keeps the
// per-container retry state in memory across ticks.
type Service struct {
	scheduler     *scheduler.Scheduler
	dataStore     dataservices.DataStore
	clientFactory *dockerclient.ClientFactory

	mu    sync.Mutex
	jobID string

	// running guards against overlapping heal ticks.
	running atomic.Bool

	retryMu sync.Mutex
	retries map[string]retryState
}

// NewService creates a new container automation service. Call Start to schedule
// the job according to the persisted settings.
func NewService(scheduler *scheduler.Scheduler, dataStore dataservices.DataStore, clientFactory *dockerclient.ClientFactory) *Service {
	return &Service{
		scheduler:     scheduler,
		dataStore:     dataStore,
		clientFactory: clientFactory,
		retries:       make(map[string]retryState),
	}
}

// Start schedules the auto-heal job if it is enabled in the settings.
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.start()
}

// Reload re-applies the current settings: it stops the running job and starts a
// fresh one with the new interval, or leaves it stopped if auto-heal is now
// disabled. It is safe to call after a settings update.
func (s *Service) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stop()
	s.start()

	return nil
}

// start (re)schedules the job from settings. Caller must hold s.mu.
func (s *Service) start() {
	settings, err := s.dataStore.Settings().Settings()
	if err != nil {
		log.Warn().Err(err).Msg("auto-heal: unable to read settings, job not scheduled")
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

	s.jobID = s.scheduler.StartJobEvery(interval, s.heal)
	log.Info().Dur("interval", interval).Msg("auto-heal: job scheduled")
}

// stop cancels the running job, if any. Caller must hold s.mu.
func (s *Service) stop() {
	if s.jobID == "" {
		return
	}

	if err := s.scheduler.StopJob(s.jobID); err != nil {
		log.Warn().Err(err).Msg("auto-heal: could not stop the job")
	}

	s.jobID = ""
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

// pruneRetries drops retry state for containers not seen unhealthy in the last
// pass, so a recovered (or removed) container starts fresh next time.
func (s *Service) pruneRetries(seen map[string]struct{}) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()

	for id := range s.retries {
		if _, ok := seen[id]; !ok {
			delete(s.retries, id)
		}
	}
}
