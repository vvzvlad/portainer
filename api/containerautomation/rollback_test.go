package containerautomation

import (
	"context"
	"errors"
	"testing"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/docker"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
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

func TestEffectiveRollbackDeadline(t *testing.T) {
	start := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	timeout := 120 * time.Second

	tests := []struct {
		name        string
		startPeriod time.Duration
		want        time.Time
	}{
		{
			name:        "no start period uses the timeout",
			startPeriod: 0,
			want:        start.Add(timeout),
		},
		{
			name:        "start period shorter than timeout uses the timeout",
			startPeriod: 30 * time.Second,
			want:        start.Add(timeout),
		},
		{
			name:        "start period longer than timeout extends to start period plus buffer",
			startPeriod: 300 * time.Second,
			want:        start.Add(300*time.Second + startPeriodBuffer),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveRollbackDeadline(start, timeout, tt.startPeriod); !got.Equal(tt.want) {
				t.Errorf("effectiveRollbackDeadline() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDecideRollbackWithLongStartPeriod proves the F3 fix end to end at the
// decision layer: with a start_period longer than the configured rollback
// timeout, the start-period-aware deadline keeps a still-starting container
// alive while it is within the start period, and only rolls back after it.
func TestDecideRollbackWithLongStartPeriod(t *testing.T) {
	start := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	timeout := 60 * time.Second
	startPeriod := 300 * time.Second

	deadline := effectiveRollbackDeadline(start, timeout, startPeriod)

	starting := containerHealth{Running: true, Status: string(container.Starting)}

	// Past the bare timeout but still within the start period: keep waiting.
	if got := decideRollback(starting, start.Add(120*time.Second), deadline); got != rollbackContinue {
		t.Errorf("within start_period: decideRollback() = %v, want rollbackContinue", got)
	}

	// After the start period (plus buffer): roll back.
	if got := decideRollback(starting, start.Add(330*time.Second), deadline); got != rollbackTrigger {
		t.Errorf("after start_period: decideRollback() = %v, want rollbackTrigger", got)
	}
}

func TestInspectErrorTolerated(t *testing.T) {
	tests := []struct {
		name        string
		consecutive int
		want        bool
	}{
		{name: "first transient error is tolerated", consecutive: 1, want: true},
		{name: "second consecutive error is tolerated", consecutive: 2, want: true},
		{name: "at the threshold is still tolerated", consecutive: maxConsecutiveInspectErrors, want: true},
		{name: "beyond the threshold is a failure", consecutive: maxConsecutiveInspectErrors + 1, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inspectErrorTolerated(tt.consecutive); got != tt.want {
				t.Errorf("inspectErrorTolerated(%d) = %v, want %v", tt.consecutive, got, tt.want)
			}
		})
	}
}

func TestIsTagReference(t *testing.T) {
	const digest = "sha256:02c921df998f95e849058af14de7045efc3954d90320967418a0d1f182bbc0b2"

	tests := []struct {
		name string
		ref  string
		want bool
	}{
		{name: "tagged reference is rollbackable", ref: "nginx:1.21", want: true},
		{name: "untagged reference (implicit latest) is rollbackable", ref: "nginx", want: true},
		{name: "fully-qualified tagged reference is rollbackable", ref: "registry.example.com/team/app:v2", want: true},
		{name: "digest-pinned reference cannot be re-tagged", ref: "nginx@" + digest, want: false},
		{name: "tagged-and-digest-pinned reference cannot be re-tagged", ref: "nginx:1.21@" + digest, want: false},
		{name: "algorithm-prefixed bare image id cannot be re-tagged", ref: digest, want: false},
		{name: "full bare hex image id cannot be re-tagged", ref: "02c921df998f95e849058af14de7045efc3954d90320967418a0d1f182bbc0b2", want: false},
		{name: "empty reference is not rollbackable", ref: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTagReference(tt.ref); got != tt.want {
				t.Errorf("isTagReference(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

func TestSkipUnnamedForRollback(t *testing.T) {
	tests := []struct {
		name     string
		rollback bool
		cName    string
		want     bool
	}{
		{name: "rollback on, unnamed -> skip (unsuppressable loop otherwise)", rollback: true, cName: "", want: true},
		{name: "rollback on, named -> proceed (guard can key it)", rollback: true, cName: "web", want: false},
		{name: "rollback off, unnamed -> proceed (no rollback to loop)", rollback: false, cName: "", want: false},
		{name: "rollback off, named -> proceed", rollback: false, cName: "web", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skipUnnamedForRollback(tt.rollback, tt.cName); got != tt.want {
				t.Errorf("skipUnnamedForRollback(%v, %q) = %v, want %v", tt.rollback, tt.cName, got, tt.want)
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

func TestDecideUpdateSkip(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	cooldown := 24 * time.Hour

	tests := []struct {
		name          string
		rec           rolledBackTarget
		currentDigest string
		want          bool
	}{
		{
			name:          "same digest within cooldown is skipped",
			rec:           rolledBackTarget{digest: "sha256:aaa", at: now.Add(-1 * time.Hour)},
			currentDigest: "sha256:aaa",
			want:          true,
		},
		{
			name:          "new digest within cooldown is not skipped",
			rec:           rolledBackTarget{digest: "sha256:aaa", at: now.Add(-1 * time.Hour)},
			currentDigest: "sha256:bbb",
			want:          false,
		},
		{
			name:          "same digest after cooldown is not skipped",
			rec:           rolledBackTarget{digest: "sha256:aaa", at: now.Add(-25 * time.Hour)},
			currentDigest: "sha256:aaa",
			want:          false,
		},
		{
			name:          "unknown recorded digest is skipped conservatively within cooldown",
			rec:           rolledBackTarget{digest: "", at: now.Add(-1 * time.Hour)},
			currentDigest: "sha256:aaa",
			want:          true,
		},
		{
			name:          "unknown recorded digest after cooldown is not skipped",
			rec:           rolledBackTarget{digest: "", at: now.Add(-25 * time.Hour)},
			currentDigest: "sha256:aaa",
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideUpdateSkip(tt.rec, tt.currentDigest, now, cooldown); got != tt.want {
				t.Errorf("decideUpdateSkip() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPruneRolledBack locks in the F8 fix: pruneRolledBack must iterate the
// rolledBack map and drop only entries whose cooldown has fully elapsed, keeping
// fresh ones, so the map cannot grow unbounded. It mirrors TestPruneRetries. The
// boundary is inclusive (production uses now.Sub(at) >= updateRollbackCooldown),
// so an entry exactly at the cooldown is pruned.
func TestPruneRolledBack(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	s := &Service{rolledBack: map[string]rolledBackTarget{
		// within the cooldown -> retained
		"fresh": {ref: "img:fresh", digest: "sha256:aaa", at: now.Add(-updateRollbackCooldown / 2)},
		// exactly at the cooldown boundary -> pruned (>= is inclusive)
		"edge": {ref: "img:edge", digest: "sha256:bbb", at: now.Add(-updateRollbackCooldown)},
		// long past the cooldown -> pruned
		"stale": {ref: "img:stale", digest: "sha256:ccc", at: now.Add(-2 * updateRollbackCooldown)},
	}}

	s.pruneRolledBack(now)

	if _, ok := s.rolledBack["fresh"]; !ok {
		t.Error("entry within the rollback cooldown should be retained")
	}
	if _, ok := s.rolledBack["edge"]; ok {
		t.Error("entry exactly at the cooldown boundary should be pruned")
	}
	if _, ok := s.rolledBack["stale"]; ok {
		t.Error("entry past the rollback cooldown should be pruned")
	}
	if len(s.rolledBack) != 1 {
		t.Errorf("rolledBack length = %d, want 1", len(s.rolledBack))
	}
}

// TestRollbackReportsAContainerLeftDown is the rollback-path twin of
// TestUpdateStandaloneReportsAContainerLeftDown. A rollback recreate that merely
// fails leaves the unhealthy-but-live container in place; a *docker.RestoreError
// means the rollback tore that container down and could not put anything back,
// so the notification must not promise a still-serving container.
func TestRollbackReportsAContainerLeftDown(t *testing.T) {
	recreateErr := errors.New("start container error: boom")

	tests := []struct {
		name            string
		err             error
		wantMessage     string
		wantServiceDown bool
	}{
		{
			name:        "an ordinary rollback failure leaves the unhealthy container running",
			err:         recreateErr,
			wantMessage: "rollback failed: could not recreate on previous image",
		},
		{
			name: "a failed restore leaves the container down",
			err: &docker.RestoreError{
				ContainerID: "new-id",
				Name:        "/web",
				Cause:       recreateErr,
				Errs:        []error{errors.New("start container error: still boom")},
			},
			wantMessage:     "rollback failed and the container is left down, manual intervention required",
			wantServiceDown: true,
		},
		{
			name: "a partial restore leaves the container running off one of its networks",
			err: &docker.RestoreError{
				ContainerID:     "new-id",
				Name:            "/web",
				Cause:           recreateErr,
				Errs:            []error{errors.New("connect container to network net-a error: boom")},
				OriginalRunning: true,
			},
			wantMessage: "rollback failed and the container was only partially restored (name or networks), manual intervention required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seq := &callSeq{}
			notif := &seqNotifier{seq: seq}

			s := &Service{
				baseCtx:          context.Background(),
				containerService: &fakeRecreator{seq: seq, err: tt.err},
				notifier:         notif,
				rolledBack:       map[string]rolledBackTarget{},
			}

			s.rollback(newFakeDockerClient(seq), &portainer.Endpoint{ID: 1}, "new-id", "sha256:old", "nginx:1.21", "web", "")

			event, n := notif.only(EventUpdateFailed)
			require.Equal(t, 1, n, "exactly one update-failed event")
			require.Equal(t, tt.wantMessage, event.Message)
			require.ErrorIs(t, event.Err, tt.err, "the event carries the failure itself")
			require.Equal(t, tt.wantServiceDown, event.ServiceDown,
				"ServiceDown is what tells a consumer the two failure outcomes apart, the wording is not machine-readable")

			_, rollbacks := notif.only(EventRollback)
			require.Zero(t, rollbacks, "a failed rollback never reports success")
		})
	}
}
