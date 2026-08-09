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
// left zero when not applicable to the event (e.g. StackName for a standalone
// container that is not a compose stack member, or OldDigest/NewDigest when the
// pre/post image identity could not be resolved). Every event is per-container:
// the update path recreates each container individually, so there is no
// whole-stack redeploy event.
type Event struct {
	Kind        EventKind
	EndpointID  int
	ContainerID string
	// ContainerName is the human-readable container name (no leading slash), used
	// by the webhook message. It may be empty for events keyed only by ID.
	ContainerName string
	StackID       int
	// StackName is the compose project (stack) name a container belongs to, sourced
	// from its com.docker.compose.project label at detection time. It is set on a
	// per-container update event for a stack member so the webhook can print a
	// "Stack [name]" line without a StackID/Stack().Read round-trip; empty for
	// standalone containers.
	StackName string
	Image     string
	// OldDigest and NewDigest carry the pre/post image identities for an update
	// (image IDs, e.g. "sha256:59b9..."). They are threaded from the update call
	// site where they are known and left empty otherwise; the webhook notifier
	// short-forms them into the "old → new" part of the message.
	OldDigest string
	NewDigest string
	Message   string
	// Err carries the underlying error for failure events; nil otherwise.
	Err error
	// ServiceDown marks a failure that left NOTHING running for the container: the
	// update (or its rollback) tore the original down and could not put it back. It
	// is the one machine-readable failure distinction a consumer gets, and it is
	// deliberately narrow: false for every failure the container survived, INCLUDING
	// a restore that put it back only partially (running again, but without its
	// original name or networks). That degraded outcome is conveyed in Message only.
	ServiceDown bool
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
// logged at warn (with the error), the rest at info. A failure that left the
// container down is logged at error instead, matching the level the call site
// uses: an outage must not read like a skipped update in the daemon log.
func (logNotifier) Notify(event Event) {
	entry := log.Info()
	if event.Kind == EventUpdateFailed {
		entry = log.Warn()
		if event.ServiceDown {
			entry = log.Error()
		}

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

// multiNotifier fans an event out to several notifiers in order. It is how the
// service composes the always-on logNotifier with the optional webhookNotifier
// without either implementation having to know about the other. Each notifier is
// itself non-blocking, so multiNotifier stays safe on the daemon hot path.
type multiNotifier []Notifier

// Notify forwards the event to every wrapped notifier. Each call is isolated by
// a recover() so one misbehaving notifier can neither abort the others nor let a
// panic reach the daemon hot path; logNotifier is kept first and unchanged.
func (m multiNotifier) Notify(event Event) {
	for _, n := range m {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warn().Interface("panic", r).Msg("container automation: recovered from panic in notifier")
				}
			}()

			n.Notify(event)
		}()
	}
}
