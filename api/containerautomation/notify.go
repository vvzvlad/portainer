package containerautomation

import "github.com/rs/zerolog/log"

// EventKind enumerates the container-automation events surfaced to a Notifier.
// The set is intentionally small: it is the seam future milestones extend with
// real senders (Slack/email/webhook) without touching the daemon call sites.
type EventKind string

const (
	// EventUpdated is emitted after a container/stack image was updated.
	EventUpdated EventKind = "updated"
	// EventRollback is emitted after a health-gated rollback to the previous image.
	EventRollback EventKind = "rollback"
	// EventUpdateFailed is emitted when an update (or its rollback) could not be applied.
	EventUpdateFailed EventKind = "update-failed"
	// EventHealRestarted is emitted after an unhealthy container was restarted.
	EventHealRestarted EventKind = "heal-restarted"
)

// Event is a structured container-automation notification. Optional fields are
// left zero when not applicable to the event (e.g. StackID for a standalone
// update, ContainerID for a stack redeploy).
type Event struct {
	Kind        EventKind
	EndpointID  int
	ContainerID string
	StackID     int
	Image       string
	Message     string
	// Err carries the underlying error for failure events; nil otherwise.
	Err error
}

// Notifier receives container-automation events. CE has no generic notification
// subsystem, so the only implementation is logNotifier; this interface is the
// seam external senders plug into later.
type Notifier interface {
	Notify(event Event)
}

// logNotifier is the default Notifier: it emits each event as a structured log
// line. It never blocks and never errors, so it is safe to call from the daemon
// hot path.
type logNotifier struct{}

// Notify logs the event with its kind and context fields. Failure events are
// logged at warn (with the error), the rest at info.
func (logNotifier) Notify(event Event) {
	entry := log.Info()
	if event.Kind == EventUpdateFailed {
		entry = log.Warn()
		if event.Err != nil {
			entry = entry.Err(event.Err)
		}
	}

	entry = entry.Str("event", string(event.Kind)).Int("endpoint_id", event.EndpointID)
	if event.ContainerID != "" {
		entry = entry.Str("container_id", event.ContainerID)
	}
	if event.StackID != 0 {
		entry = entry.Int("stack_id", event.StackID)
	}
	if event.Image != "" {
		entry = entry.Str("image", event.Image)
	}

	message := event.Message
	if message == "" {
		message = "container automation event"
	}

	entry.Msg("container automation: " + message)
}
