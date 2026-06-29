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
