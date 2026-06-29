package containerautomation

import (
	"testing"
	"time"
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
