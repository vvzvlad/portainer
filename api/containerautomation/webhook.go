package containerautomation

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/dataservices"

	"github.com/rs/zerolog/log"
)

const (
	// webhookMessagePlaceholder is the token replaced in the configured webhook
	// URL with the URL-encoded event message. When present, the notifier issues a
	// GET on the substituted URL ("message in the address"); when absent, it POSTs
	// the plain-text message as the request body.
	webhookMessagePlaceholder = "{{message}}"

	// webhookTimeout bounds each webhook HTTP call so a slow or unresponsive
	// endpoint cannot pile up goroutines. The call already runs off the hot path.
	webhookTimeout = 10 * time.Second

	// shortDigestLen is how many leading hex characters of an image digest the
	// message keeps (matches the maintainer's example, e.g. "59b94983c73a").
	shortDigestLen = 12
)

// webhookNotifier delivers container-automation events to a user-configured HTTP
// endpoint. It reads the current webhook URL from the datastore on every event
// so a settings change takes effect without a restart, formats a human-readable
// message, and performs the HTTP call in a background goroutine so a slow or
// broken endpoint never delays or fails the daemon hot path.
type webhookNotifier struct {
	dataStore dataservices.DataStore
	client    *http.Client
}

// newWebhookNotifier builds a webhookNotifier bound to the datastore. The HTTP
// client carries the per-call timeout so a request cannot hang indefinitely.
func newWebhookNotifier(dataStore dataservices.DataStore) webhookNotifier {
	return webhookNotifier{
		dataStore: dataStore,
		client:    &http.Client{Timeout: webhookTimeout},
	}
}

// Notify reads the current webhook URL and, when set, dispatches the event in a
// background goroutine. Only the settings read and the empty-URL short-circuit
// run synchronously (they decide whether to spawn at all); message formatting —
// which itself reads Endpoint()/Stack() from the datastore — and the HTTP call
// both happen off the daemon hot path, under a single recover(). It never blocks
// the caller and never returns an error: the webhook is strictly best-effort.
func (n webhookNotifier) Notify(event Event) {
	settings, err := n.dataStore.Settings().Settings()
	if err != nil {
		log.Warn().Err(err).Msg("container automation webhook: unable to read settings, skipping notification")
		return
	}

	webhookURL := strings.TrimSpace(settings.ContainerAutomation.Notification.WebhookURL)
	if webhookURL == "" {
		return
	}

	// Best-effort delivery: never block or fail the caller (the update/heal hot
	// path). Everything below — the env/stack datastore reads in formatMessage and
	// the bounded HTTP call — runs in its own goroutine, and any panic there is
	// recovered so it can never crash the daemon.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Warn().Interface("panic", r).Msg("container automation webhook: recovered from panic during delivery")
			}
		}()

		message := n.formatMessage(settings, event)
		n.deliver(webhookURL, message)
	}()
}

// deliver performs the HTTP call for a single event. It is always invoked from
// the Notify goroutine (which recovers any panic), so a broken endpoint can
// never block or crash the daemon.
func (n webhookNotifier) deliver(webhookURL, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
	defer cancel()

	var (
		req *http.Request
		err error
	)

	if strings.Contains(webhookURL, webhookMessagePlaceholder) {
		// Substitution mode: replace the placeholder with the URL-encoded message
		// and GET the resulting address (the maintainer's "message in the URL").
		target := strings.ReplaceAll(webhookURL, webhookMessagePlaceholder, url.QueryEscape(message))
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	} else {
		// No placeholder: POST the plain-text message as the body, useful for
		// generic POST-style webhooks.
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(message))
		if err == nil {
			req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		}
	}

	if err != nil {
		log.Warn().Err(err).Msg("container automation webhook: unable to build request")
		return
	}

	resp, err := n.client.Do(req)
	if err != nil {
		log.Warn().Err(err).Msg("container automation webhook: delivery failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		log.Warn().Int("status", resp.StatusCode).Msg("container automation webhook: endpoint returned an error status")
	}
}

// formatMessage builds the human-readable message for an event. It resolves the
// environment name from the endpoint and the stack name from the stack via the
// datastore, mirroring the maintainer's example:
//
//	Environment | nebula.lc
//	Stack [cache-demo]
//	Update [esphome]: 59b94983c73a → 2231ca5d676d
//
// The context line is the stack for stack-scoped events, otherwise the container;
// the action line is adapted per event kind (update / rollback / update-failed /
// auto-heal restart). Auto-heal renders as:
//
//	Environment | nebula.lc
//	Container [nginx]
//	Auto-heal: restarted unhealthy container
func (n webhookNotifier) formatMessage(settings *portainer.Settings, event Event) string {
	lines := []string{"Environment | " + n.environmentName(event.EndpointID)}

	// Context line: the stack for stack-scoped events, otherwise the container.
	switch {
	case event.StackID != 0:
		lines = append(lines, fmt.Sprintf("Stack [%s]", n.stackName(event.StackID)))
	case event.ContainerName != "":
		lines = append(lines, fmt.Sprintf("Container [%s]", event.ContainerName))
	}

	// Subject for the action line: the container name when known, else the stack
	// name, else a short container id.
	subject := event.ContainerName
	if subject == "" && event.StackID != 0 {
		subject = n.stackName(event.StackID)
	}
	if subject == "" {
		subject = shortDigest(event.ContainerID)
	}

	switch event.Kind {
	case EventUpdated:
		if event.OldDigest != "" && event.NewDigest != "" {
			lines = append(lines, fmt.Sprintf("Update [%s]: %s → %s", subject, shortDigest(event.OldDigest), shortDigest(event.NewDigest)))
		} else {
			lines = append(lines, fmt.Sprintf("Update [%s]: image updated", subject))
		}
	case EventRollback:
		lines = append(lines, fmt.Sprintf("Rollback [%s]: rolled back to previous image after failed health check", subject))
	case EventUpdateFailed:
		line := fmt.Sprintf("Update failed [%s]", subject)
		if event.Message != "" {
			line += ": " + event.Message
		}
		if event.Err != nil {
			line += fmt.Sprintf(" (%s)", event.Err)
		}
		lines = append(lines, line)
	case EventHealRestarted:
		lines = append(lines, "Auto-heal: restarted unhealthy container")
	default:
		if event.Message != "" {
			lines = append(lines, event.Message)
		}
	}

	return strings.Join(lines, "\n")
}

// environmentName resolves an endpoint id to its display name, degrading to a
// "#<id>" placeholder when the endpoint cannot be read (deleted, or a zero id).
func (n webhookNotifier) environmentName(endpointID int) string {
	if endpointID == 0 {
		return "unknown"
	}

	endpoint, err := n.dataStore.Endpoint().Endpoint(portainer.EndpointID(endpointID))
	if err != nil || endpoint == nil {
		return fmt.Sprintf("#%d", endpointID)
	}

	return endpoint.Name
}

// stackName resolves a stack id to its name, degrading to a "#<id>" placeholder
// when the stack cannot be read.
func (n webhookNotifier) stackName(stackID int) string {
	stack, err := n.dataStore.Stack().Read(portainer.StackID(stackID))
	if err != nil || stack == nil {
		return fmt.Sprintf("#%d", stackID)
	}

	return stack.Name
}

// shortDigest trims an image id/digest to a short, human-friendly hex form
// (shortDigestLen chars), matching the maintainer's example. It drops a leading
// "sha256:" algorithm prefix so "sha256:59b94983c73a..." -> "59b94983c73a".
func shortDigest(s string) string {
	if s == "" {
		return ""
	}

	if i := strings.LastIndex(s, "sha256:"); i >= 0 {
		s = s[i+len("sha256:"):]
	}

	if len(s) > shortDigestLen {
		return s[:shortDigestLen]
	}

	return s
}
