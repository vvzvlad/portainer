package containerautomation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/portainer/portainer/api/datastore"
	"github.com/stretchr/testify/require"
)

// newTriggerTestService builds a Service wired only for the webhook trigger worker,
// with a stubbed updatePass seam and short timings, and starts the worker on a
// cancellable context that is torn down at the end of the test.
func newTriggerTestService(t *testing.T, pass func() bool) *Service {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s := &Service{
		baseCtx:        ctx,
		kickCh:         make(chan struct{}, 1),
		debounceWindow: 40 * time.Millisecond,
		retryBackoff:   10 * time.Millisecond,
		updatePass:     pass,
	}
	go s.triggerWorker()

	return s
}

// TestTriggerUpdateCoalescesBurst verifies that a burst of TriggerUpdate calls
// arriving within the debounce window collapses into a single update pass, so a
// multi-arch / multi-tag registry push does not run one pass per delivery.
func TestTriggerUpdateCoalescesBurst(t *testing.T) {
	var mu sync.Mutex
	passes := 0
	s := newTriggerTestService(t, func() bool {
		mu.Lock()
		passes++
		mu.Unlock()
		return true
	})

	// A burst of deliveries in a tight loop, all well within the debounce window.
	for range 10 {
		s.TriggerUpdate()
	}

	// Wait out the debounce window (plus margin) and confirm exactly one pass ran.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return passes == 1
	}, time.Second, 5*time.Millisecond, "the burst should trigger exactly one pass")

	// Give any erroneously-scheduled extra pass a chance to appear, then re-assert.
	time.Sleep(120 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, passes, "no extra pass should run after the coalesced burst")
}

// TestTriggerUpdateRetriesWhenPassBusy verifies the critical edge case: when a
// scheduled pass is already running (runUpdatePass returns false because the
// updateRunning CAS is not acquired), the kick is NOT lost — the worker waits and
// re-runs the pass after the running one finishes.
func TestTriggerUpdateRetriesWhenPassBusy(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	var ranAfterBusy bool

	s := newTriggerTestService(t, func() bool {
		mu.Lock()
		attempts++
		n := attempts
		if n > 1 {
			ranAfterBusy = true
		}
		mu.Unlock()

		// First attempt models a scheduled pass holding the lock: report the CAS
		// miss so the worker must back off and retry.
		return n > 1
	})

	s.TriggerUpdate()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return ranAfterBusy && attempts >= 2
	}, time.Second, 5*time.Millisecond, "the worker must retry and run a fresh pass after the CAS miss")
}

// TestTriggerUpdateBufferedDuringRunningPass verifies that a kick arriving WHILE a
// pass is already running is not lost: the buffer-1 channel holds it, and the
// worker reads it on the next loop iteration and runs a second pass. This is the
// coalescing path for a push that lands mid-pass (distinct from the CAS-miss retry
// path, which models a SCHEDULED pass holding the lock).
func TestTriggerUpdateBufferedDuringRunningPass(t *testing.T) {
	var mu sync.Mutex
	var passes int
	release := make(chan struct{})
	firstStarted := make(chan struct{})

	s := newTriggerTestService(t, func() bool {
		mu.Lock()
		passes++
		n := passes
		mu.Unlock()
		if n == 1 {
			close(firstStarted) // signal the first pass is in progress
			<-release           // ...and block here until the test releases it
		}
		return true
	})

	s.TriggerUpdate()
	<-firstStarted // the first pass is now running

	// A second push lands while the first pass is still running. It must be
	// buffered (not dropped) and processed after the current pass finishes.
	s.TriggerUpdate()
	close(release)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return passes == 2
	}, time.Second, 5*time.Millisecond, "a kick during a running pass must run a second pass, not be lost")
}

// TestTriggerWorkerRecoversFromPanic verifies the webhook worker survives a panic in
// the update pass (poll parity: the poll path is protected by cron.Recover). Without
// the recover, a panic in this bare goroutine would crash the whole test binary — so
// the fact that a LATER kick still runs a pass proves recovery works.
func TestTriggerWorkerRecoversFromPanic(t *testing.T) {
	var mu sync.Mutex
	var attempts int

	s := newTriggerTestService(t, func() bool {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			panic("boom in the update pass")
		}
		return true
	})

	s.TriggerUpdate() // first pass panics; the worker must survive
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts == 1
	}, time.Second, 5*time.Millisecond, "the panicking pass must have been attempted")

	// The worker is still alive: a second kick runs a clean pass.
	s.TriggerUpdate()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts >= 2
	}, time.Second, 5*time.Millisecond, "the worker must survive the panic and run later kicks")
}

// TestRunUpdatePassCASGuard verifies that runUpdatePass reports whether it acquired
// the overlap lock: a call while a pass is already in progress returns false without
// running, which is what lets the poll timer drop overlapping ticks and the webhook
// worker know it must retry rather than silently losing the kick.
func TestRunUpdatePassCASGuard(t *testing.T) {
	s := &Service{baseCtx: context.Background()}

	// Simulate an in-progress pass by holding the CAS as runUpdatePass would.
	require.True(t, s.updateRunning.CompareAndSwap(false, true))
	require.False(t, s.runUpdatePass(), "runUpdatePass must return false when a pass is already running")
}

// TestRunUpdatePassReleasesLock pins the CAS-RELEASE contract of the real
// runUpdatePass (not the stub): after a pass completes, the updateRunning lock
// MUST be released, or every future pass — poll tick AND webhook kick — would be
// permanently wedged (the poll drops the tick, the webhook worker spins its retry
// loop forever) while the rest of the suite stays green. A test store with
// auto-update enabled and no endpoints runs a trivial pass to completion.
func TestRunUpdatePassReleasesLock(t *testing.T) {
	_, store := datastore.MustNewTestStore(t, true, false)

	settings, err := store.Settings().Settings()
	require.NoError(t, err)
	settings.ContainerAutomation.AutoUpdate.Enabled = true
	require.NoError(t, store.Settings().UpdateSettings(settings))

	s := &Service{baseCtx: context.Background(), dataStore: store}

	// First pass: acquires the lock and runs to completion (no endpoints → no work).
	require.True(t, s.runUpdatePass(), "the pass should acquire the lock and run")
	require.False(t, s.updateRunning.Load(), "the lock MUST be released after the pass")

	// A second pass must be able to acquire the lock again — proves the release,
	// not just the flag read.
	require.True(t, s.runUpdatePass(), "a subsequent pass must re-acquire the released lock")
	require.False(t, s.updateRunning.Load())
}
