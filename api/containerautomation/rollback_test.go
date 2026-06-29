package containerautomation

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

func TestDecideRollback(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(120 * time.Second)

	tests := []struct {
		name   string
		health containerHealth
		at     time.Time
		want   rollbackOutcome
	}{
		{
			name:   "healthy within the window accepts the update",
			health: containerHealth{Running: true, Status: string(container.Healthy)},
			at:     now.Add(10 * time.Second),
			want:   rollbackHealthy,
		},
		{
			name:   "unhealthy triggers an immediate rollback",
			health: containerHealth{Running: true, Status: string(container.Unhealthy)},
			at:     now.Add(10 * time.Second),
			want:   rollbackTrigger,
		},
		{
			name:   "still starting before the deadline keeps polling",
			health: containerHealth{Running: true, Status: string(container.Starting)},
			at:     now.Add(10 * time.Second),
			want:   rollbackContinue,
		},
		{
			name:   "still starting past the deadline rolls back",
			health: containerHealth{Running: true, Status: string(container.Starting)},
			at:     now.Add(121 * time.Second),
			want:   rollbackTrigger,
		},
		{
			name:   "starting exactly at the deadline rolls back",
			health: containerHealth{Running: true, Status: string(container.Starting)},
			at:     deadline,
			want:   rollbackTrigger,
		},
		{
			name:   "exited container rolls back even before the deadline",
			health: containerHealth{Running: false, Status: string(container.Starting)},
			at:     now.Add(5 * time.Second),
			want:   rollbackTrigger,
		},
		{
			name:   "unhealthy wins over a stopped state",
			health: containerHealth{Running: false, Status: string(container.Unhealthy)},
			at:     now.Add(5 * time.Second),
			want:   rollbackTrigger,
		},
		{
			name:   "healthy wins even past the deadline",
			health: containerHealth{Running: true, Status: string(container.Healthy)},
			at:     now.Add(200 * time.Second),
			want:   rollbackHealthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideRollback(tt.health, tt.at, deadline); got != tt.want {
				t.Errorf("decideRollback() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasHealthGate(t *testing.T) {
	tests := []struct {
		name string
		hc   *container.HealthConfig
		want bool
	}{
		{name: "nil config has no gate", hc: nil, want: false},
		{name: "empty test inherits, no usable gate", hc: &container.HealthConfig{Test: nil}, want: false},
		{name: "explicit NONE disables the gate", hc: &container.HealthConfig{Test: []string{"NONE"}}, want: false},
		{name: "CMD healthcheck yields a gate", hc: &container.HealthConfig{Test: []string{"CMD", "curl", "-f", "localhost"}}, want: true},
		{name: "CMD-SHELL healthcheck yields a gate", hc: &container.HealthConfig{Test: []string{"CMD-SHELL", "exit 0"}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasHealthGate(tt.hc); got != tt.want {
				t.Errorf("hasHealthGate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseRollbackTimeout(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "valid duration", raw: "90s", want: 90 * time.Second},
		{name: "empty falls back to default", raw: "", want: defaultRollbackTimeout},
		{name: "unparseable falls back to default", raw: "nope", want: defaultRollbackTimeout},
		{name: "zero falls back to default", raw: "0s", want: defaultRollbackTimeout},
		{name: "negative falls back to default", raw: "-5s", want: defaultRollbackTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRollbackTimeout(tt.raw); got != tt.want {
				t.Errorf("parseRollbackTimeout(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
