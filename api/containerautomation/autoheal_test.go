package containerautomation

import (
	"context"
	"testing"
	"time"

	portainer "github.com/portainer/portainer/api"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

func TestDecideRestart(t *testing.T) {
	policy := retryPolicy{
		maxRetries: 3,
		window:     10 * time.Minute,
		cooldown:   60 * time.Second,
	}
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	t.Run("first restart on empty state", func(t *testing.T) {
		ok, state := decideRestart(retryState{}, policy, base)
		if !ok {
			t.Fatal("expected restart on first unhealthy observation")
		}
		if state.attempts != 1 {
			t.Errorf("attempts = %d, want 1", state.attempts)
		}
		if !state.windowStart.Equal(base) || !state.lastRestart.Equal(base) {
			t.Error("windowStart/lastRestart should be set to now")
		}
	})

	t.Run("blocked during cooldown", func(t *testing.T) {
		_, state := decideRestart(retryState{}, policy, base)
		ok, _ := decideRestart(state, policy, base.Add(30*time.Second))
		if ok {
			t.Error("expected restart to be blocked within cooldown")
		}
	})

	t.Run("allowed after cooldown", func(t *testing.T) {
		_, state := decideRestart(retryState{}, policy, base)
		ok, state := decideRestart(state, policy, base.Add(61*time.Second))
		if !ok {
			t.Error("expected restart allowed after cooldown")
		}
		if state.attempts != 2 {
			t.Errorf("attempts = %d, want 2", state.attempts)
		}
	})

	t.Run("max retries enforced within window", func(t *testing.T) {
		state := retryState{}
		now := base
		allowed := 0
		for i := 0; i < 6; i++ {
			ok, newState := decideRestart(state, policy, now)
			state = newState
			if ok {
				allowed++
			}
			now = now.Add(policy.cooldown + time.Second)
		}
		if allowed != policy.maxRetries {
			t.Errorf("allowed %d restarts, want %d (max per window)", allowed, policy.maxRetries)
		}
	})

	t.Run("counter resets after window elapses", func(t *testing.T) {
		state := retryState{attempts: 3, windowStart: base, lastRestart: base}
		ok, newState := decideRestart(state, policy, base.Add(policy.window+time.Second))
		if !ok {
			t.Error("expected restart allowed once the window elapsed")
		}
		if newState.attempts != 1 {
			t.Errorf("attempts = %d, want 1 after window reset", newState.attempts)
		}
	})
}

func TestPruneRetries(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	s := &Service{retries: map[string]retryState{
		// within the window -> retained
		"fresh": {attempts: 1, windowStart: now.Add(-time.Minute), lastRestart: now.Add(-time.Minute)},
		// exactly at the window boundary -> pruned
		"edge": {attempts: 2, windowStart: now.Add(-retryWindow), lastRestart: now.Add(-retryWindow)},
		// long past the window -> pruned
		"stale": {attempts: 3, windowStart: now.Add(-2 * retryWindow), lastRestart: now.Add(-2 * retryWindow)},
	}}

	s.pruneRetries(now)

	if _, ok := s.retries["fresh"]; !ok {
		t.Error("entry within the retry window should be retained")
	}
	if _, ok := s.retries["edge"]; ok {
		t.Error("entry exactly at the window boundary should be pruned")
	}
	if _, ok := s.retries["stale"]; ok {
		t.Error("entry past the retry window should be pruned")
	}
}

// TestRetryStateSurvivesStartingTick locks in the F1 fix: a container that flaps
// through "starting" right after a restart (and so briefly drops out of the
// health=unhealthy filter) must keep its retry accounting across the tick where
// it is not observed, otherwise the cooldown / max-retries storm guard is
// defeated and the next unhealthy observation triggers an immediate restart.
func TestRetryStateSurvivesStartingTick(t *testing.T) {
	policy := retryPolicy{maxRetries: 3, window: retryWindow, cooldown: restartCooldown}
	const id = "flapper"
	s := &Service{retries: make(map[string]retryState)}

	t0 := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	// Tick 1: container is unhealthy -> first restart.
	ok, state := decideRestart(s.getRetry(id), policy, t0)
	s.setRetry(id, state)
	if !ok || state.attempts != 1 {
		t.Fatalf("tick 1: ok=%v attempts=%d, want restart with attempts=1", ok, state.attempts)
	}

	// Tick 2 (t0+30s): the container is "starting" and not in the unhealthy list.
	// Prune must NOT drop its state because the window has not elapsed.
	s.pruneRetries(t0.Add(30 * time.Second))
	if _, kept := s.retries[id]; !kept {
		t.Fatal("tick 2: retry state was pruned while the container was 'starting'")
	}

	// Tick 3 (t0+45s): unhealthy again, still within the cooldown. The surviving
	// state must block the restart and the attempt count must not be reset.
	ok, state = decideRestart(s.getRetry(id), policy, t0.Add(45*time.Second))
	s.setRetry(id, state)
	if ok {
		t.Error("tick 3: restart should be blocked by the surviving cooldown")
	}
	if state.attempts != 1 {
		t.Errorf("tick 3: attempts = %d, want 1 (state survived, not reset)", state.attempts)
	}
}

// TestHealContainersSkipsContainersHeldByAnUpdate locks in the auto-heal side of
// the interlock with auto-update. While an update pass holds a container
// (recreate -> health gate -> rollback), auto-heal must not restart it: a restart
// mid-gate resets Docker health to "starting", so the gate keeps waiting instead
// of rolling back and a failed update can be accepted as a good one.
//
// Both keys are covered. A hold by id is the ordinary case; a hold by NAME is the
// one the recreate window produces, where the running container is brand new, its
// id has never been seen by anybody, and the name is the only thing tying it to
// the update in progress.
//
// A hold also has to be pinned across ticks, not merely honoured on the first
// one: it spans the whole recreate plus the gate window, i.e. many heal ticks,
// and auto-heal must suppress every one of them. Folding the lookup and the
// once-per-hold log claim into one condition would honour only the first tick
// and restart the container mid-gate on every tick after it — the exact
// failure this interlock exists to prevent.
//
// The skip is also free: it happens before the retry accounting, so a held tick
// does not consume the container's restart budget and healing resumes with a full
// budget once the hold is released. Holds are endpoint-scoped, so the same
// container on another environment is unaffected.
func TestHealContainersSkipsContainersHeldByAnUpdate(t *testing.T) {
	const (
		containerID    = "c1"
		name           = "web"
		healEndpointID = portainer.EndpointID(1)
	)

	tests := []struct {
		name string
		// holdEndpoint is the environment the hold is taken for; healing always runs
		// against healEndpointID.
		holdEndpoint portainer.EndpointID
		// byName takes the hold on the container's NAME instead of its id, leaving the
		// id unheld — the state the recreate window produces.
		byName bool
		// ticksUnderHold is how many heal passes run while the hold is still taken. A
		// hold spans the whole recreate plus the gate window — many ticks — and every
		// one of them must be suppressed, so more than one pass here is not padding: it
		// is the only way to catch a skip that is honoured once and then dropped.
		ticksUnderHold int
		// releaseAndReheal releases the hold after those passes and runs one more, as
		// auto-heal would do on its next tick once the update finished.
		releaseAndReheal bool
		wantRestarts     int
		wantAttempts     int
	}{
		{
			name:           "a held container is neither restarted nor charged for the tick",
			holdEndpoint:   healEndpointID,
			ticksUnderHold: 1,
			wantRestarts:   0,
			wantAttempts:   0,
		},
		{
			name:           "a hold suppresses every tick it spans, not just the first",
			holdEndpoint:   healEndpointID,
			ticksUnderHold: 3,
			wantRestarts:   0,
			wantAttempts:   0,
		},
		{
			name:           "a container whose name is held is skipped even though its id is not",
			holdEndpoint:   healEndpointID,
			byName:         true,
			ticksUnderHold: 3,
			wantRestarts:   0,
			wantAttempts:   0,
		},
		{
			name:             "healing resumes with a full budget once the hold is released",
			holdEndpoint:     healEndpointID,
			ticksUnderHold:   1,
			releaseAndReheal: true,
			wantRestarts:     1,
			wantAttempts:     1,
		},
		{
			name:           "a hold on another environment does not suppress healing here",
			holdEndpoint:   healEndpointID + 1,
			ticksUnderHold: 1,
			wantRestarts:   1,
			wantAttempts:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seq := &callSeq{}
			cli := newFakeDockerClient(seq)

			s := &Service{
				baseCtx:     context.Background(),
				notifier:    &seqNotifier{seq: seq},
				retries:     map[string]retryState{},
				updateHolds: map[string]updateHold{},
			}

			endpoint := &portainer.Endpoint{ID: healEndpointID}
			containers := []container.Summary{{ID: containerID, Names: []string{"/" + name}}}

			acquire, release := s.acquireUpdateHold, s.releaseUpdateHold
			held := containerID
			if tt.byName {
				acquire, release = s.acquireUpdateHoldByName, s.releaseUpdateHoldByName
				held = name
			}

			acquire(tt.holdEndpoint, held)

			for range tt.ticksUnderHold {
				s.healContainers(cli, endpoint, ScopeAll, containers)
			}

			if tt.holdEndpoint == healEndpointID {
				// A hold on THIS environment covers every tick it spans, so after all of
				// them nothing may have happened yet. Assert it BEFORE any release below:
				// afterwards the restart cooldown would deny the extra pass anyway, and the
				// totals at the end would come out the same whether the hold was honoured
				// or ignored.
				require.Empty(t, seq.snapshot(),
					"a hold suppresses every tick it spans, not just the first")
			}

			if tt.releaseAndReheal {
				release(tt.holdEndpoint, held)
				s.healContainers(cli, endpoint, ScopeAll, containers)
			}

			require.Equal(t, tt.wantRestarts, countPrefix(seq.snapshot(), "restart:"),
				"a held container must not be restarted while an update acts on it")
			require.Equal(t, tt.wantAttempts, s.getRetry(containerID).attempts,
				"the skip happens before the retry accounting, so a held tick costs no restart budget")
		})
	}
}
