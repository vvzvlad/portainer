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
	// webhookDebounceWindow collapses a burst of inbound registry-push webhooks into a
	// single update pass. A multi-arch / multi-tag push fires several deliveries in a
	// row; after the first kick the worker waits this long, draining any further kicks,
	// so the burst runs one pass instead of one per delivery.
	webhookDebounceWindow = 10 * time.Second
	// webhookRetryBackoff is how long the webhook worker waits before re-running a pass
	// that could not acquire the update lock because a scheduled pass was already in
	// progress. It keeps the retry from spinning while ensuring a mid-pass kick is not
	// lost until the next poll.
	webhookRetryBackoff = 5 * time.Second
)

// Service manages the lifecycle of the auto-heal and auto-update scheduler jobs
// and keeps the per-container retry state in memory across ticks.
type Service struct {
	// baseCtx is the application shutdown context. It is the base for every
	// per-operation timeout context, so a server shutdown cancels in-flight heal
	// restarts and update recreates instead of letting them run detached.
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

	// kickCh signals the webhook worker to run an out-of-band update pass. It has a
	// buffer of 1 and TriggerUpdate sends non-blockingly, so a burst of concurrent
	// triggers naturally coalesces into at most one pending kick.
	kickCh chan struct{}
	// debounceWindow / retryBackoff are the worker timings, held as fields (defaulted
	// from the package constants in NewService) so tests can shrink them.
	debounceWindow time.Duration
	retryBackoff   time.Duration
	// updatePass is the seam the webhook worker runs; it defaults to runUpdatePass and
	// is overridable in tests to observe the kick/debounce/retry behaviour without a
	// live engine. It returns whether the pass acquired the update lock and ran.
	updatePass func() bool

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

	s := &Service{
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
		notifier:       multiNotifier{logNotifier{}, newWebhookNotifier(dataStore)},
		kickCh:         make(chan struct{}, 1),
		debounceWindow: webhookDebounceWindow,
		retryBackoff:   webhookRetryBackoff,
		retries:        make(map[string]retryState),
		rolledBack:     make(map[string]rolledBackTarget),
	}
	s.updatePass = s.runUpdatePass

	// The webhook worker lives for the whole application lifetime (bounded by
	// baseCtx), independently of whether the poll job is scheduled: a kick must be
	// serviceable even between polls.
	go s.triggerWorker()

	return s
}

// TriggerUpdate requests an immediate auto-update pass out of band from the poll
// timer. It is called by the inbound registry-push webhook handler so a container
// updates within seconds of a push instead of waiting up to a full poll interval.
// The send is non-blocking onto the buffer-1 kick channel: a burst of concurrent
// triggers coalesces into at most one pending kick, and the worker then debounces
// the burst and, if a scheduled pass is already running, waits and re-runs so a
// kick that lands mid-pass is not lost. Safe to call on a zero-value Service (a
// nil kick channel makes it a no-op).
func (s *Service) TriggerUpdate() {
	select {
	case s.kickCh <- struct{}{}:
	default:
		// A kick is already pending (or the channel is nil on a zero-value service):
		// the coming pass will cover this trigger too, so drop the duplicate.
	}
}

// triggerWorker services webhook-driven update kicks for the life of baseCtx. It
// waits for a kick, debounces the burst, then runs an update pass — re-running
// after a short backoff if a scheduled pass currently holds the update lock, so a
// kick that arrives mid-pass is not dropped until the next poll.
func (s *Service) triggerWorker() {
	for {
		// Block until the first kick of a burst (or shutdown).
		select {
		case <-s.baseCtx.Done():
			return
		case <-s.kickCh:
		}

		if !s.debounceKicks() {
			return // shutdown during the debounce window
		}

		// Run the pass, retrying while a scheduled pass holds the lock. runUpdatePass
		// returns false only when the CAS is not acquired (another pass in progress);
		// backing off and retrying guarantees this kick eventually runs a fresh pass
		// that observes the just-pushed image, rather than being silently dropped.
		//
		// runPassRecovered wraps each attempt in a recover(): the poll path runs under
		// the scheduler's cron.Recover, so a panic there is logged and the daemon
		// survives. This webhook worker is a bare goroutine, so without its own
		// recover an unrelated panic in the update pass (reachable by a token holder)
		// would crash the whole process. Match the poll path's guarantee.
		for !s.runPassRecovered() {
			log.Debug().Msg("auto-update: webhook-triggered pass deferred, a scheduled pass is running; will retry")
			select {
			case <-s.baseCtx.Done():
				return
			case <-time.After(s.retryBackoff):
			}
		}
	}
}

// runPassRecovered runs one update pass with a recover() so a panic in the pass
// cannot crash the process from this bare worker goroutine (the poll path is
// already protected by the scheduler's cron.Recover). It returns whatever the
// pass returned; on a recovered panic it returns true ("done") so the retry loop
// does not spin on a deterministically-panicking pass — the next kick starts
// fresh. runUpdatePass releases the updateRunning CAS via defer even on panic, so
// no lock is leaked here.
func (s *Service) runPassRecovered() (done bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Msg("auto-update: recovered from panic in webhook-triggered update pass")
			done = true
		}
	}()

	return s.updatePass()
}

// debounceKicks waits a fixed window after the first kick, draining any further
// kicks that arrive during it, so a burst of registry deliveries collapses into a
// single pass. It returns false if baseCtx is cancelled while waiting.
func (s *Service) debounceKicks() bool {
	timer := time.NewTimer(s.debounceWindow)
	defer timer.Stop()

	for {
		select {
		case <-s.baseCtx.Done():
			return false
		case <-s.kickCh:
			// Additional kick within the window: coalesce it (fixed window from the
			// first kick, not reset — a bounded, single collapsed pass).
		case <-timer.C:
			return true
		}
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
