package containerautomation

import (
	"errors"
	"testing"

	portainer "github.com/portainer/portainer/api"
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
	n.Notify(Event{})                                       // zero value
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
