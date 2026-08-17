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

	// updateHoldMu guards updateHolds.
	updateHoldMu sync.Mutex
	// updateHolds records the containers an auto-update pass is currently acting on
	// (recreate -> health gate -> rollback), keyed by endpoint plus EITHER a
	// container id or a (recreate-stable) container name — see updateHoldIDKey /
	// updateHoldNameKey. Auto-heal skips a held container: restarting it mid-gate
	// resets Docker health to "starting", which makes the gate wait instead of
	// rolling back and can get a failed update accepted as a good one.
	//
	// The interlock is one-way and advisory: it narrows the window, it does not
	// eliminate the race. Auto-heal reads the hold at one line and calls
	// ContainerRestart at another, so a restart decided a moment before the hold was
	// taken still lands on a container an update is already acting on; and a restart
	// already in flight when Recreate issues its ContainerStop and rename fights the
	// recreate over the same container from the other side. There is no hold in the
	// opposite direction either — auto-heal never blocks an update from starting.
	//
	// The entries are leases, not records: every hold is released by a defer in
	// updateStandalone, so unlike retries/rolledBack this map needs no pruning.
	updateHolds map[string]updateHold
}

// updateHold is one lease taken by an auto-update pass over a container it is
// acting on, for as long as it is acting on it.
type updateHold struct {
	at time.Time
	// skipLogged records that auto-heal has already reported skipping this hold (see
	// claimFirstSkipLog), so the skip is announced once per hold instead of once per
	// heal tick. A hold spans the whole recreate (bounded by recreateTimeout, image
	// pull included) plus the gate window, which is many ticks at any realistic
	// check interval: the event is rare per update, not per tick.
	skipLogged bool
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
		updateHolds:    make(map[string]updateHold),
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

// addHold records a lease under an already-built key. It is the key-agnostic core
// of the acquire wrappers below. The map is created lazily so a Service literal
// that omits the field cannot panic on a hold.
func (s *Service) addHold(key string) {
	s.updateHoldMu.Lock()
	defer s.updateHoldMu.Unlock()

	if s.updateHolds == nil {
		s.updateHolds = make(map[string]updateHold)
	}

	s.updateHolds[key] = updateHold{at: time.Now()}
}

// dropHold releases the lease under an already-built key. It is the key-agnostic
// core of the release wrappers below.
func (s *Service) dropHold(key string) {
	s.updateHoldMu.Lock()
	defer s.updateHoldMu.Unlock()

	delete(s.updateHolds, key)
}

// acquireUpdateHold marks a concrete container as being acted on by an
// auto-update pass, so auto-heal leaves it alone until the hold is released. An
// empty container id is never held: it identifies no container and would make
// every other unidentified container collide on the same key.
func (s *Service) acquireUpdateHold(endpointID portainer.EndpointID, containerID string) {
	if containerID == "" {
		return
	}

	s.addHold(updateHoldIDKey(endpointID, containerID))
}

// releaseUpdateHold drops the hold on a container id, letting auto-heal act on it
// again. It is always called from a defer, so the hold cannot outlive the pass.
// The empty-id guard mirrors acquireUpdateHold's: what is never held is never
// released.
func (s *Service) releaseUpdateHold(endpointID portainer.EndpointID, containerID string) {
	if containerID == "" {
		return
	}

	s.dropHold(updateHoldIDKey(endpointID, containerID))
}

// acquireUpdateHoldByName holds whatever container currently owns a name. The
// name is recreate-stable, so unlike an id hold it covers the container a recreate
// is about to create under that name, from the moment the engine starts it. An
// empty name is never held, for the same reason an empty id is not.
func (s *Service) acquireUpdateHoldByName(endpointID portainer.EndpointID, name string) {
	if name == "" {
		return
	}

	s.addHold(updateHoldNameKey(endpointID, name))
}

// releaseUpdateHoldByName drops the hold on a container name. Like the id
// release, it is always called from a defer and guards the empty name symmetrically
// with the acquire.
func (s *Service) releaseUpdateHoldByName(endpointID portainer.EndpointID, name string) {
	if name == "" {
		return
	}

	s.dropHold(updateHoldNameKey(endpointID, name))
}

// heldByUpdate reports whether an auto-update pass is currently acting on this
// container, by id or by its (recreate-stable) name, and when the hold was taken.
// It is a pure lookup: use claimFirstSkipLog to consume the once-per-hold Info,
// so that asking about the state — as tests and any future caller do — cannot
// silently burn the one line an operator gets.
//
// The id key is checked first and the name key second, the same order
// claimFirstSkipLog uses, so with both taken the two agree on which hold they
// mean whenever the map does not change between the two calls. They take
// updateHoldMu separately, though, so if the id hold is released while the name
// hold is still live, the claim may resolve a different hold than the lookup
// reported. That costs at most a wrong held_for in one line — and it is the Info
// one when the name hold's token is still unspent, so do not read this as a
// Debug-only inaccuracy.
func (s *Service) heldByUpdate(endpointID portainer.EndpointID, containerID, name string) (since time.Time, held bool) {
	// Both keys are built before the lock: the formatting has no business inside the
	// critical section. An empty id or name is never held (the acquires refuse it),
	// so its key simply misses and the lookup falls through to the other one.
	keys := [2]string{updateHoldIDKey(endpointID, containerID), updateHoldNameKey(endpointID, name)}

	s.updateHoldMu.Lock()
	defer s.updateHoldMu.Unlock()

	for _, key := range keys {
		if hold, ok := s.updateHolds[key]; ok {
			return hold.at, true
		}
	}

	return time.Time{}, false
}

// claimFirstSkipLog reports whether this is the first heal tick suppressed by the
// hold on this container, marking it so every later tick is reported at Debug: a
// hold spans the whole recreate plus the gate window, i.e. many ticks, and the
// event is rare per update, not per tick. A fresh hold on the same key starts
// unclaimed, so the next update over that container is announced again.
//
// The lock is not about heal passes racing each other — heal() admits one pass at
// a time via s.running.CompareAndSwap. It is about the heal pass racing the update
// pass: two different scheduler jobs on two different goroutines, one reading and
// marking this map while the other adds and drops holds in it.
//
// Callers ask heldByUpdate first and claim only if held, so the hold can in
// principle be released in between; the claim then finds nothing and returns
// false, and the skip is logged at Debug. That is harmless — and cheaper than
// holding the mutex across the log call.
func (s *Service) claimFirstSkipLog(endpointID portainer.EndpointID, containerID, name string) bool {
	keys := [2]string{updateHoldIDKey(endpointID, containerID), updateHoldNameKey(endpointID, name)}

	s.updateHoldMu.Lock()
	defer s.updateHoldMu.Unlock()

	for _, key := range keys {
		hold, ok := s.updateHolds[key]
		if !ok {
			continue
		}

		if hold.skipLogged {
			return false
		}

		hold.skipLogged = true
		s.updateHolds[key] = hold

		return true
	}

	return false
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
