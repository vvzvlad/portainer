package containerautomation

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	portainer "github.com/portainer/portainer/api"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// recordingNotifier captures emitted events for assertions in tests.
type recordingNotifier struct {
	events []Event
}

func (r *recordingNotifier) Notify(event Event) {
	r.events = append(r.events, event)
}

func TestLogNotifierDoesNotPanic(t *testing.T) {
	n := logNotifier{}

	// Every event kind, including a failure carrying an error, must log without
	// panicking and without requiring any optional field.
	n.Notify(Event{Kind: EventUpdated, EndpointID: 1, ContainerID: "abc", Image: "nginx:latest"})
	n.Notify(Event{Kind: EventUpdated, EndpointID: 1, StackID: 7})
	n.Notify(Event{Kind: EventRollback, EndpointID: 2, ContainerID: "def", Image: "nginx:1.0"})
	n.Notify(Event{Kind: EventHealRestarted, EndpointID: 3, ContainerID: "ghi"})
	n.Notify(Event{Kind: EventUpdateFailed, EndpointID: 4, ContainerID: "jkl", Err: errors.New("boom")})
	n.Notify(Event{Kind: EventUpdateFailed, EndpointID: 4}) // failure without an error
	// A failure that left nothing running takes the error level branch.
	n.Notify(Event{Kind: EventUpdateFailed, EndpointID: 4, ContainerID: "jkl", Err: errors.New("boom"), ServiceDown: true})
	n.Notify(Event{}) // zero value
}

// TestLogNotifierLevelsAFailureThatLeftNothingRunning pins the LEVEL, not just
// the absence of a panic: an outage that reads like an ordinary skipped update
// is exactly the #36 complaint, and a log line filtered at warn is a log line
// nobody sees. The global zerolog logger is swapped for the duration, which is
// safe because no test in this package runs in parallel.
func TestLogNotifierLevelsAFailureThatLeftNothingRunning(t *testing.T) {
	var buf bytes.Buffer

	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	tests := []struct {
		name      string
		event     Event
		wantLevel string
	}{
		{
			name:      "a failure the container survived is a warning",
			event:     Event{Kind: EventUpdateFailed, EndpointID: 1, ContainerID: "abc", Err: errors.New("boom")},
			wantLevel: `"level":"warn"`,
		},
		{
			name:      "a failure that left nothing running is an error",
			event:     Event{Kind: EventUpdateFailed, EndpointID: 1, ContainerID: "abc", Err: errors.New("boom"), ServiceDown: true},
			wantLevel: `"level":"error"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			logNotifier{}.Notify(tt.event)

			if got := buf.String(); !strings.Contains(got, tt.wantLevel) {
				t.Errorf("logged %s, want a line containing %s", got, tt.wantLevel)
			}
		})
	}
}

func TestRecordingNotifierCapturesEvents(t *testing.T) {
	r := &recordingNotifier{}
	r.Notify(Event{Kind: EventUpdated, EndpointID: 1})
	r.Notify(Event{Kind: EventRollback, EndpointID: 1})

	if len(r.events) != 2 {
		t.Fatalf("captured %d events, want 2", len(r.events))
	}
	if r.events[0].Kind != EventUpdated || r.events[1].Kind != EventRollback {
		t.Errorf("unexpected event kinds: %v, %v", r.events[0].Kind, r.events[1].Kind)
	}
}

// panicNotifier always panics, standing in for a misbehaving notifier.
type panicNotifier struct{}

func (panicNotifier) Notify(Event) {
	panic("boom")
}

// TestMultiNotifierIsolatesPanics verifies a panicking notifier neither aborts
// the sibling notifiers nor lets the panic reach the caller.
func TestMultiNotifierIsolatesPanics(t *testing.T) {
	before := &recordingNotifier{}
	after := &recordingNotifier{}

	m := multiNotifier{before, panicNotifier{}, after}

	// Must not panic even though a wrapped notifier does.
	m.Notify(Event{Kind: EventUpdated, EndpointID: 1})

	if len(before.events) != 1 {
		t.Errorf("notifier before the panicking one got %d events, want 1", len(before.events))
	}
	if len(after.events) != 1 {
		t.Errorf("notifier after the panicking one got %d events, want 1 (panic must not abort the loop)", len(after.events))
	}
}

func TestAutomationEnabledForEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint *portainer.Endpoint
		want     bool
	}{
		{name: "nil endpoint is not enabled", endpoint: nil, want: false},
		{name: "default (zero value) participates", endpoint: &portainer.Endpoint{}, want: true},
		{name: "explicitly disabled opts out", endpoint: &portainer.Endpoint{ContainerAutomationDisabled: true}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AutomationEnabledForEndpoint(tt.endpoint); got != tt.want {
				t.Errorf("AutomationEnabledForEndpoint() = %v, want %v", got, tt.want)
			}
		})
	}
}
