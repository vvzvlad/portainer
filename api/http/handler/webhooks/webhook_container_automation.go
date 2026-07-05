package webhooks

import (
	"crypto/subtle"
	"errors"
	"net/http"

	httperror "github.com/portainer/portainer/pkg/libhttp/error"
	"github.com/portainer/portainer/pkg/libhttp/request"
)

// @summary Trigger a container auto-update pass
// @description Inbound registry-push webhook: any POST with the configured token
// @description immediately runs a full native container auto-update pass (the same
// @description pass the poll timer runs), so a container updates within seconds of a
// @description registry push instead of waiting for the next poll. The payload is not
// @description parsed — the pass itself compares digests and updates only what is
// @description stale, so it is registry-agnostic (Gitea, GHCR, Docker Hub, ...).
// @description **Access policy**: public (guarded by the secret token)
// @tags webhooks
// @param token path string true "Auto-update webhook token"
// @success 202 "Update pass triggered"
// @failure 404 "Unknown or empty token"
// @failure 409 "Container auto-update is disabled"
// @failure 500 "Server error"
// @router /webhooks/container-automation/{token} [post]
func (handler *Handler) webhookContainerAutomation(w http.ResponseWriter, r *http.Request) *httperror.HandlerError {
	// An empty/absent token is reported as such via the error; treat it as an empty
	// token so it falls through to the constant-time compare below and yields a 404
	// (never a 500), matching how a wrong token is handled.
	token, _ := request.RetrieveRouteVariableValue(r, "token")

	settings, err := handler.DataStore.Settings().Settings()
	if err != nil {
		return httperror.InternalServerError("Unable to retrieve the settings from the database", err)
	}

	autoUpdate := settings.ContainerAutomation.AutoUpdate

	// Compare in constant time and treat an empty configured token as "endpoint
	// disabled": without this an empty request token would match an empty configured
	// token and let anyone trigger a pass. A mismatch (or empty token) is a 404 so the
	// endpoint does not reveal whether a token is configured.
	expected := autoUpdate.WebhookToken
	if expected == "" || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		return httperror.NotFound("Invalid webhook token", errors.New("invalid container-automation webhook token"))
	}

	// The token is valid but auto-update is turned off: report a conflict rather than
	// silently accepting a kick that would never do anything.
	if !autoUpdate.Enabled {
		return httperror.Conflict("Container auto-update is disabled", errors.New("container auto-update is disabled"))
	}

	if handler.ContainerAutomationService == nil {
		return httperror.InternalServerError("Container automation service is not available", errors.New("container automation service not wired"))
	}

	// Fire-and-forget: the service coalesces/debounces bursts and runs the pass
	// asynchronously, so respond 202 Accepted immediately with no body.
	handler.ContainerAutomationService.TriggerUpdate()

	w.WriteHeader(http.StatusAccepted)
	return nil
}
